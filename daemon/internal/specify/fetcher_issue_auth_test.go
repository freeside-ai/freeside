package specify_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
)

const (
	issueRepo         = "freeside-ai/private-repo"
	issueRepositoryID = 4242
	// issueTokenNeedle is recognizable so a leak into an envelope or error
	// shows up as a substring match.
	issueTokenNeedle = "ghs_ISSUEREADNEEDLE0000000000000" //nolint:gosec // leak-detection needle, not a credential
)

var issueScope = specify.ManagedRepository{Repo: issueRepo, RepositoryID: issueRepositoryID}

var issueAllowlist = []string{"https://api.github.com", "https://docs.example"}

type fakeIssueTokens struct {
	mu    sync.Mutex
	token publish.InstallationToken
	err   error
	repos []string
}

func (f *fakeIssueTokens) Token(_ context.Context, repo string) (publish.InstallationToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repos = append(f.repos, repo)
	return f.token, f.err
}

func (f *fakeIssueTokens) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.repos...)
}

func validIssueTokens() *fakeIssueTokens {
	return &fakeIssueTokens{token: publish.InstallationToken{
		Token: publish.Secret(issueTokenNeedle), RepositoryID: issueRepositoryID,
		Repo: issueRepo, Permissions: publish.IssueReadPermissions,
	}}
}

type seenRequest struct {
	url           string
	authorization string
}

// issueTransport plays GitHub for a private repository: its issue and pull
// request endpoints answer 404 unless the request carries the needle bearer
// token. Issue 100
// is readable without credentials (a public issue), issue 404 is absent even
// with them, and issues 8 and 10 redirect. A non-zero rejectToken is the
// status every authenticated request gets, as GitHub answers a revoked token.
type issueTransport struct {
	rejectToken int
	mu          sync.Mutex
	seen        []seenRequest
}

func (tr *issueTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	tr.mu.Lock()
	tr.seen = append(tr.seen, seenRequest{url: request.URL.String(), authorization: request.Header.Get("Authorization")})
	tr.mu.Unlock()
	authorized := request.Header.Get("Authorization") == "Bearer "+issueTokenNeedle
	path := request.URL.Path
	switch {
	case request.URL.Host != "api.github.com":
		return response(request, http.StatusOK, "text/plain", "public page"), nil
	case authorized && tr.rejectToken != 0:
		return response(request, tr.rejectToken, "application/json", `{"message":"Bad credentials"}`), nil
	case path == "/repositories/4242/issues/8":
		redirect := response(request, http.StatusFound, "", "")
		redirect.Header.Set("Location", "https://api.github.com/repositories/4242/issues/9")
		return redirect, nil
	case path == "/repositories/4242/issues/10":
		redirect := response(request, http.StatusFound, "", "")
		redirect.Header.Set("Location", "https://docs.example/landing")
		return redirect, nil
	case path == "/repositories/4242/issues/100":
		return response(request, http.StatusOK, "application/json", `{"body":"public issue"}`), nil
	case path == "/repositories/4242/issues/404":
		return response(request, http.StatusNotFound, "application/json", `{"message":"Not Found"}`), nil
	case strings.HasPrefix(path, "/repositories/4242/issues/") && authorized:
		return response(request, http.StatusOK, "application/json", `{"body":"private issue text"}`), nil
	case strings.HasPrefix(path, "/repositories/4242/pulls/") && authorized:
		return response(request, http.StatusOK, "application/json", `{"body":"private pull request text"}`), nil
	default:
		return response(request, http.StatusNotFound, "application/json", `{"message":"Not Found"}`), nil
	}
}

func (tr *issueTransport) requests() []seenRequest {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]seenRequest(nil), tr.seen...)
}

func newIssueFetcher(t *testing.T, tokens publish.TokenSource) (*specify.Fetcher, *issueTransport, *signet.BlobStore) {
	t.Helper()
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transport := &issueTransport{}
	fetcher, err := specify.NewFetcherWithIssueTokens(openStore(t), blobs, transport, tokens)
	if err != nil {
		t.Fatal(err)
	}
	return fetcher, transport, blobs
}

