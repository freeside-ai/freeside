package engine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// researchTokenNeedle is a recognizable token value, so a leak into an item
// reason or error shows up as a substring match.
const researchTokenNeedle = "ghs_ENGINEISSUEREADNEEDLE00000000" //nolint:gosec // leak-detection needle, not a credential

type fakeResearchTokens struct {
	mu           sync.Mutex
	repositoryID int64
	repos        []string
}

func (f *fakeResearchTokens) Token(_ context.Context, repo string) (publish.InstallationToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, repo)
	return publish.InstallationToken{
		Token: publish.Secret(researchTokenNeedle), RepositoryID: f.repositoryID,
		Repo: repo, Permissions: publish.IssueReadPermissions,
	}, nil
}

func (f *fakeResearchTokens) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.repos...)
}

type privateIssueRequest struct {
	url        string
	authorized bool
	bare       bool
}

// privateIssueTransport plays GitHub for a private repository with the given
// ID: its issue and pull request endpoints answer 404 unless the request
// carries the needle bearer token, and issue 404 is absent even with it. Any other URL answers
// 200 as public research.
type privateIssueTransport struct {
	repositoryID string
	mu           sync.Mutex
	seen         []privateIssueRequest
}

func (tr *privateIssueTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	authorization := request.Header.Get("Authorization")
	authorized := authorization == "Bearer "+researchTokenNeedle
	tr.mu.Lock()
	tr.seen = append(tr.seen, privateIssueRequest{url: request.URL.String(), authorized: authorized, bare: authorization == ""})
	tr.mu.Unlock()
	status, body := http.StatusOK, "public research"
	if request.URL.Host == "api.github.com" {
		status, body = http.StatusNotFound, `{"message":"Not Found"}`
		prefix := "/repositories/" + tr.repositoryID + "/issues/"
		pullPrefix := "/repositories/" + tr.repositoryID + "/pulls/"
		switch {
		case authorized && strings.HasPrefix(request.URL.Path, prefix) && request.URL.Path != prefix+"404":
			status, body = http.StatusOK, `{"title":"Private work","body":"Private issue text."}`
		case authorized && strings.HasPrefix(request.URL.Path, pullPrefix):
			status, body = http.StatusOK, `{"title":"Earlier work","body":"Settled contract."}`
		}
	}
	return &http.Response{
		StatusCode: status, Request: request,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}, nil
}

func (tr *privateIssueTransport) requests() []privateIssueRequest {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]privateIssueRequest(nil), tr.seen...)
}

// TestTaskNamerReadsAPrivateIssue drives label intake's first read of the
// source issue: the namer fetches the issue by repository ID, authenticated
// to the run's registered project.
func TestTaskNamerReadsAPrivateIssue(t *testing.T) {
	for _, tc := range []struct {
		label  string
		tokens publish.TokenSource
		want   string
	}{
		{"with token source", &fakeResearchTokens{repositoryID: 42}, ""},
		{"without token source", nil, "no installation token source is composed"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			r := newIssueSubjectReservation(t)
			if _, err := SubmitSpecificationRun(t.Context(), r.store, r.spec); err != nil {
				t.Fatal(err)
			}
			var run domain.Run
			if err := r.store.Write(t.Context(), func(tx *store.WriteTx) error {
				var err error
				if run, err = tx.GetRun(t.Context(), r.specificationRunID); err != nil {
					return err
				}
				return tx.RegisterProject(t.Context(), domain.Project{
					ID: run.ProjectID, Repo: "freeasinbird/freeside", RepositoryID: 42,
				})
			}); err != nil {
				t.Fatal(err)
			}
			transport := &privateIssueTransport{repositoryID: "42"}
			fetcher, err := specify.NewFetcherWithIssueTokens(r.store, r.blobs, transport, tc.tokens)
			if err != nil {
				t.Fatal(err)
			}
			namer := inferencefake.New()
			namer.Script(inference.TaskNamerSiteID, inferencefake.Script{Response: inference.Response{Output: []byte(`{"name":"Private work"}`)}})
			e := &Engine{store: r.store, inference: taskNamingClient(t, namer), specification: &specificationWorkflow{fetcher: fetcher, blobs: r.blobs}}
			err = e.nameTaskIfUnnamed(t.Context(), run)
			seen := transport.requests()
			if len(seen) != 1 || seen[0].url != "https://api.github.com/repositories/42/issues/7" {
				t.Fatalf("research requests = %+v", seen)
			}
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), researchTokenNeedle) {
					t.Fatalf("private issue without credentials = %v, want the cause %q", err, tc.want)
				}
				return
			}
			if err != nil || !seen[0].authorized {
				t.Fatalf("private issue naming = %v, authorized = %t", err, seen[0].authorized)
			}
			requests := namer.Requests()
			if len(requests) != 1 || requests[0].Fields["issue_title"] != "Private work" ||
				requests[0].Fields["issue_body"] != "Private issue text." {
				t.Fatalf("namer inputs = %+v", requests)
			}
		})
	}
}

