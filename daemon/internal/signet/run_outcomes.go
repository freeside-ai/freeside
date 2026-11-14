package signet

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RunOutcomes is what a run produced besides its pull request (issue #3149):
// each follow-up issue it filed or proposed, each issue comment it posted or
// proposed, and each finding whose latest disposition set it aside. It is an
// advisory read of records that already exist and decides nothing.
// Complete is false when part of the list could not be read; the parts that
// could are still listed.
type RunOutcomes struct {
	Posts    []RunOutcomePost    `json:"posts"`
	Findings []RunOutcomeFinding `json:"findings"`
	Complete bool                `json:"complete"`
}

// RunOutcomePostState is where one follow-up filing or issue comment stands.
// The daemon never serves the wire's unknown member.
type RunOutcomePostState string

const (
	// RunOutcomePending is nothing final yet: the card is open, or the
	// proposal is approved, on its card or by policy, and the post isn't
	// finished.
	RunOutcomePending RunOutcomePostState = "pending"
	// RunOutcomeCardDeclined is a proposal a person declined on its card.
	RunOutcomeCardDeclined RunOutcomePostState = "card_declined"
	// RunOutcomeWithdrawn is a proposal that will never be sent: no
	// approval and no open card, or no intent when the run's workflow
	// closed, after which the dispatchers open none.
	RunOutcomeWithdrawn RunOutcomePostState = "withdrawn"
	// RunOutcomeCompleted is an issue or comment that exists and is in the
	// ledger.
	RunOutcomeCompleted RunOutcomePostState = "completed"
	// RunOutcomeRefused is an intent that ended refused.
	RunOutcomeRefused RunOutcomePostState = "refused"
	// RunOutcomeCapped is a refusal whose notice carries the capped code.
	RunOutcomeCapped RunOutcomePostState = "capped"
	// RunOutcomeAmbiguous is a create that may have reached GitHub with no
	// issue or comment recorded.
	RunOutcomeAmbiguous RunOutcomePostState = "ambiguous"
	// RunOutcomeUnreadable is an instance the daemon could not read: its
	// stored rows fail the store's checks, or the read itself failed.
	RunOutcomeUnreadable RunOutcomePostState = "unreadable"
)

// AllRunOutcomePostStates registers every post state.
var AllRunOutcomePostStates = []RunOutcomePostState{
	RunOutcomePending, RunOutcomeCardDeclined, RunOutcomeWithdrawn, RunOutcomeCompleted,
	RunOutcomeRefused, RunOutcomeCapped, RunOutcomeAmbiguous, RunOutcomeUnreadable,
}

func (s RunOutcomePostState) valid() bool {
	switch s {
	case RunOutcomePending, RunOutcomeCardDeclined, RunOutcomeWithdrawn, RunOutcomeCompleted,
		RunOutcomeRefused, RunOutcomeCapped, RunOutcomeAmbiguous, RunOutcomeUnreadable:
		return true
	default:
		return false
	}
}

// RunOutcomePost is one follow-up filing or issue comment proposal instance
// of the run. FindingID and Repository are nil only when the instance is
// unreadable. IssueNumber is the filed issue once a filing completes, and
// the target issue of a comment in any readable state. CommentID is set once
// a comment completes. RefusalReason is the ledger's reason for a refused or
// capped intent. Notice is the terminal notice the filer or comment
// dispatcher raised for a refused, capped, or ambiguous outcome, when it
// raised one.
type RunOutcomePost struct {
	EffectKind    domain.EffectKind                   `json:"effect_kind"`
	FindingID     *domain.FindingID                   `json:"finding_id"`
	State         RunOutcomePostState                 `json:"state"`
	Repository    *domain.FollowUpFilingRepository    `json:"repository"`
	IssueNumber   *int                                `json:"issue_number"`
	CommentID     *int64                              `json:"comment_id"`
	RefusalReason *domain.FollowUpFilingRefusalReason `json:"refusal_reason"`
	Notice        *RunOutcomeNotice                   `json:"notice"`
}

// RunOutcomeNotice is the code and text of the notice that explains a
// post's terminal outcome.
type RunOutcomeNotice struct {
	Code   string `json:"code"`
	Reason string `json:"reason"`
}

// RunOutcomeFinding is one finding whose latest disposition is declined or
// deferred. Message and Reason are agent or reviewer text, cut to
// maxRunOutcomeTextBytes.
type RunOutcomeFinding struct {
	FindingID   domain.FindingID         `json:"finding_id"`
	Disposition domain.ReviewDisposition `json:"disposition"`
	Round       int                      `json:"round"`
	External    bool                     `json:"external"`
	Message     string                   `json:"message"`
	Reason      string                   `json:"reason"`
}

// maxRunOutcomeTextBytes bounds each finding's message and reason, the cap
// the pull request body's disposition history applies to the same claims.
const maxRunOutcomeTextBytes = 4 << 10

const runOutcomeTruncatedNote = " (truncated)"

// The notice IDs and codes below repeat the ones daemon/internal/publish
// writes (follow_up_filer.go and issue_comment_dispatcher.go), which are
// private to that package. TestRunOutcomesReadTheDispatchersCappedNotices and
// TestRunOutcomesReadEachRefusedAndAmbiguousNotice in daemon/internal/publish
// drive the real filer and comment dispatcher to each terminal notice and
// fail if these drift from what they write.
const (
	followUpFilingNoticePrefix = "follow-up-filing/"
	issueCommentNoticePrefix   = "issue-comment/"
)

// postNoticeCodes are one effect kind's terminal notice codes.
type postNoticeCodes struct {
	prefix, refused, capped, ambiguous string
}

func noticeCodesFor(kind domain.EffectKind) (postNoticeCodes, error) {
	switch kind {
	case domain.EffectIssueComment:
		return postNoticeCodes{
			prefix: issueCommentNoticePrefix, refused: "issue_comment_refused",
			capped: "issue_comment_capped", ambiguous: "issue_comment_ambiguous",
		}, nil
	case domain.EffectFollowUpFiling:
		return postNoticeCodes{
			prefix: followUpFilingNoticePrefix, refused: "follow_up_filing_refused",
			capped: "follow_up_filing_capped", ambiguous: "follow_up_filing_ambiguous",
		}, nil
	case domain.EffectTaskProposal, domain.EffectSourceIssueClosure, domain.EffectImagePolicyAdoption:
	}
	return postNoticeCodes{}, fmt.Errorf("effect kind %q posts nothing: %w", kind, domain.ErrEffectProposalInconsistent)
}

// ReadRunOutcomes builds the run's outcome list inside tx. A part that
// cannot be read never fails the list: an unreadable instance becomes an
// unreadable entry, an unreadable findings history becomes an empty
// findings list, and either sets Complete to false and adds its error to
// faults for the caller to log. err is set only when ctx is done.
func ReadRunOutcomes(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID,
) (outcomes RunOutcomes, faults []error, err error) {
	outcomes = RunOutcomes{Posts: []RunOutcomePost{}, Findings: []RunOutcomeFinding{}, Complete: true}
	fault := func(err error) {
		outcomes.Complete = false
		faults = append(faults, err)
	}
	handle := domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(runID))
	candidates, err := tx.RunPostCandidates(ctx, handle)
	if err != nil {
		fault(err)
	}
	var repos map[int64]string
	closed := false
	if len(candidates) > 0 {
		// Every filing or comment was admitted against the run's project, so
		// a run with any has a project to read.
		if repos, err = projectRepositoryNames(ctx, tx, runID); err != nil {
			fault(fmt.Errorf("run %q outcome repository names: %w", runID, err))
		}
		// Once the run's workflow closes, the dispatchers open no new intent,
		// so a proposal without one is never sent. An unread closure leaves
		// such posts pending and the list incomplete.
		switch err := tx.RequireRunWorkflowOpen(ctx, runID); {
		case errors.Is(err, store.ErrTaskWorkflowClosed):
			closed = true
		case err != nil:
			fault(fmt.Errorf("run %q outcome workflow closure: %w", runID, err))
		}
	}
	for _, candidate := range candidates {
		post, err := readRunOutcomePost(ctx, tx, candidate, repos, closed)
		if err != nil {
			fault(fmt.Errorf("run %q outcome for proposal instance %q: %w", runID, candidate.InstanceID, err))
			post = RunOutcomePost{EffectKind: candidate.Kind, State: RunOutcomeUnreadable}
		}
		outcomes.Posts = append(outcomes.Posts, post)
	}
	findings, err := readRunOutcomeFindings(ctx, tx, runID)
	if err != nil {
		fault(fmt.Errorf("run %q outcome findings: %w", runID, err))
		findings = []RunOutcomeFinding{}
	}
	outcomes.Findings = findings
	if err := ctx.Err(); err != nil {
		return RunOutcomes{}, nil, err
	}
	return outcomes, faults, nil
}