func storedEnvelope(t *testing.T, blobs *signet.BlobStore, artifact specify.ResearchArtifact) []byte {
	t.Helper()
	reader, err := blobs.Open(artifact.Artifact.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func fetchIssue(
	t *testing.T, fetcher *specify.Fetcher, id string, rawURL string, scope specify.ManagedRepository,
) (specify.ResearchArtifact, error) {
	t.Helper()
	return fetcher.Fetch(t.Context(), domain.InvocationID(id), 1,
		specify.FetchRequest{URL: rawURL, Purpose: "read the source issue"}, scope, issueAllowlist, 1024)
}

func TestFetcherAuthenticatesTheManagedRepositoryIssue(t *testing.T) {
	t.Parallel()
	tokens := validIssueTokens()
	fetcher, transport, blobs := newIssueFetcher(t, tokens)
	const idURL = "https://api.github.com/repositories/4242/issues/7"
	got, err := fetchIssue(t, fetcher, "inv-id-form", idURL, issueScope)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != idURL || got.FinalURL != idURL {
		t.Fatalf("result URLs = %q, %q", got.URL, got.FinalURL)
	}
	seen := transport.requests()
	if len(seen) != 1 || seen[0].url != idURL || seen[0].authorization != "Bearer "+issueTokenNeedle {
		t.Fatalf("transport saw %+v", redactSeen(seen))
	}
	if calls := tokens.calls(); len(calls) != 1 || calls[0] != issueRepo {
		t.Fatalf("token source calls = %v", calls)
	}
	evidence, err := specify.DecodeResearchEvidence(storedEnvelope(t, blobs, got))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Body != `{"body":"private issue text"}` {
		t.Fatalf("evidence body = %q", evidence.Body)
	}
}

// TestFetcherAuthenticatesTheManagedRepositoryPullRequest covers the earlier
// pull request a specifier reads for its settled contract (#2311): the pull
// request and its sub-resources read with the token on a private repository
// and 404 without it.
func TestFetcherAuthenticatesTheManagedRepositoryPullRequest(t *testing.T) {
	t.Parallel()
	for _, resource := range []string{"pulls/1", "pulls/1/comments", "pulls/1/reviews", "pulls/1/files"} {
		t.Run(resource, func(t *testing.T) {
			t.Parallel()
			idURL := "https://api.github.com/repositories/4242/" + resource
			id := "inv-" + strings.ReplaceAll(resource, "/", "-")
			fetcher, transport, blobs := newIssueFetcher(t, validIssueTokens())
			got, err := fetchIssue(t, fetcher, id, idURL, issueScope)
			if err != nil {
				t.Fatal(err)
			}
			seen := transport.requests()
			if len(seen) != 1 || seen[0].url != idURL || seen[0].authorization != "Bearer "+issueTokenNeedle {
				t.Fatalf("transport saw %+v", redactSeen(seen))
			}
			evidence, err := specify.DecodeResearchEvidence(storedEnvelope(t, blobs, got))
			if err != nil {
				t.Fatal(err)
			}
			if evidence.Body != `{"body":"private pull request text"}` {
				t.Fatalf("evidence body = %q", evidence.Body)
			}
			unauthenticated, _, _ := newIssueFetcher(t, nil)
			if _, err := fetchIssue(t, unauthenticated, id, idURL, issueScope); !errors.Is(err, specify.ErrResearchFetchFailed) ||
				!strings.Contains(err.Error(), "HTTP 404") {
				t.Fatalf("unauthenticated fetch error = %v, want HTTP 404", err)
			}
		})
	}
}

// TestFetcherRewritesTheOwnerNameIssueURL covers the writer's form: an
// owner/name issue URL for the scope's repository, in any case and with a
// sub-resource and query, goes to the repository-ID endpoint, and the
// requested URL stays the artifact's identity so a retry recovers it.
func TestFetcherRewritesTheOwnerNameIssueURL(t *testing.T) {
	t.Parallel()
	fetcher, transport, blobs := newIssueFetcher(t, validIssueTokens())
	const (
		writerURL = "https://api.github.com/repos/Freeside-AI/Private-Repo/issues/7/comments?per_page=5"
		idURL     = "https://api.github.com/repositories/4242/issues/7/comments?per_page=5"
	)
	got, err := fetchIssue(t, fetcher, "inv-owner-name", writerURL, issueScope)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != writerURL || got.FinalURL != idURL {
		t.Fatalf("result URLs = %q, %q", got.URL, got.FinalURL)
	}
	seen := transport.requests()
	if len(seen) != 1 || seen[0].url != idURL || seen[0].authorization != "Bearer "+issueTokenNeedle {
		t.Fatalf("transport saw %+v", redactSeen(seen))
	}
	evidence, err := specify.DecodeResearchEvidence(storedEnvelope(t, blobs, got))
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Source.URL != writerURL || evidence.Source.FinalURL != idURL {
		t.Fatalf("evidence source = %+v", evidence.Source)
	}
	recovered, err := fetchIssue(t, fetcher, "inv-owner-name", writerURL, issueScope)
	if err != nil || recovered.Artifact.Digest != got.Artifact.Digest || len(transport.requests()) != 1 {
		t.Fatalf("recovery = %+v, %v; transport calls = %d", recovered, err, len(transport.requests()))
	}
}

func TestFetcherRewritesTheOwnerNamePullRequestURL(t *testing.T) {
	t.Parallel()
	fetcher, transport, _ := newIssueFetcher(t, validIssueTokens())
	const (
		writerURL = "https://api.github.com/repos/freeside-ai/private-repo/pulls/1/comments"
		idURL     = "https://api.github.com/repositories/4242/pulls/1/comments"
	)
	got, err := fetchIssue(t, fetcher, "inv-owner-name-pull", writerURL, issueScope)
	if err != nil {
		t.Fatal(err)
	}
	if got.URL != writerURL || got.FinalURL != idURL {
		t.Fatalf("result URLs = %q, %q", got.URL, got.FinalURL)
	}
	seen := transport.requests()
	if len(seen) != 1 || seen[0].url != idURL || seen[0].authorization != "Bearer "+issueTokenNeedle {
		t.Fatalf("transport saw %+v", redactSeen(seen))
	}
}

func TestFetcherSendsNoCredentialOutsideTheManagedRepositoryEndpoints(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		url   string
		scope specify.ManagedRepository
	}{
		{"another repository ID", "https://api.github.com/repositories/9999/issues/7", issueScope},
		{"another owner/name", "https://api.github.com/repos/freeside-ai/other-repo/issues/7", issueScope},
		{"owner/name prefix", "https://api.github.com/repos/freeside-ai/private-repo-fork/issues/7", issueScope},
		{"another host", "https://docs.example/repositories/4242/issues/7", issueScope},
		{"non-issue path", "https://api.github.com/repositories/4242/commits/7", issueScope},
		{"issue listing", "https://api.github.com/repos/freeside-ai/private-repo/issues", issueScope},
		{"pull request listing", "https://api.github.com/repos/freeside-ai/private-repo/pulls", issueScope},
		{"zero pull request", "https://api.github.com/repositories/4242/pulls/0", issueScope},
		{"singular pull path", "https://api.github.com/repos/freeside-ai/private-repo/pull/1", issueScope},
		{"pull request in another repository", "https://api.github.com/repos/freeside-ai/other-repo/pulls/1", issueScope},
		{"non-numeric issue", "https://api.github.com/repositories/4242/issues/abc", issueScope},
		{"zero issue", "https://api.github.com/repositories/4242/issues/0", issueScope},
		{"leading-zero ID", "https://api.github.com/repositories/04242/issues/7", issueScope},
		{"dot segments", "https://api.github.com/repositories/4242/issues/7/../../../9999/issues/7", issueScope},
		{"encoded slash", "https://api.github.com/repos/freeside-ai%2Fprivate-repo/issues/7", issueScope},
		{"empty segment", "https://api.github.com/repositories/4242//issues/7", issueScope},
		{"query with a space", "https://api.github.com/repositories/4242/issues/7?a /repos/x/y/issues/1", issueScope},
		{"malformed query escape", "https://api.github.com/repositories/4242/issues/7?a=%zz", issueScope},
		{"zero scope", "https://api.github.com/repositories/4242/issues/7", specify.ManagedRepository{}},
		{
			"scope without ID", "https://api.github.com/repos/freeside-ai/private-repo/issues/7",
			specify.ManagedRepository{Repo: issueRepo},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tokens := validIssueTokens()
			fetcher, transport, _ := newIssueFetcher(t, tokens)
			_, err := fetchIssue(t, fetcher, "inv-"+strings.ReplaceAll(tc.name, " ", "-"), tc.url, tc.scope)
			if err != nil && strings.Contains(err.Error(), "managed repository") {
				t.Fatalf("out-of-scope failure names the managed repository: %v", err)
			}
			seen := transport.requests()
			if len(seen) != 1 || seen[0].url != tc.url || seen[0].authorization != "" {
				t.Fatalf("transport saw %+v, want one unauthenticated request for %q", redactSeen(seen), tc.url)
			}
			if calls := tokens.calls(); len(calls) != 0 {
				t.Fatalf("out-of-scope request asked for a token: %v", calls)
			}
		})
	}
}

