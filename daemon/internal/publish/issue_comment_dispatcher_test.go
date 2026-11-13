package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The issue comment dispatcher runs on the filing harness: one store, one
// fake forge, and a clock the test moves. These are its own fixtures.

// fakeIssueComment is one comment the fake holds on an issue.
type fakeIssueComment struct {
	ID        int64
	Issue     int
	UserID    int64
	CreatedAt time.Time
	Body      string
}

// fakeIssueComments is the fake's issue comment state. The harness's tests
// drive the dispatcher from one goroutine, so it needs no lock of its own.
type fakeIssueComments struct {
	comments []fakeIssueComment
	nextID   int64
	// posts counts create requests that reached the forge.
	posts int
	// responses are consumed one per create; none left answers a plain 201.
	responses []fakeCreateResponse
	// listFailures makes the next n listings answer 500.
	listFailures int
	// onList runs once, at the start of the next listing, so a test can act
	// while the dispatcher is between its cap read and its attempt.
	onList func()
}

var issueCommentsPath = regexp.MustCompile(`^` + regexp.QuoteMeta(testRepoPath) + `/issues/(\d+)/comments$`)

// serveIssueComments answers the issue comment endpoints and reports whether
// the request was one.
func (h *filingHarness) serveIssueComments(w http.ResponseWriter, req *http.Request) bool {
	match := issueCommentsPath.FindStringSubmatch(req.URL.Path)
	if match == nil {
		return false
	}
	issue, _ := strconv.Atoi(match[1])
	state := &h.issueComments
	switch req.Method {
	case http.MethodGet:
		if hook := state.onList; hook != nil {
			state.onList = nil
			hook()
		}
		if state.listFailures > 0 {
			state.listFailures--
			w.WriteHeader(http.StatusInternalServerError)
			return true
		}
		rows := []map[string]any{}
		for _, c := range state.comments {
			if c.Issue == issue {
				rows = append(rows, issueCommentJSON(c))
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	case http.MethodPost:
		state.posts++
		var request struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
			h.t.Errorf("decode comment request: %v", err)
		}
		response := fakeCreateResponse{Status: http.StatusCreated}
		if len(state.responses) > 0 {
			response, state.responses = state.responses[0], state.responses[1:]
		}
		if response.Drop {
			h.gh.mu.Lock()
			h.gh.filing.dropNext = true
			h.gh.mu.Unlock()
		}
		var created *fakeIssueComment
		if response.Status == http.StatusCreated || response.Commit {
			state.nextID++
			comment := fakeIssueComment{
				ID: 9000 + state.nextID, Issue: issue, UserID: h.bot.BotUserID,
				CreatedAt: h.now.UTC().Truncate(time.Second), Body: request.Body,
			}
			state.comments = append(state.comments, comment)
			created = &comment
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.Status)
		switch {
		case response.Body != "":
			_, _ = w.Write([]byte(response.Body))
		case response.Status == http.StatusCreated && created != nil:
			_ = json.NewEncoder(w).Encode(issueCommentJSON(*created))
		default:
			_, _ = w.Write([]byte(`{"message":"failed"}`))
		}
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
	return true
}

func issueCommentJSON(c fakeIssueComment) map[string]any {
	return map[string]any{
		"id":         c.ID,
		"issue_url":  fmt.Sprintf("http://github.test%s/issues/%d", testRepoPath, c.Issue),
		"user":       map[string]any{"id": c.UserID, "type": "Bot"},
		"created_at": c.CreatedAt.Format(time.RFC3339),
		"body":       c.Body,
	}
}

// plantComment adds a comment on issue as if something else posted it.
func (h *filingHarness) plantComment(issue int, userID int64) {
	h.issueComments.nextID++
	h.issueComments.comments = append(h.issueComments.comments, fakeIssueComment{
		ID: 9000 + h.issueComments.nextID, Issue: issue, UserID: userID,
		CreatedAt: h.now.UTC().Truncate(time.Second),
	})
}

// harnessAuditor answers the harness's audit and records each ref it read.
type harnessAuditor struct{ h *filingHarness }

func (a harnessAuditor) Audit(_ context.Context, repo, baseRef string) (domain.WorkflowAudit, error) {
	a.h.audits++
	a.h.auditedRefs = append(a.h.auditedRefs, baseRef)
	audit := a.h.audit
	audit.AuditedRef = baseRef
	return audit, nil
}

const commentWorkflowPath = ".github/workflows/triage.yml"

// trustComments records a trust profile that accepts one issue_comment
// workflow, and points the auditor at a default branch that runs exactly that
// workflow.
func (h *filingHarness) trustComments() {
	h.t.Helper()
	h.trustCommentsFor(testRepoID)
}

// trustCommentsFor records the comment trust profile under repositoryID.
func (h *filingHarness) trustCommentsFor(repositoryID int64) {
	h.t.Helper()
	accepted := []domain.IssueCommentWorkflow{{Path: commentWorkflowPath, Digest: filingDigest("c")}}
	h.audit = workflowAuditForRepo(h.t, filingRepo)
	h.audit.IssueCommentWorkflows = accepted
	evidence := workflowAuditEvidenceForRepo(h.t, filingRepo)
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo:                       filingRepo,
		RepositoryID:               repositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        evidence.Digest(),
		Review:                     domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: testRecipe},
		AllowIssueCommentWorkflows: true,
		IssueCommentWorkflows:      accepted,
	})
	if err != nil {
		h.t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	if err := h.store.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(context.Background(), profile, filingAt)
	}); err != nil {
		h.t.Fatalf("record trust profile: %v", err)
	}
}