// projectRepositoryNames maps the run's project's repository ID to its
// current name, so a renamed repository shows its new name as the cards do
// (followUpFilingFacts). When the project cannot be read, a post shows the
// name stored with its proposal.
func projectRepositoryNames(ctx context.Context, tx *store.ReadTx, runID domain.RunID) (map[int64]string, error) {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	project, err := tx.GetProject(ctx, run.ProjectID)
	if err != nil {
		return nil, err
	}
	return map[int64]string{project.RepositoryID: project.Repo}, nil
}

// readRunOutcomePost reads one instance through the store's gated reads and
// decides its state. Any read that fails makes the whole entry unreadable.
func readRunOutcomePost(
	ctx context.Context, tx *store.ReadTx, candidate store.RunPostCandidate, repos map[int64]string, closed bool,
) (RunOutcomePost, error) {
	codes, err := noticeCodesFor(candidate.Kind)
	if err != nil {
		return RunOutcomePost{}, err
	}
	instance, err := tx.GetProposalInstance(ctx, candidate.InstanceID)
	if err != nil {
		return RunOutcomePost{}, err
	}
	post := RunOutcomePost{EffectKind: candidate.Kind}
	var repository domain.FollowUpFilingRepository
	if filing := instance.Proposal.FilingProposal; candidate.Kind == domain.EffectFollowUpFiling && filing != nil {
		repository, post.FindingID = filing.Repository, &filing.Source.FindingID
	} else if comment := instance.Proposal.CommentProposal; candidate.Kind == domain.EffectIssueComment && comment != nil {
		repository, post.FindingID = comment.Repository, &comment.Source.FindingID
		post.IssueNumber = &comment.IssueNumber
	} else {
		return RunOutcomePost{}, fmt.Errorf("instance is not a %q proposal: %w",
			candidate.Kind, domain.ErrEffectProposalInconsistent)
	}
	if name, ok := repos[repository.RepositoryID]; ok {
		repository.Repo = name
	}
	post.Repository = &repository

	terminal, opened, err := readPostIntent(ctx, tx, candidate)
	if err != nil {
		return RunOutcomePost{}, err
	}
	if !opened {
		post.State, err = undispatchedState(ctx, tx, candidate, closed)
		return post, err
	}
	if terminal == nil {
		post.State = RunOutcomePending
		return post, nil
	}
	switch terminal.Outcome {
	case domain.FollowUpFilingLedgered:
		post.State = RunOutcomeCompleted
		return post, readLedgeredPost(ctx, tx, candidate, &post)
	case domain.FollowUpFilingRefused:
		post.State, post.RefusalReason = RunOutcomeRefused, terminal.Reason
		capped, err := readPostNotice(ctx, tx, codes.prefix, candidate.InstanceID, codes.capped)
		if err != nil {
			return RunOutcomePost{}, err
		}
		if capped != nil {
			post.State, post.Notice = RunOutcomeCapped, capped
			return post, nil
		}
		post.Notice, err = readPostNotice(ctx, tx, codes.prefix, candidate.InstanceID, codes.refused)
		return post, err
	case domain.FollowUpFilingAmbiguous:
		post.State = RunOutcomeAmbiguous
		post.Notice, err = readPostNotice(ctx, tx, codes.prefix, candidate.InstanceID, codes.ambiguous)
		return post, err
	}
	return RunOutcomePost{}, fmt.Errorf("intent outcome %q: %w", terminal.Outcome, domain.ErrFollowUpFilingLedgerInconsistent)
}

