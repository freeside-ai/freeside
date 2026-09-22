package publish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The draft hold (issue #1419 Part C) opens a closable-source publication as a
// draft and converges its draft state on every pass: a draft cannot be merged
// on GitHub, and no internal signal can stop a direct merge. These tests drive
// the forge's draft flag and the mark-ready/convert-to-draft mutations, and
// convergePR's decision to toggle, against a focused fake GitHub server.

const draftTestHeadSHA = "1111111111111111111111111111111111111111"

type draftTestTokenSource struct{}

func (draftTestTokenSource) Token(context.Context, string) (InstallationToken, error) {
	return InstallationToken{Token: Secret("fixture-token")}, nil
}

type draftFakePR struct {
	number  int
	state   string
	title   string
	body    string
	headRef string
	headSHA string
	baseRef string
	draft   bool
	nodeID  string
}

type draftFakeGitHub struct {
	t        *testing.T
	prs      map[int]*draftFakePR
	refs     map[string]string // branch -> head sha a created PR resolves to
	nextPR   int
	requests []string

	// Returned-object misbehavior knobs for the draft mutation, so the
	// fail-closed verification in setPRDraft is exercised.
	graphErrors     bool
	graphWrongNum   bool
	graphWrongDraft bool
	graphOmitDraft  bool
	// graphForeignRepo makes the mutation response name another repository,
	// same pull number; graphOmitRepo drops the repository field entirely. Both
	// exercise the returned-object repository-identity guard in setPRDraft.
	graphForeignRepo bool
	graphOmitRepo    bool
	// createEchoWrongDraft makes a created PR report the negation of the
	// requested draft, so the create-path draft-echo check is exercised.
	createEchoWrongDraft bool
	// omitDraftField drops the REST "draft" field from every PR response, so
	// the pointer-fail-closed guard at each decode site is exercised: an absent
	// bit must never read as "not a draft" and report a hold released.
	omitDraftField bool
}

func newDraftFake(t *testing.T) (*forge, *draftFakeGitHub, repoRef) {
	t.Helper()
	g := &draftFakeGitHub{
		t:      t,
		prs:    map[int]*draftFakePR{},
		refs:   map[string]string{},
		nextPR: 101,
	}
	srv := httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(srv.Close)
	f := newForge(draftTestTokenSource{}, srv.Client(), srv.URL)
	return f, g, repoRef{owner: "owner", name: "name"}
}

func (g *draftFakeGitHub) prJSON(pr *draftFakePR) map[string]any {
	out := map[string]any{
		"number":  pr.number,
		"state":   pr.state,
		"title":   pr.title,
		"body":    pr.body,
		"draft":   pr.draft,
		"node_id": pr.nodeID,
		"head": map[string]any{
			"ref":  pr.headRef,
			"sha":  pr.headSHA,
			"repo": map[string]any{"full_name": "owner/name"},
		},
		"base": map[string]any{
			"ref":  pr.baseRef,
			"repo": map[string]any{"id": int64(1), "full_name": "owner/name"},
		},
	}
	if g.omitDraftField {
		delete(out, "draft")
	}
	return out
}

func (g *draftFakeGitHub) sentGraphQL() bool {
	return slices.Contains(g.requests, http.MethodPost+" /graphql")
}

func (g *draftFakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	g.requests = append(g.requests, r.Method+" "+r.URL.Path)
	if got := r.Header.Get("Authorization"); got != "Bearer fixture-token" {
		g.t.Errorf("Authorization = %q", got)
	}
	const repoPath = "/repos/owner/name"
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/graphql":
		g.handleGraphQL(w, r)

	case r.Method == http.MethodGet && path == repoPath+"/pulls":
		head := r.URL.Query().Get("head")
		out := []map[string]any{}
		for _, pr := range g.prs {
			if head != "" && head != "owner:"+pr.headRef {
				continue
			}
			out = append(out, g.prJSON(pr))
		}
		_ = json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPost && path == repoPath+"/pulls":
		var body struct {
			Title string `json:"title"`
			Head  string `json:"head"`
			Base  string `json:"base"`
			Body  string `json:"body"`
			Draft bool   `json:"draft"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pr := &draftFakePR{
			number:  g.nextPR,
			state:   "open",
			title:   body.Title,
			body:    body.Body,
			headRef: body.Head,
			headSHA: g.refs[body.Head],
			baseRef: body.Base,
			draft:   body.Draft != g.createEchoWrongDraft,
			nodeID:  "PR_node_" + strconv.Itoa(g.nextPR),
		}
		g.prs[pr.number] = pr
		g.nextPR++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(g.prJSON(pr))

	case r.Method == http.MethodGet && strings.HasPrefix(path, repoPath+"/pulls/"):
		pr := g.lookup(w, path, repoPath)
		if pr == nil {
			return
		}
		_ = json.NewEncoder(w).Encode(g.prJSON(pr))

	case r.Method == http.MethodPatch && strings.HasPrefix(path, repoPath+"/pulls/"):
		pr := g.lookup(w, path, repoPath)
		if pr == nil {
			return
		}
		var body struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		pr.title, pr.body = body.Title, body.Body
		_ = json.NewEncoder(w).Encode(g.prJSON(pr))

	default:
		g.t.Errorf("unexpected request: %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (g *draftFakeGitHub) lookup(w http.ResponseWriter, path, repoPath string) *draftFakePR {
	n, err := strconv.Atoi(strings.TrimPrefix(path, repoPath+"/pulls/"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	pr, ok := g.prs[n]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return nil
	}
	return pr
}

func (g *draftFakeGitHub) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	var request struct {
		Query     string `json:"query"`
		Variables struct {
			ID string `json:"id"`
		} `json:"variables"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		g.t.Errorf("decode graphql request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if g.graphErrors {
		_, _ = io.WriteString(w, `{"errors":[{"message":"boom"}],"data":null}`)
		return
	}
	toDraft := strings.Contains(request.Query, "convertPullRequestToDraft")
	toReady := strings.Contains(request.Query, "markPullRequestReadyForReview")
	if toDraft == toReady {
		g.t.Errorf("graphql query selects neither or both mutations: %s", request.Query)
		return
	}
	// The repository-identity guard depends on the mutation selecting the
	// repository; pin the query shape so the selection can't be dropped without
	// this test failing, mirroring the issue-query check in publisher_test.go.
	if !strings.Contains(request.Query, "repository { nameWithOwner }") {
		g.t.Errorf("graphql query does not select repository { nameWithOwner }: %s", request.Query)
	}
	var pr *draftFakePR
	for _, p := range g.prs {
		if p.nodeID == request.Variables.ID {
			pr = p
			break
		}
	}
	if pr == nil {
		// No pullRequest in the response: the caller must fail closed.
		_, _ = io.WriteString(w, `{"data":{}}`)
		return
	}
	pr.draft = toDraft
	num, isDraft := pr.number, pr.draft
	if g.graphWrongNum {
		num += 1000
	}
	if g.graphWrongDraft {
		isDraft = !isDraft
	}
	field := "markPullRequestReadyForReview"
	if toDraft {
		field = "convertPullRequestToDraft"
	}
	// The response names the target repository unless a knob changes it. The
	// omit-draft case keeps this field so it still exercises the draft guard,
	// not the repository guard.
	repoField := `,"repository":{"nameWithOwner":"owner/name"}`
	if g.graphForeignRepo {
		repoField = `,"repository":{"nameWithOwner":"other/name"}`
	}
	if g.graphOmitRepo {
		repoField = ""
	}
	if g.graphOmitDraft {
		_, _ = fmt.Fprintf(w, `{"data":{%q:{"pullRequest":{"number":%d%s}}}}`, field, num, repoField)
		return
	}
	_, _ = fmt.Fprintf(w, `{"data":{%q:{"pullRequest":{"number":%d,"isDraft":%t%s}}}}`, field, num, isDraft, repoField)
}

// TestDesiredDraftStateFromClosure pins the Part D draft intent: only a gate-on
// (managed) closable source owns the pull request's draft state, holding it
// draft while the closure proposal holds and marking it ready once it resolves.
// An unmanaged resolution (a default-policy source, or no closable source at
// all) leaves the draft state untouched (nil), so a default-policy PR opens
// mergeable (plan revision 68, issue #1419).
func TestDesiredDraftStateFromClosure(t *testing.T) {
	draft := closureResolution{managed: true, outcome: domain.ClosureOutcome{Hold: domain.ClosureHoldDraft}}
	if got := desiredDraftState(draft); got == nil || !*got {
		t.Errorf("managed draft hold: desiredDraftState = %v, want *true", got)
	}
	ready := closureResolution{managed: true, outcome: domain.ClosureOutcome{Hold: domain.ClosureHoldNone}}
	if got := desiredDraftState(ready); got == nil || *got {
		t.Errorf("managed resolved hold: desiredDraftState = %v, want *false", got)
	}
	for _, unmanaged := range []closureResolution{
		{},
		{managed: false, outcome: domain.ClosureOutcome{Hold: domain.ClosureHoldDraft}},
	} {
		if got := desiredDraftState(unmanaged); got != nil {
			t.Errorf("unmanaged resolution: desiredDraftState = %v, want nil", *got)
		}
	}
}

func TestForgeCreatePRDraftFlag(t *testing.T) {
	for _, draft := range []bool{true, false} {
		f, g, repo := newDraftFake(t)
		g.refs["feat/x"] = draftTestHeadSHA
		pr, err := f.createPR(context.Background(), repo, "feat/x", "main", "Title", "Body", draft)
		if err != nil {
			t.Fatalf("draft=%t: createPR: %v", draft, err)
		}
		if pr.Draft != draft {
			t.Errorf("draft=%t: created PR Draft = %t", draft, pr.Draft)
		}
		if got := g.prs[pr.Number].draft; got != draft {
			t.Errorf("draft=%t: server stored draft = %t", draft, got)
		}
		if pr.NodeID == "" {
			t.Errorf("draft=%t: created PR carries no node id", draft)
		}
	}
}

// TestForgePRDraftFieldFailsClosedWhenAbsent locks the REST decode boundary:
// when GitHub's create, list, or get response omits the draft bit, decoding it
// into a plain bool would silently read "not a draft" and let a wantDraft-false
// convergence report the hold released without evidence. The pointer guard at
// each site must fail closed instead.
func TestForgePRDraftFieldFailsClosedWhenAbsent(t *testing.T) {
	ctx := context.Background()

	t.Run("create", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		g.refs["feat/x"] = draftTestHeadSHA
		g.omitDraftField = true
		if _, err := f.createPR(ctx, repo, "feat/x", "main", "Title", "Body", false); err == nil {
			t.Fatal("createPR accepted a response with no draft state")
		}
	})

	t.Run("list", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		g.omitDraftField = true
		g.prs[301] = &draftFakePR{
			number: 301, state: "open", title: "t", body: "b",
			headRef: "feat/x", headSHA: draftTestHeadSHA, baseRef: "main",
			nodeID: "PR_node_301",
		}
		if _, err := f.listPRsByHead(ctx, repo, "feat/x"); err == nil {
			t.Fatal("listPRsByHead accepted a row with no draft state")
		}
	})

	t.Run("get", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		g.omitDraftField = true
		g.prs[302] = &draftFakePR{
			number: 302, state: "open", title: "t", body: "b",
			headRef: "feat/x", headSHA: draftTestHeadSHA, baseRef: "main",
			nodeID: "PR_node_302",
		}
		if _, err := f.getPR(ctx, repo, 302, ""); err == nil {
			t.Fatal("getPR accepted a response with no draft state")
		}
	})
}