const commentBody = "A review finding matches this issue.\n\nFinding: the sync client retries without a budget."

// proposeComment records that one of the run's findings duplicates issue,
// admits the comment proposal on it, and opens its card. decision is as
// propose's.
func (h *filingHarness) proposeComment(run filingRun, finding, issue int, decision domain.Action) domain.ProposalInstance {
	h.t.Helper()
	ctx := context.Background()
	created := filingAt.Add(time.Hour + time.Duration(finding)*time.Minute)
	repository := domain.FollowUpFilingRepository{Repo: run.target.Repo, RepositoryID: run.target.RepositoryID}
	search, err := domain.NewDuplicateSearch(domain.DuplicateSearchInput{
		RunID: run.id, AdjudicationDigest: run.adjudication.Digest, FindingID: run.findings[finding].ID,
		Kind: domain.FollowUpSourceDeferredDisposition, Repository: repository,
		CandidateLimit: domain.MaxDuplicateSearchCandidates,
		Candidates: []domain.DuplicateSearchCandidate{
			{Handle: domain.DuplicateSearchHandle(1), Number: issue, Title: "Bound the sync client's retries"},
			{Handle: domain.DuplicateSearchHandle(2), Number: issue + 1000, Title: "Unrelated cleanup"},
		},
		DuplicateOf: domain.DuplicateSearchHandle(1), Confidence: domain.ConfidenceHigh,
		Reason: "candidate-1 already tracks the retry budget.", CreatedAt: created,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	proposal, err := domain.NewEffectProposal(domain.EffectIssueComment, domain.IssueCommentInput{
		SubjectHandle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(run.id)),
		Repository:    repository,
		Search:        search,
		Body: domain.ScreenedIssueText{
			Text: commentBody, Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
		},
	}, run.policy)
	if err != nil {
		h.t.Fatal(err)
	}
	var instance domain.ProposalInstance
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDuplicateSearch(ctx, search); err != nil {
			return err
		}
		var err error
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{
				Source:          domain.ProposalSourceUpstreamEvent,
				UpstreamEventID: fmt.Sprintf("comment-%s-%d", run.id, finding),
			},
			domain.ProposalBatchID(fmt.Sprintf("batch-%s-%d", run.id, finding)), proposal, created)
		if err != nil {
			return err
		}
		item, err := signet.OpenIssueCommentItem(ctx, tx, h.blobs, instance.ID, created)
		if err != nil {
			return err
		}
		return decideCard(ctx, tx, instance, item, decision, created)
	}); err != nil {
		h.t.Fatalf("propose comment %s/%d: %v", run.id, finding, err)
	}
	return instance
}

// commentPass runs one dispatcher pass. No pass error may carry the
// installation token.
func (h *filingHarness) commentPass() error {
	h.t.Helper()
	err := h.comments.Pass(context.Background())
	if err != nil && strings.Contains(err.Error(), fixtureTokenValue) {
		h.t.Fatalf("pass error carries the installation token: %v", err)
	}
	return err
}

func (h *filingHarness) mustCommentPass() {
	h.t.Helper()
	if err := h.commentPass(); err != nil {
		h.t.Fatalf("comment Pass: %v", err)
	}
}

func (h *filingHarness) commentIntent(id domain.ProposalInstanceID) *domain.IssueCommentIntent {
	h.t.Helper()
	var intent *domain.IssueCommentIntent
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		got, err := tx.GetIssueCommentIntent(context.Background(), id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		intent = &got
		return err
	}); err != nil {
		h.t.Fatalf("read comment intent %q: %v", id, err)
	}
	return intent
}

