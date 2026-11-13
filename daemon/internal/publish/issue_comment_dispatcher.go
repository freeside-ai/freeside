package publish

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Issue comment dispatch (plan §5.13, issue #3147). The comment ledger
// (store/issue_comment_ledger.go) owns the evidence rule, as the filing
// ledger does for the follow-up filer: every store write below is one
// transition it accepts or refuses. This file only decides which transition
// to ask for next, from the intent's durable state and what GitHub answers,
// so a crash at any point resumes from the ledger and never from memory.
//
// It mirrors FollowUpFiler with three differences. The scope that serializes
// posts is the target issue, so the pre-dispatch set is the App's comments on
// that issue and an ambiguous outcome blocks adoption on that issue only. An
// App-authored comment starts the repository's issue_comment workflows, so
// the trust profile's accepted set is rechecked at the default-branch tip
// immediately before every post. And a comment creates no issue, so only the
// per-run cap applies, counted with the run's filings.
//
// A comment is approved by a person on its card, or by the policy actor when
// gates.issue_comment is off for its run (issue #3150); the store's approval
// read accepts exactly one of the two. A policy-approved comment has no card,
// so its notices go where the card would have been, and its post raises one
// notice naming the comment, since no person saw it before it went out.

// Health diagnostic codes for the dispatcher's system_health items.
const (
	issueCommentAmbiguousCode  = "issue_comment_ambiguous"
	issueCommentRefusedCode    = "issue_comment_refused"
	issueCommentCappedCode     = "issue_comment_capped"
	issueCommentStalledCode    = "issue_comment_stalled"
	issueCommentUnreadableCode = "issue_comment_unreadable"
	issueCommentPostedCode     = "issue_comment_posted"
)

const issueCommentItemPrefix = "issue-comment/"

// IssueCommentDispatcher posts the approved issue comments, exactly once
// each, and recovers a post a crash interrupted.
type IssueCommentDispatcher struct {
	store    *store.Store
	forge    *forge
	identity FollowUpFilingIdentitySource
	auditor  WorkflowAuditor
	now      func() time.Time
	wake     chan struct{}

	// mu serializes passes, so a wake during a sweep cannot drive one intent
	// from two goroutines.
	mu              sync.Mutex
	listingFailures map[domain.ProposalInstanceID]int
	// failingSince is when each comment's current run of failed passes began.
	failingSince map[domain.ProposalInstanceID]time.Time
	notices      *proposalNotices
}

// NewIssueCommentDispatcher wires a dispatcher. tokens must mint the filing
// permission set (issues: write, metadata: read), which covers issue
// comments; auditor reads workflow files for the recheck. The dispatcher
// holds no other credential.
func NewIssueCommentDispatcher(
	s *store.Store, tokens TokenSource, client *http.Client, baseURL string,
	identity FollowUpFilingIdentitySource, auditor WorkflowAuditor, now func() time.Time,
) (*IssueCommentDispatcher, error) {
	if s == nil || tokens == nil || client == nil || strings.TrimSpace(baseURL) == "" ||
		identity == nil || auditor == nil || now == nil {
		return nil, errors.New("issue comment dispatcher: nil or empty dependency")
	}
	return &IssueCommentDispatcher{
		store: s, forge: newForge(tokens, client, strings.TrimRight(baseURL, "/")),
		identity: identity, auditor: auditor, now: now, wake: make(chan struct{}, 1),
		listingFailures: map[domain.ProposalInstanceID]int{},
		failingSince:    map[domain.ProposalInstanceID]time.Time{},
		notices: newProposalNotices(issueCommentItemPrefix,
			[]string{issueCommentStalledCode, issueCommentUnreadableCode},
			"Resolved when the condition holding the issue comment cleared.", now),
	}, nil
}

// Wake asks for a pass now. It never blocks: a wake that arrives while one is
// already pending is the same request.
func (d *IssueCommentDispatcher) Wake() {
	if d == nil {
		return
	}
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Run sweeps once at start, then on every tick and every wake, and returns
// nil when ctx ends. The start sweep is the crash recovery. report receives
// each pass's error; a failed pass never stops the loop.
func (d *IssueCommentDispatcher) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval <= 0 {
		return errors.New("issue comment dispatcher: sweep interval is not positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := d.Pass(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-d.wake:
		}
	}
}

// Pass drives every approved comment one step at a time, oldest first. An
// error on one comment is collected and does not stop the pass.
func (d *IssueCommentDispatcher) Pass(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	comments, unreadable, err := d.discover(ctx)
	if err != nil {
		return fmt.Errorf("issue comment: %w", err)
	}
	d.notices.startPass()
	var errs []error
	for _, ref := range unreadable {
		errs = append(errs, d.reportUnreadable(ctx, ref.filingRef, ref.err))
	}
	for _, comment := range comments {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		err := d.drive(ctx, comment)
		if err == nil {
			delete(d.failingSince, comment.instanceID)
			continue
		}
		errs = append(errs, fmt.Errorf("issue comment %q: %w", comment.instanceID, err))
		if ctx.Err() == nil && !errors.Is(err, errIssueCommentUnreadable) {
			errs = append(errs, d.noteStall(ctx, comment.filingRef))
		}
	}
	if ctx.Err() == nil {
		errs = append(errs, d.notices.resolveCleared(ctx, d.store))
	}
	return errors.Join(errs...)
}

// errIssueCommentUnreadable marks a pass error whose comment already has the
// unreadable notice.
var errIssueCommentUnreadable = errors.New("issue comment does not read")

// noteStall raises the stall notice once a comment has failed every pass for
// followUpFilingStallNoticeAfter. The run of failures is held in memory.
func (d *IssueCommentDispatcher) noteStall(ctx context.Context, ref filingRef) error {
	d.notices.hold(ref.instanceID, issueCommentStalledCode)
	since, failing := d.failingSince[ref.instanceID]
	if !failing {
		d.failingSince[ref.instanceID] = d.now()
		return nil
	}
	if d.now().Before(since.Add(followUpFilingStallNoticeAfter)) {
		return nil
	}
	return d.store.Write(ctx, func(tx *store.WriteTx) error {
		return d.notices.put(ctx, tx, ref, issueCommentStalledCode, fmt.Sprintf(
			"Issue comment for proposal instance %s has failed on every attempt for %d minutes and is not posted. It is retried automatically, and later comments on the same issue wait behind it. The daemon log carries the error.",
			ref.instanceID, int(followUpFilingStallNoticeAfter.Minutes())))
	})
}

type approvedComment struct {
	filingRef
	createdAt time.Time
}

type unreadableComment struct {
	filingRef
	err error
}

// discover lists the comments whose proposal card a human approved and the
// comments the policy actor approved. It is a pre-filter only: the store
// re-derives the approval when the intent opens and on every view, so a card
// that was not an approved comment posts nothing. As the filer's discover
// does, it walks the accepted approve commands and reads each item's record,
// and it reads each policy approval's subject on its own, so one tampered
// instance cannot stop every other comment.
func (d *IssueCommentDispatcher) discover(ctx context.Context) ([]approvedComment, []unreadableComment, error) {
	var comments []approvedComment
	var unreadable []unreadableComment
	err := d.store.Read(ctx, func(tx *store.ReadTx) error {
		commands, err := tx.ListCommandsForActions(ctx, domain.ActionApprove)
		if err != nil {
			return err
		}
		seen := map[domain.ItemID]bool{}
		for _, command := range commands {
			if seen[command.ItemID] {
				continue
			}
			seen[command.ItemID] = true
			item, err := tx.GetAttentionItemRecord(ctx, command.ItemID)
			if err != nil {
				return err
			}
			if item.Type != domain.AttentionEffectProposal || item.Status != domain.StatusResolved {
				continue
			}
			ref := filingRef{
				instanceID: domain.ProposalInstanceID(strings.TrimSuffix(string(item.ID), "/effect")),
				projectID:  item.ProjectID, subject: item.Subject,
			}
			// A closure card binds a prospective merge; the binding read runs
			// no proposal gate, so a closure whose gate now refuses is never
			// mistaken for a comment.
			merge, err := tx.ProspectiveMergeForItem(ctx, item.ID)
			if err != nil {
				unreadable = append(unreadable, unreadableComment{ref, err})
				continue
			}
			if merge != nil {
				continue
			}
			instance, proposal, err := tx.ProposalForItem(ctx, item.ID)
			if err != nil {
				unreadable = append(unreadable, unreadableComment{ref, err})
				continue
			}
			if proposal.Kind != domain.EffectIssueComment || proposal.CommentProposal == nil {
				continue
			}
			ref.instanceID = instance.ID
			comments = append(comments, approvedComment{filingRef: ref, createdAt: instance.CreatedAt})
		}
		policyApproved, err := tx.ListPolicyApprovedIssueComments(ctx)
		if err != nil {
			return err
		}
		for _, approved := range policyApproved {
			// The subject is the one its card would have had
			// (signet.OpenIssueCommentItem).
			ref := filingRef{
				instanceID: approved.InstanceID, projectID: approved.ProjectID,
				subject: domain.Subject{
					Type: domain.SubjectProposalBatch, ID: domain.SubjectID(approved.ProposalBatchID),
				},
			}
			if approved.Err != nil {
				unreadable = append(unreadable, unreadableComment{ref, approved.Err})
				continue
			}
			comments = append(comments, approvedComment{filingRef: ref, createdAt: approved.CreatedAt})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	slices.SortFunc(comments, func(a, b approvedComment) int {
		return cmp.Or(a.createdAt.Compare(b.createdAt), cmp.Compare(a.instanceID, b.instanceID))
	})
	return comments, unreadable, nil
}

// commentView is one comment's durable state, read in one transaction. Every
// read here re-runs the store's gates: the approval, the target issue
// re-derived from the duplicate search record, the repository from the
// project row, and the body re-screened under its ruleset.
type commentView struct {
	approved store.ApprovedIssueComment
	caps     followUpFilingCaps
	capsErr  error
	intent   *domain.IssueCommentIntent
}

func (d *IssueCommentDispatcher) view(ctx context.Context, id domain.ProposalInstanceID) (commentView, error) {
	var view commentView
	err := d.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		view.approved, err = tx.ApprovedIssueComment(ctx, id)
		if err != nil {
			return err
		}
		_, policy, err := tx.ResolveProposalSubject(ctx, view.approved.Handle)
		if err != nil {
			return err
		}
		view.caps, view.capsErr = parseFollowUpFilingCaps(policy)
		intent, err := tx.GetIssueCommentIntent(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			view.intent = &intent
		}
		return nil
	})
	return view, err
}

// drive advances one comment as far as its durable state and GitHub's
// answers allow in this pass.
func (d *IssueCommentDispatcher) drive(ctx context.Context, comment approvedComment) error {
	err := d.driveWorkflow(ctx, comment)
	if errors.Is(err, store.ErrTaskWorkflowClosed) {
		return nil
	}
	return err
}

func (d *IssueCommentDispatcher) driveWorkflow(ctx context.Context, comment approvedComment) error {
	view, err := d.view(ctx, comment.instanceID)
	if errors.Is(err, domain.ErrIssueCommentNotApproved) {
		// discover is a pre-filter; the store's approval read is the gate,
		// and a card it does not find approved posts nothing.
		return nil
	}
	if err != nil {
		return d.reportUnreadable(ctx, comment.filingRef, err)
	}
	fenceErr := d.store.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireRunWorkflowOpen(ctx, view.approved.RunID)
	})
	closed := errors.Is(fenceErr, store.ErrTaskWorkflowClosed)
	if fenceErr != nil && !closed {
		return fenceErr
	}
	if closed {
		if view.intent == nil {
			return nil
		}
		retired, err := d.retireClosed(ctx, comment.instanceID)
		if err != nil || retired {
			return err
		}
	}
	if view.intent == nil {
		var intent domain.IssueCommentIntent
		err := d.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			var err error
			intent, err = tx.OpenIssueCommentIntent(ctx, comment.instanceID, d.now())
			return err
		})
		switch {
		case errors.Is(err, domain.ErrIssueCommentIssueBusy):
			// Another comment on the issue is outstanding; the next pass
			// asks again.
			return nil
		case err != nil:
			return err
		}
		view.intent = &intent
	}
	intent := *view.intent
	if intent.Terminal != nil {
		return nil
	}
	if len(intent.Attempts) == 0 {
		return d.dispatch(ctx, comment, view)
	}
	// An attempt whose response was never recorded was dispatched: the crash
	// fell between the marker and the response, so its post is unproven.
	last := intent.Attempts[len(intent.Attempts)-1]
	class, at := domain.FollowUpFilingResponseUnproven, last.DispatchStartedAt
	if last.Response != nil {
		class, at = last.Response.Class, last.Response.RecordedAt
	}
	switch class {
	case domain.FollowUpFilingResponseSuccess, domain.FollowUpFilingResponseUnproven:
		return d.settle(ctx, comment, view, at)
	case domain.FollowUpFilingResponseDefiniteRejection:
		// The crash fell between the response and its refusal.
		return d.refuse(ctx, comment, domain.FollowUpFilingRefusalDefiniteRejection, issueCommentRefusedCode,
			"GitHub rejected the comment request", nil)
	case domain.FollowUpFilingResponseTransientRejection:
		if len(intent.Attempts) >= followUpFilingAttemptBound {
			return d.refuse(ctx, comment, domain.FollowUpFilingRefusalRetryBoundSpent, issueCommentRefusedCode,
				commentRetryBoundSpentReason, nil)
		}
		if d.now().Before(at.Add(followUpFilingRetryBackoff)) {
			return nil
		}
		return d.dispatch(ctx, comment, view)
	}
	return fmt.Errorf("attempt response class %q is not registered", class)
}

