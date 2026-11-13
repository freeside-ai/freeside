package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type openIssueTestTokens struct{}

func (openIssueTestTokens) Token(context.Context, string) (publish.InstallationToken, error) {
	return publish.InstallationToken{Token: publish.Secret("issue-read-token")}, nil
}

// openIssueStub counts the listings the workflow asked for.
type openIssueStub struct {
	requests atomic.Int32
}

// stubOpenIssues points the workflow's lister at a server that answers the
// filing target's (owner/repo, 123) open-issue listing with status and, on
// 200, issues.
func stubOpenIssues(
	t *testing.T, f *findingAdjudicationFixture, status int, issues ...publish.OpenIssue,
) *openIssueStub {
	t.Helper()
	stub := &openIssueStub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repositories/123/issues" {
			t.Errorf("unexpected listing path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		stub.requests.Add(1)
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		repository := "http://" + r.Host + "/repos/owner/repo"
		elements := make([]map[string]any, 0, len(issues))
		for _, issue := range issues {
			elements = append(elements, map[string]any{
				"repository_url": repository, "url": fmt.Sprintf("%s/issues/%d", repository, issue.Number),
				"number": issue.Number, "title": issue.Title, "body": issue.BodyExcerpt,
			})
		}
		_ = json.NewEncoder(w).Encode(elements)
	}))
	t.Cleanup(srv.Close)
	f.workflow.openIssues = publish.NewOpenIssueLister(openIssueTestTokens{}, srv.Client(), srv.URL)
	return stub
}

var twoOpenIssues = []publish.OpenIssue{
	{Number: 41, Title: "Bound the sync client's retries", BodyExcerpt: "The client retries forever."},
	{Number: 7, Title: "Unrelated cleanup"},
}

func scriptDuplicateSearch(f *findingAdjudicationFixture, body string) {
	f.driver.Script(inference.DuplicateSearchSiteID,
		fake.Script{Response: inference.Response{Output: []byte(body), ComputeUnits: 1}})
}

func duplicateSearchRequests(f *findingAdjudicationFixture) []inference.Request {
	var requests []inference.Request
	for _, request := range f.driver.Requests() {
		if request.SiteID == inference.DuplicateSearchSiteID {
			requests = append(requests, request)
		}
	}
	return requests
}

func readDuplicateSearch(
	t *testing.T, f *findingAdjudicationFixture, artifact domain.FindingAdjudication,
) (domain.DuplicateSearch, error) {
	t.Helper()
	var search domain.DuplicateSearch
	err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		search, err = tx.GetDuplicateSearch(f.ctx, artifact.Digest, f.finding.ID)
		return err
	})
	return search, err
}

// A search that names an open issue records it and proposes no filing; the
// disposition still commits, and a replay neither lists nor asks again. With
// no trust profile accepting the repository's issue_comment workflows, no
// comment is proposed either, and the run's notice says why.
func TestDuplicateSearchMatchRecordsTheIssueAndProposesNoFiling(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	seedFollowUpFilingSubject(t, f, true)
	stub := stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchMatch)
	reconcileFollowUpFiling(t, f, productionReviewPassed)
	replayFollowUpFiling(t, f, readFollowUpFilingState(t, f, 1).artifact)

	state := readFollowUpFilingState(t, f, 1)
	if len(state.dispositions) != 1 || len(state.instances) != 0 || len(state.cards) != 0 || len(state.notices) != 1 {
		t.Fatalf("dispositions, instances, cards, notices = %d, %d, %d, %d, want 1, 0, 0, 1",
			len(state.dispositions), len(state.instances), len(state.cards), len(state.notices))
	}
	assertIssueCommentRefusedNotice(t, f, state)
	search, err := readDuplicateSearch(t, f, state.artifact)
	if err != nil {
		t.Fatal(err)
	}
	if number, ok := search.DuplicateIssue(); !ok || number != 41 ||
		search.Kind != domain.FollowUpSourceDeferredDisposition || len(search.Candidates) != 2 {
		t.Fatalf("search = %#v, want a deferred record naming issue 41", search)
	}
	requests := duplicateSearchRequests(f)
	if len(requests) != 1 || stub.requests.Load() != 1 {
		t.Fatalf("site calls, listings = %d, %d, want 1, 1", len(requests), stub.requests.Load())
	}
	// The site sees handles and text, never an issue number, and the finding
	// with the adjudicator's rationale.
	if candidates := requests[0].Fields["candidates"]; strings.Contains(candidates, "41") ||
		!strings.Contains(candidates, `"handle":"candidate-1"`) {
		t.Fatalf("candidates = %s", candidates)
	}
	if finding := requests[0].Fields["finding"]; !strings.Contains(finding, "review finding") ||
		!strings.Contains(finding, "deterministic fake route") {
		t.Fatalf("finding = %s", finding)
	}
}

