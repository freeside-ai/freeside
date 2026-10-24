package forge

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// Config fixes what one Forge serves: the one repository, the one work-unit
// issue publication reads, and the bearer token every request must carry.
type Config struct {
	// Repo is the owner/name repository. Head listings strip "owner:".
	Repo  string
	Token string
	Issue Issue
	// Report receives every request the double does not expect: a wrong
	// token, an undecodable body, an unknown route. Tests pass t.Errorf.
	Report func(format string, args ...any)
}

// Issue is the work-unit issue GET /repos/{repo}/issues/{number} returns.
type Issue struct {
	Number int
	Title  string
	Body   string
}

// TokenSource mints the installation token the Forge accepts, valid for an
// hour from Now.
type TokenSource struct {
	Secret string
	Repo   string
	Now    func() time.Time
}

func (s TokenSource) Token(context.Context, string) (publish.InstallationToken, error) {
	return publish.InstallationToken{
		Token: publish.Secret(s.Secret), ExpiresAt: s.Now().Add(time.Hour), Repo: s.Repo,
	}, nil
}

// Auditor returns one fixed workflow audit for every repository and commit.
type Auditor struct {
	Result domain.WorkflowAudit
}

func (a Auditor) Audit(context.Context, string, string) (domain.WorkflowAudit, error) {
	return a.Result, nil
}

// PR is one pull request the Forge holds.
type PR struct {
	Number  int
	State   string
	Title   string
	Body    string
	HeadRef string
	HeadSHA string
	BaseRef string
	// Draft is emitted in the PR response; GitHub always sends the field and
	// the forge fails closed on its absence. It stays false unless the
	// publisher holds a human-gate closure (plan §5.15), which opens the PR as a
	// draft and toggles it through the GraphQL mutations below.
	Draft bool
}

// Forge serves the publisher's REST and GraphQL calls from memory. Every
// exported field is guarded by Mu.
type Forge struct {
	config Config

	Mu                   sync.Mutex
	Refs                 map[string]string
	PRs                  []PR
	nextPR               int
	WriteCounts          map[string]int
	failPRCreateResponse bool
	RequestHook          func(method, path string) bool
	Visibility           string
	FailRepoRead         bool
	FailIssueRead        bool
	IssueReads           int
	// IssueTitle, when set, replaces Config.Issue.Title in issue reads.
	IssueTitle *string
	// lagListings serves the next lagListings head-branch pull listings as
	// GitHub's view lagging a successor push (#1544): an empty list when
	// lagHeadSHA is empty, otherwise every listed PR at lagHeadSHA.
	lagListings int
	lagHeadSHA  string
}

// New returns an empty public repository whose first pull request is #101.
func New(config Config) *Forge {
	return &Forge{
		config: config, Refs: map[string]string{}, nextPR: 101, WriteCounts: map[string]int{},
		Visibility: "public",
	}
}

// Repo is the owner/name repository the Forge serves.
func (f *Forge) Repo() string { return f.config.Repo }

func (f *Forge) report(format string, args ...any) {
	if f.config.Report != nil {
		f.config.Report(format, args...)
	}
}

// decodeForgeRequest decodes one request body. It allows unknown fields
// because the double models only the fields it answers on, not every field
// the publisher sends.
func decodeForgeRequest(r *http.Request, dst any) error {
	return strictjson.DecodeReaderAllowingUnknownFields(r.Body, dst, strictjson.TolerateInvalidUTF8, 1<<20)
}