var commentRetryBoundSpentReason = fmt.Sprintf(
	"GitHub rate-limited the comment request %d times", followUpFilingAttemptBound)

// retireClosed releases only proven-unsent reservations, as the filer's
// does. Closure and the latest attempt are re-read under the writer lock.
func (d *IssueCommentDispatcher) retireClosed(ctx context.Context, id domain.ProposalInstanceID) (bool, error) {
	var retired bool
	err := d.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RequireProposalWorkflowOpen(ctx, id); !errors.Is(err, store.ErrTaskWorkflowClosed) {
			return err
		}
		intent, err := tx.GetIssueCommentIntent(ctx, id)
		if err != nil {
			return err
		}
		if intent.Terminal != nil {
			retired = true
			return nil
		}
		reason := domain.FollowUpFilingRefusalPreconditionFailed
		if len(intent.Attempts) != 0 {
			last := intent.Attempts[len(intent.Attempts)-1]
			if last.Response == nil {
				return nil
			}
			switch last.Response.Class {
			case domain.FollowUpFilingResponseDefiniteRejection:
				reason = domain.FollowUpFilingRefusalDefiniteRejection
			case domain.FollowUpFilingResponseTransientRejection:
			case domain.FollowUpFilingResponseSuccess, domain.FollowUpFilingResponseUnproven:
				return nil
			}
		}
		_, err = tx.RefuseIssueComment(ctx, id, reason, d.now())
		retired = err == nil
		return err
	})
	return retired, err
}

