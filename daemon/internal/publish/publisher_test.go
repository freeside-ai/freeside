package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// fixedTokenSource hands out one static token; publisher tests do not
// exercise minting.
type fixedTokenSource struct{ token publish.InstallationToken }

func (s fixedTokenSource) Token(context.Context, string) (publish.InstallationToken, error) {
	return s.token, nil
}

func testTokenSource() publish.TokenSource {
	return fixedTokenSource{token: publish.InstallationToken{
		Token:     publish.Secret(fixtureTokenValue),
		ExpiresAt: fixtureTime.Add(time.Hour),
		Repo:      "evidence-repo",
	}}
}

// fakePR is one pull request held by the fake forge. HeadRepo empty
// means the PR's head lives in the repository itself; a fork PR sets
// it to the fork's full name.
type fakePR struct {
	Number   int
	State    string
	Title    string
	Body     string
	HeadRef  string
	HeadSHA  string
	HeadRepo string
	BaseRef  string // empty means "main"
}

// fakeGitHub is a stateful in-memory GitHub for the endpoints the
// publisher and reconciler drive: branch refs and pull requests, with
// a request log for no-write and ordering assertions.
type fakeGitHub struct {
	t *testing.T

	mu       sync.Mutex
	refs     map[string]string // branch -> sha
	prs      []fakePR
	prRevs   map[int]int // PR number -> revision, drives PR ETags
	nextPR   int
	requests []string // "METHOD path" plus " if-none-match" when conditional

	// createHeadRepo, when set, is the head repository the fake reports
	// for PRs created through it (simulating a head resolved to a fork).
	createHeadRepo string
	// Server-misbehavior overrides for returned-object tests:
	// mangleRefName makes ref reads name a different ref;
	// mangleRefCreate makes the ref-create response echo a wrong SHA;
	// mangleStoredTitle stores created/patched titles altered;
	// pullListBody, when non-empty, is written raw as the list response.
	mangleRefName     bool
	mangleRefCreate   bool
	mangleStoredTitle bool
	pullListBody      string

	// onRequest, when set, runs under the lock before each request is
	// handled (for ordering assertions and mid-flow interleavings).
	onRequest func(method, path string)
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	return &fakeGitHub{t: t, refs: map[string]string{}, prRevs: map[int]int{}, nextPR: 101}
}

func (g *fakeGitHub) server() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(g.handle))
	g.t.Cleanup(srv.Close)
	return srv
}

func (g *fakeGitHub) requestLog() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.requests...)
}

func (g *fakeGitHub) writeRequests() []string {
	var writes []string
	for _, r := range g.requestLog() {
		if !strings.HasPrefix(r, http.MethodGet+" ") {
			writes = append(writes, r)
		}
	}
	return writes
}

const testRepoPath = "/repos/freeside-ai/evidence-repo"