func (h *filingHarness) posted(id domain.ProposalInstanceID) *domain.PostedIssueComment {
	h.t.Helper()
	var posted *domain.PostedIssueComment
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		posted, err = tx.PostedIssueCommentForInstance(context.Background(), id)
		return err
	}); err != nil {
		h.t.Fatalf("read posted comment %q: %v", id, err)
	}
	return posted
}

func (h *filingHarness) commentNotice(id domain.ProposalInstanceID, code string) *domain.AttentionItem {
	h.t.Helper()
	var item *domain.AttentionItem
	itemID := domain.ItemID("issue-comment/" + string(id) + "/" + code)
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(context.Background(), itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		item = &got
		return err
	}); err != nil {
		h.t.Fatalf("read health item %q: %v", itemID, err)
	}
	return item
}

// assertPosted fails unless the comment ended ledgered as commentID on issue.
func (h *filingHarness) assertPosted(id domain.ProposalInstanceID, issue int, commentID int64) {
	h.t.Helper()
	intent, posted := h.commentIntent(id), h.posted(id)
	if intent == nil || intent.Terminal == nil || intent.Terminal.Outcome != domain.FollowUpFilingLedgered ||
		posted == nil || posted.IssueNumber != issue || posted.CommentID != commentID || posted.RepositoryID != testRepoID {
		h.t.Fatalf("comment %q: intent = %+v, posted = %+v, want ledgered as comment %d on issue %d",
			id, intent, posted, commentID, issue)
	}
}

// assertCommentTerminal fails unless the comment ended with the outcome and
// reason, no ledger row, and an open advisory notice under code.
func (h *filingHarness) assertCommentTerminal(
	id domain.ProposalInstanceID, outcome domain.FollowUpFilingOutcome,
	reason domain.FollowUpFilingRefusalReason, code string,
) domain.AttentionItem {
	h.t.Helper()
	intent := h.commentIntent(id)
	if intent == nil || intent.Terminal == nil || intent.Terminal.Outcome != outcome ||
		(reason == "") != (intent.Terminal.Reason == nil) ||
		(reason != "" && *intent.Terminal.Reason != reason) {
		h.t.Fatalf("comment %q: intent = %+v, want terminal %s %q", id, intent, outcome, reason)
	}
	if posted := h.posted(id); posted != nil {
		h.t.Fatalf("comment %q: ledger row %+v beside a %s outcome", id, posted, outcome)
	}
	item := h.commentNotice(id, code)
	if item == nil || item.Type != domain.AttentionSystemHealth || item.Status != domain.StatusOpen ||
		item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory ||
		item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != code ||
		!strings.Contains(item.Reason, string(id)) || strings.Contains(item.Reason, fixtureTokenValue) {
		h.t.Fatalf("comment %q %s notice = %+v", id, code, item)
	}
	return *item
}

func (h *filingHarness) issueCommentsOn(issue int) []fakeIssueComment {
	var on []fakeIssueComment
	for _, c := range h.issueComments.comments {
		if c.Issue == issue {
			on = append(on, c)
		}
	}
	return on
}

// TestIssueCommentDispatcherPostsOnceAfterApproval: only an approved comment
// posts, it posts once across passes and a restart, and the workflow recheck
// reads the default branch before the post.
func TestIssueCommentDispatcherPostsOnceAfterApproval(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	h.trustComments()
	run := h.seedRun("run-a", nil, nil)
	approved := h.proposeComment(run, 0, 41, domain.ActionApprove)
	declined := h.proposeComment(run, 1, 42, domain.ActionDecline)
	undecided := h.proposeComment(run, 2, 43, "")

	h.mustCommentPass()
	if h.issueComments.posts != 1 {
		t.Fatalf("comment posts = %d, want 1", h.issueComments.posts)
	}
	on := h.issueCommentsOn(41)
	if len(on) != 1 || on[0].Body != commentBody {
		t.Fatalf("comments on issue 41 = %+v, want the approved body once", on)
	}
	h.assertPosted(approved.ID, 41, on[0].ID)
	for _, id := range []domain.ProposalInstanceID{declined.ID, undecided.ID} {
		if intent := h.commentIntent(id); intent != nil {
			t.Fatalf("comment %q without an approval opened an intent: %+v", id, intent)
		}
	}
	// One audit, at the branch the repository reports as its default.
	if h.audits != 1 || h.auditedRefs[0] != "main" {
		t.Fatalf("audits = %d at %v, want one at main", h.audits, h.auditedRefs)
	}

	h.mustCommentPass()
	h.restart()
	h.advance(pastSettle)
	h.mustCommentPass()
	if h.issueComments.posts != 1 || h.audits != 1 {
		t.Fatalf("after more passes and a restart: posts = %d, audits = %d, want 1 and 1",
			h.issueComments.posts, h.audits)
	}
}