// readPostIntent reads the instance's intent through its gated getter. opened
// is false when the instance has none.
func readPostIntent(
	ctx context.Context, tx *store.ReadTx, candidate store.RunPostCandidate,
) (terminal *domain.FollowUpFilingTerminal, opened bool, err error) {
	if candidate.Kind == domain.EffectIssueComment {
		intent, err := tx.GetIssueCommentIntent(ctx, candidate.InstanceID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, false, nil
		}
		return intent.Terminal, err == nil, err
	}
	intent, err := tx.GetFollowUpFilingIntent(ctx, candidate.InstanceID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, false, nil
	}
	return intent.Terminal, err == nil, err
}

// undispatchedState is the state of an instance with no intent: declined on
// its card, never to be sent because the run's workflow closed, or waiting
// on its card or its dispatcher.
func undispatchedState(
	ctx context.Context, tx *store.ReadTx, candidate store.RunPostCandidate, closed bool,
) (RunOutcomePostState, error) {
	instanceID := candidate.InstanceID
	decision, err := tx.ProposalDecisionForInstance(ctx, instanceID)
	if err != nil {
		return "", err
	}
	if decision != nil && decision.Action == domain.ActionDecline {
		return RunOutcomeCardDeclined, nil
	}
	if closed {
		return RunOutcomeWithdrawn, nil
	}
	if decision != nil {
		// Approved, and the dispatcher has not opened the intent yet.
		return RunOutcomePending, nil
	}
	if candidate.Kind == domain.EffectIssueComment {
		// A comment whose gate is off has a policy approval in place of a
		// card and a decision.
		approved, err := tx.IssueCommentPolicyApproved(ctx, instanceID)
		if err != nil {
			return "", err
		}
		if approved {
			return RunOutcomePending, nil
		}
	}
	_, _, open, err := tx.OpenEffectItemForInstance(ctx, instanceID)
	if err != nil {
		return "", err
	}
	if open {
		return RunOutcomePending, nil
	}
	return RunOutcomeWithdrawn, nil
}