func (g *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.onRequest != nil {
		g.onRequest(r.Method, r.URL.Path)
	}
	logged := r.Method + " " + r.URL.Path
	if r.Header.Get("If-None-Match") != "" {
		logged += " if-none-match"
	}
	g.requests = append(g.requests, logged)

	if got := r.Header.Get("Authorization"); got != "Bearer "+fixtureTokenValue {
		g.t.Errorf("Authorization = %q, want the installation token", got)
	}
	if got := r.Header.Get("X-GitHub-Api-Version"); got != "2022-11-28" {
		g.t.Errorf("X-GitHub-Api-Version = %q", got)
	}

	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(path, testRepoPath+"/git/ref/heads/"):
		branch := strings.TrimPrefix(path, testRepoPath+"/git/ref/heads/")
		sha, ok := g.refs[branch]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		etag := `"ref-` + sha + `"`
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		refName := "refs/heads/" + branch
		if g.mangleRefName {
			refName = "refs/heads/somewhere-else"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": refName, "object": map[string]string{"sha": sha}})

	case r.Method == http.MethodPost && path == testRepoPath+"/git/refs":
		var body struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		branch := strings.TrimPrefix(body.Ref, "refs/heads/")
		if _, exists := g.refs[branch]; exists {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		g.refs[branch] = body.SHA
		echoSHA := body.SHA
		if g.mangleRefCreate {
			echoSHA = testOtherSHA
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"ref": body.Ref, "object": map[string]string{"sha": echoSHA}})

	case r.Method == http.MethodGet && path == testRepoPath+"/pulls":
		if g.pullListBody != "" {
			_, _ = io.WriteString(w, g.pullListBody)
			return
		}
		head := r.URL.Query().Get("head")
		out := []map[string]any{} // GitHub returns [], never null, for an empty page
		for _, pr := range g.prs {
			if head != "" && head != "freeside-ai:"+pr.HeadRef {
				continue
			}
			out = append(out, prJSON(pr))
		}
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPost && path == testRepoPath+"/pulls":
		var body struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Body  string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		title := body.Title
		if g.mangleStoredTitle {
			title += " (normalized)"
		}
		pr := fakePR{
			Number:   g.nextPR,
			State:    "open",
			Title:    title,
			Body:     body.Body,
			HeadRef:  body.Head,
			HeadSHA:  g.refs[body.Head],
			HeadRepo: g.createHeadRepo,
			BaseRef:  body.Base,
		}
		g.nextPR++
		g.prs = append(g.prs, pr)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(prJSON(pr))

	case r.Method == http.MethodGet && strings.HasPrefix(path, testRepoPath+"/pulls/"):
		number, err := strconv.Atoi(strings.TrimPrefix(path, testRepoPath+"/pulls/"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for _, pr := range g.prs {
			if pr.Number != number {
				continue
			}
			etag := fmt.Sprintf(`"pr-%d-%d"`, number, g.prRevs[number])
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
			_ = json.NewEncoder(w).Encode(prJSON(pr))
			return
		}
		w.WriteHeader(http.StatusNotFound)

	case r.Method == http.MethodPatch && strings.HasPrefix(path, testRepoPath+"/pulls/"):
		number, err := strconv.Atoi(strings.TrimPrefix(path, testRepoPath+"/pulls/"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var body struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		for i := range g.prs {
			if g.prs[i].Number == number {
				g.prs[i].Title = body.Title
				if g.mangleStoredTitle {
					g.prs[i].Title += " (normalized)"
				}
				g.prs[i].Body = body.Body
				g.prRevs[number]++
				_ = json.NewEncoder(w).Encode(prJSON(g.prs[i]))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)

	default:
		g.t.Errorf("unexpected request: %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func prJSON(pr fakePR) map[string]any {
	headRepo := pr.HeadRepo
	if headRepo == "" {
		headRepo = "freeside-ai/evidence-repo"
	}
	baseRef := pr.BaseRef
	if baseRef == "" {
		baseRef = "main"
	}
	return map[string]any{
		"number": pr.Number,
		"state":  pr.State,
		"title":  pr.Title,
		"body":   pr.Body,
		"head": map[string]any{
			"ref":  pr.HeadRef,
			"sha":  pr.HeadSHA,
			"repo": map[string]string{"full_name": headRepo},
		},
		"base": map[string]any{
			"ref":  baseRef,
			"repo": map[string]string{"full_name": "freeside-ai/evidence-repo"},
		},
	}
}

const (
	testHeadSHA   = "6dcb09b5b57875f334f61aebed695e2e4193db5e"
	testOtherSHA  = "0000000000000000000000000000000000000000"
	testRecipe    = domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	testArtifactD = domain.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
)

func testApprovedRecipes() map[domain.Digest]bool {
	return map[domain.Digest]bool{testRecipe: true}
}

// testArtifact builds a valid publish-eligible evidence artifact via
// the trusted constructor.
func testArtifact(t *testing.T, headSHA string) domain.Artifact {
	t.Helper()
	recipe := testRecipe
	a, err := domain.NewArtifact(domain.ArtifactInput{
		ID:     "artifact-1",
		Type:   "verification-evidence",
		Digest: testArtifactD,
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-producer",
			HeadBinding:              domain.HeadBound,
			SourceHeadSHA:            headSHA,
			VerificationRecipeDigest: &recipe,
			SensitivityClass:         domain.SensitivityNormal,
		},
	}, testApprovedRecipes())
	if err != nil {
		t.Fatalf("NewArtifact: %v", err)
	}
	return a
}

func testCandidate(t *testing.T) publish.Candidate {
	t.Helper()
	recipe := testRecipe
	profileDigest := testTrustProfileDigest(t)
	authID := testCandidateAuthorization(t).ID
	return publish.Candidate{
		Repo:               testTrustRepo,
		BaseRef:            "main",
		HeadSHA:            testHeadSHA,
		Title:              "Candidate: evidence-backed change",
		Body:               "Verified candidate publication.",
		Artifacts:          []domain.Artifact{testArtifact(t, testHeadSHA)},
		RecipeDigest:       &recipe,
		InvocationID:       "inv-0001",
		AuthorizationID:    &authID,
		TrustProfileDigest: &profileDigest,
	}
}

// newTestPublisher wires a publisher whose drift gate passes for
// testCandidate: a conformant in-memory trust source. Tests exercising the
// drift gate itself use newTestPublisherWithTrust.
func newTestPublisher(t *testing.T, gh *fakeGitHub, ledger publish.IntentLedger) *publish.Publisher {
	t.Helper()
	return newTestPublisherWithTrust(t, gh, ledger, conformantTrust(t))
}

func newTestPublisherWithTrust(t *testing.T, gh *fakeGitHub, ledger publish.IntentLedger, trust publish.TrustSource) *publish.Publisher {
	t.Helper()
	return newTestPublisherFull(t, gh, ledger, trust, conformantAuthz(t))
}

// newTestPublisherFull wires a publisher over explicit trust and
// authorization sources, for tests that drive either gate directly.
func newTestPublisherFull(t *testing.T, gh *fakeGitHub, ledger publish.IntentLedger, trust publish.TrustSource, authz publish.AuthorizationSource) *publish.Publisher {
	t.Helper()
	srv := gh.server()
	return publish.NewPublisher(testTokenSource(), srv.Client(), srv.URL, ledger, trust, authz)
}

// TestPublishCreatesBranchAndPR is the clean-path publication (issue
// #81 acceptance 2 and 4): intent recorded before any dispatch, branch
// created at the candidate head, PR created with the identity marker.
func TestPublishCreatesBranchAndPR(t *testing.T) {
	gh := newFakeGitHub(t)
	ledger := newMemoryLedger()
	// Every GitHub request must observe the already-recorded intent:
	// the outbox write precedes dispatch.
	gh.onRequest = func(string, string) {
		if len(ledger.keys) == 0 {
			t.Error("GitHub request arrived before the intent was recorded")
		}
	}
	p := newTestPublisher(t, gh, ledger)

	res, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.BranchCreated || !res.PRCreated {
		t.Errorf("result = %+v, want branch and PR created", res)
	}
	if got := gh.refs[res.Branch]; got != testHeadSHA {
		t.Errorf("branch %s at %q, want the candidate head", res.Branch, got)
	}
	if len(gh.prs) != 1 {
		t.Fatalf("%d PRs, want 1", len(gh.prs))
	}
	pr := gh.prs[0]
	if pr.Number != res.PRNumber || pr.HeadRef != res.Branch {
		t.Errorf("PR = %+v, result = %+v", pr, res)
	}
	if parsed, ok := publish.ParseMarker(pr.Body); !ok || parsed != res.Identity.Digest() {
		t.Errorf("PR body marker = (%q, %t), want the identity", parsed, ok)
	}
	if !strings.HasPrefix(pr.Body, "Verified candidate publication.") {
		t.Errorf("PR body lost the prose: %q", pr.Body)
	}
	if key := ledger.keys[0]; key != "publish/inv-0001/publish.publication" {
		t.Errorf("intent key = %q", key)
	}
}

// TestPublishRetryConverges: a full re-run of the same candidate finds
// the branch and PR and issues no writes at all (issue #81 acceptance
// 2: converge, not duplicate).
func TestPublishRetryConverges(t *testing.T) {
	gh := newFakeGitHub(t)
	ledger := newMemoryLedger()
	p := newTestPublisher(t, gh, ledger)

	first, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes())
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	writesBefore := len(gh.writeRequests())

	second, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes())
	if err != nil {
		t.Fatalf("retried Publish: %v", err)
	}
	if second.BranchCreated || second.PRCreated {
		t.Errorf("retry reports creations: %+v", second)
	}
	if second.PRNumber != first.PRNumber || second.Branch != first.Branch {
		t.Errorf("retry converged on %+v, first was %+v", second, first)
	}
	if got := len(gh.writeRequests()); got != writesBefore {
		t.Errorf("retry issued %d extra writes: %v", got-writesBefore, gh.writeRequests()[writesBefore:])
	}
	if len(ledger.keys) != 1 {
		t.Errorf("%d intent rows, want 1", len(ledger.keys))
	}
}

// TestPublishRetryAfterPartialCrash: the branch exists (a prior
// attempt died between ref create and PR create); the retry finds it,
// creates only the PR, and never re-creates the ref.
func TestPublishRetryAfterPartialCrash(t *testing.T) {
	gh := newFakeGitHub(t)
	ledger := newMemoryLedger()
	p := newTestPublisher(t, gh, ledger)

	c := testCandidate(t)
	// Derive the branch the same way the publisher will.
	id, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo:            c.Repo,
		BaseRef:         c.BaseRef,
		SourceHeadSHA:   c.HeadSHA,
		ArtifactDigests: []domain.Digest{testArtifactD},
		RecipeDigest:    c.RecipeDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	gh.refs[id.BranchName()] = testHeadSHA

	res, err := p.Publish(context.Background(), c, testApprovedRecipes())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if res.BranchCreated {
		t.Error("retry re-created an existing branch")
	}
	if !res.PRCreated {
		t.Error("retry did not create the missing PR")
	}
	for _, w := range gh.writeRequests() {
		if strings.HasPrefix(w, http.MethodPost+" ") && strings.HasSuffix(w, "/git/refs") {
			t.Errorf("retry issued a ref create: %v", gh.writeRequests())
		}
	}
}

// TestPublishRefusesMovedBranch: the deterministic branch exists at a
// different commit — unknown external state the publisher never
// overwrites (never force-pushes).
func TestPublishRefusesMovedBranch(t *testing.T) {
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())

	c := testCandidate(t)
	id, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo:            c.Repo,
		BaseRef:         c.BaseRef,
		SourceHeadSHA:   c.HeadSHA,
		ArtifactDigests: []domain.Digest{testArtifactD},
		RecipeDigest:    c.RecipeDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	gh.refs[id.BranchName()] = testOtherSHA

	if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("err = %v, want ErrPublicationConflict", err)
	}
	if writes := gh.writeRequests(); len(writes) != 0 {
		t.Errorf("conflicting publication issued writes: %v", writes)
	}
}

// TestPublishRefusesForeignPR: a PR occupies the publication branch
// without the identity marker; it is not ours to converge.
func TestPublishRefusesForeignPR(t *testing.T) {
	for name, body := range map[string]string{
		"no marker":      "someone else's PR",
		"foreign marker": "<!-- freeside:publication-identity=sha256:" + strings.Repeat("d", 64) + " -->",
		"mangled marker": "<!-- freeside:publication-identity=sha256:zz -->",
	} {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			p := newTestPublisher(t, gh, newMemoryLedger())

			c := testCandidate(t)
			id, err := publish.DeriveIdentity(publish.IdentityInput{
				Repo:            c.Repo,
				BaseRef:         c.BaseRef,
				SourceHeadSHA:   c.HeadSHA,
				ArtifactDigests: []domain.Digest{testArtifactD},
				RecipeDigest:    c.RecipeDigest,
			})
			if err != nil {
				t.Fatal(err)
			}
			gh.refs[id.BranchName()] = testHeadSHA
			gh.prs = append(gh.prs, fakePR{Number: 7, State: "open", Title: "foreign", Body: body, HeadRef: id.BranchName(), HeadSHA: testHeadSHA})

			if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrForeignResource) {
				t.Fatalf("err = %v, want ErrForeignResource", err)
			}
			if writes := gh.writeRequests(); len(writes) != 0 {
				t.Errorf("foreign occupation issued writes: %v", writes)
			}
		})
	}
}