// TestIssueCommentDispatcherAdoptsACommittedPost: a post whose response was
// lost is adopted from the settled listing, never sent twice.
func TestIssueCommentDispatcherAdoptsACommittedPost(t *testing.T) {
	t.Parallel()
	cases := map[string]fakeCreateResponse{
		"server error after commit": {Status: http.StatusInternalServerError, Commit: true},
		"connection dropped":        {Status: http.StatusCreated, Drop: true},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			h.trustComments()
			run := h.seedRun("run-a", nil, nil)
			comment := h.proposeComment(run, 0, 41, domain.ActionApprove)
			h.issueComments.responses = []fakeCreateResponse{response}

			_ = h.commentPass()
			intent := h.commentIntent(comment.ID)
			if intent == nil || len(intent.Attempts) != 1 || intent.Terminal != nil {
				t.Fatalf("intent after the lost response = %+v, want one attempt and no terminal", intent)
			}
			// Inside the settle interval nothing is sent or decided.
			h.mustCommentPass()
			h.restart()
			if h.posted(comment.ID) != nil || h.issueComments.posts != 1 {
				t.Fatalf("before settling: posted = %+v, posts = %d", h.posted(comment.ID), h.issueComments.posts)
			}
			h.advance(pastSettle)
			h.mustCommentPass()
			on := h.issueCommentsOn(41)
			if len(on) != 1 || h.issueComments.posts != 1 {
				t.Fatalf("comments = %+v, posts = %d, want the one committed post", on, h.issueComments.posts)
			}
			h.assertPosted(comment.ID, 41, on[0].ID)
		})
	}
}

// TestIssueCommentDispatcherRecordsAmbiguity: an unproven post the listing
// cannot settle to one comment of its own ends ambiguous, and a comment the
// App posted before the attempt is never adopted.
func TestIssueCommentDispatcherRecordsAmbiguity(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		response fakeCreateResponse
		// after runs once the unproven attempt is recorded.
		after func(h *filingHarness)
		// before runs before the first pass.
		before func(h *filingHarness)
		want   string
	}{
		"two candidates": {
			response: fakeCreateResponse{Status: http.StatusInternalServerError, Commit: true},
			after:    func(h *filingHarness) { h.plantComment(41, filingBotID) },
			want:     "2 comments could be",
		},
		"pre-existing comment only": {
			response: fakeCreateResponse{Status: http.StatusInternalServerError},
			before:   func(h *filingHarness) { h.plantComment(41, filingBotID) },
			want:     "0 comments could be",
		},
		"listing keeps failing": {
			response: fakeCreateResponse{Status: http.StatusInternalServerError, Commit: true},
			after:    func(h *filingHarness) { h.issueComments.listFailures = 100 },
			want:     "could not complete",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			h.trustComments()
			run := h.seedRun("run-a", nil, nil)
			comment := h.proposeComment(run, 0, 41, domain.ActionApprove)
			if tc.before != nil {
				tc.before(h)
			}
			h.issueComments.responses = []fakeCreateResponse{tc.response}
			_ = h.commentPass()
			if tc.after != nil {
				tc.after(h)
			}
			h.advance(pastSettle)
			for range 5 {
				if intent := h.commentIntent(comment.ID); intent != nil && intent.Terminal != nil {
					break
				}
				_ = h.commentPass()
			}
			item := h.assertCommentTerminal(comment.ID, domain.FollowUpFilingAmbiguous, "", "issue_comment_ambiguous")
			if !strings.Contains(item.Reason, tc.want) {
				t.Fatalf("ambiguity notice = %q, want it to say %q", item.Reason, tc.want)
			}
			if h.issueComments.posts != 1 {
				t.Fatalf("comment posts = %d, want 1", h.issueComments.posts)
			}
		})
	}
}