func TestFetcherDropsTheCredentialOnRedirect(t *testing.T) {
	t.Parallel()
	for name, issue := range map[string]string{"same repository": "8", "another host": "10"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fetcher, transport, _ := newIssueFetcher(t, validIssueTokens())
			_, _ = fetchIssue(t, fetcher, "inv-redirect-"+issue,
				"https://api.github.com/repositories/4242/issues/"+issue, issueScope)
			seen := transport.requests()
			if len(seen) != 2 || seen[0].authorization == "" || seen[1].authorization != "" {
				t.Fatalf("transport saw %+v, want an authenticated request then a bare redirect", redactSeen(seen))
			}
		})
	}
}

func TestFetcherNamesWhyAnInScopeFetchFailed(t *testing.T) {
	t.Parallel()
	wrongRepository := validIssueTokens()
	wrongRepository.token.RepositoryID = 1
	cases := []struct {
		name   string
		tokens publish.TokenSource
		issue  string
		want   string
	}{
		{"no token source", nil, "7", "no installation token source is composed"},
		{
			"mint failure", &fakeIssueTokens{err: errors.New("mint: installation lacks issues access")}, "7",
			"the installation token could not be minted: mint: installation lacks issues access",
		},
		{"wrong repository token", wrongRepository, "7", "the installation token is for repository 1, not the task's repository 4242"},
		{"authenticated miss", validIssueTokens(), "404", "with the managed repository's read-only installation token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fetcher, transport, _ := newIssueFetcher(t, tc.tokens)
			_, err := fetchIssue(t, fetcher, "inv-"+strings.ReplaceAll(tc.name, " ", "-"),
				"https://api.github.com/repos/freeside-ai/private-repo/issues/"+tc.issue, issueScope)
			if !errors.Is(err, specify.ErrResearchFetchFailed) || !strings.Contains(err.Error(), "HTTP 404") ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want HTTP 404 naming %q", err, tc.want)
			}
			if strings.Contains(err.Error(), issueTokenNeedle) {
				t.Fatal("failure text carries the token")
			}
			seen := transport.requests()
			if tc.issue != "404" && (len(seen) != 1 || seen[0].authorization != "") {
				t.Fatalf("transport saw %+v, want one unauthenticated request", redactSeen(seen))
			}
		})
	}
}