// TestPublishRefusesClosedMarkedPR: a closed publication PR is a human
// decision; recreating or reopening it would override that decision.
func TestPublishRefusesClosedMarkedPR(t *testing.T) {
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())

	c := testCandidate(t)
	first, err := p.Publish(context.Background(), c, testApprovedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	for i := range gh.prs {
		if gh.prs[i].Number == first.PRNumber {
			gh.prs[i].State = "closed"
		}
	}

	if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("err = %v, want ErrPublicationConflict", err)
	}
}

// TestPublishConvergesDriftedPR: an open marked PR whose title or body
// drifted is patched back to the deterministic content; an undrifted
// one is left untouched.
func TestPublishConvergesDriftedPR(t *testing.T) {
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())

	c := testCandidate(t)
	first, err := p.Publish(context.Background(), c, testApprovedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	wantBody := gh.prs[0].Body

	// Drift the title; the marker stays.
	gh.prs[0].Title = "edited by hand"
	res, err := p.Publish(context.Background(), c, testApprovedRecipes())
	if err != nil {
		t.Fatalf("converging Publish: %v", err)
	}
	if res.PRNumber != first.PRNumber || res.PRCreated {
		t.Errorf("converged result = %+v", res)
	}
	if gh.prs[0].Title != c.Title || gh.prs[0].Body != wantBody {
		t.Errorf("PR not converged: %+v", gh.prs[0])
	}

	patches := len(gh.writeRequests())
	if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); err != nil {
		t.Fatal(err)
	}
	if got := len(gh.writeRequests()); got != patches {
		t.Errorf("undrifted convergence issued writes: %v", gh.writeRequests()[patches:])
	}
}

