package publish_test

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// parseTestPEM parses the PKCS#1 PEM GitHub issues for App keys.
func parseTestPEM(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("key is not PEM")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// newLiveMinter builds a Minter against the real GitHub API from the
// opt-in live-test environment, skipping when it is not set (issue #80
// acceptance 1: CI documents these as Not run). It needs an
// already-registered App (registration requires a browser).
func newLiveMinter(t *testing.T) (m *publish.Minter, repo string, profile domain.AutomationTrustProfile) {
	t.Helper()
	if os.Getenv("FREESIDE_PUBLISH_LIVE_TEST") != "1" {
		t.Skip("live GitHub integration is opt-in: set FREESIDE_PUBLISH_LIVE_TEST=1, " +
			"FREESIDE_PUBLISH_LIVE_APP_ID, FREESIDE_PUBLISH_LIVE_OWNER_ID, FREESIDE_PUBLISH_LIVE_KEY_PATH, " +
			"FREESIDE_PUBLISH_LIVE_REPOSITORY_ID, FREESIDE_PUBLISH_LIVE_REPO (owner/name); set " +
			"FREESIDE_PUBLISH_LIVE_APP_VISIBILITY=public for any public App (default: private), and " +
			"FREESIDE_PUBLISH_LIVE_APP_OWNER when the registration owner differs from the repo owner " +
			"(default: the repo owner)")
	}

	appID := liveInt64(t, "FREESIDE_PUBLISH_LIVE_APP_ID")
	ownerID := liveInt64(t, "FREESIDE_PUBLISH_LIVE_OWNER_ID")
	repositoryID := liveInt64(t, "FREESIDE_PUBLISH_LIVE_REPOSITORY_ID")
	repo = os.Getenv("FREESIDE_PUBLISH_LIVE_REPO")
	if repo == "" {
		t.Fatal("FREESIDE_PUBLISH_LIVE_REPO is empty")
	}
	if _, err := bareRepoName(repo); err != nil {
		t.Fatalf("FREESIDE_PUBLISH_LIVE_REPO: %v", err)
	}

	// G304: the key path is supplied by the operator running the opt-in
	// test against their own App.
	keyPEM, err := os.ReadFile(os.Getenv("FREESIDE_PUBLISH_LIVE_KEY_PATH")) //nolint:gosec // operator-supplied path for the opt-in live test
	if err != nil {
		t.Fatalf("read live key: %v", err)
	}
	key, err := parseTestPEM(keyPEM)
	if err != nil {
		t.Fatalf("parse live key: %v", err)
	}

	// The registration's true shape: by default the legacy assumption
	// (a private App owned by the account it is installed on), overridden
	// by the optional shape variables for a real registration like the
	// live App, which is public and owned by a different account than
	// the installation's.
	appOwner := os.Getenv("FREESIDE_PUBLISH_LIVE_APP_OWNER")
	if appOwner == "" {
		appOwner = strings.SplitN(repo, "/", 2)[0]
	}
	visibility := publish.AppVisibilityPrivate
	switch os.Getenv("FREESIDE_PUBLISH_LIVE_APP_VISIBILITY") {
	case "", "private":
	case "public":
		visibility = publish.AppVisibilityPublic
	default:
		t.Fatal("FREESIDE_PUBLISH_LIVE_APP_VISIBILITY must be public or private")
	}

	base := t.TempDir()
	ks, err := publish.NewKeystore(filepath.Join(base, "credentials"), filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.SaveApp(publish.AppCredentials{
		Owner:      appOwner,
		OwnerID:    ownerID,
		Visibility: visibility,
		AppID:      appID,
		Name:       "live-test-app",
		Key:        key,
	}); err != nil {
		t.Fatal(err)
	}
	// The live mint audits through the production store-backed path, so
	// the opt-in run exercises the same recorder the daemon composes.
	rec, err := publish.NewStoreRecorder(newTestStore(t))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	profile = trustProfileForRepoID(t, repo, repositoryID)
	trust := memoryTrustSource{profile: &profile}
	return newCoveredMinter(ks, client, "https://api.github.com", rec, trust, time.Now), repo, profile
}

// TestLiveMintInstallationToken exercises the App JWT and
// installation-token lifecycle against the real GitHub API.
func TestLiveMintInstallationToken(t *testing.T) {
	m, repo, _ := newLiveMinter(t)
	tok, err := m.MintInstallationToken(context.Background(), repo)
	if err != nil {
		t.Fatalf("live mint: %v", err)
	}
	if tok.Token.Reveal() == "" {
		t.Error("live mint returned an empty token")
	}
	if !tok.ExpiresAt.After(time.Now()) {
		t.Errorf("live token already expired at %v", tok.ExpiresAt)
	}
}

// TestLivePublishEffectivelyOnce runs the after-publish-before-acceptance
// and after-acceptance kill boundaries (issue #82 acceptance 1) against
// the real GitHub API: publish, simulate a daemon death by reopening the
// store, drain to convergence with no second external effect, then drain
// again to prove the accepted publication does not re-advance. It needs a
// commit that already exists in the live repo (the publisher creates
// refs, it does not upload objects) and cleans up the branch and PR it
// creates.
func TestLivePublishEffectivelyOnce(t *testing.T) {
	m, repo, liveProfile := newLiveMinter(t)
	headSHA := os.Getenv("FREESIDE_PUBLISH_LIVE_HEAD_SHA")
	if headSHA == "" {
		t.Skip("set FREESIDE_PUBLISH_LIVE_HEAD_SHA to a commit that exists in the live repo")
	}
	baseRef := os.Getenv("FREESIDE_PUBLISH_LIVE_BASE")
	if baseRef == "" {
		baseRef = "main"
	}
	ctx := context.Background()
	const baseURL = "https://api.github.com"
	client := &http.Client{Timeout: 30 * time.Second}
	ts := publish.NewCachedTokenSource(m, time.Now)

	// A unique nonce per run gives a fresh publication identity, so a
	// leftover branch or PR from an earlier run never collides with this
	// one.
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	recipe := liveDigest("freeside-live-recipe-" + nonce)
	artifactDigest := liveDigest("freeside-live-artifact-" + nonce)
	approved := map[domain.Digest]bool{recipe: true}
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID:     "artifact-live",
		Type:   "verification-evidence",
		Digest: artifactDigest,
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-live-producer",
			HeadBinding:              domain.HeadBound,
			SourceHeadSHA:            headSHA,
			VerificationRecipeDigest: &recipe,
			SensitivityClass:         domain.SensitivityNormal,
		},
	}, approved)
	if err != nil {
		t.Fatalf("NewArtifact: %v", err)
	}
	liveProfileDigest := liveProfile.ProfileDigest
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest([]domain.Artifact{artifact})
	if err != nil {
		t.Fatalf("ComputeEvidenceSnapshotDigest: %v", err)
	}
	// The authorization gate (#168) requires a daemon-authored record that
	// authorizes this candidate: a passed verification with no blocking
	// finding, bound to the candidate's repo/head/recipe/trust profile. It is
	// seeded below alongside trust so Publish reaches the real GitHub path.
	liveAuth := newAuthorization(t, domain.CandidateAuthorizationInput{
		Repo:                     repo,
		BaseSHA:                  headSHA,
		HeadSHA:                  headSHA,
		ImportResultDigest:       liveDigest("freeside-live-import-" + nonce),
		VerificationRecipeDigest: recipe,
		EvidenceSnapshotDigest:   evidenceDigest,
		VerificationOutcome:      domain.VerificationPassed,
		TrustProfileDigest:       liveProfileDigest,
		InvocationID:             domain.InvocationID("inv-live-verify-" + nonce),
		CreatedAt:                time.Now().UTC(),
	})
	authID := liveAuth.ID
	cand := publish.Candidate{
		Repo:               repo,
		BaseRef:            baseRef,
		HeadSHA:            headSHA,
		Title:              "Freeside live effectively-once test " + nonce,
		Body:               "Automated opt-in test (issue #82). Safe to close.",
		Artifacts:          []domain.Artifact{artifact},
		RecipeDigest:       &recipe,
		InvocationID:       domain.InvocationID("inv-live-" + nonce),
		AuthorizationID:    &authID,
		TrustProfileDigest: &liveProfileDigest,
	}
	// The drain refuses a recovered candidate without a head transport.
	// This test's premise is a head commit that already exists in the
	// live repo (the publisher creates refs, it does not upload
	// objects), so there is nothing to push and a no-op capability is
	// the live equivalent of the engine's transport-backed one.
	resolver := fakeResolver{
		cand: cand, approved: approved,
		publishHead: func(context.Context, publish.GatedHead) error { return nil },
	}
	id, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo:            repo,
		BaseRef:         baseRef,
		SourceHeadSHA:   headSHA,
		ArtifactDigests: []domain.Digest{artifactDigest},
		RecipeDigest:    &recipe,
	})
	if err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(t.TempDir(), "store.db")
	s1, p1 := openKillHarness(t, dbPath, client, baseURL, ts)
	seedTrustProfile(t, s1, liveProfile) // conformant trust so the drift gate passes; persists across the restart
	seedAuthz(t, s1, liveAuth)           // authorizing record for the live candidate; persists across the restart
	// Register before Publish: it can create the deterministic branch or
	// PR and then fail returned-object validation without returning a
	// Result. The identity supplies the branch up front; a zero PR number
	// makes cleanup discover any partial PR by that unique head branch.
	cleanupPRNumber := 0
	t.Cleanup(func() {
		cleanupLivePublication(t, client, baseURL, ts, repo, id.BranchName(), cleanupPRNumber)
	})
	res, err := p1.Publish(ctx, cand, approved)
	if err != nil {
		_ = s1.Close()
		t.Fatalf("live publish: %v", err)
	}
	cleanupPRNumber = res.PRNumber
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Boundary b2: reopen (daemon death) and drain to convergence.
	s2, p2 := openKillHarness(t, dbPath, client, baseURL, ts)
	defer func() { _ = s2.Close() }()
	n, err := publish.DrainPendingPublications(ctx, s2, p2, resolver)
	if err != nil {
		t.Fatalf("recovery drain: %v", err)
	}
	if n != 1 {
		t.Errorf("recovery drain finalized %d, want 1", n)
	}

	assertOutcomeRecorded(t, s2, publish.OutcomeKey(id), publish.Outcome{
		Identity:         id.Digest(),
		Repo:             repo,
		BaseRef:          baseRef,
		HeadSHA:          headSHA,
		Branch:           res.Branch,
		PRNumber:         res.PRNumber,
		EvidenceEligible: true,
	})

	// Boundary c: a re-drain after acceptance is a clean no-op.
	if n, err := publish.DrainPendingPublications(ctx, s2, p2, resolver); err != nil || n != 0 {
		t.Errorf("re-drain after acceptance = (%d, %v), want (0, nil)", n, err)
	}
}