// commentTarget is the live GitHub state one post or listing needs, each
// field read through the filing token and checked against the stored target.
type commentTarget struct {
	repo          repoRef
	repositoryID  int64
	defaultBranch string
	bot           AppBotIdentity
}

// resolveTarget reads the live target. A definite failure returns
// *filingPreconditionError; anything else is a transient read failure.
func (d *IssueCommentDispatcher) resolveTarget(ctx context.Context, view commentView) (commentTarget, error) {
	repo, err := parseRepo(view.approved.Repo)
	if err != nil {
		return commentTarget{}, &filingPreconditionError{"the project names no owner/name repository"}
	}
	definite := func(err error) error {
		var api *APIError
		switch {
		case errors.Is(err, ErrGrantMismatch),
			errors.As(err, &api) && api.Status == http.StatusUnprocessableEntity &&
				strings.HasSuffix(api.RequestPath, "/access_tokens"):
			return &filingPreconditionError{
				"the GitHub App installation does not grant the issues: write permission commenting needs",
			}
		case errors.As(err, &api) && (api.Status == http.StatusNotFound || api.Status == http.StatusGone):
			return &filingPreconditionError{"the target repository is gone or no longer visible to the GitHub App"}
		}
		return err
	}
	target := commentTarget{repo: repo}
	target.repositoryID, target.defaultBranch, err = d.forge.getRepositoryDefaultBranch(ctx, repo)
	if err != nil {
		return commentTarget{}, definite(err)
	}
	// A name rebound to another repository is detectable only by ID.
	if target.repositoryID != view.approved.RepositoryID {
		return commentTarget{}, &filingPreconditionError{"the repository name now resolves to a different repository"}
	}
	target.bot, err = d.identity.Resolve(ctx, repo.path())
	if err != nil {
		return commentTarget{}, definite(err)
	}
	if target.bot.BotUserID <= 0 || target.bot.AppSlug == "" {
		return commentTarget{}, errors.New("resolve App bot identity: incomplete identity")
	}
	return target, nil
}

