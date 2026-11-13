package engine

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference/fake"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// seedIssueCommentTrust records the fixture repository's trust profile. allow
// sets whether it accepts the repository's issue_comment workflows; a profile
// that does not is what a profile from before the set was recorded reads as.
func seedIssueCommentTrust(t *testing.T, f *findingAdjudicationFixture, allow bool) {
	t.Helper()
	input := domain.AutomationTrustProfileInput{
		Repo: "owner/repo", RepositoryID: 123,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanPlanPreferred,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: f.record.ConfigurationDigest,
		},
	}
	if allow {
		input.AllowIssueCommentWorkflows = true
		input.IssueCommentWorkflows = []domain.IssueCommentWorkflow{
			{Path: ".github/workflows/triage.yml", Digest: adjudicationDigest("9")},
		}
	}
	profile, err := domain.NewAutomationTrustProfile(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.WriteInternal(f.ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(f.ctx, profile, time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC))
	}); err != nil {
		t.Fatal(err)
	}
}

// assertIssueCommentRefusedNotice fails unless the state's one notice is the
// run's issue_comment_refused advisory.
func assertIssueCommentRefusedNotice(t *testing.T, f *findingAdjudicationFixture, state followUpFilingState) {
	t.Helper()
	if len(state.notices) != 1 {
		t.Fatalf("notices = %d, want 1", len(state.notices))
	}
	notice := state.notices[0]
	if notice.ID != issueCommentRefusedItemID(f.task.RunID) || notice.HealthDiagnostic == nil ||
		notice.HealthDiagnostic.Code != "issue_comment_refused" ||
		notice.Posture == nil || *notice.Posture != domain.HealthPostureAdvisory ||
		!strings.Contains(notice.Reason, "owner/repo") {
		t.Fatalf("notice = %#v, want the run's issue_comment_refused advisory", notice)
	}
}

// assertOneCommentCard fails unless the state holds one comment proposal on
// issue 41, from the search's record, with its open card and no filing.
func assertOneCommentCard(t *testing.T, f *findingAdjudicationFixture, state followUpFilingState) {
	t.Helper()
	if len(state.instances) != 1 || len(state.cards) != 1 || len(state.notices) != 0 {
		t.Fatalf("instances, cards, notices = %d, %d, %d, want 1, 1, 0",
			len(state.instances), len(state.cards), len(state.notices))
	}
	instance, card := state.instances[0], state.cards[0]
	search, err := readDuplicateSearch(t, f, state.artifact)
	if err != nil {
		t.Fatal(err)
	}
	comment := instance.Proposal.CommentProposal
	if instance.Proposal.Kind != domain.EffectIssueComment || comment == nil ||
		comment.IssueNumber != 41 || comment.Repository.RepositoryID != 123 ||
		comment.Source.SearchDigest != search.Digest || comment.Source.FindingID != f.finding.ID ||
		instance.Admission.ExportIdentity != state.artifact.Digest || instance.Admission.EmissionOrdinal != 1 {
		t.Fatalf("instance = %#v, want one comment on issue 41", instance)
	}
	if card.Reason != "Decide whether to comment on the matching issue" ||
		len(card.ArtifactDigests) != 1 || card.ArtifactDigests[0] != instance.Proposal.Digest {
		t.Fatalf("card = %#v", card)
	}
	if err := publicationtext.ScreenIssueBody(
		comment.Body.Ruleset, comment.Body.Text, domain.MaxFollowUpFilingBodyBytes); err != nil ||
		!strings.HasPrefix(comment.Body.Text, issueCommentLead) ||
		!strings.Contains(comment.Body.Text, "Why it matches: candidate-1 already tracks this work.") {
		t.Fatalf("body = %q (screen %v)", comment.Body.Text, err)
	}
}

// A deferred finding whose search names an open issue, in a repository whose
// trust profile accepts its issue_comment workflows, gets one comment
// proposal on that issue and no filing; a replay writes nothing more.
func TestDuplicateSearchMatchProposesAComment(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	seedFollowUpFilingSubject(t, f, true)
	seedIssueCommentTrust(t, f, true)
	stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchMatch)
	reconcileFollowUpFiling(t, f, productionReviewPassed)
	state := readFollowUpFilingState(t, f, 1)
	assertOneCommentCard(t, f, state)

	replayFollowUpFiling(t, f, state.artifact)
	if again := readFollowUpFilingState(t, f, 1); len(again.instances) != 1 || len(again.cards) != 1 {
		t.Fatalf("after replay: instances, cards = %d, %d, want 1, 1", len(again.instances), len(again.cards))
	}
}

// A profile that does not accept the repository's issue_comment workflows
// proposes no comment and raises the run's notice.
func TestDuplicateSearchMatchUnderAnUnacceptedProfileRaisesTheNotice(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	seedFollowUpFilingSubject(t, f, true)
	seedIssueCommentTrust(t, f, false)
	stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchMatch)
	reconcileFollowUpFiling(t, f, productionReviewPassed)
	state := readFollowUpFilingState(t, f, 1)
	if len(state.instances) != 0 || len(state.cards) != 0 {
		t.Fatalf("instances, cards = %d, %d, want 0, 0", len(state.instances), len(state.cards))
	}
	assertIssueCommentRefusedNotice(t, f, state)
}

// A search that answers "new" still proposes the filing and no comment, even
// under a profile that accepts comments.
func TestDuplicateSearchNewProposesNoComment(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	seedFollowUpFilingSubject(t, f, true)
	seedIssueCommentTrust(t, f, true)
	stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchNew)
	reconcileFollowUpFiling(t, f, productionReviewPassed)
	assertOneFilingCard(t, f, readFollowUpFilingState(t, f, 1), domain.FollowUpSourceDeferredDisposition)
}