// TestFetcherRetriesWithoutARejectedToken covers a cached token GitHub no
// longer honours: the request is repeated once without it, so a public issue
// still reads and a private one fails naming the refusal.
func TestFetcherRetriesWithoutARejectedToken(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, tc := range []struct{ issue, wantErr string }{
			{"100", ""},
			{"7", fmt.Sprintf("HTTP 404 without credentials, after GitHub refused the managed repository's read-only installation token with HTTP %d", status)},
		} {
			t.Run(fmt.Sprintf("%d issue %s", status, tc.issue), func(t *testing.T) {
				t.Parallel()
				fetcher, transport, _ := newIssueFetcher(t, validIssueTokens())
				transport.rejectToken = status
				_, err := fetchIssue(t, fetcher, fmt.Sprintf("inv-rejected-%d-%s", status, tc.issue),
					"https://api.github.com/repos/freeside-ai/private-repo/issues/"+tc.issue, issueScope)
				if tc.wantErr == "" && err != nil {
					t.Fatalf("public issue fetch failed on a rejected token: %v", err)
				}
				if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr) ||
					strings.Contains(err.Error(), issueTokenNeedle)) {
					t.Fatalf("error = %v, want %q", err, tc.wantErr)
				}
				seen := transport.requests()
				if len(seen) != 2 || seen[0].authorization == "" || seen[1].authorization != "" || seen[0].url != seen[1].url {
					t.Fatalf("transport saw %+v, want an authenticated request then one bare retry", redactSeen(seen))
				}
			})
		}
	}
}