// commentIdentityFault says why bot cannot stand for the intent whose
// pre-dispatch set is given, or "" when the set was listed under bot. The set
// lists one App's comments and every post is sent as that App, so each step
// that sends a post or adopts a candidate holds the repository's current
// identity to the recorded one, as dispatchIdentityFault does for filings.
func commentIdentityFault(set *domain.IssueCommentPreDispatchSet, bot AppBotIdentity) string {
	if set.BotUserID != bot.BotUserID {
		return fmt.Sprintf(
			"the GitHub App that listed the issue's comments before dispatch (bot user ID %d) is not the one registered for it now (bot user ID %d)",
			set.BotUserID, bot.BotUserID)
	}
	return ""
}

// admitsComment reports whether a comment can be this intent's post. It reads
// the author's numeric identity, the creation time, and the pre-dispatch set,
// never comment text. The forge decoder has already proved the comment is on
// the requested issue; resolveTarget binds the repository to the intent's
// numeric ID.
func admitsComment(comment replyComment, bot AppBotIdentity, intent domain.IssueCommentIntent) bool {
	return comment.AuthorID == bot.BotUserID &&
		!comment.CreatedAt.Before(intent.OpenedAt.Add(-followUpFilingClockSkew)) &&
		(intent.PreDispatch == nil || !slices.Contains(intent.PreDispatch.CommentIDs, comment.ID))
}