// TestPublishRecordsIntentBeforeAnyDispatch: a failing ledger stops
// the publication before a single external request (issue #81
// acceptance 4).
func TestPublishRecordsIntentBeforeAnyDispatch(t *testing.T) {
	gh := newFakeGitHub(t)
	ledger := newMemoryLedger()
	ledger.err = errors.New("ledger unavailable")
	p := newTestPublisher(t, gh, ledger)

	if _, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes()); err == nil {
		t.Fatal("Publish succeeded with a failing ledger")
	}
	if reqs := gh.requestLog(); len(reqs) != 0 {
		t.Errorf("dispatched %v before the intent was durable", reqs)
	}
}

// TestPublishRefusesReusedInvocation: an invocation ID whose recorded
// intent names a different identity fails closed — the retry must
// converge on the original intent, never publish new content under an
// old key.
func TestPublishRefusesReusedInvocation(t *testing.T) {
	gh := newFakeGitHub(t)
	ledger := newMemoryLedger()

	// The reused-invocation candidate publishes different content, so it
	// carries its own authorizing record; both must be resolvable, or the
	// authorization gate would intercept before the intent-conflict check.
	otherIn := authorizingInput(t)
	otherIn.HeadSHA = testOtherSHA
	otherAuth := newAuthorization(t, otherIn)
	otherID := otherAuth.ID
	p := newTestPublisherFull(t, gh, ledger, conformantTrust(t), authzWith(testCandidateAuthorization(t), otherAuth))

	c := testCandidate(t)
	if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); err != nil {
		t.Fatal(err)
	}

	changed := c
	changed.HeadSHA = testOtherSHA
	changed.Artifacts = []domain.Artifact{testArtifact(t, testOtherSHA)}
	changed.AuthorizationID = &otherID
	requests := len(gh.requestLog())
	if _, err := p.Publish(context.Background(), changed, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("err = %v, want ErrPublicationConflict", err)
	}
	if got := len(gh.requestLog()); got != requests {
		t.Errorf("conflicting invocation reuse still dispatched %d requests", got-requests)
	}
}

// TestPublishGatesArtifacts: the trust gate re-runs the policy
// computation before any effect — a head-bound artifact for another
// head, an unapproved recipe, and a forged eligibility bit all fail
// closed with nothing recorded and nothing dispatched.
func TestPublishGatesArtifacts(t *testing.T) {
	otherHead := testArtifact(t, testOtherSHA)

	forgedBit := testArtifact(t, testHeadSHA)
	forgedBit.PublishEligible = false // stale/forged bit; policy computes true

	cases := map[string]struct {
		mutate  func(*publish.Candidate)
		recipes map[domain.Digest]bool
		wantErr error
	}{
		"head mismatch": {
			mutate:  func(c *publish.Candidate) { c.Artifacts = []domain.Artifact{otherHead} },
			recipes: testApprovedRecipes(),
			wantErr: publish.ErrHeadMismatch,
		},
		"unapproved recipe": {
			mutate:  func(*publish.Candidate) {},
			recipes: map[domain.Digest]bool{},
			wantErr: nil, // domain error; any refusal with no effects passes
		},
		"forged eligibility bit": {
			mutate:  func(c *publish.Candidate) { c.Artifacts = []domain.Artifact{forgedBit} },
			recipes: testApprovedRecipes(),
			wantErr: nil,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			ledger := newMemoryLedger()
			p := newTestPublisher(t, gh, ledger)

			c := testCandidate(t)
			tc.mutate(&c)
			_, err := p.Publish(context.Background(), c, tc.recipes)
			if err == nil {
				t.Fatal("gated candidate published")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
			if len(ledger.keys) != 0 {
				t.Error("gated candidate recorded an intent")
			}
			if reqs := gh.requestLog(); len(reqs) != 0 {
				t.Errorf("gated candidate dispatched %v", reqs)
			}
		})
	}
}