// readLedgeredPost fills in the issue or comment the ledger recorded.
func readLedgeredPost(
	ctx context.Context, tx *store.ReadTx, candidate store.RunPostCandidate, post *RunOutcomePost,
) error {
	if candidate.Kind == domain.EffectIssueComment {
		posted, err := tx.PostedIssueCommentForInstance(ctx, candidate.InstanceID)
		if err != nil {
			return err
		}
		if posted == nil {
			return fmt.Errorf("ledgered comment has no row: %w", domain.ErrFollowUpFilingLedgerInconsistent)
		}
		post.IssueNumber, post.CommentID = &posted.IssueNumber, &posted.CommentID
		return nil
	}
	filed, err := tx.FiledFollowUpIssueForInstance(ctx, candidate.InstanceID)
	if err != nil {
		return err
	}
	if filed == nil {
		return fmt.Errorf("ledgered filing has no row: %w", domain.ErrFollowUpFilingLedgerInconsistent)
	}
	post.IssueNumber = &filed.IssueNumber
	return nil
}

// readPostNotice reads one terminal notice by its fixed item ID, whatever its
// status, or nil when the dispatcher raised none: it raises nothing once the
// proposal's workflow has closed.
func readPostNotice(
	ctx context.Context, tx *store.ReadTx, prefix string, instanceID domain.ProposalInstanceID, code string,
) (*RunOutcomeNotice, error) {
	item, err := tx.GetAttentionItem(ctx, domain.ItemID(prefix+string(instanceID)+"/"+code))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != code {
		return nil, fmt.Errorf("notice %q carries another code: %w", item.ID, domain.ErrCardFactInconsistent)
	}
	return &RunOutcomeNotice{Code: code, Reason: item.Reason}, nil
}