// dispatch checks the preconditions, records the pre-dispatch set if none is
// recorded, rechecks the issue_comment workflows, and sends one post.
func (d *IssueCommentDispatcher) dispatch(ctx context.Context, comment approvedComment, view commentView) error {
	intent := *view.intent
	refusePrecondition := func(code, reason string) error {
		return d.refuse(ctx, comment, domain.FollowUpFilingRefusalPreconditionFailed, code, reason, nil)
	}
	if view.capsErr != nil {
		return refusePrecondition(issueCommentRefusedCode, "its policy carries a malformed cap ("+view.capsErr.Error()+")")
	}
	// The attempt's write counts again; this read skips the forge work below
	// for a run already at the cap.
	var capped string
	if err := d.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		capped, err = runCapReason(ctx, tx, view.approved.Handle, comment.instanceID, view.caps.MaxPerRun)
		return err
	}); err != nil {
		return err
	}
	if capped != "" {
		return refusePrecondition(issueCommentCappedCode, capped)
	}
	target, err := d.resolveTarget(ctx, view)
	var precondition *filingPreconditionError
	if errors.As(err, &precondition) {
		return refusePrecondition(issueCommentRefusedCode, precondition.reason)
	}
	if err != nil {
		return err
	}
	number := view.approved.IssueNumber
	if intent.PreDispatch == nil {
		// The listing pages to completion or fails, so the set is never a
		// partial one.
		existing, err := d.forge.listConversationComments(ctx, target.repo, number)
		if err != nil {
			return err
		}
		var ids []int64
		for _, c := range existing {
			if c.AuthorID == target.bot.BotUserID {
				ids = append(ids, c.ID)
			}
		}
		if err := d.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			var err error
			intent, err = tx.RecordIssueCommentPreDispatch(
				ctx, comment.instanceID, ids, target.bot.BotUserID, d.now())
			return err
		}); err != nil {
			return err
		}
	}
	// The workflow recheck runs at the current default-branch tip, as late as
	// it can before the post: GitHub runs issue_comment workflows from that
	// branch with the repository's secrets, and an App-authored comment
	// starts them (plan §5.5).
	if refusal, err := d.recheckWorkflows(ctx, view, target); err != nil || refusal != "" {
		if refusal != "" {
			return refusePrecondition(issueCommentRefusedCode, refusal)
		}
		return err
	}
	// Build the request, token included, before the dispatch marker: once the
	// marker is durable the only thing left that can fail is the send, and a
	// failed send is unproven by rule.
	request, err := d.forge.newReviewReplyRequest(ctx, target.repo, number, 0, view.approved.Body.Text)
	if err != nil {
		return err
	}
	// The identity is read after the request holds its token, so a
	// registration that changed while the set was listed cannot send this
	// post as the new App.
	target.bot, err = d.identity.Resolve(ctx, target.repo.path())
	if err != nil {
		return err
	}
	if fault := commentIdentityFault(intent.PreDispatch, target.bot); fault != "" {
		return refusePrecondition(issueCommentRefusedCode, fault)
	}
	if err := d.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		// Counted again under the writer lock: the filer starts attempts
		// against the same cap from its own goroutine.
		if capped, err = runCapReason(ctx, &tx.ReadTx, view.approved.Handle, comment.instanceID,
			view.caps.MaxPerRun); err != nil || capped != "" {
			return err
		}
		intent, err = tx.StartIssueCommentAttempt(ctx, comment.instanceID, d.now())
		return err
	}); err != nil {
		return err
	}
	if capped != "" {
		return refusePrecondition(issueCommentCappedCode, capped)
	}
	result, sendErr := d.forge.sendReviewReply(request, target.repo, number, 0)
	class := classifyForgeCreate(result.Status, result.Header, result.Comment != nil, sendErr)
	// Returned-object boundary: a 201 is ledgered only when its comment would
	// also pass as a candidate. One that would not is unproven, and the
	// settled listing decides.
	if class == domain.FollowUpFilingResponseSuccess && !admitsComment(*result.Comment, target.bot, intent) {
		class = domain.FollowUpFilingResponseUnproven
	}
	switch class {
	case domain.FollowUpFilingResponseSuccess:
		return d.recordPosted(ctx, comment, view, result.Comment.ID, func(tx *store.InternalTx) error {
			_, err := tx.LedgerIssueCommentSuccess(ctx, comment.instanceID, result.Comment.ID, d.now())
			return err
		})
	case domain.FollowUpFilingResponseDefiniteRejection:
		return d.refuse(ctx, comment, domain.FollowUpFilingRefusalDefiniteRejection, issueCommentRefusedCode,
			fmt.Sprintf("GitHub rejected the comment request (HTTP %d)", result.Status), &class)
	case domain.FollowUpFilingResponseTransientRejection:
		if len(intent.Attempts) >= followUpFilingAttemptBound {
			return d.refuse(ctx, comment, domain.FollowUpFilingRefusalRetryBoundSpent, issueCommentRefusedCode,
				commentRetryBoundSpentReason, &class)
		}
		return d.recordResponse(ctx, comment.instanceID, class)
	case domain.FollowUpFilingResponseUnproven:
		return d.recordResponse(ctx, comment.instanceID, class)
	}
	return fmt.Errorf("attempt response class %q is not registered", class)
}

