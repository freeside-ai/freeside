package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const plannedCommentsURL = "https://api.github.com/repositories/1/issues/7/comments?per_page=100"

func planComment(id int64, association, body string) map[string]any {
	return map[string]any{
		"id": id, "author_association": association, "body": body,
		"user": map[string]any{"login": "someone"}, "html_url": "https://github.com/owner/repo/issues/7",
	}
}

func commentsPage(t *testing.T, comments ...map[string]any) string {
	t.Helper()
	if comments == nil {
		comments = []map[string]any{}
	}
	body, err := json.Marshal(comments)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestCurrentPlanCommentID(t *testing.T) {
	t.Parallel()
	plan := implementationPlanMarker + "\nPlan for #7."
	issue := domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 7}
	ok := specify.ResearchSource{FinalURL: plannedCommentsURL, Status: http.StatusOK}
	withURL := func(finalURL string) specify.ResearchSource {
		source := ok
		source.FinalURL = finalURL
		return source
	}
	for _, tc := range []struct {
		name   string
		source specify.ResearchSource
		body   string
		want   int64
	}{
		{"one plan among other comments", ok, commentsPage(t,
			planComment(10, "CONTRIBUTOR", "A question."),
			planComment(11, "MEMBER", plan),
			planComment(12, "OWNER", "<!-- freeside-work-claim:v1 -->\nClaim: feat/x"),
		), 11},
		{"CRLF line ending", ok, commentsPage(t, planComment(11, "OWNER", implementationPlanMarker+"\r\nPlan.")), 11},
		{"marker alone", ok, commentsPage(t, planComment(11, "COLLABORATOR", implementationPlanMarker)), 11},
		{"no comments", ok, commentsPage(t), 0},
		{"no marker", ok, commentsPage(t, planComment(11, "MEMBER", "Plan for #7.")), 0},
		{"marker not on the first line", ok, commentsPage(t, planComment(11, "MEMBER", "Plan.\n"+implementationPlanMarker)), 0},
		{"marker with leading space", ok, commentsPage(t, planComment(11, "MEMBER", " "+plan)), 0},
		{"author outside the three", ok, commentsPage(t, planComment(11, "CONTRIBUTOR", plan)), 0},
		{"two marked comments", ok, commentsPage(t, planComment(11, "MEMBER", plan), planComment(12, "OWNER", plan)), 0},
		{"another issue's comments", withURL("https://api.github.com/repositories/1/issues/8/comments?per_page=100"), commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"another repository's comments", withURL("https://api.github.com/repositories/2/issues/7/comments?per_page=100"), commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"a later page", withURL(plannedCommentsURL + "&page=2"), commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"a smaller page", withURL("https://api.github.com/repositories/1/issues/7/comments?per_page=1"), commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"GitHub's default page", withURL("https://api.github.com/repositories/1/issues/7/comments"), commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"a filtered page", withURL(plannedCommentsURL + "&since=2026-01-01T00:00:00Z"), commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"the owner/name spelling", withURL("https://api.github.com/repos/owner/repo/issues/7/comments?per_page=100"), commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"the issue itself", withURL("https://api.github.com/repositories/1/issues/7"), commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"a failed fetch", specify.ResearchSource{FinalURL: plannedCommentsURL, Status: http.StatusOK, Failure: "fetch failed"}, commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"a non-200 fetch", specify.ResearchSource{FinalURL: plannedCommentsURL, Status: http.StatusNotFound}, commentsPage(t, planComment(11, "MEMBER", plan)), 0},
		{"an undecodable body", ok, `{"message":"` + plan + `"}`, 0},
		{"a truncated body", ok, strings.TrimSuffix(commentsPage(t, planComment(11, "MEMBER", plan)), "]"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, found := currentPlanCommentID(tc.source, tc.body, issue)
			if got != tc.want || found != (tc.want != 0) {
				t.Fatalf("currentPlanCommentID = %d, %t; want %d", got, found, tc.want)
			}
		})
	}
}

// plannedIssueTransport plays GitHub for repository 1: an authenticated read
// of issue 7 returns its body, and of its comments returns comments. Any other
// URL is 404.
type plannedIssueTransport struct {
	comments string
}

func (tr *plannedIssueTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	status, body := http.StatusNotFound, `{"message":"Not Found"}`
	if request.Header.Get("Authorization") == "Bearer "+researchTokenNeedle {
		switch request.URL.Path {
		case "/repositories/1/issues/7":
			status, body = http.StatusOK, `{"number":7,"title":"Planned work","body":"The work contract."}`
		case "/repositories/1/issues/7/comments":
			status, body = http.StatusOK, tr.comments
		}
	}
	return &http.Response{
		StatusCode: status, Request: request,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}, nil
}