func liveDigest(seed string) domain.Digest {
	sum := sha256.Sum256([]byte(seed))
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// bareRepoName is the repository name without its owner: the form the
// minter grants by and the token source caches under. FREESIDE_PUBLISH_
// LIVE_REPO is owner/name (the publish test needs it as Candidate.Repo),
// so every direct mint/token call extracts the name here.
func bareRepoName(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("repository %q must be owner/name", repo)
	}
	return name, nil
}

// cleanupLivePublication best-effort deletes the branch and closes the PR
// the live test created, so opt-in runs leave no residue.
func cleanupLivePublication(t *testing.T, client *http.Client, baseURL string, ts publish.TokenSource, repo, branch string, prNumber int) {
	t.Helper()
	tok, err := ts.Token(context.Background(), repo)
	if err != nil {
		t.Logf("cleanup: token: %v", err)
		return
	}
	auth := "Bearer " + tok.Token.Reveal()

	prNumbers := []int{prNumber}
	if prNumber <= 0 {
		prNumbers, err = findLivePublicationPRs(context.Background(), client, baseURL, repo, branch, auth)
		if err != nil {
			t.Logf("cleanup: find PR for branch %s: %v", branch, err)
			prNumbers = nil
		}
	}
	for _, number := range prNumbers {
		if _, err := doLiveCleanupRequest(context.Background(), client, http.MethodPatch,
			fmt.Sprintf("%s/repos/%s/pulls/%d", baseURL, repo, number),
			[]byte(`{"state":"closed"}`), auth, http.StatusOK); err != nil {
			t.Logf("cleanup: close PR #%d: %v", number, err)
		}
	}
	if _, err := doLiveCleanupRequest(context.Background(), client, http.MethodDelete,
		fmt.Sprintf("%s/repos/%s/git/refs/heads/%s", baseURL, repo, branch),
		nil, auth, http.StatusNoContent, http.StatusNotFound, http.StatusUnprocessableEntity); err != nil {
		t.Logf("cleanup: delete branch %s: %v", branch, err)
	}
}