func TestForgeSetPRDraftTogglesState(t *testing.T) {
	ctx := context.Background()
	f, g, repo := newDraftFake(t)
	g.refs["feat/x"] = draftTestHeadSHA
	pr, err := f.createPR(ctx, repo, "feat/x", "main", "Title", "Body", true)
	if err != nil {
		t.Fatalf("createPR: %v", err)
	}

	if err := f.setPRDraft(ctx, repo, pr.Number, pr.NodeID, false); err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	if g.prs[pr.Number].draft {
		t.Error("PR still a draft after mark ready")
	}

	if err := f.setPRDraft(ctx, repo, pr.Number, pr.NodeID, true); err != nil {
		t.Fatalf("convert to draft: %v", err)
	}
	if !g.prs[pr.Number].draft {
		t.Error("PR not a draft after convert to draft")
	}

	if err := f.setPRDraft(ctx, repo, pr.Number, "", false); err == nil {
		t.Error("setPRDraft accepted an empty node id")
	}
}

func TestForgeSetPRDraftReturnedObjectFailsClosed(t *testing.T) {
	ctx := context.Background()
	cases := map[string]func(*draftFakeGitHub){
		"graphql errors":      func(g *draftFakeGitHub) { g.graphErrors = true },
		"wrong pull number":   func(g *draftFakeGitHub) { g.graphWrongNum = true },
		"wrong draft state":   func(g *draftFakeGitHub) { g.graphWrongDraft = true },
		"omitted draft state": func(g *draftFakeGitHub) { g.graphOmitDraft = true },
		"foreign repository":  func(g *draftFakeGitHub) { g.graphForeignRepo = true },
		"omitted repository":  func(g *draftFakeGitHub) { g.graphOmitRepo = true },
	}
	for name, misbehave := range cases {
		t.Run(name, func(t *testing.T) {
			f, g, repo := newDraftFake(t)
			g.refs["feat/x"] = draftTestHeadSHA
			pr, err := f.createPR(ctx, repo, "feat/x", "main", "Title", "Body", true)
			if err != nil {
				t.Fatalf("createPR: %v", err)
			}
			misbehave(g)
			if err := f.setPRDraft(ctx, repo, pr.Number, pr.NodeID, false); err == nil {
				t.Fatalf("setPRDraft accepted a misbehaving response (%s)", name)
			}
		})
	}

	t.Run("unknown node id", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		g.refs["feat/x"] = draftTestHeadSHA
		pr, err := f.createPR(ctx, repo, "feat/x", "main", "Title", "Body", true)
		if err != nil {
			t.Fatalf("createPR: %v", err)
		}
		if err := f.setPRDraft(ctx, repo, pr.Number, "PR_node_does_not_exist", false); err == nil {
			t.Fatal("setPRDraft accepted a response with no pull request")
		}
	})
}