// plannedIssueRun submits a task bound to issue 7, as `freesided submit` does
// for a bare issue URL, whose first specifier invocation asks for the issue
// and its comments and whose second returns second. It runs two passes and
// returns the second invocation's captured prior-artifact envelopes.
func plannedIssueRun(t *testing.T, comments string, second specify.Output) (specificationFixture, [][]byte) {
	t.Helper()
	f := newSpecificationFixtureAllowing(t, false, 3, "https://api.github.com")
	f.researchTransport = &plannedIssueTransport{comments: comments}
	f.issueTokens = &fakeResearchTokens{repositoryID: 1}
	driver, capturing := newCapturingResearchDriver(t, f)
	firstID := specificationInvocationID("specification-run", 1)
	secondID := specificationInvocationID("specification-run", 2)
	if err := specifyfake.Script(driver, firstID, 0, 0, specify.Output{FetchRequests: []specify.FetchRequest{
		{URL: "https://api.github.com/repos/owner/repo/issues/7", Purpose: "read the source issue"},
		{URL: "https://api.github.com/repos/owner/repo/issues/7/comments?per_page=100", Purpose: "read the plan"},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := specifyfake.Script(driver, secondID, 0, 0, second); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	run, err := f.run("specification-run")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.SetTaskSubmittedIssue(t.Context(), run.TaskID,
			domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 7})
	}); err != nil {
		t.Fatal(err)
	}
	engine := f.newEngine(t, capturing)
	for pass := 1; pass <= 2; pass++ {
		if _, err := reconcileSettled(t, engine); err != nil {
			t.Fatalf("reconcile pass %d: %v", pass, err)
		}
	}
	assertNoExecutionFailure(t, f, firstID)
	prompt, ok := capturing.prompt(secondID)
	if !ok || len(prompt.bodies) != 2 {
		t.Fatalf("second iteration prompt captured = %t with %d prior artifacts, want 2", ok, len(prompt.bodies))
	}
	return f, prompt.bodies
}

func decodePriorEnvelope(t *testing.T, body []byte) specificationPriorArtifactEnvelope {
	t.Helper()
	var envelope specificationPriorArtifactEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

var plannedSpecification = specify.Output{Specification: &specify.Specification{
	Summary: "Specified from the plan.", Body: "# Specification\n\nFollow the plan.",
	Addressals: []specify.Addressal{},
}}

// A bound issue whose comments hold its one current plan reaches its
// specification in two invocations, research then specification, and the
// comments block tells the specifier which comment is the plan.
func TestSpecificationOfAPlannedIssueCarriesThePlanComment(t *testing.T) {
	t.Parallel()
	comments := commentsPage(t,
		planComment(10, "CONTRIBUTOR", "A question."),
		planComment(11, "MEMBER", implementationPlanMarker+"\nPlan for #7."),
	)
	f, bodies := plannedIssueRun(t, comments, plannedSpecification)
	issueBlock, commentsBlock := decodePriorEnvelope(t, bodies[0]), decodePriorEnvelope(t, bodies[1])
	if issueBlock.Role != "research" || issueBlock.PlanCommentID != 0 ||
		bytes.Contains(bodies[0], []byte("plan_comment_id")) {
		t.Fatalf("issue block = %s, want no plan_comment_id", bodies[0])
	}
	if commentsBlock.Role != "research" || commentsBlock.PlanCommentID != 11 || commentsBlock.Body != comments ||
		commentsBlock.Source == nil || commentsBlock.Source.FinalURL != plannedCommentsURL {
		t.Fatalf("comments block = %s, want plan_comment_id 11", bodies[1])
	}
	if items := f.questionItemsForRun(t, "specification-run"); len(items) != 0 {
		t.Fatalf("agent_question items = %+v, want none", items)
	}
	if _, err := f.run("implementation-run"); err != nil {
		t.Fatalf("implementation run after the planned specification: %v", err)
	}
}

// The plan comment never suppresses a question: a specifier that finds the
// plan names something the base lacks returns decisions, and the owner is
// asked as before.
func TestSpecificationOfAPlannedIssueStillAsksWhenTheSpecifierDoes(t *testing.T) {
	t.Parallel()
	comments := commentsPage(t, planComment(11, "OWNER", implementationPlanMarker+"\nRename missing.go."))
	f, bodies := plannedIssueRun(t, comments, specify.Output{Decisions: decisionsFixture()})
	if decodePriorEnvelope(t, bodies[1]).PlanCommentID != 11 {
		t.Fatalf("comments block = %s, want plan_comment_id 11", bodies[1])
	}
	items := f.questionItemsForRun(t, "specification-run")
	if len(items) != 1 || items[0].ID != domain.ItemID("question-"+string(specificationInvocationID("specification-run", 2))) {
		t.Fatalf("agent_question items = %+v, want the second invocation's question", items)
	}
	if _, err := f.run("implementation-run"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("implementation run after a question = %v, want ErrNotFound", err)
	}
}

// An issue with no current plan renders every block without the field, so
// the envelopes are the bytes they were before plan_comment_id existed.
func TestSpecificationOfAnUnplannedIssueRendersAsBefore(t *testing.T) {
	t.Parallel()
	comments := commentsPage(t, planComment(11, "CONTRIBUTOR", implementationPlanMarker+"\nPlan for #7."))
	_, bodies := plannedIssueRun(t, comments, plannedSpecification)
	for index, body := range bodies {
		if bytes.Contains(body, []byte("plan_comment_id")) {
			t.Fatalf("block %d = %s, want no plan_comment_id", index, body)
		}
	}
}