// A search that answers "new" over listed candidates proposes the filing.
func TestDuplicateSearchNewProposesTheFiling(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	seedFollowUpFilingSubject(t, f, true)
	stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchNew)
	reconcileFollowUpFiling(t, f, productionReviewPassed)

	state := readFollowUpFilingState(t, f, 1)
	assertOneFilingCard(t, f, state, domain.FollowUpSourceDeferredDisposition)
	search, err := readDuplicateSearch(t, f, state.artifact)
	if err != nil || !search.IsNew() || len(search.Candidates) != 2 || search.Reason == "" {
		t.Fatalf("search = %#v, %v, want a judged new record", search, err)
	}
}

// An empty listing records "new" without asking the site.
func TestDuplicateSearchWithNoCandidatesMakesNoSiteCall(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	seedFollowUpFilingSubject(t, f, true)
	reconcileFollowUpFiling(t, f, productionReviewPassed)

	state := readFollowUpFilingState(t, f, 1)
	assertOneFilingCard(t, f, state, domain.FollowUpSourceDeferredDisposition)
	search, err := readDuplicateSearch(t, f, state.artifact)
	if err != nil || !search.IsNew() || len(search.Candidates) != 0 || search.Reason != "" {
		t.Fatalf("search = %#v, %v, want an empty new record", search, err)
	}
	if requests := duplicateSearchRequests(f); len(requests) != 0 {
		t.Fatalf("site calls = %d, want none", len(requests))
	}
}

// A search that cannot run raises the run's notice and proposes nothing,
// while the disposition commits. A replay neither lists nor asks again.
func TestDuplicateSearchUnavailableRaisesTheNotice(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup     func(*testing.T, *findingAdjudicationFixture) *openIssueStub
		siteCalls int
	}{
		"listing fails": {func(t *testing.T, f *findingAdjudicationFixture) *openIssueStub {
			return stubOpenIssues(t, f, http.StatusBadGateway)
		}, 0},
		"no lister": {func(_ *testing.T, f *findingAdjudicationFixture) *openIssueStub {
			f.workflow.openIssues = nil
			return nil
		}, 0},
		"malformed answer": {func(t *testing.T, f *findingAdjudicationFixture) *openIssueStub {
			scriptDuplicateSearch(f, fake.DuplicateSearchMalformed)
			return stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
		}, 1},
		"unshown handle": {func(t *testing.T, f *findingAdjudicationFixture) *openIssueStub {
			scriptDuplicateSearch(f, fake.DuplicateSearchUnknownHandle)
			return stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
		}, 1},
		"no inference": {func(t *testing.T, f *findingAdjudicationFixture) *openIssueStub {
			f.workflow.inference = nil
			return stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
		}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
			seedFollowUpFilingSubject(t, f, true)
			stub := tc.setup(t, f)
			reconcileFollowUpFiling(t, f, productionReviewPassed)
			replayFollowUpFiling(t, f, readFollowUpFilingState(t, f, 1).artifact)

			state := readFollowUpFilingState(t, f, 1)
			if len(state.dispositions) != 1 || len(state.instances) != 0 || len(state.cards) != 0 {
				t.Fatalf("dispositions, instances, cards = %d, %d, %d, want 1, 0, 0",
					len(state.dispositions), len(state.instances), len(state.cards))
			}
			if len(state.notices) != 1 || state.notices[0].ID != followUpFilingRefusedItemID(f.task.RunID) ||
				!strings.Contains(state.notices[0].Reason, followUpFilingDuplicateSearchUnavailable) {
				t.Fatalf("notices = %#v, want the run's duplicate_search_unavailable notice", state.notices)
			}
			if _, err := readDuplicateSearch(t, f, state.artifact); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("search record = %v, want none", err)
			}
			if calls := len(duplicateSearchRequests(f)); calls != tc.siteCalls {
				t.Fatalf("site calls = %d, want %d", calls, tc.siteCalls)
			}
			if stub != nil && stub.requests.Load() != 1 {
				t.Fatalf("listings = %d, want 1", stub.requests.Load())
			}
		})
	}
}