// TestPublishRefusesTrustProfileDrift (#169, plan §5.5): the drift gate
// fails closed before any external effect when the candidate carries no
// trust binding, its bound profile is no longer current, there is no current
// profile or audit to compare against, or the latest audit exceeds the
// approved profile. The bound digest is a lookup key, never a verdict.
func TestPublishRefusesTrustProfileDrift(t *testing.T) {
	// A profile whose content (and so digest) differs from the one
	// testCandidate binds to: a superseded revision (§5.5 drift recovery).
	superseded, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       testTrustRepo,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		WorkflowAuditDigest:        "sha256:revised-audit",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config"},
	})
	if err != nil {
		t.Fatalf("superseded profile: %v", err)
	}
	supersededAudit := testWorkflowAudit(t)
	onlyProfile := testTrustProfile(t)
	profile := testTrustProfile(t)
	driftAudit := testWorkflowAudit(t)
	driftAudit.OIDCAvailable = true

	cases := map[string]struct {
		trust  publish.TrustSource
		mutate func(*publish.Candidate)
	}{
		"nil binding": {
			trust:  conformantTrust(t),
			mutate: func(c *publish.Candidate) { c.TrustProfileDigest = nil },
		},
		"no current profile": {
			trust:  memoryTrustSource{},
			mutate: func(*publish.Candidate) {},
		},
		"superseded profile": {
			trust:  memoryTrustSource{profile: &superseded, audit: &supersededAudit},
			mutate: func(*publish.Candidate) {},
		},
		"no current audit": {
			trust:  memoryTrustSource{profile: &onlyProfile},
			mutate: func(*publish.Candidate) {},
		},
		"audit drift": {
			trust:  memoryTrustSource{profile: &profile, audit: &driftAudit},
			mutate: func(*publish.Candidate) {},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			ledger := newMemoryLedger()
			p := newTestPublisherWithTrust(t, gh, ledger, tc.trust)

			c := testCandidate(t)
			tc.mutate(&c)
			if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrTrustProfileDrift) {
				t.Fatalf("err = %v, want ErrTrustProfileDrift", err)
			}
			if len(ledger.keys) != 0 {
				t.Error("drifted candidate recorded an intent")
			}
			if reqs := gh.requestLog(); len(reqs) != 0 {
				t.Errorf("drifted candidate dispatched %v", reqs)
			}
		})
	}

	// The audit-drift case reports the specific axis, so a human decision
	// record can act on what changed.
	t.Run("drift names the axis", func(t *testing.T) {
		gh := newFakeGitHub(t)
		p := newTestPublisherWithTrust(t, gh, newMemoryLedger(), memoryTrustSource{profile: &profile, audit: &driftAudit})
		_, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes())
		var de *domain.TrustDriftError
		if !errors.As(err, &de) || de.Axis != "oidc" {
			t.Fatalf("err = %v, want *TrustDriftError on the oidc axis", err)
		}
	})

	// A trust-source read failure fails closed too, with no effect.
	t.Run("trust source error", func(t *testing.T) {
		gh := newFakeGitHub(t)
		ledger := newMemoryLedger()
		p := newTestPublisherWithTrust(t, gh, ledger, memoryTrustSource{err: errors.New("store unavailable")})
		if _, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes()); err == nil {
			t.Fatal("published despite a trust-source failure")
		}
		if len(ledger.keys) != 0 || len(gh.requestLog()) != 0 {
			t.Error("trust-source failure left an effect")
		}
	})
}

// catPtr returns a pointer to a control-plane category, for the
// control-plane finding fixtures below.
func catPtr(c domain.ControlPlaneCategory) *domain.ControlPlaneCategory { return &c }

