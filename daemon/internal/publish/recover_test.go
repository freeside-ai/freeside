package publish_test

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The kill-test matrix (issue #82 acceptance 1, plan §5.9). Each test
// drives a publication to a boundary, simulates a daemon death by closing
// the store and reopening it at the same path (GitHub, modeled by the one
// fakeGitHub, outlives the daemon), then drains and asserts the
// publication converges to exactly one branch, one PR, one recorded
// outcome — never a duplicate or a double advance. The recorded-fixture
// surface (this in-memory fakeGitHub) is always on; the live path is the
// opt-in FREESIDE_PUBLISH_LIVE_TEST gate in live_test.go.

// quietServer starts the fake over a discarding error log, so the
// deliberate failpoint panics do not spew http.Server panic traces.
func (g *fakeGitHub) quietServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(g.handle))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.Start()
	t.Cleanup(srv.Close)
	return srv
}

// failOnce arms a one-shot failpoint: the next request whose method and
// path match panics inside the handler before any effect is applied.
// net/http recovers the panic and drops the connection, so the client
// (the publisher, mid-Publish) sees a transport error with GitHub's state
// left exactly at the pre-request point. It clears itself after firing —
// the recovery pass that follows must not fail.
func (g *fakeGitHub) failOnce(method, pathSubstr string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.onRequest = func(m, p string) {
		if m == method && strings.Contains(p, pathSubstr) {
			g.onRequest = nil // one-shot; the handler holds g.mu here
			panic("failpoint: " + m + " " + p)
		}
	}
}