// recheckWorkflows reruns the trust profile's issue_comment workflow check
// against a fresh audit at the default-branch tip. It returns a refusal
// reason for a definite answer (no profile, a profile that does not
// validate, or a set that drifted) and an error for an audit that could not
// complete, which the next pass retries.
func (d *IssueCommentDispatcher) recheckWorkflows(
	ctx context.Context, view commentView, target commentTarget,
) (string, error) {
	var profile domain.AutomationTrustProfile
	if err := d.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		profile, err = tx.LatestTrustProfile(ctx, view.approved.Repo)
		return err
	}); err != nil {
		if ctx.Err() != nil {
			return "", err
		}
		return "the repository's latest trust profile does not read", nil
	}
	// Checked here as well as inside the recheck, so a profile that no longer
	// validates is a definite refusal and not an audit failure to retry.
	if err := profile.Validate(); err != nil {
		return "the repository's latest trust profile does not validate", nil
	}
	// The profile is read by name, so a name now bound to another
	// repository must not answer for the one the comment was approved on.
	if profile.RepositoryID != view.approved.RepositoryID {
		return "the repository's latest trust profile names another repository", nil
	}
	err := RecheckIssueCommentWorkflows(ctx, d.auditor, profile, target.defaultBranch)
	if errors.Is(err, domain.ErrIssueCommentWorkflowsNotApproved) {
		return fmt.Sprintf(
			"the issue_comment workflows on %s are no longer the set the repository's trust profile accepted",
			target.defaultBranch), nil
	}
	return "", err
}