// A parked run whose separate-work finding duplicates an open issue proposes
// nothing, and its later passes commit no write.
func TestSeparateWorkDuplicateReplayWritesNothing(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteParkSeparateWork, nil)
	seedFollowUpFilingSubject(t, f, true)
	stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchMatch)
	reconcileFollowUpFiling(t, f, productionReviewPending)
	acceptFindingAdjudicationItem(t, f, productionReviewItemID(f.task.RunID, 1))
	artifact := readFollowUpFilingState(t, f, 1).artifact
	routes := map[domain.FindingID]domain.AdjudicationRoute{f.finding.ID: domain.RouteParkSeparateWork}
	var revisions [3]int64
	for pass := range revisions {
		if err := f.workflow.proposeSeparateWorkFilings(f.ctx, f.task, artifact, routes); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		server, err := f.store.ServerState(f.ctx)
		if err != nil {
			t.Fatal(err)
		}
		revisions[pass] = server.Revision
	}
	state := readFollowUpFilingState(t, f, 1)
	if revisions[1] != revisions[2] || len(state.instances) != 0 || len(state.notices) != 1 {
		t.Fatalf("revisions = %v, instances, notices = %d, %d", revisions, len(state.instances), len(state.notices))
	}
	search, err := readDuplicateSearch(t, f, artifact)
	if number, ok := search.DuplicateIssue(); err != nil || !ok || number != 41 ||
		search.Kind != domain.FollowUpSourceSeparateWorkVerdict {
		t.Fatalf("search = %#v, %v, want a separate-work record naming issue 41", search, err)
	}
	if calls := len(duplicateSearchRequests(f)); calls != 1 {
		t.Fatalf("site calls = %d, want 1", calls)
	}
}

// The run's bound issue is left out of the candidates: the run is closing it,
// so it cannot already track the run's follow-up work.
func TestDuplicateSearchLeavesOutTheBoundIssue(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	bound := 41
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(f.ctx, domain.Project{
			ID: f.task.ProjectID, Repo: "owner/repo", RepositoryID: 123,
		}); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged, BoundIssue: &bound,
			DeclaredPaths: domain.CanonicalDeclaredPaths(f.binding.resolvedPolicy),
		}, f.task.RunID, f.task.ProjectID, f.finding.CreatedAt.Add(-time.Hour))
		if err != nil {
			return err
		}
		return tx.RecordWorkUnitDeclaration(f.ctx, declaration)
	}); err != nil {
		t.Fatalf("seed a bound declaration: %v", err)
	}
	stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchNew)
	reconcileFollowUpFiling(t, f, productionReviewPassed)

	state := readFollowUpFilingState(t, f, 1)
	assertOneFilingCard(t, f, state, domain.FollowUpSourceDeferredDisposition)
	search, err := readDuplicateSearch(t, f, state.artifact)
	if err != nil || len(search.Candidates) != 1 || search.Candidates[0].Number != 7 {
		t.Fatalf("search = %#v, %v, want issue 7 alone", search, err)
	}
	requests := duplicateSearchRequests(f)
	if len(requests) != 1 || strings.Contains(requests[0].Fields["candidates"], "retries") {
		t.Fatalf("site saw %#v, want the bound issue left out", requests)
	}
}