// TestPublishRefusesUnauthorizedCandidate (#168, plan §5.6): the
// authorization gate fails closed before any external effect when the
// candidate carries no authorization binding, names no recorded record, or
// the recorded record does not authorize this candidate — a failed
// verification, any class of publish-blocking importer/verifier finding, or a
// record whose bound facts describe a different candidate. Nothing is recorded
// and nothing is dispatched.
func TestPublishRefusesUnauthorizedCandidate(t *testing.T) {
	// mkCase builds a non-authorizing (or mis-binding) authorization from the
	// conformant input, points the candidate at it, and returns the source
	// holding it. authorizingInput matches the candidate's coordinates, so
	// only the mutated fact drives the refusal.
	mkCase := func(mutate func(*domain.CandidateAuthorizationInput)) func(*testing.T, *publish.Candidate) publish.AuthorizationSource {
		return func(t *testing.T, c *publish.Candidate) publish.AuthorizationSource {
			in := authorizingInput(t)
			mutate(&in)
			auth := newAuthorization(t, in)
			id := auth.ID
			c.AuthorizationID = &id
			return authzWith(auth)
		}
	}
	withFinding := func(f domain.CandidateFinding) func(*domain.CandidateAuthorizationInput) {
		return func(in *domain.CandidateAuthorizationInput) { in.Findings = []domain.CandidateFinding{f} }
	}

	cases := map[string]struct {
		build func(*testing.T, *publish.Candidate) publish.AuthorizationSource
	}{
		"nil authorization id": {build: func(t *testing.T, c *publish.Candidate) publish.AuthorizationSource {
			c.AuthorizationID = nil
			return conformantAuthz(t)
		}},
		"authorization not recorded": {build: func(t *testing.T, _ *publish.Candidate) publish.AuthorizationSource {
			return authzWith() // candidate keeps its conformant id; the source is empty
		}},
		"failed verification": {build: mkCase(func(in *domain.CandidateAuthorizationInput) {
			in.VerificationOutcome = domain.VerificationFailed
		})},
		"blocking secret finding": {build: mkCase(withFinding(domain.CandidateFinding{
			Class: domain.FindingClassSecret, Origin: domain.FindingOriginImport,
			Kind: "secret", Path: "config/.env", Disposition: domain.DispositionBlocking,
		}))},
		"blocking repo-change-policy finding": {build: mkCase(withFinding(domain.CandidateFinding{
			Class: domain.FindingClassRepoChangePolicy, Origin: domain.FindingOriginImport,
			Kind: "size_violation", Path: "artifacts/big.bin", Disposition: domain.DispositionBlocking,
		}))},
		"blocking automation-control finding": {build: mkCase(withFinding(domain.CandidateFinding{
			Class: domain.FindingClassControlPlane, Category: catPtr(domain.ControlPlaneWorkflowConfiguration),
			Origin: domain.FindingOriginImport, Kind: "automation_control_path",
			Path: ".github/workflows/ci.yml", Disposition: domain.DispositionBlocking,
		}))},
		"blocking reviewer-instruction finding": {build: mkCase(withFinding(domain.CandidateFinding{
			Class: domain.FindingClassControlPlane, Category: catPtr(domain.ControlPlaneReviewerInstructions),
			Origin: domain.FindingOriginImport, Kind: "reviewer_instruction_path",
			Path: ".github/CODEOWNERS", Disposition: domain.DispositionBlocking,
		}))},
		"blocking verification-control finding": {build: mkCase(withFinding(domain.CandidateFinding{
			Class: domain.FindingClassControlPlane, Category: catPtr(domain.ControlPlaneVerificationRecipes),
			Origin: domain.FindingOriginVerification, Kind: "verification_control_path",
			Path: ".freeside/recipe.yml", Disposition: domain.DispositionBlocking,
		}))},
		"authorization for a different head": {build: mkCase(func(in *domain.CandidateAuthorizationInput) {
			in.HeadSHA = testOtherSHA // authorizes, but not this candidate's head
		})},
		"authorization for a different recipe": {build: mkCase(func(in *domain.CandidateAuthorizationInput) {
			in.VerificationRecipeDigest = domain.Digest("sha256:" + strings.Repeat("c", 64))
		})},
		"authorization for a different trust profile": {build: mkCase(func(in *domain.CandidateAuthorizationInput) {
			in.TrustProfileDigest = domain.Digest("sha256:" + strings.Repeat("e", 64))
		})},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			ledger := newMemoryLedger()
			c := testCandidate(t)
			authz := tc.build(t, &c)
			p := newTestPublisherFull(t, gh, ledger, conformantTrust(t), authz)

			if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrUnauthorizedPublication) {
				t.Fatalf("err = %v, want ErrUnauthorizedPublication", err)
			}
			if len(ledger.keys) != 0 {
				t.Error("unauthorized candidate recorded an intent")
			}
			if reqs := gh.requestLog(); len(reqs) != 0 {
				t.Errorf("unauthorized candidate dispatched %v", reqs)
			}
		})
	}

	// A record whose derived trust bit was forged fails closed at the #52
	// re-gate (Validate recomputes it), surfacing the domain inconsistency
	// rather than silently trusting the forged value.
	t.Run("forged authorizes bit fails closed", func(t *testing.T) {
		gh := newFakeGitHub(t)
		ledger := newMemoryLedger()
		in := authorizingInput(t)
		in.VerificationOutcome = domain.VerificationFailed // truthfully non-authorizing
		auth := newAuthorization(t, in)
		auth.AuthorizesPublication = true // forge the trust bit post-construction
		c := testCandidate(t)
		id := auth.ID
		c.AuthorizationID = &id
		p := newTestPublisherFull(t, gh, ledger, conformantTrust(t), memoryAuthorizationSource{
			auths: map[domain.Digest]domain.CandidateAuthorization{id: auth},
		})
		if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, domain.ErrAuthorizationInconsistent) {
			t.Fatalf("err = %v, want ErrAuthorizationInconsistent", err)
		}
		if len(ledger.keys) != 0 || len(gh.requestLog()) != 0 {
			t.Error("forged authorization left an effect")
		}
	})

	// An authorization-source read failure fails closed too, with no effect.
	t.Run("authorization source error", func(t *testing.T) {
		gh := newFakeGitHub(t)
		ledger := newMemoryLedger()
		p := newTestPublisherFull(t, gh, ledger, conformantTrust(t),
			memoryAuthorizationSource{err: errors.New("store unavailable")})
		if _, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes()); err == nil {
			t.Fatal("published despite an authorization-source failure")
		}
		if len(ledger.keys) != 0 || len(gh.requestLog()) != 0 {
			t.Error("authorization-source failure left an effect")
		}
	})

	// A waivable finding carrying a valid non-agent waiver authorizes: the
	// human decision record makes an otherwise-blocking repo-change-policy
	// finding publishable (§5.12), and the candidate converges normally.
	t.Run("waived finding authorizes publication", func(t *testing.T) {
		gh := newFakeGitHub(t)
		ledger := newMemoryLedger()
		in := authorizingInput(t)
		in.Findings = []domain.CandidateFinding{{
			Class: domain.FindingClassRepoChangePolicy, Origin: domain.FindingOriginImport,
			Kind: "size_violation", Path: "artifacts/big.bin",
			Disposition: domain.DispositionWaived,
			Waiver: &domain.WaiverRecord{
				DecisionID: "decision-1", DecidedBy: domain.AuthorUser,
				DecidedAt:      time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				Justification:  "accepted large fixture",
				DecisionDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
			},
		}}
		auth := newAuthorization(t, in)
		if !auth.AuthorizesPublication {
			t.Fatal("waived finding did not authorize publication")
		}
		c := testCandidate(t)
		id := auth.ID
		c.AuthorizationID = &id
		p := newTestPublisherFull(t, gh, ledger, conformantTrust(t), authzWith(auth))
		res, err := p.Publish(context.Background(), c, testApprovedRecipes())
		if err != nil {
			t.Fatalf("Publish with waived finding: %v", err)
		}
		if !res.BranchCreated || !res.PRCreated {
			t.Errorf("waived candidate did not converge: %+v", res)
		}
	})
}

// TestPublishRejectsMarkerShapedBody: prose that would parse as (or
// conflict with) an identity marker is refused before any effect —
// otherwise the publisher's own PR would later fail marker parsing
// and convergence would deadlock as ErrForeignResource.
func TestPublishRejectsMarkerShapedBody(t *testing.T) {
	for name, body := range map[string]string{
		"quoted foreign marker": "Quoting the other PR:\n<!-- freeside:publication-identity=sha256:" + strings.Repeat("d", 64) + " -->",
		"malformed marker line": "<!-- freeside:publication-identity=sha256:zz -->",
	} {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			ledger := newMemoryLedger()
			p := newTestPublisher(t, gh, ledger)

			c := testCandidate(t)
			c.Body = body
			if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); err == nil {
				t.Fatal("marker-shaped body published")
			}
			if len(ledger.keys) != 0 {
				t.Error("marker-shaped body recorded an intent")
			}
			if reqs := gh.requestLog(); len(reqs) != 0 {
				t.Errorf("marker-shaped body dispatched %v", reqs)
			}
		})
	}
}