// TestIssueCommentDispatcherRefusesDrift: a default branch whose
// issue_comment workflows are not the accepted set refuses the post, as does
// a repository with no trust profile.
func TestIssueCommentDispatcherRefusesDrift(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		setup func(h *filingHarness)
		want  string
	}{
		"workflow changed": {
			setup: func(h *filingHarness) {
				h.trustComments()
				h.audit.IssueCommentWorkflows = []domain.IssueCommentWorkflow{
					{Path: commentWorkflowPath, Digest: filingDigest("d")},
				}
			},
			want: "no longer the set",
		},
		"workflow added": {
			setup: func(h *filingHarness) {
				h.trustComments()
				h.audit.IssueCommentWorkflows = append(h.audit.IssueCommentWorkflows,
					domain.IssueCommentWorkflow{Path: ".github/workflows/zz.yml", Digest: filingDigest("e")})
			},
			want: "no longer the set",
		},
		"no trust profile": {
			setup: func(*filingHarness) {},
			want:  "trust profile does not read",
		},
		"profile for another repository": {
			setup: func(h *filingHarness) { h.trustCommentsFor(testRepoID + 1) },
			want:  "names another repository",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newFilingHarness(t)
			tc.setup(h)
			run := h.seedRun("run-a", nil, nil)
			comment := h.proposeComment(run, 0, 41, domain.ActionApprove)
			h.mustCommentPass()
			item := h.assertCommentTerminal(comment.ID, domain.FollowUpFilingRefused,
				domain.FollowUpFilingRefusalPreconditionFailed, "issue_comment_refused")
			if !strings.Contains(item.Reason, tc.want) {
				t.Fatalf("refusal notice = %q, want it to say %q", item.Reason, tc.want)
			}
			if h.issueComments.posts != 0 {
				t.Fatalf("comment posts = %d, want none", h.issueComments.posts)
			}
		})
	}
}

// TestIssueCommentPerRunCapCountsFilings: the per-run cap counts a run's
// filings and comments together, whichever dispatcher posts first.
func TestIssueCommentPerRunCapCountsFilings(t *testing.T) {
	t.Parallel()
	capped := map[string]string{"follow_up_filing.max_per_run": "2"}
	t.Run("filing first", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		h.trustComments()
		run := h.seedRun("run-a", capped, nil)
		filing := h.approve(run, 0)
		first := h.proposeComment(run, 1, 41, domain.ActionApprove)
		second := h.proposeComment(run, 2, 42, domain.ActionApprove)
		h.mustPass()
		h.assertLedgered(filing.ID, h.forgeIssues()[0].Number)
		h.mustCommentPass()
		h.assertPosted(first.ID, 41, h.issueCommentsOn(41)[0].ID)
		item := h.assertCommentTerminal(second.ID, domain.FollowUpFilingRefused,
			domain.FollowUpFilingRefusalPreconditionFailed, "issue_comment_capped")
		if !strings.Contains(item.Reason, "follow_up_filing.max_per_run") {
			t.Fatalf("cap notice = %q", item.Reason)
		}
	})
	t.Run("filing started while the comment lists", func(t *testing.T) {
		t.Parallel()
		// The filer starts its attempt after the comment dispatcher read the
		// count and before it starts its own, as their separate goroutines
		// can; the count in the attempt's write refuses the comment.
		h := newFilingHarness(t)
		h.trustComments()
		run := h.seedRun("run-a", map[string]string{"follow_up_filing.max_per_run": "1"}, nil)
		filing := h.approve(run, 0)
		comment := h.proposeComment(run, 1, 41, domain.ActionApprove)
		h.issueComments.onList = h.mustPass
		h.mustCommentPass()
		h.assertLedgered(filing.ID, h.forgeIssues()[0].Number)
		if h.issueComments.posts != 0 {
			t.Fatalf("comment posts = %d, want none", h.issueComments.posts)
		}
		h.assertCommentTerminal(comment.ID, domain.FollowUpFilingRefused,
			domain.FollowUpFilingRefusalPreconditionFailed, "issue_comment_capped")
	})
	t.Run("comments first", func(t *testing.T) {
		t.Parallel()
		h := newFilingHarness(t)
		h.trustComments()
		run := h.seedRun("run-a", capped, nil)
		h.proposeComment(run, 1, 41, domain.ActionApprove)
		h.proposeComment(run, 2, 42, domain.ActionApprove)
		filing := h.approve(run, 0)
		h.mustCommentPass()
		if h.issueComments.posts != 2 {
			t.Fatalf("comment posts = %d, want 2", h.issueComments.posts)
		}
		h.mustPass()
		h.assertTerminal(filing.ID, domain.FollowUpFilingRefused,
			domain.FollowUpFilingRefusalPreconditionFailed, "follow_up_filing_capped")
		if h.creates() != 0 {
			t.Fatalf("create requests = %d, want none", h.creates())
		}
	})
}