// managedDraft is a non-nil draft intent: the Part D managed case, where the
// publisher converges a closable source's PR to the given draft state. A nil
// intent (the Part C default) means unmanaged and is passed as a literal nil.
func managedDraft(b bool) *bool { return &b }

func TestConvergePRDraftHold(t *testing.T) {
	ctx := context.Background()
	id, err := DeriveIdentity(IdentityInput{
		Repo:            "owner/name",
		BaseRef:         "main",
		SourceHeadSHA:   draftTestHeadSHA,
		ArtifactDigests: []domain.Digest{domain.Digest("sha256:" + strings.Repeat("a", 64))},
	})
	if err != nil {
		t.Fatalf("DeriveIdentity: %v", err)
	}
	branch := id.BranchName()
	c := Candidate{Repo: "owner/name", BaseRef: "main", HeadSHA: draftTestHeadSHA}
	title := "Publication title"
	body := "Prose paragraph.\n\n" + id.Marker()

	seedPR := func(g *draftFakeGitHub, number int, draft bool) {
		g.prs[number] = &draftFakePR{
			number: number, state: "open", title: title, body: body,
			headRef: branch, headSHA: draftTestHeadSHA, baseRef: "main",
			draft: draft, nodeID: "PR_node_" + strconv.Itoa(number),
		}
	}

	t.Run("open as draft", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		g.refs[branch] = draftTestHeadSHA
		p := &Publisher{forge: f}
		num, created, err := p.convergePR(ctx, repo, id, c, title, body, managedDraft(true), true, 0, nil)
		if err != nil {
			t.Fatalf("convergePR: %v", err)
		}
		if !created {
			t.Error("expected the PR to be created")
		}
		if !g.prs[num].draft {
			t.Error("created PR is not a draft")
		}
	})

	t.Run("mark ready when hold released", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		seedPR(g, 201, true)
		p := &Publisher{forge: f}
		num, created, err := p.convergePR(ctx, repo, id, c, title, body, managedDraft(false), false, 0, nil)
		if err != nil {
			t.Fatalf("convergePR: %v", err)
		}
		if created || num != 201 {
			t.Errorf("convergePR = (%d, %t), want (201, false)", num, created)
		}
		if g.prs[201].draft {
			t.Error("PR still a draft after the hold released")
		}
		if !g.sentGraphQL() {
			t.Error("expected a mark-ready mutation")
		}
	})

	t.Run("put back in draft after early ready", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		seedPR(g, 202, false) // someone marked it ready early
		p := &Publisher{forge: f}
		if _, _, err := p.convergePR(ctx, repo, id, c, title, body, managedDraft(true), false, 0, nil); err != nil {
			t.Fatalf("convergePR: %v", err)
		}
		if !g.prs[202].draft {
			t.Error("PR not put back into draft")
		}
		if !g.sentGraphQL() {
			t.Error("expected a convert-to-draft mutation")
		}
	})

	t.Run("no toggle when already in wanted state", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		seedPR(g, 203, false)
		p := &Publisher{forge: f}
		if _, _, err := p.convergePR(ctx, repo, id, c, title, body, managedDraft(false), false, 0, nil); err != nil {
			t.Fatalf("convergePR: %v", err)
		}
		if g.sentGraphQL() {
			t.Error("convergePR issued a draft mutation for a PR already in the wanted state")
		}
	})

	t.Run("content and draft both differ", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		// Seed a draft PR with stale content, then converge to fresh content
		// and a released hold: the content patch and the mark-ready toggle both
		// run, and the toggle uses the node id from the patch response.
		g.prs[204] = &draftFakePR{
			number: 204, state: "open", title: "Stale title",
			body:    "Stale prose.\n\n" + id.Marker(),
			headRef: branch, headSHA: draftTestHeadSHA, baseRef: "main",
			draft: true, nodeID: "PR_node_204",
		}
		p := &Publisher{forge: f}
		if _, _, err := p.convergePR(ctx, repo, id, c, title, body, managedDraft(false), false, 0, nil); err != nil {
			t.Fatalf("convergePR: %v", err)
		}
		if g.prs[204].title != title || g.prs[204].body != body {
			t.Error("content was not patched")
		}
		if g.prs[204].draft {
			t.Error("PR still a draft after the hold released")
		}
		if !slices.Contains(g.requests, http.MethodPatch+" /repos/owner/name/pulls/204") {
			t.Error("expected a content PATCH")
		}
		if !g.sentGraphQL() {
			t.Error("expected a mark-ready mutation")
		}
	})

	t.Run("created PR draft echo mismatch fails closed", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		g.refs[branch] = draftTestHeadSHA
		g.createEchoWrongDraft = true
		p := &Publisher{forge: f}
		_, _, err := p.convergePR(ctx, repo, id, c, title, body, managedDraft(true), true, 0, nil)
		if err == nil || !errors.Is(err, ErrPublicationConflict) {
			t.Fatalf("convergePR error = %v, want ErrPublicationConflict", err)
		}
	})

	// A managed toggle whose mutation response names another repository must
	// fail closed at the convergePR level, not report the PR converged: the
	// returned-object repository-identity guard in setPRDraft propagates up.
	t.Run("draft mutation on a foreign repository fails closed", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		seedPR(g, 207, true) // hold released below, so the mark-ready toggle runs
		g.graphForeignRepo = true
		p := &Publisher{forge: f}
		if _, _, err := p.convergePR(ctx, repo, id, c, title, body, managedDraft(false), false, 0, nil); err == nil {
			t.Fatal("convergePR reported convergence for a foreign-repository draft mutation")
		}
	})

	// The §5.15 "unaffected" case, the Part C production path: an unmanaged
	// intent (nil) never touches a PR's draft state, even one a human converted
	// to draft, and never issues a draft mutation.
	t.Run("unmanaged leaves a human draft untouched", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		seedPR(g, 205, true) // a human converted our PR to draft; content current
		p := &Publisher{forge: f}
		num, created, err := p.convergePR(ctx, repo, id, c, title, body, nil, false, 0, nil)
		if err != nil {
			t.Fatalf("convergePR: %v", err)
		}
		if created || num != 205 {
			t.Errorf("convergePR = (%d, %t), want (205, false)", num, created)
		}
		if !g.prs[205].draft {
			t.Error("unmanaged convergePR released a human's draft hold")
		}
		if g.sentGraphQL() {
			t.Error("unmanaged convergePR issued a draft mutation")
		}
	})

	t.Run("unmanaged patches content but leaves draft untouched", func(t *testing.T) {
		f, g, repo := newDraftFake(t)
		// Stale content on a human-drafted PR: content is repaired, but the
		// draft state is still not the publisher's to manage.
		g.prs[206] = &draftFakePR{
			number: 206, state: "open", title: "Stale title",
			body:    "Stale prose.\n\n" + id.Marker(),
			headRef: branch, headSHA: draftTestHeadSHA, baseRef: "main",
			draft: true, nodeID: "PR_node_206",
		}
		p := &Publisher{forge: f}
		if _, _, err := p.convergePR(ctx, repo, id, c, title, body, nil, false, 0, nil); err != nil {
			t.Fatalf("convergePR: %v", err)
		}
		if g.prs[206].title != title || g.prs[206].body != body {
			t.Error("content was not patched")
		}
		if !g.prs[206].draft {
			t.Error("unmanaged convergePR released a human's draft hold after a content patch")
		}
		if g.sentGraphQL() {
			t.Error("unmanaged convergePR issued a draft mutation")
		}
	})
}