// openKillHarness opens (or reopens) a store at dbPath and builds a
// publisher over it and the given GitHub endpoint. Caller closes the
// returned store to model the daemon death. Taking the endpoint as
// (client, baseURL, tokenSource) lets both the fake-server kill tests and
// the live opt-in path reuse it.
func openKillHarness(t *testing.T, dbPath string, client *http.Client, baseURL string, ts publish.TokenSource) (*store.Store, *publish.Publisher) {
	t.Helper()
	s, err := store.Open(context.Background(), dbPath, store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	trust, err := publish.NewStoreTrustSource(s)
	if err != nil {
		t.Fatalf("NewStoreTrustSource: %v", err)
	}
	authz, err := publish.NewStoreAuthorizationSource(s)
	if err != nil {
		t.Fatalf("NewStoreAuthorizationSource: %v", err)
	}
	return s, publish.NewPublisher(ts, client, baseURL, ledger, trust, authz)
}

func createRefRequest() string { return http.MethodPost + " " + testRepoPath + "/git/refs" }
func createPRRequest() string  { return http.MethodPost + " " + testRepoPath + "/pulls" }

// assertConverged asserts the end state every boundary must reach:
// exactly one branch, exactly one PR creation across the whole run, no
// pending intent, and the outcome recorded.
func assertConverged(t *testing.T, s *store.Store, gh *fakeGitHub, cand publish.Candidate) {
	t.Helper()
	if got := len(gh.refs); got != 1 {
		t.Errorf("branches = %d, want 1", got)
	}
	if got := len(gh.prs); got != 1 {
		t.Errorf("pull requests = %d, want 1", got)
	}
	if got := countRequests(gh.requestLog(), createRefRequest()); got != 1 {
		t.Errorf("branch creations = %d, want exactly 1", got)
	}
	if got := countRequests(gh.requestLog(), createPRRequest()); got != 1 {
		t.Errorf("PR creations = %d, want exactly 1", got)
	}
	if got := len(pendingPublications(t, s)); got != 0 {
		t.Errorf("pending intents = %d, want 0", got)
	}
	id := testCandidateIdentity(t)
	assertOutcomeRecorded(t, s, publish.OutcomeKey(id), publish.Outcome{
		Identity:         id.Digest(),
		Repo:             cand.Repo,
		BaseRef:          cand.BaseRef,
		HeadSHA:          cand.HeadSHA,
		Branch:           id.BranchName(),
		PRNumber:         101,
		EvidenceEligible: true,
	})
}

// TestKillBeforeExternalEffect (boundary a): the daemon dies after the
// intent commits but before any GitHub effect. Recovery produces exactly
// one branch and PR.
func TestKillBeforeExternalEffect(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	gh := newFakeGitHub(t)
	srv := gh.quietServer(t)
	cand := testCandidate(t)

	s1, p1 := openKillHarness(t, dbPath, srv.Client(), srv.URL, testTokenSource())
	seedTrust(t, s1, testTrustRepo)                 // conformant trust so the drift gate passes; persists to s2
	seedAuthz(t, s1, testCandidateAuthorization(t)) // authorizing record for the same candidate
	gh.failOnce(http.MethodGet, "/git/ref/")        // die at the first read, after recordIntent
	if _, err := p1.Publish(ctx, cand, testApprovedRecipes()); err == nil {
		t.Fatal("publish reached GitHub without hitting the failpoint")
	}
	if got := len(pendingPublications(t, s1)); got != 1 {
		t.Fatalf("pending after seed = %d, want 1 (intent committed)", got)
	}
	if len(gh.refs) != 0 || len(gh.prs) != 0 {
		t.Fatalf("external effect happened before the failpoint: refs=%d prs=%d", len(gh.refs), len(gh.prs))
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, p2 := openKillHarness(t, dbPath, srv.Client(), srv.URL, testTokenSource())
	defer func() { _ = s2.Close() }()
	n, err := publish.DrainPendingPublications(ctx, s2, p2, resolverFor(t, cand))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 1 {
		t.Errorf("drain finalized %d, want 1", n)
	}
	assertConverged(t, s2, gh, cand)
}

// TestKillAfterBranchBeforePR (boundary b, variant 1): the branch is
// created, then the daemon dies before the PR. Recovery creates only the
// missing PR — exactly one branch, exactly one PR, no duplicate branch.
func TestKillAfterBranchBeforePR(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	gh := newFakeGitHub(t)
	srv := gh.quietServer(t)
	cand := testCandidate(t)

	s1, p1 := openKillHarness(t, dbPath, srv.Client(), srv.URL, testTokenSource())
	seedTrust(t, s1, testTrustRepo)
	seedAuthz(t, s1, testCandidateAuthorization(t))
	gh.failOnce(http.MethodPost, "/pulls") // branch created, die at PR creation
	if _, err := p1.Publish(ctx, cand, testApprovedRecipes()); err == nil {
		t.Fatal("publish created the PR without hitting the failpoint")
	}
	if len(gh.refs) != 1 {
		t.Fatalf("branches after seed = %d, want 1", len(gh.refs))
	}
	if len(gh.prs) != 0 {
		t.Fatalf("pull requests after seed = %d, want 0 (died before PR)", len(gh.prs))
	}
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, p2 := openKillHarness(t, dbPath, srv.Client(), srv.URL, testTokenSource())
	defer func() { _ = s2.Close() }()
	n, err := publish.DrainPendingPublications(ctx, s2, p2, resolverFor(t, cand))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 1 {
		t.Errorf("drain finalized %d, want 1", n)
	}
	assertConverged(t, s2, gh, cand)
}

// TestKillAfterPublishBeforeAcceptance (boundary b, variant 2): the
// branch and PR both exist, but the daemon dies before the finalize
// (acceptance). Recovery records the outcome and dispatches the intent
// with NO second external effect (acceptance 2).
func TestKillAfterPublishBeforeAcceptance(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	gh := newFakeGitHub(t)
	srv := gh.quietServer(t)
	cand := testCandidate(t)

	s1, p1 := openKillHarness(t, dbPath, srv.Client(), srv.URL, testTokenSource())
	seedTrust(t, s1, testTrustRepo)
	seedAuthz(t, s1, testCandidateAuthorization(t))
	// A full publish creates the branch and PR but never finalizes: the
	// outbox row is left pending, as after a crash before acceptance.
	if _, err := p1.Publish(ctx, cand, testApprovedRecipes()); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	writesBefore := len(gh.writeRequests())
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, p2 := openKillHarness(t, dbPath, srv.Client(), srv.URL, testTokenSource())
	defer func() { _ = s2.Close() }()
	n, err := publish.DrainPendingPublications(ctx, s2, p2, resolverFor(t, cand))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 1 {
		t.Errorf("drain finalized %d, want 1", n)
	}
	// The recovery re-converge must emit no new write: GitHub already holds
	// the branch and PR.
	if got := len(gh.writeRequests()); got != writesBefore {
		t.Errorf("write requests after recovery = %d, want %d (no second external effect)", got, writesBefore)
	}
	assertConverged(t, s2, gh, cand)
}

// TestKillAfterAcceptance (boundary c): the daemon dies after acceptance
// (the outcome is recorded and the intent dispatched) but before any
// downstream transition. A recovery drain finds nothing pending, does
// nothing, and the workflow never advances twice.
func TestKillAfterAcceptance(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	gh := newFakeGitHub(t)
	srv := gh.quietServer(t)
	cand := testCandidate(t)

	s1, p1 := openKillHarness(t, dbPath, srv.Client(), srv.URL, testTokenSource())
	seedTrust(t, s1, testTrustRepo)
	seedAuthz(t, s1, testCandidateAuthorization(t))
	if _, err := p1.Publish(ctx, cand, testApprovedRecipes()); err != nil {
		t.Fatalf("seed publish: %v", err)
	}
	if n, err := publish.DrainPendingPublications(ctx, s1, p1, resolverFor(t, cand)); err != nil || n != 1 {
		t.Fatalf("seed drain = (%d, %v), want (1, nil)", n, err)
	}
	requestsBefore := len(gh.requestLog())
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, p2 := openKillHarness(t, dbPath, srv.Client(), srv.URL, testTokenSource())
	defer func() { _ = s2.Close() }()
	n, err := publish.DrainPendingPublications(ctx, s2, p2, resolverFor(t, cand))
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if n != 0 {
		t.Errorf("drain finalized %d, want 0 (already accepted)", n)
	}
	if got := len(gh.requestLog()); got != requestsBefore {
		t.Errorf("GitHub requests after re-drain = %d, want %d (no double advance)", got, requestsBefore)
	}
	assertConverged(t, s2, gh, cand)
}

// TestDrainReGateDriftLeavesPending (refute-first: re-gate drift +
// eligibility provenance): if a recipe is no longer approved when the
// drain runs, the publish re-gate must fail closed — no external effect,
// no recorded outcome, and the intent stays pending. The outcome's
// eligibility is the gate's verdict, never an assumed one.
func TestDrainReGateDriftLeavesPending(t *testing.T) {
	ctx := context.Background()
	h := newDrainHarness(t)
	cand := testCandidate(t)

	// Commit the intent directly, with no external effect yet.
	id := testCandidateIdentity(t)
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	intentPayload, err := publish.Intent{
		Identity:      id.Digest(),
		InvocationID:  "inv-0001",
		Repo:          cand.Repo,
		BaseRef:       cand.BaseRef,
		SourceHeadSHA: cand.HeadSHA,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ledger.Record(ctx, key, publish.IntentKindPublication, intentPayload); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	// The resolver hands back the real candidate but an approved set that
	// no longer trusts its recipe: eligibility has drifted.
	drifted := fakeResolver{cand: cand, approved: map[domain.Digest]bool{}}
	if _, err := publish.DrainPendingPublications(ctx, h.store, h.pub, drifted); err == nil {
		t.Fatal("drain converged an ineligible candidate, want error")
	}
	if got := len(h.gh.requestLog()); got != 0 {
		t.Errorf("GitHub requests = %d, want 0 (re-gate failed before any effect)", got)
	}
	if got := len(pendingPublications(t, h.store)); got != 1 {
		t.Errorf("pending = %d, want 1 (no false-eligible outcome, intent stays pending)", got)
	}
}

// TestDrainTrustDriftLeavesPending (#169, plan §5.5): if the automation
// trust profile the candidate was approved under has drifted by the time the
// drain runs (here the audit observed at recovery shows OIDC newly
// available), the publish drift gate must fail closed during recovery — no
// external effect, and the intent stays pending until a human records an
// approved new profile.
func TestDrainTrustDriftLeavesPending(t *testing.T) {
	ctx := context.Background()
	// The current profile still forbids OIDC, but the latest audit observes
	// it available: drift on the oidc axis.
	profile := testTrustProfile(t)
	audit := testWorkflowAudit(t)
	audit.OIDCAvailable = true
	h := newDrainHarnessWithTrust(t, memoryTrustSource{profile: &profile, audit: &audit})
	cand := testCandidate(t)

	id := testCandidateIdentity(t)
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	intentPayload, err := publish.Intent{
		Identity:      id.Digest(),
		InvocationID:  "inv-0001",
		Repo:          cand.Repo,
		BaseRef:       cand.BaseRef,
		SourceHeadSHA: cand.HeadSHA,
	}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.ledger.Record(ctx, key, publish.IntentKindPublication, intentPayload); err != nil {
		t.Fatalf("seed intent: %v", err)
	}

	if _, err := publish.DrainPendingPublications(ctx, h.store, h.pub, resolverFor(t, cand)); !errors.Is(err, publish.ErrTrustProfileDrift) {
		t.Fatalf("drain error = %v, want ErrTrustProfileDrift", err)
	}
	if got := len(h.gh.requestLog()); got != 0 {
		t.Errorf("GitHub requests = %d, want 0 (drift gate failed before any effect)", got)
	}
	if got := len(pendingPublications(t, h.store)); got != 1 {
		t.Errorf("pending = %d, want 1 (intent stays pending under trust drift)", got)
	}
}