func (f *Forge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if f.RequestHook != nil && f.RequestHook(r.Method, r.URL.Path) {
		f.RequestHook = nil
	}
	if got := r.Header.Get("Authorization"); got != "Bearer "+f.config.Token {
		f.report("Authorization = %q", got)
	}
	if r.Method != http.MethodGet {
		f.WriteCounts[r.Method+" "+r.URL.Path]++
	}
	root := "/repos/" + f.config.Repo
	switch {
	case r.Method == http.MethodGet && r.URL.Path == root:
		// GET /repos/{owner}/{repo}: recipe v2 reads the target repository's
		// visibility here to derive and re-check the artifact's sensitivity class.
		if f.FailRepoRead {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": 424242, "visibility": f.Visibility, "private": f.Visibility != "public",
		})
	case r.Method == http.MethodGet && r.URL.Path == root+"/issues/"+strconv.Itoa(f.config.Issue.Number):
		f.IssueReads++
		if f.FailIssueRead {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": f.config.Issue.Number, "state": "open", "title": f.publicationIssueTitle(),
			"body": f.config.Issue.Body,
		})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, root+"/git/ref/heads/"):
		branch := strings.TrimPrefix(r.URL.Path, root+"/git/ref/heads/")
		sha, ok := f.Refs[branch]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref": "refs/heads/" + branch, "object": map[string]string{"sha": sha},
		})
	case r.Method == http.MethodPost && r.URL.Path == root+"/git/refs":
		var request struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		}
		if err := decodeForgeRequest(r, &request); err != nil {
			f.report("decode ref request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		branch := strings.TrimPrefix(request.Ref, "refs/heads/")
		if _, exists := f.Refs[branch]; exists {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		f.Refs[branch] = request.SHA
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref": request.Ref, "object": map[string]string{"sha": request.SHA},
		})
	case r.Method == http.MethodGet && r.URL.Path == root+"/pulls":
		owner, _, _ := strings.Cut(f.config.Repo, "/")
		head := strings.TrimPrefix(r.URL.Query().Get("head"), owner+":")
		out := make([]map[string]any, 0, len(f.PRs))
		lagging := f.lagListings > 0 && head != ""
		if lagging {
			f.lagListings--
		}
		for _, pr := range f.PRs {
			if lagging && f.lagHeadSHA == "" {
				break
			}
			if lagging {
				pr.HeadSHA = f.lagHeadSHA
			}
			if head == "" || head == pr.HeadRef {
				out = append(out, f.prJSON(pr))
			}
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodPost && r.URL.Path == root+"/pulls":
		var request struct {
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Draft bool   `json:"draft"`
		}
		if err := decodeForgeRequest(r, &request); err != nil {
			f.report("decode PR request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		pr := PR{
			Number: f.nextPR, Title: request.Title, Body: request.Body,
			HeadRef: request.Head, HeadSHA: f.Refs[request.Head], BaseRef: request.Base,
			Draft: request.Draft,
		}
		f.nextPR++
		f.PRs = append(f.PRs, pr)
		if f.failPRCreateResponse {
			f.failPRCreateResponse = false
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(f.prJSON(pr))
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, root+"/pulls/"):
		number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, root+"/pulls/"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for _, pr := range f.PRs {
			if pr.Number == number {
				_ = json.NewEncoder(w).Encode(f.prJSON(pr))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, root+"/pulls/"):
		number, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, root+"/pulls/"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := decodeForgeRequest(r, &request); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for i := range f.PRs {
			if f.PRs[i].Number == number {
				f.PRs[i].Title, f.PRs[i].Body = request.Title, request.Body
				_ = json.NewEncoder(w).Encode(f.prJSON(f.PRs[i]))
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && r.URL.Path == "/graphql":
		f.handleDraftMutation(w, r)
	default:
		body, _ := io.ReadAll(r.Body)
		f.report("unexpected forge request %s %s: %s", r.Method, r.URL.Path, body)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *Forge) publicationIssueTitle() string {
	if f.IssueTitle != nil {
		return *f.IssueTitle
	}
	return f.config.Issue.Title
}

// handleDraftMutation serves the publisher's GraphQL
// markPullRequestReadyForReview / convertPullRequestToDraft call against the PR
// its node id names, answering in the shape the publisher verifies.
func (f *Forge) handleDraftMutation(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query     string `json:"query"`
		Variables struct {
			ID string `json:"id"`
		} `json:"variables"`
	}
	if err := decodeForgeRequest(r, &request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	field, draft := "markPullRequestReadyForReview", false
	if strings.Contains(request.Query, "convertPullRequestToDraft") {
		field, draft = "convertPullRequestToDraft", true
	}
	for i := range f.PRs {
		if prNodeID(f.PRs[i].Number) != request.Variables.ID {
			continue
		}
		f.PRs[i].Draft = draft
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{field: map[string]any{
			"pullRequest": map[string]any{
				"number": f.PRs[i].Number, "isDraft": draft,
				"repository": map[string]string{"nameWithOwner": f.config.Repo},
			},
		}}})
		return
	}
	f.report("draft mutation names unknown node %q", request.Variables.ID)
	w.WriteHeader(http.StatusNotFound)
}

func prNodeID(number int) string {
	return "PR_node_" + strconv.Itoa(number)
}

// LagPRListings makes the next n head-branch pull listings lag the branch:
// empty when head is "", otherwise showing every PR at head.
func (f *Forge) LagPRListings(n int, head string) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.lagListings, f.lagHeadSHA = n, head
}

func (f *Forge) InterceptRequest(hook func(method, path string) bool) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.RequestHook = hook
}

func (f *Forge) prJSON(pr PR) map[string]any {
	state := pr.State
	if state == "" {
		state = "open"
	}
	return map[string]any{
		"number": pr.Number, "state": state, "title": pr.Title, "body": pr.Body,
		"draft": pr.Draft, "node_id": prNodeID(pr.Number),
		"head": map[string]any{
			"ref": pr.HeadRef, "sha": pr.HeadSHA,
			"repo": map[string]string{"full_name": f.config.Repo},
		},
		"base": map[string]any{
			"ref": pr.BaseRef, "repo": map[string]string{"full_name": f.config.Repo},
		},
	}
}

// SetRef records branch at sha unless the branch already exists, and
// reports whether an existing branch names a different commit.
func (f *Forge) SetRef(branch, sha string) bool {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if current, ok := f.Refs[branch]; ok {
		return current != sha
	}
	f.Refs[branch] = sha
	return false
}

func (f *Forge) ClearRefs() {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	clear(f.Refs)
}

func (f *Forge) Counts() (refs, prs int) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return len(f.Refs), len(f.PRs)
}

func (f *Forge) WriteCountSnapshot() map[string]int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return maps.Clone(f.WriteCounts)
}

func (f *Forge) PullRequests() []PR {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return append([]PR(nil), f.PRs...)
}

// FailAfterNextPRCreate makes the next pull request creation take effect and
// then answer 500, the way a lost response looks to the publisher.
func (f *Forge) FailAfterNextPRCreate() {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.failPRCreateResponse = true
}