// The separate-work path counts a duplicate with no comment yet as work left,
// so it proposes the comment, and its later passes write nothing.
func TestSeparateWorkDuplicateProposesTheComment(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteParkSeparateWork, nil)
	seedFollowUpFilingSubject(t, f, true)
	seedIssueCommentTrust(t, f, true)
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
	if revisions[1] != revisions[2] {
		t.Fatalf("revisions = %v, want no write after the comment", revisions)
	}
	if len(state.instances) != 1 || state.instances[0].Proposal.Kind != domain.EffectIssueComment {
		t.Fatalf("instances = %#v, want one comment", state.instances)
	}
}

// A finding whose text fails the screen takes the fixed body naming its ID,
// and an ID the screen would refuse is left out of it.
func TestIssueCommentTextFallsBackOnAScreenRefusal(t *testing.T) {
	t.Parallel()
	search := domain.DuplicateSearch{Reason: "candidate-1 tracks it."}
	entry := domain.FindingAdjudicationEntry{Rationale: "adjacent"}
	cases := []struct {
		name     string
		finding  domain.Finding
		fallback string
		suffix   string
	}{
		{
			name:     "own finding",
			finding:  domain.Finding{ID: "review-0a1b", Message: "ping @someone about this"},
			fallback: issueCommentFallbackBody, suffix: " (review-0a1b)",
		},
		{
			name: "external finding",
			finding: domain.Finding{
				ID: "external-0a1b", Message: "ping @someone about this",
				External: &domain.ExternalFindingProvenance{ReviewerLogin: "reviewer"},
			},
			fallback: externalIssueCommentFallbackBody, suffix: " (external-0a1b)",
		},
		{
			name:     "an ID the screen refuses",
			finding:  domain.Finding{ID: "@someone", Message: "ping @someone about this"},
			fallback: issueCommentFallbackBody, suffix: "",
		},
	}
	for _, tc := range cases {
		got := issueCommentText(tc.finding, entry, search)
		if want := fmt.Sprintf(tc.fallback, tc.suffix); got.Text != want {
			t.Errorf("%s: body = %q, want %q", tc.name, got.Text, want)
		}
		if err := publicationtext.ScreenIssueBody(
			got.Ruleset, got.Text, domain.MaxFollowUpFilingBodyBytes); err != nil {
			t.Errorf("%s: fallback fails the screen: %v", tc.name, err)
		}
	}
}

var commentGateOff = map[string]string{string(domain.WorkflowGateIssueComment): "false"}

// With gates.issue_comment off and the profile accepting the issue_comment
// set, a matching finding's comment gets a policy approval and no card, and
// a replay writes nothing more.
func TestDuplicateSearchMatchWithTheGateOffApprovesByPolicy(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteParkSeparateWork, commentGateOff)
	seedFollowUpFilingSubject(t, f, true)
	seedIssueCommentTrust(t, f, true)
	stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchMatch)
	reconcileFollowUpFiling(t, f, productionReviewPending)
	acceptFindingAdjudicationItem(t, f, productionReviewItemID(f.task.RunID, 1))
	artifact := readFollowUpFilingState(t, f, 1).artifact
	routes := map[domain.FindingID]domain.AdjudicationRoute{f.finding.ID: domain.RouteParkSeparateWork}
	var revisions [2]int64
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
	if revisions[0] != revisions[1] {
		t.Fatalf("revisions = %v, want no write after the policy approval", revisions)
	}
	if len(state.instances) != 1 || len(state.cards) != 0 || len(state.notices) != 0 {
		t.Fatalf("instances, cards, notices = %d, %d, %d, want 1, 0, 0",
			len(state.instances), len(state.cards), len(state.notices))
	}
	var approved store.ApprovedIssueComment
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		approved, err = tx.ApprovedIssueComment(f.ctx, state.instances[0].ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !approved.ByPolicy || approved.IssueNumber != 41 || approved.RepositoryID != 123 {
		t.Fatalf("approved = %+v, want a policy approval on issue 41", approved)
	}
}

// A gate value that does not parse keeps the card, as a gate that is on does.
func TestDuplicateSearchMatchUnderAMalformedGateTakesTheCard(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer,
		map[string]string{string(domain.WorkflowGateIssueComment): "sometimes"})
	seedFollowUpFilingSubject(t, f, true)
	seedIssueCommentTrust(t, f, true)
	stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchMatch)
	reconcileFollowUpFiling(t, f, productionReviewPassed)
	assertOneCommentCard(t, f, readFollowUpFilingState(t, f, 1))
}

// With the gate off, a profile that does not accept the issue_comment set
// still refuses the comment: the acceptance binds both paths, so there is no
// card to fall back to.
func TestDuplicateSearchMatchWithTheGateOffUnderAnUnacceptedProfileRaisesTheNotice(t *testing.T) {
	t.Parallel()
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, commentGateOff)
	seedFollowUpFilingSubject(t, f, true)
	seedIssueCommentTrust(t, f, false)
	stubOpenIssues(t, f, http.StatusOK, twoOpenIssues...)
	scriptDuplicateSearch(f, fake.DuplicateSearchMatch)
	reconcileFollowUpFiling(t, f, productionReviewPassed)
	state := readFollowUpFilingState(t, f, 1)
	if len(state.instances) != 0 || len(state.cards) != 0 {
		t.Fatalf("instances, cards = %d, %d, want 0, 0", len(state.instances), len(state.cards))
	}
	assertIssueCommentRefusedNotice(t, f, state)
}