// TestFetcherMintFailureLeavesPublicIssuesReadable is the fallback's
// purpose: an installation whose grant can't mint the read token still
// researches a public issue.
func TestFetcherMintFailureLeavesPublicIssuesReadable(t *testing.T) {
	t.Parallel()
	fetcher, transport, _ := newIssueFetcher(t, &fakeIssueTokens{err: publish.ErrGrantMismatch})
	if _, err := fetchIssue(t, fetcher, "inv-public", "https://api.github.com/repositories/4242/issues/100", issueScope); err != nil {
		t.Fatalf("public issue fetch failed on a mint failure: %v", err)
	}
	if seen := transport.requests(); len(seen) != 1 || seen[0].authorization != "" {
		t.Fatalf("transport saw %+v", redactSeen(seen))
	}
}

// TestFetcherKeepsTheTokenOutOfStoredResearch checks the stored envelope and
// every returned error for the minted value.
func TestFetcherKeepsTheTokenOutOfStoredResearch(t *testing.T) {
	t.Parallel()
	fetcher, _, blobs := newIssueFetcher(t, validIssueTokens())
	got, err := fetchIssue(t, fetcher, "inv-leak-ok", "https://api.github.com/repos/freeside-ai/private-repo/issues/7", issueScope)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(storedEnvelope(t, blobs, got)), issueTokenNeedle) {
		t.Fatal("stored envelope carries the token")
	}
	for index, rawURL := range []string{
		"https://api.github.com/repositories/4242/issues/404",
		"https://api.github.com/repos/freeside-ai/private-repo/issues/404",
		// Redirects to issue 9, which the stripped credential can't read.
		"https://api.github.com/repositories/4242/issues/8",
	} {
		_, err := fetchIssue(t, fetcher, "inv-leak-"+string(rune('a'+index)), rawURL, issueScope)
		if err == nil {
			t.Fatalf("%s: fetch succeeded", rawURL)
		}
		if strings.Contains(err.Error(), issueTokenNeedle) {
			t.Fatalf("%s: error carries the token", rawURL)
		}
	}
}

// redactSeen keeps a failing assertion's output free of the needle.
func redactSeen(seen []seenRequest) []seenRequest {
	out := make([]seenRequest, len(seen))
	for index, request := range seen {
		out[index] = seenRequest{url: request.url, authorization: strings.ReplaceAll(request.authorization, issueTokenNeedle, "<token>")}
	}
	return out
}