// TestSpecificationResearchReadsAPrivateProjectIssue drives the path
// `freesided submit -task <bare URL>` and the composer share: the writer
// sees only the task text, so it asks for the issue's owner/name API URL.
// The fetch goes to the project's repository-ID endpoint with the token,
// while the writer's other request goes without one.
func TestSpecificationResearchReadsAPrivateProjectIssue(t *testing.T) {
	f := newSpecificationFixtureAllowing(t, true, 3, "https://api.github.com,https://docs.example")
	tokens := &fakeResearchTokens{repositoryID: 1}
	transport := &privateIssueTransport{repositoryID: "1"}
	f.researchTransport, f.issueTokens = transport, tokens
	driver := f.newDriver(t)
	firstID := specificationInvocationID("specification-run", 1)
	if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{FetchRequests: []specify.FetchRequest{
		{URL: "https://api.github.com/repos/owner/repo/issues/7", Purpose: "read the source issue"},
		{URL: "https://docs.example/notes", Purpose: "read the linked notes"},
		{URL: "https://api.github.com/repos/other/repo/issues/7", Purpose: "read another repository's issue"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 2), 0, 0, specify.Output{
		Specification: &specify.Specification{
			Summary: "The private issue was read.", Body: "# Specification\n\nUse the private issue.",
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	// The third request names a repository the token is not for, so its
	// unauthenticated 404 is recorded as research and the run goes on.
	for pass := 1; pass <= 3; pass++ {
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	seen := transport.requests()
	if len(seen) != 3 {
		t.Fatalf("research requests = %+v", seen)
	}
	if seen[0].url != "https://api.github.com/repositories/1/issues/7" || !seen[0].authorized {
		t.Errorf("project issue request = %+v, want the authenticated ID endpoint", seen[0])
	}
	if seen[1].url != "https://docs.example/notes" || !seen[1].bare {
		t.Errorf("outside request = %+v, want no credential", seen[1])
	}
	if seen[2].url != "https://api.github.com/repos/other/repo/issues/7" || !seen[2].bare {
		t.Errorf("other repository request = %+v, want no credential", seen[2])
	}
	if calls := tokens.calls(); len(calls) != 1 || calls[0] != "owner/repo" {
		t.Errorf("token source calls = %v", calls)
	}
	var artifact domain.Artifact
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		artifact, err = tx.GetArtifact(t.Context(), domain.ArtifactID("research-"+string(firstID)+"-1"))
		return err
	}); err != nil {
		t.Fatalf("private issue research artifact: %v", err)
	}
	body, err := readBoundedArtifactBlob(f.blobs, artifact.Digest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := specify.DecodeResearchEvidence(body)
	if err != nil || evidence.Body != `{"title":"Private work","body":"Private issue text."}` ||
		evidence.Source.URL != "https://api.github.com/repos/owner/repo/issues/7" ||
		evidence.Source.FinalURL != "https://api.github.com/repositories/1/issues/7" {
		t.Fatalf("private issue evidence = %+v, %v", evidence, err)
	}
	if strings.Contains(string(body), researchTokenNeedle) {
		t.Fatal("stored research carries the token")
	}
	assertNoExecutionFailure(t, f, firstID)
	other := f.artifact(t, domain.ArtifactID("research-"+string(firstID)+"-3"))
	body, err = readBoundedArtifactBlob(f.blobs, other.Digest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err = specify.DecodeResearchEvidence(body)
	if err != nil || !strings.Contains(evidence.Source.Failure, "HTTP 404") ||
		strings.Contains(evidence.Source.Failure, "managed repository") {
		t.Fatalf("other repository failure = %+v, %v", evidence.Source, err)
	}
}

// TestSpecificationResearchReadsAPrivateProjectPullRequest covers a writer
// reading an earlier pull request of the project for its settled contract
// (#2311): the owner/name request goes to the ID endpoint with the token.
func TestSpecificationResearchReadsAPrivateProjectPullRequest(t *testing.T) {
	f := newSpecificationFixtureAllowing(t, true, 3, "https://api.github.com")
	tokens := &fakeResearchTokens{repositoryID: 1}
	transport := &privateIssueTransport{repositoryID: "1"}
	f.researchTransport, f.issueTokens = transport, tokens
	driver := f.newDriver(t)
	firstID := specificationInvocationID("specification-run", 1)
	if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{FetchRequests: []specify.FetchRequest{
		{URL: "https://api.github.com/repos/owner/repo/pulls/1", Purpose: "read the earlier pull request"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 2), 0, 0, specify.Output{
		Specification: &specify.Specification{
			Summary: "The earlier pull request was read.", Body: "# Specification\n\nKeep the settled contract.",
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	for pass := 1; pass <= 2; pass++ {
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	seen := transport.requests()
	if len(seen) != 1 || seen[0].url != "https://api.github.com/repositories/1/pulls/1" || !seen[0].authorized {
		t.Fatalf("research requests = %+v, want one authenticated ID-endpoint request", seen)
	}
	var artifact domain.Artifact
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		artifact, err = tx.GetArtifact(t.Context(), domain.ArtifactID("research-"+string(firstID)+"-1"))
		return err
	}); err != nil {
		t.Fatalf("private pull request research artifact: %v", err)
	}
	body, err := readBoundedArtifactBlob(f.blobs, artifact.Digest, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := specify.DecodeResearchEvidence(body)
	if err != nil || evidence.Body != `{"title":"Earlier work","body":"Settled contract."}` ||
		evidence.Source.FinalURL != "https://api.github.com/repositories/1/pulls/1" {
		t.Fatalf("private pull request evidence = %+v, %v", evidence, err)
	}
	if strings.Contains(string(body), researchTokenNeedle) {
		t.Fatal("stored research carries the token")
	}
}

// TestSpecificationResearchHandsBackWhyAPrivateIssueFailed covers a fetch
// the token can't make (#2311): the run goes on, and the next iteration sees
// a research envelope naming the cause, never the token or the server's body.
func TestSpecificationResearchHandsBackWhyAPrivateIssueFailed(t *testing.T) {
	for _, tc := range []struct {
		label  string
		tokens publish.TokenSource
		issue  string
		want   string
	}{
		{"no token source", nil, "7", "no installation token source is composed"},
		{"authenticated miss", &fakeResearchTokens{repositoryID: 1}, "404", "with the managed repository's read-only installation token"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			f := newSpecificationFixtureAllowing(t, true, 2, "https://api.github.com")
			f.researchTransport, f.issueTokens = &privateIssueTransport{repositoryID: "1"}, tc.tokens
			driver, capturing := newCapturingResearchDriver(t, f)
			firstID := specificationInvocationID("specification-run", 1)
			secondID := specificationInvocationID("specification-run", 2)
			if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{FetchRequests: []specify.FetchRequest{{
				URL: "https://api.github.com/repos/owner/repo/issues/" + tc.issue, Purpose: "read the source issue",
			}}}); err != nil {
				t.Fatal(err)
			}
			if err := specifyfake.Script(driver, secondID, 0, 0, specify.Output{Specification: &specify.Specification{
				Summary: "Worked from the task text.", Body: "# Specification\n\nThe issue was unreadable.",
				Addressals: []specify.Addressal{},
			}}); err != nil {
				t.Fatal(err)
			}
			f.submit(t)
			engine := f.newEngine(t, capturing)
			for pass := 1; pass <= 2; pass++ {
				if _, err := engine.Reconcile(t.Context()); err != nil {
					t.Fatalf("reconcile pass %d: %v", pass, err)
				}
			}
			assertNoExecutionFailure(t, f, firstID)
			prompt, ok := capturing.prompt(secondID)
			if !ok || len(prompt.bodies) != 1 {
				t.Fatalf("second iteration prompt captured = %t with %d prior artifacts", ok, len(prompt.bodies))
			}
			if strings.Contains(string(prompt.bodies[0]), researchTokenNeedle) ||
				strings.Contains(string(prompt.bodies[0]), "Not Found") {
				t.Fatal("research envelope carries the token or the server's error body")
			}
			var envelope specificationPriorArtifactEnvelope
			if err := json.Unmarshal(prompt.bodies[0], &envelope); err != nil {
				t.Fatal(err)
			}
			if envelope.Role != "research" || envelope.Body != "" || envelope.Source == nil ||
				envelope.Source.Status != http.StatusNotFound || !strings.Contains(envelope.Source.Failure, "HTTP 404") ||
				!strings.Contains(envelope.Source.Failure, tc.want) {
				t.Fatalf("research envelope = %+v, source = %+v; want a 404 naming %q", envelope, envelope.Source, tc.want)
			}
		})
	}
}

// TestSpecificationResearchFailuresStayWithinTheIterationBudget covers a
// specifier that keeps asking for research that fails: max_iterations still
// ends the run.
func TestSpecificationResearchFailuresStayWithinTheIterationBudget(t *testing.T) {
	f := newSpecificationFixtureAllowing(t, true, 2, "https://api.github.com")
	f.researchTransport, f.issueTokens = &privateIssueTransport{repositoryID: "1"}, &fakeResearchTokens{repositoryID: 1}
	driver := f.newDriver(t)
	missing := specify.Output{FetchRequests: []specify.FetchRequest{{
		URL: "https://api.github.com/repos/owner/repo/issues/404", Purpose: "read the source issue",
	}}}
	for iteration := 1; iteration <= 2; iteration++ {
		if err := specifyfake.Script(driver, specificationInvocationID("specification-run", iteration), 0, 0, missing); err != nil {
			t.Fatal(err)
		}
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	for pass := 1; pass <= 2; pass++ {
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	assertNoExecutionFailure(t, f, specificationInvocationID("specification-run", 1))
	item, _ := f.item(t, domain.ItemID("execution-failure-"+string(specificationInvocationID("specification-run", 2))))
	if !strings.Contains(item.Reason, ErrSpecificationIterationsExhausted.Error()) {
		t.Fatalf("second iteration failure reason = %q, want the iteration budget", item.Reason)
	}
}

func newCapturingResearchDriver(t *testing.T, f specificationFixture) (*execfake.StageDriver, *capturingSpecificationDriver) {
	t.Helper()
	driver := f.newDriver(t)
	materializer, err := exec.NewMaterializer(f.blobs, exec.MaterializerOptions{
		MaxInputBytes: exec.ProductionMaxInputBytes, MaxTotalBytes: exec.ProductionMaxTotalInputBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	return driver, &capturingSpecificationDriver{
		StageDriver: driver, materializer: materializer,
		prompts: make(map[domain.InvocationID]capturedSpecificationPrompt),
	}
}

func assertNoExecutionFailure(t *testing.T, f specificationFixture, id domain.InvocationID) {
	t.Helper()
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(t.Context(), domain.ItemID("execution-failure-"+string(id)))
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("execution failure for %s = %v, want none", id, err)
	}
}