func (d *IssueCommentDispatcher) recordResponse(
	ctx context.Context, id domain.ProposalInstanceID, class domain.FollowUpFilingResponseClass,
) error {
	return d.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordIssueCommentResponse(ctx, id, class, d.now())
		return err
	})
}

// settle resolves an unproven post: it waits the settle interval, lists the
// issue's comments, and adopts the single candidate or records residual
// ambiguity. It never sends another post. When the issue's App is no longer
// the one the post was sent as, no listing can find that post's comment, so
// the comment ends ambiguous without one.
func (d *IssueCommentDispatcher) settle(ctx context.Context, comment approvedComment, view commentView, since time.Time) error {
	intent := *view.intent
	if intent.PreDispatch == nil {
		// The ledger refuses an attempt before the set is recorded, so an
		// intent with attempts always carries one.
		return d.recordAmbiguous(ctx, comment, view, "its ledger entry records no pre-dispatch set")
	}
	var issueAmbiguous bool
	if err := d.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		issueAmbiguous, err = tx.IssueCommentIssueAmbiguous(ctx, intent.RepositoryID, intent.IssueNumber)
		return err
	}); err != nil {
		return err
	}
	if issueAmbiguous {
		return d.recordAmbiguous(ctx, comment, view,
			"an earlier comment on the issue is ambiguous, so no comment there can be confirmed as this one")
	}
	if view.capsErr != nil {
		return view.capsErr
	}
	if d.now().Before(since.Add(view.caps.SettleInterval)) {
		return nil
	}
	target, err := d.resolveTarget(ctx, view)
	var candidates []int64
	if err == nil {
		// A definite answer, not a listing failure: it does not wait out the
		// listing bound.
		if fault := commentIdentityFault(intent.PreDispatch, target.bot); fault != "" {
			delete(d.listingFailures, comment.instanceID)
			return d.recordAmbiguous(ctx, comment, view, fault)
		}
		candidates, err = d.candidates(ctx, intent, target)
	}
	if err != nil {
		d.listingFailures[comment.instanceID]++
		if d.listingFailures[comment.instanceID] < followUpFilingListingBound {
			return err
		}
		delete(d.listingFailures, comment.instanceID)
		return d.recordAmbiguous(ctx, comment, view, fmt.Sprintf(
			"the listing of the issue's comments could not complete in %d tries", followUpFilingListingBound))
	}
	delete(d.listingFailures, comment.instanceID)
	if len(candidates) != 1 {
		return d.recordAmbiguous(ctx, comment, view, fmt.Sprintf(
			"%d comments could be the one this proposal posted", len(candidates)))
	}
	err = d.recordPosted(ctx, comment, view, candidates[0], func(tx *store.InternalTx) error {
		_, err := tx.AdoptIssueCommentCandidate(ctx, comment.instanceID, candidates[0], d.now())
		return err
	})
	if errors.Is(err, domain.ErrIssueCommentAdoptionRefused) || errors.Is(err, domain.ErrIssueCommentLedgered) {
		return d.recordAmbiguous(ctx, comment, view, "the ledger refused the only comment that could be this one")
	}
	return err
}

// candidates lists the comments that could be this intent's post: authored
// by the App's bot account on the target issue, created in the intent's
// window, outside the pre-dispatch set, and not ledgered to another intent.
func (d *IssueCommentDispatcher) candidates(
	ctx context.Context, intent domain.IssueCommentIntent, target commentTarget,
) ([]int64, error) {
	listed, err := d.forge.listConversationComments(ctx, target.repo, intent.IssueNumber)
	if err != nil {
		return nil, err
	}
	// A listing that shifts between pages can return a comment twice, and a
	// repeat is not a second candidate.
	slices.SortFunc(listed, func(a, b replyComment) int { return cmp.Compare(a.ID, b.ID) })
	listed = slices.CompactFunc(listed, func(a, b replyComment) bool { return a.ID == b.ID })
	var ids []int64
	err = d.store.Read(ctx, func(tx *store.ReadTx) error {
		for _, c := range listed {
			if !admitsComment(c, target.bot, intent) {
				continue
			}
			posted, err := tx.PostedIssueComment(ctx, intent.RepositoryID, c.ID)
			if err != nil {
				return err
			}
			if posted == nil {
				ids = append(ids, c.ID)
			}
		}
		return nil
	})
	return ids, err
}