// TestPublishIgnoresForkPRWithCopiedMarker: markers are public, so a
// fork PR whose fork branch copies our branch name and marker can
// appear in a broader-than-asked list response; it does not occupy
// this repository's head ref and must be skipped, not adopted.
func TestPublishIgnoresForkPRWithCopiedMarker(t *testing.T) {
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())

	c := testCandidate(t)
	id, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo:            c.Repo,
		BaseRef:         c.BaseRef,
		SourceHeadSHA:   c.HeadSHA,
		ArtifactDigests: []domain.Digest{testArtifactD},
		RecipeDigest:    c.RecipeDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	gh.prs = append(gh.prs, fakePR{
		Number:   7,
		State:    "open",
		Title:    "attacker",
		Body:     id.Marker(),
		HeadRef:  id.BranchName(),
		HeadSHA:  testOtherSHA,
		HeadRepo: "attacker/evidence-repo",
	})

	res, err := p.Publish(context.Background(), c, testApprovedRecipes())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !res.PRCreated || res.PRNumber == 7 {
		t.Errorf("publisher adopted the fork PR: %+v", res)
	}
	if gh.prs[0].Title != "attacker" {
		t.Error("publisher patched the fork PR")
	}
}

// TestPublishRefusesMarkedPRAtWrongHead: a marked PR whose head SHA is
// not the candidate head means the branch moved between checks;
// converging would publish under the wrong commit.
func TestPublishRefusesMarkedPRAtWrongHead(t *testing.T) {
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())

	c := testCandidate(t)
	id, err := publish.DeriveIdentity(publish.IdentityInput{
		Repo:            c.Repo,
		BaseRef:         c.BaseRef,
		SourceHeadSHA:   c.HeadSHA,
		ArtifactDigests: []domain.Digest{testArtifactD},
		RecipeDigest:    c.RecipeDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	gh.refs[id.BranchName()] = testHeadSHA
	gh.prs = append(gh.prs, fakePR{Number: 8, State: "open", Title: c.Title, Body: id.Marker(), HeadRef: id.BranchName(), HeadSHA: testOtherSHA})

	if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("err = %v, want ErrPublicationConflict", err)
	}
}

// TestPublishRefusesCreatedPRAtWrongHead: the branch moves between the
// ref check and PR creation, so GitHub opens the PR from the moved
// tip; the returned head must be verified before reporting success, or
// the publication would claim a commit its evidence was not produced
// for.
func TestPublishRefusesCreatedPRAtWrongHead(t *testing.T) {
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())

	c := testCandidate(t)
	gh.onRequest = func(method, path string) {
		// Simulate a concurrent writer moving every branch right before
		// the PR is opened.
		if method == http.MethodPost && strings.HasSuffix(path, "/pulls") {
			for branch := range gh.refs {
				gh.refs[branch] = testOtherSHA
			}
		}
	}

	if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("err = %v, want ErrPublicationConflict", err)
	}
}

// TestPublishRefusesCreatedPRFromForkHead: a created PR whose returned
// head resolves to a fork repository (same branch name, even the same
// SHA) is not a publication of this repository's branch and must fail
// closed rather than report success.
func TestPublishRefusesCreatedPRFromForkHead(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.createHeadRepo = "attacker/evidence-repo"
	p := newTestPublisher(t, gh, newMemoryLedger())

	if _, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("err = %v, want ErrPublicationConflict", err)
	}
}

// TestPublishRefusesRetargetedPR: a human retargeting the publication
// PR to another base changes coordinates the identity binds; the
// publisher must refuse rather than report success or patch a PR that
// now merges the candidate into the wrong branch.
func TestPublishRefusesRetargetedPR(t *testing.T) {
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())

	c := testCandidate(t)
	first, err := p.Publish(context.Background(), c, testApprovedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	for i := range gh.prs {
		if gh.prs[i].Number == first.PRNumber {
			gh.prs[i].BaseRef = "release"
		}
	}

	writes := len(gh.writeRequests())
	if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("err = %v, want ErrPublicationConflict", err)
	}
	if got := len(gh.writeRequests()); got != writes {
		t.Errorf("retargeted PR was written to: %v", gh.writeRequests()[writes:])
	}
}

// TestPublishRefusesPRMovedDuringPatch: the PR is retargeted between
// the list read and the drift PATCH; the patched response reveals the
// move, and the publisher must refuse rather than return success for
// coordinates the identity does not name.
func TestPublishRefusesPRMovedDuringPatch(t *testing.T) {
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())

	c := testCandidate(t)
	first, err := p.Publish(context.Background(), c, testApprovedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	gh.prs[0].Title = "drifted"
	gh.onRequest = func(method, path string) {
		if method == http.MethodPatch && strings.Contains(path, "/pulls/") {
			for i := range gh.prs {
				if gh.prs[i].Number == first.PRNumber {
					gh.prs[i].BaseRef = "release"
				}
			}
		}
	}

	if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("err = %v, want ErrPublicationConflict", err)
	}
}

// TestPublishRefusesPRClosedDuringPatch: the PR is closed between the
// list read and the drift PATCH; GitHub still accepts the edit and
// returns the closed PR, but a closed publication PR is a human
// decision, so the patched response's state must refuse success.
func TestPublishRefusesPRClosedDuringPatch(t *testing.T) {
	gh := newFakeGitHub(t)
	p := newTestPublisher(t, gh, newMemoryLedger())

	c := testCandidate(t)
	first, err := p.Publish(context.Background(), c, testApprovedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	gh.prs[0].Title = "drifted"
	gh.onRequest = func(method, path string) {
		if method == http.MethodPatch && strings.Contains(path, "/pulls/") {
			for i := range gh.prs {
				if gh.prs[i].Number == first.PRNumber {
					gh.prs[i].State = "closed"
				}
			}
		}
	}

	if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
		t.Fatalf("err = %v, want ErrPublicationConflict", err)
	}
}