// readRunOutcomeFindings lists the findings whose latest disposition is
// declined or deferred: the run's own review findings as they stand after
// its latest round, drift reversals applied, then its external findings by
// their latest round.
func readRunOutcomeFindings(ctx context.Context, tx *store.ReadTx, runID domain.RunID) ([]RunOutcomeFinding, error) {
	out := []RunOutcomeFinding{}
	latest, err := tx.LatestReviewRecord(ctx, runID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	default:
		effective, err := tx.EffectiveFindingDispositions(ctx, runID, latest.Round)
		if err != nil {
			return nil, err
		}
		for _, disposition := range effective {
			if entry, ok := setAsideFinding(disposition); ok {
				out = append(out, entry)
			}
		}
	}
	external, err := tx.ListExternalFindingDispositions(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, disposition := range latestExternalDispositions(external) {
		if !setAside(disposition.Disposition) {
			continue
		}
		out = append(out, RunOutcomeFinding{
			FindingID: disposition.FindingID, Disposition: disposition.Disposition,
			Round: disposition.Round, External: true, Reason: disposition.Reason,
		})
	}
	for index := range out {
		finding, err := tx.GetFinding(ctx, out[index].FindingID)
		if err != nil {
			return nil, err
		}
		out[index].Message = boundedRunOutcomeText(finding.Message)
		out[index].Reason = boundedRunOutcomeText(out[index].Reason)
	}
	return out, nil
}

// setAsideFinding is the entry for one of the run's own findings, and false
// when its effective disposition is a fix. A fix a drift audit reversed reads
// as declined, and its stored reason is the fix note; the reversal record
// carries no reason of its own, so the entry says what happened instead.
func setAsideFinding(disposition store.EffectiveFindingDisposition) (RunOutcomeFinding, bool) {
	if !setAside(disposition.Effective) {
		return RunOutcomeFinding{}, false
	}
	entry := RunOutcomeFinding{
		FindingID: disposition.Stored.FindingID, Disposition: disposition.Effective,
		Round: disposition.Stored.Round, Reason: disposition.Stored.Reason,
	}
	if reversal := disposition.Supersession; reversal != nil {
		entry.Round = reversal.ReversingRound
		entry.Reason = fmt.Sprintf("Review round %d found the round %d fix reversed.",
			reversal.ReversingRound, reversal.SupersededRound)
	}
	return entry, true
}

func setAside(disposition domain.ReviewDisposition) bool {
	return disposition == domain.ReviewDispositionDeclined || disposition == domain.ReviewDispositionDeferred
}

// latestExternalDispositions keeps each external finding's highest-round
// disposition, in finding order.
func latestExternalDispositions(
	dispositions []domain.ExternalFindingDisposition,
) []domain.ExternalFindingDisposition {
	latest := map[domain.FindingID]domain.ExternalFindingDisposition{}
	for _, disposition := range dispositions {
		if current, ok := latest[disposition.FindingID]; !ok || disposition.Round > current.Round {
			latest[disposition.FindingID] = disposition
		}
	}
	out := make([]domain.ExternalFindingDisposition, 0, len(latest))
	for _, disposition := range latest {
		out = append(out, disposition)
	}
	slices.SortFunc(out, func(a, b domain.ExternalFindingDisposition) int {
		return cmp.Compare(a.FindingID, b.FindingID)
	})
	return out
}

// boundedRunOutcomeText cuts value to maxRunOutcomeTextBytes at a rune
// boundary, ending with a note that it was cut. The full text stays in the
// store.
func boundedRunOutcomeText(value string) string {
	if len(value) <= maxRunOutcomeTextBytes {
		return value
	}
	limit := maxRunOutcomeTextBytes - len(runOutcomeTruncatedNote)
	for limit > 0 && !utf8.RuneStart(value[limit]) {
		limit--
	}
	return value[:limit] + runOutcomeTruncatedNote
}