func findLivePublicationPRs(ctx context.Context, client *http.Client, baseURL, repo, branch, auth string) ([]int, error) {
	owner, _, _ := strings.Cut(repo, "/") // repo was validated by bareRepoName
	endpoint, err := url.Parse(fmt.Sprintf("%s/repos/%s/pulls", baseURL, repo))
	if err != nil {
		return nil, fmt.Errorf("build list URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("state", "open")
	query.Set("head", owner+":"+branch)
	endpoint.RawQuery = query.Encode()
	payload, err := doLiveCleanupRequest(ctx, client, http.MethodGet, endpoint.String(), nil, auth, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var pulls []struct {
		Number int `json:"number"`
	}
	if err := json.Unmarshal(payload, &pulls); err != nil {
		return nil, fmt.Errorf("decode pull list: %w", err)
	}
	numbers := make([]int, 0, len(pulls))
	for _, pr := range pulls {
		if pr.Number <= 0 {
			return nil, fmt.Errorf("pull list returned non-positive number %d", pr.Number)
		}
		numbers = append(numbers, pr.Number)
	}
	return numbers, nil
}

func doLiveCleanupRequest(ctx context.Context, client *http.Client, method, requestURL string, body []byte, auth string, wantStatuses ...int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	liveAuthed(req, auth)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	accepted := false
	for _, status := range wantStatuses {
		accepted = accepted || resp.StatusCode == status
	}
	if !accepted {
		return nil, fmt.Errorf("%s %s returned HTTP %d, want one of %v", method, req.URL.Path, resp.StatusCode, wantStatuses)
	}
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	return payload, nil
}

type cleanupTransportFunc func(*http.Request) (*http.Response, error)

func (f cleanupTransportFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBareRepoName(t *testing.T) {
	got, err := bareRepoName("freeside-ai/freeside")
	if err != nil {
		t.Fatal(err)
	}
	if got != "freeside" {
		t.Errorf("bareRepoName = %q, want freeside", got)
	}
	for _, malformed := range []string{"", "freeside", "/freeside", "freeside-ai/", "freeside-ai/freeside/extra"} {
		if _, err := bareRepoName(malformed); err == nil {
			t.Errorf("bareRepoName(%q) accepted, want error", malformed)
		}
	}
}

func TestDoLiveCleanupRequestChecksStatus(t *testing.T) {
	client := &http.Client{Transport: cleanupTransportFunc(func(req *http.Request) (*http.Response, error) {
		if got := req.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    req,
		}, nil
	})}
	if _, err := doLiveCleanupRequest(context.Background(), client, http.MethodDelete,
		"https://api.github.test/repos/freeside-ai/freeside/git/refs/heads/test", nil,
		"Bearer secret", http.StatusNoContent); err == nil {
		t.Fatal("cleanup accepted HTTP 500, want error")
	}
}

func TestCleanupLivePublicationDiscoversPartialPR(t *testing.T) {
	var requests []string
	client := &http.Client{Transport: cleanupTransportFunc(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.Method+" "+req.URL.RequestURI())
		status := http.StatusOK
		body := `[{"number":123}]`
		switch {
		case req.Method == http.MethodGet && req.URL.Path == "/repos/freeside-ai/evidence-repo/pulls":
			if req.URL.Query().Get("head") != "freeside-ai:freeside/publish/abcdef" || req.URL.Query().Get("state") != "open" {
				t.Errorf("list query = %q", req.URL.RawQuery)
			}
		case req.Method == http.MethodPatch && req.URL.Path == "/repos/freeside-ai/evidence-repo/pulls/123":
			body = `{}`
		case req.Method == http.MethodDelete && req.URL.Path == "/repos/freeside-ai/evidence-repo/git/refs/heads/freeside/publish/abcdef":
			status = http.StatusNoContent
			body = ""
		default:
			t.Fatalf("unexpected cleanup request: %s %s", req.Method, req.URL.RequestURI())
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}

	cleanupLivePublication(t, client, "https://api.github.test", testTokenSource(),
		"freeside-ai/evidence-repo", "freeside/publish/abcdef", 0)
	if len(requests) != 3 {
		t.Fatalf("cleanup requests = %v, want list, close, delete", requests)
	}
}

func liveAuthed(req *http.Request, auth string) {
	req.Header.Set("Authorization", auth)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Accept", "application/vnd.github+json")
}

// liveGuardRecorder makes the opt-in live janitor pass non-destructive, with no
// way to opt back in. The janitor commits an audit barrier before it suspends or
// deletes anything and abandons the pass when that fails, so refusing here turns
// an unexpected drift on the operator's real App into a failed test rather than
// a deleted installation.
//
// There is deliberately no escape hatch. This fixture builds a keystore and a
// state directory under t.TempDir(), which the framework removes when the test
// ends: a genuinely destructive pass out of it would suspend and delete a real
// installation and then discard the audit record and the durable withdrawal that
// justified doing so, leaving the operator's own daemon to re-trust whatever
// survived. Exercising removal against a live App needs the operator's real
// state directory, which is not a thing a test fixture may borrow.
type liveGuardRecorder struct{}

func (liveGuardRecorder) RecordInstallationRemoval(record publish.InstallationRemovalRecord) error {
	return fmt.Errorf("live guard: refusing to remove installation %d", record.InstallationID)
}

func (liveGuardRecorder) RecordInstallationQuarantine(record publish.InstallationRemovalRecord) error {
	return fmt.Errorf("live guard: refusing to quarantine installation %d", record.InstallationID)
}

// TestLiveInstallationJanitorPublishesCoverage runs the always-on janitor against
// the real GitHub API with its authority read from a state-directory snapshot
// (issue #276 acceptance 6). It proves the gate every mint depends on can
// actually be opened by a standalone host: the janitor reconciles the operator's
// authored bindings against the App's real installations and publishes coverage
// for the exact repository.
func TestLiveInstallationJanitorPublishesCoverage(t *testing.T) {
	if os.Getenv("FREESIDE_PUBLISH_LIVE_TEST") != "1" {
		t.Skip("live GitHub integration is opt-in: set FREESIDE_PUBLISH_LIVE_TEST=1, " +
			"FREESIDE_PUBLISH_LIVE_APP_ID, FREESIDE_PUBLISH_LIVE_APP_OWNER, FREESIDE_PUBLISH_LIVE_OWNER_ID, " +
			"FREESIDE_PUBLISH_LIVE_APP_VISIBILITY (public|private), FREESIDE_PUBLISH_LIVE_KEY_PATH, " +
			"FREESIDE_PUBLISH_LIVE_REPO (owner/name), FREESIDE_PUBLISH_LIVE_REPOSITORY_ID, " +
			"FREESIDE_PUBLISH_LIVE_INSTALLATION_ID, FREESIDE_PUBLISH_LIVE_ACCOUNT_ID")
	}
	appID := liveInt64(t, "FREESIDE_PUBLISH_LIVE_APP_ID")
	ownerID := liveInt64(t, "FREESIDE_PUBLISH_LIVE_OWNER_ID")
	repositoryID := liveInt64(t, "FREESIDE_PUBLISH_LIVE_REPOSITORY_ID")
	installationID := liveInt64(t, "FREESIDE_PUBLISH_LIVE_INSTALLATION_ID")
	accountID := liveInt64(t, "FREESIDE_PUBLISH_LIVE_ACCOUNT_ID")
	appOwner := os.Getenv("FREESIDE_PUBLISH_LIVE_APP_OWNER")
	repo := os.Getenv("FREESIDE_PUBLISH_LIVE_REPO")
	if _, err := bareRepoName(repo); err != nil {
		t.Fatalf("FREESIDE_PUBLISH_LIVE_REPO: %v", err)
	}
	// The installation's account is the repository's owner: an installation can
	// only grant repositories the account it is installed on holds.
	account := strings.SplitN(repo, "/", 2)[0]
	var visibility publish.AppVisibility
	switch os.Getenv("FREESIDE_PUBLISH_LIVE_APP_VISIBILITY") {
	case "public":
		visibility = publish.AppVisibilityPublic
	case "private":
		visibility = publish.AppVisibilityPrivate
	default:
		t.Fatal("FREESIDE_PUBLISH_LIVE_APP_VISIBILITY must be public or private")
	}

	keyPEM, err := os.ReadFile(os.Getenv("FREESIDE_PUBLISH_LIVE_KEY_PATH")) //nolint:gosec // operator-supplied path for the opt-in live test
	if err != nil {
		t.Fatalf("read live key: %v", err)
	}
	key, err := parseTestPEM(keyPEM)
	if err != nil {
		t.Fatalf("parse live key: %v", err)
	}

	// Credentials and state are siblings, the arrangement the keystore enforces
	// and the one freesided will compose.
	base := t.TempDir()
	stateDir := filepath.Join(base, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatalf("create state dir: %v", err)
	}
	ks, err := publish.NewKeystore(filepath.Join(base, "credentials"), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.SaveApp(publish.AppCredentials{
		Owner:      appOwner,
		OwnerID:    ownerID,
		Visibility: visibility,
		AppID:      appID,
		Name:       "live-test-app",
		Key:        key,
	}); err != nil {
		t.Fatal(err)
	}

	// A public registration must trust its own owner as well as the account it
	// is installed on; when they are the same account, one entry says both.
	owners := []publish.TrustedOwnerRecord{{Login: appOwner, ID: ownerID}}
	if accountID != ownerID {
		owners = append(owners, publish.TrustedOwnerRecord{Login: account, ID: accountID})
	}
	document := publish.InstallationAuthorityDocument{
		Version: 1,
		Registrations: []publish.InstallationAuthorityEntry{{
			RegistrationID:        appID,
			ActiveEpoch:           1,
			DurableIntentRevision: 1,
			TrustedOwners:         owners,
			TrustedInstallations: []publish.TrustedInstallationRecord{{
				InstallationID: installationID,
				Account:        account,
				AccountID:      accountID,
				RepositoryIDs:  []int64{repositoryID},
			}},
		}},
	}
	payload, err := document.Encode()
	if err != nil {
		t.Fatalf("encode authority: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "installation-authority.json"), payload, 0o600); err != nil {
		t.Fatalf("write authority: %v", err)
	}

	store, err := publish.NewInstallationAuthorityStore(stateDir)
	if err != nil {
		t.Fatalf("new authority store: %v", err)
	}
	janitor, err := publish.NewInstallationJanitor(
		ks,
		&http.Client{Timeout: 30 * time.Second},
		"https://api.github.com",
		store,
		liveGuardRecorder{},
		time.Now,
		1,
	)
	if err != nil {
		t.Fatalf("NewInstallationJanitor: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Hour) }()
	deadline := time.Now().Add(60 * time.Second)
	for !janitor.ActiveFor(appID) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	activated := janitor.ActiveFor(appID)
	allowed := janitor.AllowsRepository(appID, installationID, repositoryID)
	cancel()
	if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("live janitor pass: %v", err)
	}

	if !activated {
		t.Fatal("the live pass published no coverage for the registration")
	}
	if !allowed {
		t.Fatalf("coverage does not authorize repository %d under installation %d", repositoryID, installationID)
	}
	// Nothing was recorded, so nothing was suspended or deleted: the journal is
	// only ever written by the audit barrier.
	if _, err := os.Lstat(filepath.Join(stateDir, "installation-janitor-journal.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the live pass recorded a destructive request (%v)", err)
	}
}

func liveInt64(t *testing.T, name string) int64 {
	t.Helper()
	value, err := strconv.ParseInt(os.Getenv(name), 10, 64)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return value
}