// TestPublishRejectsRecipeMismatch: the identity records the recipe
// the candidate was verified under, so an artifact gated under a
// different (even approved) recipe, or a candidate without a recipe,
// is refused before any effect.
func TestPublishRejectsRecipeMismatch(t *testing.T) {
	otherRecipe := domain.Digest("sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	recipes := testApprovedRecipes()
	recipes[otherRecipe] = true

	a, err := domain.NewArtifact(domain.ArtifactInput{
		ID:     "artifact-2",
		Type:   "verification-evidence",
		Digest: testArtifactD,
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-producer",
			HeadBinding:              domain.HeadBound,
			SourceHeadSHA:            testHeadSHA,
			VerificationRecipeDigest: &otherRecipe,
			SensitivityClass:         domain.SensitivityNormal,
		},
	}, recipes)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]func(*publish.Candidate){
		"artifact under another recipe": func(c *publish.Candidate) { c.Artifacts = []domain.Artifact{a} },
		"candidate without a recipe":    func(c *publish.Candidate) { c.RecipeDigest = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			ledger := newMemoryLedger()
			p := newTestPublisher(t, gh, ledger)

			c := testCandidate(t)
			mutate(&c)
			if _, err := p.Publish(context.Background(), c, recipes); !errors.Is(err, publish.ErrPublicationConflict) {
				t.Fatalf("err = %v, want ErrPublicationConflict", err)
			}
			if len(ledger.keys) != 0 || len(gh.requestLog()) != 0 {
				t.Error("recipe mismatch produced effects")
			}
		})
	}
}

// TestPublishRefusesMangledStoredContent: the store must hold exactly
// the content that was sent, on both write paths — otherwise the
// publisher reports converged while the PR stays drifted and every
// later publication silently re-patches.
func TestPublishRefusesMangledStoredContent(t *testing.T) {
	t.Run("create path", func(t *testing.T) {
		gh := newFakeGitHub(t)
		gh.mangleStoredTitle = true
		p := newTestPublisher(t, gh, newMemoryLedger())
		if _, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
			t.Fatalf("err = %v, want ErrPublicationConflict", err)
		}
	})
	t.Run("patch path", func(t *testing.T) {
		gh := newFakeGitHub(t)
		p := newTestPublisher(t, gh, newMemoryLedger())
		c := testCandidate(t)
		if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); err != nil {
			t.Fatal(err)
		}
		gh.prs[0].Title = "drifted"
		gh.mangleStoredTitle = true
		if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); !errors.Is(err, publish.ErrPublicationConflict) {
			t.Fatalf("err = %v, want ErrPublicationConflict", err)
		}
	})
}

// TestPublishRefusesWrongRefCreateEcho: a ref-create response that does
// not echo the requested ref and commit is not proof the branch exists
// where the candidate needs it.
func TestPublishRefusesWrongRefCreateEcho(t *testing.T) {
	gh := newFakeGitHub(t)
	gh.mangleRefCreate = true
	p := newTestPublisher(t, gh, newMemoryLedger())
	if _, err := p.Publish(context.Background(), testCandidate(t), testApprovedRecipes()); err == nil {
		t.Fatal("mangled ref-create echo accepted, want error")
	}
}

// TestPublishRejectsMalformedPullList: a list response the package
// cannot fully interpret must not drive publication — JSON null is not
// an authoritative empty list, trailing data is rejected, and a
// decision-driving row without a positive number fails the read
// instead of becoming a "successful" PR #0.
func TestPublishRejectsMalformedPullList(t *testing.T) {
	c := func(t *testing.T) publish.Candidate { t.Helper(); return testCandidate(t) }
	id := func(t *testing.T) publish.Identity {
		t.Helper()
		cand := c(t)
		derived, err := publish.DeriveIdentity(publish.IdentityInput{
			Repo:            cand.Repo,
			BaseRef:         cand.BaseRef,
			SourceHeadSHA:   cand.HeadSHA,
			ArtifactDigests: []domain.Digest{testArtifactD},
			RecipeDigest:    cand.RecipeDigest,
		})
		if err != nil {
			t.Fatal(err)
		}
		return derived
	}

	zeroNumberRow := func(t *testing.T) string {
		t.Helper()
		derived := id(t)
		return fmt.Sprintf(`[{"number":0,"state":"open","title":"t","body":%q,`+
			`"head":{"ref":%q,"sha":%q,"repo":{"full_name":"freeside-ai/evidence-repo"}},`+
			`"base":{"ref":"main","repo":{"full_name":"freeside-ai/evidence-repo"}}}]`,
			derived.Marker(), derived.BranchName(), testHeadSHA)
	}

	cases := map[string]func(*testing.T) string{
		"null list":     func(*testing.T) string { return `null` },
		"trailing data": func(*testing.T) string { return `[]garbage` },
		"zero number":   zeroNumberRow,
	}
	for name, bodyFn := range cases {
		t.Run(name, func(t *testing.T) {
			gh := newFakeGitHub(t)
			gh.pullListBody = bodyFn(t)
			p := newTestPublisher(t, gh, newMemoryLedger())
			if _, err := p.Publish(context.Background(), c(t), testApprovedRecipes()); err == nil {
				t.Fatal("malformed pull list accepted, want error")
			}
			if len(gh.prs) != 0 {
				t.Errorf("malformed pull list drove a PR create: %+v", gh.prs)
			}
		})
	}
}

// TestPublishValidation covers fail-fast candidate checks.
func TestPublishValidation(t *testing.T) {
	p := newTestPublisher(t, newFakeGitHub(t), newMemoryLedger())
	cases := map[string]func(*publish.Candidate){
		"bad repo":      func(c *publish.Candidate) { c.Repo = "no-owner" },
		"empty title":   func(c *publish.Candidate) { c.Title = "" },
		"no artifacts":  func(c *publish.Candidate) { c.Artifacts = nil },
		"empty base":    func(c *publish.Candidate) { c.BaseRef = "" },
		"empty head":    func(c *publish.Candidate) { c.HeadSHA = "" },
		"no invocation": func(c *publish.Candidate) { c.InvocationID = "" },
	}
	for name, mutate := range cases {
		c := testCandidate(t)
		mutate(&c)
		if _, err := p.Publish(context.Background(), c, testApprovedRecipes()); err == nil {
			t.Errorf("%s accepted, want error", name)
		}
	}
}