// recordPosted runs the ledger write that records a posted comment. For a
// policy-approved comment it raises the posted notice in the same
// transaction, so the notice exists exactly when the post is recorded and a
// crash cannot leave one without the other. The notice is raised even when
// the run's workflow closed while the post was in flight, because the
// comment is public either way.
func (d *IssueCommentDispatcher) recordPosted(
	ctx context.Context, comment approvedComment, view commentView, commentID int64,
	ledger func(tx *store.InternalTx) error,
) error {
	if !view.approved.ByPolicy {
		return d.store.WriteInternal(ctx, ledger)
	}
	return d.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := ledger(&tx.InternalTx); err != nil {
			return err
		}
		return d.notices.observe(ctx, tx, comment.filingRef, issueCommentPostedCode, fmt.Sprintf(
			"Freeside posted a comment on %s#%d without asking, because gates.issue_comment is off for its run: https://github.com/%s/issues/%d#issuecomment-%d (proposal instance %s).",
			view.approved.Repo, view.approved.IssueNumber,
			view.approved.Repo, view.approved.IssueNumber, commentID, comment.instanceID))
	})
}

// refuse records a terminal refusal and its item in one transaction, so a
// refused comment never exists without the notice that explains it.
// response, when set, is the rejection the dispatched attempt just observed.
func (d *IssueCommentDispatcher) refuse(
	ctx context.Context, comment approvedComment, reason domain.FollowUpFilingRefusalReason,
	code, why string, response *domain.FollowUpFilingResponseClass,
) error {
	return d.store.Write(ctx, func(tx *store.WriteTx) error {
		if response != nil {
			if _, err := tx.RecordIssueCommentResponse(ctx, comment.instanceID, *response, d.now()); err != nil {
				return err
			}
		}
		if _, err := tx.RefuseIssueComment(ctx, comment.instanceID, reason, d.now()); err != nil {
			return err
		}
		return d.notices.put(ctx, tx, comment.filingRef, code, fmt.Sprintf(
			"Issue comment for proposal instance %s was not posted: %s. No comment was posted, and this proposal will not be retried.",
			comment.instanceID, why))
	})
}

// recordAmbiguous records residual ambiguity and raises its item in one
// transaction.
func (d *IssueCommentDispatcher) recordAmbiguous(ctx context.Context, comment approvedComment, view commentView, why string) error {
	return d.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.RecordIssueCommentAmbiguous(ctx, comment.instanceID, d.now()); err != nil {
			return err
		}
		return d.notices.put(ctx, tx, comment.filingRef, issueCommentAmbiguousCode, fmt.Sprintf(
			"Issue comment for proposal instance %s on %s#%d is ambiguous: %s. The comment request may have committed, so no comment is recorded for this proposal and it will not be retried. Later comments on this issue record a comment only from a confirmed create response.",
			comment.instanceID, view.approved.Repo, view.approved.IssueNumber, why))
	})
}

// reportUnreadable raises the notice for a comment whose durable rows no
// longer pass the store's gates, and returns the read error. Nothing more is
// sent for such a comment: every ledger write re-runs the same gates.
func (d *IssueCommentDispatcher) reportUnreadable(ctx context.Context, ref filingRef, cause error) error {
	failed := fmt.Errorf("%w: %q: %w", errIssueCommentUnreadable, ref.instanceID, cause)
	if ref.projectID == "" {
		// A policy-approved comment whose subject does not resolve has no
		// project to raise a notice in; the pass error carries it to the log.
		return failed
	}
	d.notices.hold(ref.instanceID, issueCommentUnreadableCode)
	err := d.store.Write(ctx, func(tx *store.WriteTx) error {
		return d.notices.put(ctx, tx, ref, issueCommentUnreadableCode, fmt.Sprintf(
			"Issue comment for proposal instance %s is stopped: its stored proposal, approval, or ledger rows no longer pass validation, so nothing more is sent to GitHub for it. If a comment request had already been sent, its outcome is not recorded and the comment may exist; check the issue before posting it by hand. If it had begun posting, later comments on the same issue wait until its rows are repaired.",
			ref.instanceID))
	})
	return errors.Join(failed, err)
}
