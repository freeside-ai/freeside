package publish

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Follow-up filing dispatch (plan §5.17, issue #1634). The #1626 ledger owns
// the evidence rule: every store write below is one transition it accepts or
// refuses. This file only decides which transition to ask for next, from the
// intent's durable state and what GitHub answers, so a crash at any point
// resumes from the ledger and never from memory.

const (
	// followUpFilingAttemptBound is how many create requests one filing may
	// send. Only a transient rejection permits another, so the bound caps the
	// retries of a rate-limited create.
	followUpFilingAttemptBound = 3
	// followUpFilingRateWindow is the trailing window max_per_day counts over.
	followUpFilingRateWindow = 24 * time.Hour
	// followUpFilingListingBound is how many passes may fail to complete the
	// settle listing before the filing ends ambiguous. The count is held in
	// memory, so a restart grants a fresh bound; it bounds a persistent
	// failure, not the total across restarts.
	followUpFilingListingBound = 3
	// followUpFilingClockSkew is how far before the intent opened an issue's
	// creation time may fall and still be the filing's own. GitHub stamps
	// creation with its clock and the intent opens on the daemon's, so a
	// daemon clock that runs ahead would otherwise put the filing's own issue
	// before its window and end a successful create ambiguous. The allowance
	// admits no older issue: every issue already in the window when the
	// create is sent is in the pre-dispatch set.
	followUpFilingClockSkew = 5 * time.Minute
	// followUpFilingStallNoticeAfter is how long a filing may fail pass after
	// pass before a notice says so. The filing is still retried; the notice
	// only makes a filing that blocks its repository visible outside the log.
	followUpFilingStallNoticeAfter = 15 * time.Minute
	// followUpFilingRetryBackoff is the least wait after a transient
	// rejection before the next create. It reads from the recorded response
	// time, so it holds across restarts and whatever the sweep interval is.
	followUpFilingRetryBackoff = time.Minute
)

// Health diagnostic codes for the filer's system_health items.
const (
	followUpFilingAmbiguousCode  = "follow_up_filing_ambiguous"
	followUpFilingRefusedCode    = "follow_up_filing_refused"
	followUpFilingCappedCode     = "follow_up_filing_capped"
	followUpFilingWaitingCode    = "follow_up_filing_waiting"
	followUpFilingStalledCode    = "follow_up_filing_stalled"
	followUpFilingUnreadableCode = "follow_up_filing_unreadable"
)

// FollowUpFilingIdentitySource resolves the App bot account that authors
// issues in a repository. Candidate validation compares its numeric user ID,
// never issue text.
type FollowUpFilingIdentitySource interface {
	Resolve(ctx context.Context, repo string) (AppBotIdentity, error)
}

// FollowUpFiler files the follow-up issues a human approved, exactly once
// each, and recovers a filing a crash interrupted.
type FollowUpFiler struct {
	store    *store.Store
	forge    *forge
	identity FollowUpFilingIdentitySource
	now      func() time.Time
	wake     chan struct{}

	// mu serializes passes, so a wake during a sweep cannot drive one intent
	// from two goroutines.
	mu              sync.Mutex
	listingFailures map[domain.ProposalInstanceID]int
	// failingSince is when each filing's current run of failed passes began.
	failingSince map[domain.ProposalInstanceID]time.Time
	// holding is the transient notices whose condition the current pass saw
	// still holding; the pass resolves every other open one.
	holding map[filingNotice]bool
}

// filingNotice names one notice kind on one filing.
type filingNotice struct {
	instanceID domain.ProposalInstanceID
	code       string
}

// transientFilingNoticeCodes are the notices that describe a condition a
// filing can leave. The others record a terminal outcome and stay open.
var transientFilingNoticeCodes = []string{
	followUpFilingWaitingCode, followUpFilingStalledCode, followUpFilingUnreadableCode,
}

const followUpFilingItemPrefix = "follow-up-filing/"

// NewFollowUpFiler wires a filer. tokens must mint the filing permission set
// (issues: write, metadata: read); the filer holds no other credential.
func NewFollowUpFiler(
	s *store.Store, tokens TokenSource, client *http.Client, baseURL string,
	identity FollowUpFilingIdentitySource, now func() time.Time,
) (*FollowUpFiler, error) {
	if s == nil || tokens == nil || client == nil || strings.TrimSpace(baseURL) == "" || identity == nil || now == nil {
		return nil, errors.New("follow-up filer: nil or empty dependency")
	}
	return &FollowUpFiler{
		store: s, forge: newForge(tokens, client, strings.TrimRight(baseURL, "/")),
		identity: identity, now: now, wake: make(chan struct{}, 1),
		listingFailures: map[domain.ProposalInstanceID]int{},
		failingSince:    map[domain.ProposalInstanceID]time.Time{},
		holding:         map[filingNotice]bool{},
	}, nil
}

// Wake asks for a pass now. It never blocks: a wake that arrives while one is
// already pending is the same request.
func (f *FollowUpFiler) Wake() {
	if f == nil {
		return
	}
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// Run sweeps once at start, then on every tick and every wake, and returns
// nil when ctx ends. The start sweep is the crash recovery: it finds an
// approved filing with no intent and an intent with no terminal outcome
// alike. report receives each pass's error; a failed pass never stops the
// loop.
func (f *FollowUpFiler) Run(ctx context.Context, interval time.Duration, report func(error)) error {
	if interval <= 0 {
		return errors.New("follow-up filer: sweep interval is not positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := f.Pass(ctx); err != nil && ctx.Err() == nil && report != nil {
			report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		case <-f.wake:
		}
	}
}

// Pass drives every approved filing one step at a time, oldest first. An
// error on one filing is collected and does not stop the pass.
func (f *FollowUpFiler) Pass(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	filings, unreadable, err := f.discover(ctx)
	if err != nil {
		return fmt.Errorf("follow-up filing: %w", err)
	}
	clear(f.holding)
	var errs []error
	for _, ref := range unreadable {
		errs = append(errs, f.reportUnreadable(ctx, ref.filingRef, ref.err))
	}
	for _, filing := range filings {
		if ctx.Err() != nil {
			return errors.Join(append(errs, ctx.Err())...)
		}
		err := f.drive(ctx, filing, filings)
		if err == nil {
			delete(f.failingSince, filing.instanceID)
			continue
		}
		errs = append(errs, fmt.Errorf("follow-up filing %q: %w", filing.instanceID, err))
		if ctx.Err() == nil && !errors.Is(err, errFollowUpFilingUnreadable) {
			errs = append(errs, f.noteStall(ctx, filing.filingRef))
		}
	}
	// Every filing was visited, so holding is complete for this pass unless
	// the context ended inside the last one.
	if ctx.Err() == nil {
		errs = append(errs, f.resolveClearedNotices(ctx))
	}
	return errors.Join(errs...)
}

// errFollowUpFilingUnreadable marks a pass error whose filing already has
// the unreadable notice.
var errFollowUpFilingUnreadable = errors.New("follow-up filing does not read")

// noteStall raises the stall notice once a filing has failed every pass for
// followUpFilingStallNoticeAfter. The run of failures is held in memory, so a
// restart starts it again.
func (f *FollowUpFiler) noteStall(ctx context.Context, ref filingRef) error {
	f.holding[filingNotice{ref.instanceID, followUpFilingStalledCode}] = true
	since, failing := f.failingSince[ref.instanceID]
	if !failing {
		f.failingSince[ref.instanceID] = f.now()
		return nil
	}
	if f.now().Before(since.Add(followUpFilingStallNoticeAfter)) {
		return nil
	}
	return f.store.Write(ctx, func(tx *store.WriteTx) error {
		return f.putHealthItem(ctx, tx, ref, followUpFilingStalledCode, fmt.Sprintf(
			"Follow-up issue filing for proposal instance %s has failed on every attempt for %d minutes and is not finished. It is retried automatically, and later filings in the same repository wait behind it. The daemon log carries the error.",
			ref.instanceID, int(followUpFilingStallNoticeAfter.Minutes())))
	})
}

// filingRef is what a health item needs about one filing: where the
// operator saw the proposal. It comes from the effect_proposal item, which
// reads even when the proposal instance behind it no longer does.
type filingRef struct {
	instanceID domain.ProposalInstanceID
	projectID  domain.ProjectID
	subject    domain.Subject
}

type approvedFiling struct {
	filingRef
	handle       domain.OpaqueSubjectHandle
	repositoryID int64
	createdAt    time.Time
}

type unreadableFiling struct {
	filingRef
	err error
}

// discover lists the filings whose proposal card a human approved. It is a
// pre-filter only: the store re-derives the approval when the intent opens,
// so a card that was not an approved filing opens nothing.
//
// It walks the accepted approve commands and reads each item's record, never
// the gated item listing: that listing fails as a whole when one item's
// proposal no longer passes its gate, which would let one tampered instance
// stop every other filing. The record read trusts none of the item's
// evidence; the instance read behind it is the gate.
func (f *FollowUpFiler) discover(ctx context.Context) ([]approvedFiling, []unreadableFiling, error) {
	var filings []approvedFiling
	var unreadable []unreadableFiling
	err := f.store.Read(ctx, func(tx *store.ReadTx) error {
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
			// A closure card binds a prospective merge and a filing card
			// binds none. The binding read runs no proposal gate, so a
			// closure whose gate now refuses is never mistaken for a filing.
			merge, err := tx.ProspectiveMergeForItem(ctx, item.ID)
			if err != nil {
				unreadable = append(unreadable, unreadableFiling{ref, err})
				continue
			}
			if merge != nil {
				continue
			}
			instance, proposal, err := tx.ProposalForItem(ctx, item.ID)
			if err != nil {
				unreadable = append(unreadable, unreadableFiling{ref, err})
				continue
			}
			if proposal.Kind != domain.EffectFollowUpFiling || proposal.FilingProposal == nil {
				continue
			}
			ref.instanceID = instance.ID
			filings = append(filings, approvedFiling{
				filingRef: ref, handle: proposal.FilingProposal.SubjectHandle,
				repositoryID: proposal.FilingProposal.Repository.RepositoryID,
				createdAt:    instance.CreatedAt,
			})
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	slices.SortFunc(filings, func(a, b approvedFiling) int {
		return cmp.Or(a.createdAt.Compare(b.createdAt), cmp.Compare(a.instanceID, b.instanceID))
	})
	return filings, unreadable, nil
}

// filingView is one filing's durable state, read in one transaction. Every
// read here re-runs the store's gates, so nothing in it is a decoded trust
// bit.
type filingView struct {
	params  domain.FollowUpFilingParameters
	project domain.Project
	runID   domain.RunID
	caps    followUpFilingCaps
	capsErr error
	intent  *domain.FollowUpFilingIntent
}

func (f *FollowUpFiler) view(ctx context.Context, id domain.ProposalInstanceID) (filingView, error) {
	var view filingView
	err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		// The instance gate re-screens the title and body under the ruleset
		// the proposal records, so this read is the screen re-run before
		// every create.
		instance, err := tx.GetProposalInstance(ctx, id)
		if err != nil {
			return err
		}
		if instance.Proposal.Kind != domain.EffectFollowUpFiling || instance.Proposal.FilingProposal == nil {
			return fmt.Errorf("proposal instance is not a follow-up filing: %w", domain.ErrFollowUpFilingNotApproved)
		}
		view.params = *instance.Proposal.FilingProposal
		declaration, policy, err := tx.ResolveProposalSubject(ctx, view.params.SubjectHandle)
		if err != nil {
			return err
		}
		view.runID = declaration.RunID
		view.project, err = tx.GetProject(ctx, declaration.ProjectID)
		if err != nil {
			return err
		}
		view.caps, view.capsErr = parseFollowUpFilingCaps(policy)
		intent, err := tx.GetFollowUpFilingIntent(ctx, id)
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

// drive advances one filing as far as its durable state and GitHub's answers
// allow in this pass.
func (f *FollowUpFiler) drive(ctx context.Context, filing approvedFiling, all []approvedFiling) error {
	view, err := f.view(ctx, filing.instanceID)
	if err != nil {
		return f.reportUnreadable(ctx, filing.filingRef, err)
	}
	if view.intent == nil {
		intent, opened, err := f.open(ctx, filing, view, all)
		if err != nil || !opened {
			// Still without an intent, so a waiting notice still describes it.
			f.holding[filingNotice{filing.instanceID, followUpFilingWaitingCode}] = true
			return err
		}
		view.intent = &intent
	}
	intent := *view.intent
	if intent.Terminal != nil {
		return nil
	}
	if len(intent.Attempts) == 0 {
		return f.dispatch(ctx, filing, view, all)
	}
	// An attempt whose response was never recorded was dispatched: the crash
	// fell between the marker and the response, so its create is unproven.
	last := intent.Attempts[len(intent.Attempts)-1]
	class, at := domain.FollowUpFilingResponseUnproven, last.DispatchStartedAt
	if last.Response != nil {
		class, at = last.Response.Class, last.Response.RecordedAt
	}
	switch class {
	case domain.FollowUpFilingResponseSuccess, domain.FollowUpFilingResponseUnproven:
		// A recorded success always carries its ledgered outcome, so an
		// outstanding intent never shows one; if it did, settling is the
		// reading that creates nothing.
		return f.settle(ctx, filing, view, at)
	case domain.FollowUpFilingResponseDefiniteRejection:
		// The crash fell between the response and its refusal.
		return f.refuse(ctx, filing, domain.FollowUpFilingRefusalDefiniteRejection, followUpFilingRefusedCode,
			"GitHub rejected the create request", nil)
	case domain.FollowUpFilingResponseTransientRejection:
		if len(intent.Attempts) >= followUpFilingAttemptBound {
			return f.refuse(ctx, filing, domain.FollowUpFilingRefusalRetryBoundSpent, followUpFilingRefusedCode,
				retryBoundSpentReason, nil)
		}
		if f.now().Before(at.Add(followUpFilingRetryBackoff)) {
			return nil
		}
		return f.dispatch(ctx, filing, view, all)
	}
	return fmt.Errorf("attempt response class %q is not registered", class)
}

var retryBoundSpentReason = fmt.Sprintf(
	"GitHub rate-limited the create request %d times", followUpFilingAttemptBound)

// open opens the creation intent unless the repository's rate cap holds the
// filing or another filing there is still outstanding. Neither is an error:
// the next pass asks again.
func (f *FollowUpFiler) open(
	ctx context.Context, filing approvedFiling, view filingView, all []approvedFiling,
) (domain.FollowUpFilingIntent, bool, error) {
	// A malformed cap cannot hold a filing open-ended; the intent opens so
	// the dispatch step can refuse it with a terminal outcome.
	if view.capsErr == nil {
		recent, err := f.countFilings(ctx, filing, all,
			func(other approvedFiling) bool { return other.repositoryID == filing.repositoryID },
			func(intent domain.FollowUpFilingIntent) bool {
				return lastCreateSentAt(intent).After(f.now().Add(-followUpFilingRateWindow))
			})
		if err != nil {
			return domain.FollowUpFilingIntent{}, false, err
		}
		if recent >= view.caps.MaxPerDay {
			return domain.FollowUpFilingIntent{}, false, f.store.Write(ctx, func(tx *store.WriteTx) error {
				return f.putHealthItem(ctx, tx, filing.filingRef, followUpFilingWaitingCode, fmt.Sprintf(
					"Follow-up issue filing for proposal instance %s in %s is waiting: %d filings there in the last 24 hours reach the %s cap of %d. It files when the window frees.",
					filing.instanceID, view.project.Repo, recent, policyFollowUpFilingMaxPerDay, view.caps.MaxPerDay))
			})
		}
	}
	var intent domain.FollowUpFilingIntent
	err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		intent, err = tx.OpenFollowUpFilingIntent(ctx, filing.instanceID, f.now())
		return err
	})
	switch {
	case errors.Is(err, domain.ErrFollowUpFilingNotApproved), errors.Is(err, domain.ErrFollowUpFilingRepositoryBusy):
		return domain.FollowUpFilingIntent{}, false, nil
	case err != nil:
		return domain.FollowUpFilingIntent{}, false, err
	}
	return intent, true, nil
}

// lastCreateSentAt is the latest time a create under this intent can have
// reached GitHub. The rate window reads it and not the intent's opening time:
// an intent can wait open through an outage longer than the window, and its
// create still counts from when it was sent.
func lastCreateSentAt(intent domain.FollowUpFilingIntent) time.Time {
	if len(intent.Attempts) == 0 {
		return intent.OpenedAt
	}
	return intent.Attempts[len(intent.Attempts)-1].DispatchStartedAt
}

// countFilings counts the other discovered filings in scope whose intent may
// have created an issue and that match. An intent in scope that does not read
// fails the count: a cap that cannot be evaluated does not pass. Filings out
// of scope are never read, so one that does not read holds back only the
// repository or run it belongs to.
func (f *FollowUpFiler) countFilings(
	ctx context.Context, self approvedFiling, all []approvedFiling,
	inScope func(approvedFiling) bool, match func(domain.FollowUpFilingIntent) bool,
) (int, error) {
	count := 0
	err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		for _, other := range all {
			if other.instanceID == self.instanceID || !inScope(other) {
				continue
			}
			intent, err := tx.GetFollowUpFilingIntent(ctx, other.instanceID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if intent.MayHaveCreated() && match(intent) {
				count++
			}
		}
		return nil
	})
	return count, err
}

// filingDepth is the new issue's depth in Freeside-filed ancestry: one more
// than the number of ledgered follow-up issues above the origin run's bound
// issue. The walk stops once the depth passes limit, which also bounds a
// cycle the ledger should never hold.
func (f *FollowUpFiler) filingDepth(ctx context.Context, runID domain.RunID, limit int) (int, error) {
	depth := 1
	err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		for depth <= limit {
			declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, runID)
			if err != nil {
				return err
			}
			if declaration.BoundIssue == nil {
				return nil
			}
			project, err := tx.GetProject(ctx, declaration.ProjectID)
			if err != nil {
				return err
			}
			filed, err := tx.FiledFollowUpIssue(ctx, project.RepositoryID, *declaration.BoundIssue)
			if err != nil {
				return err
			}
			if filed == nil {
				return nil
			}
			depth++
			runID = filed.Origin.RunID
		}
		return nil
	})
	return depth, err
}

// filingTarget is the live GitHub state one create or listing needs, each
// field read through the filing token and checked against the stored target.
type filingTarget struct {
	repo      repoRef
	bot       AppBotIdentity
	milestone *int
}

// filingPreconditionError marks a precondition failure no retry changes. Its
// message is operator-facing and carries no forge response content.
type filingPreconditionError struct{ reason string }

func (e *filingPreconditionError) Error() string { return e.reason }

// resolveTarget reads the live target. A definite failure returns
// *filingPreconditionError; anything else is a transient read failure.
func (f *FollowUpFiler) resolveTarget(ctx context.Context, view filingView, withMilestone bool) (filingTarget, error) {
	repo, err := parseRepo(view.project.Repo)
	if err != nil {
		return filingTarget{}, &filingPreconditionError{"the project names no owner/name repository"}
	}
	definite := func(err error) error {
		var api *APIError
		switch {
		case errors.Is(err, ErrGrantMismatch),
			errors.As(err, &api) && api.Status == http.StatusUnprocessableEntity &&
				strings.HasSuffix(api.RequestPath, "/access_tokens"):
			return &filingPreconditionError{
				"the GitHub App installation does not grant the issues: write permission filing needs",
			}
		case errors.Is(err, errMilestoneUnresolved):
			return &filingPreconditionError{"the target milestone title does not name exactly one milestone in the repository"}
		case errors.As(err, &api) && (api.Status == http.StatusNotFound || api.Status == http.StatusGone):
			return &filingPreconditionError{"the target repository is gone or no longer visible to the GitHub App"}
		}
		return err
	}
	target := filingTarget{repo: repo}
	repositoryID, err := f.forge.getRepositoryID(ctx, repo)
	if err != nil {
		return filingTarget{}, definite(err)
	}
	// A name rebound to another repository is detectable only by ID.
	if repositoryID != view.params.Repository.RepositoryID {
		return filingTarget{}, &filingPreconditionError{"the repository name now resolves to a different repository"}
	}
	target.bot, err = f.identity.Resolve(ctx, repo.path())
	if err != nil {
		return filingTarget{}, definite(err)
	}
	if target.bot.BotUserID <= 0 || target.bot.AppSlug == "" {
		return filingTarget{}, errors.New("resolve App bot identity: incomplete identity")
	}
	if withMilestone && view.params.Milestone != nil {
		number, err := f.forge.getMilestoneNumber(ctx, repo, *view.params.Milestone)
		if err != nil {
			return filingTarget{}, definite(err)
		}
		target.milestone = &number
	}
	return target, nil
}

// admits reports whether an issue can be this intent's create. It reads the
// author's numeric identity, the creation time, and the pre-dispatch set,
// never issue text.
func admits(issue filedIssue, bot AppBotIdentity, intent domain.FollowUpFilingIntent) bool {
	return !issue.PullRequest && authoredBy(issue, bot) &&
		!issue.CreatedAt.Before(candidateWindowStart(intent)) &&
		(intent.PreDispatch == nil || !slices.Contains(intent.PreDispatch.IssueNumbers, issue.Number))
}

func authoredBy(issue filedIssue, bot AppBotIdentity) bool {
	return issue.AuthorID == bot.BotUserID && issue.AuthorType == "Bot"
}

// candidateWindowStart is the earliest creation time, on GitHub's clock, an
// issue this intent created can carry.
func candidateWindowStart(intent domain.FollowUpFilingIntent) time.Time {
	return intent.OpenedAt.Add(-followUpFilingClockSkew)
}

// dispatchIdentityFault says why bot cannot stand for the intent whose
// pre-dispatch set is given, or "" when the set was listed under bot. The set
// lists one App's issues and every create is sent as that App, so each step
// that sends a create or adopts a candidate holds the repository's current
// identity to the recorded one (issue #1777). The App registered for a
// repository can change between steps; without this check a settle after the
// change would list the new App's issues against the old App's set and adopt
// an issue this filing never created.
//
// The comparison is by bot user ID, so the same App under new credentials is
// the same identity.
func dispatchIdentityFault(set *domain.FollowUpFilingPreDispatchSet, bot AppBotIdentity) string {
	if fault := dispatchIdentityMissing(set); fault != "" {
		return fault
	}
	if *set.BotUserID != bot.BotUserID {
		return fmt.Sprintf(
			"the GitHub App that listed the repository's issues before dispatch (bot user ID %d) is not the one registered for it now (bot user ID %d)",
			*set.BotUserID, bot.BotUserID)
	}
	return ""
}

// dispatchIdentityMissing is the fault of a set that names no dispatching
// identity, or "" when it names one. Such a set predates the identity's
// recording (migration 0093) and matches no App, so the filing's outcome is
// fixed by the ledger alone. A step asks this before anything else: a
// repository or App that cannot be read would otherwise hold a filing whose
// outcome no answer can change, and hold the repository's later filings
// behind it.
func dispatchIdentityMissing(set *domain.FollowUpFilingPreDispatchSet) string {
	if set == nil || set.BotUserID == nil {
		return "its ledger entry does not record which GitHub App listed the repository's issues before dispatch"
	}
	return ""
}

// listBotIssues lists what the App's bot account created in the intent's
// window, one entry per issue number: a listing that shifts between pages can
// return an issue twice, and a repeat is not a second candidate.
func (f *FollowUpFiler) listBotIssues(
	ctx context.Context, target filingTarget, intent domain.FollowUpFilingIntent,
) ([]filedIssue, error) {
	issues, err := f.forge.listIssuesCreatedBy(ctx, target.repo, target.bot.AppSlug+"[bot]", candidateWindowStart(intent))
	if err != nil {
		return nil, err
	}
	slices.SortFunc(issues, func(a, b filedIssue) int { return cmp.Compare(a.Number, b.Number) })
	return slices.CompactFunc(issues, func(a, b filedIssue) bool { return a.Number == b.Number }), nil
}

// dispatch checks the preconditions, records the pre-dispatch set if none is
// recorded, and sends one create request. A set already recorded was listed
// under one App, and a create under any other is refused: nothing was sent
// under the new one, and the set says nothing about its issues.
func (f *FollowUpFiler) dispatch(ctx context.Context, filing approvedFiling, view filingView, all []approvedFiling) error {
	intent := *view.intent
	refusePrecondition := func(code, reason string) error {
		return f.refuse(ctx, filing, domain.FollowUpFilingRefusalPreconditionFailed, code, reason, nil)
	}
	// A set recorded without an identity is refused first, on the ledger
	// alone; a filing with no set yet records both below.
	if intent.PreDispatch != nil {
		if fault := dispatchIdentityMissing(intent.PreDispatch); fault != "" {
			return refusePrecondition(followUpFilingRefusedCode, fault)
		}
	}
	if view.capsErr != nil {
		return refusePrecondition(followUpFilingRefusedCode, "its policy carries a malformed cap ("+view.capsErr.Error()+")")
	}
	depth, err := f.filingDepth(ctx, view.runID, view.caps.MaxDepth)
	if err != nil {
		return err
	}
	if depth > view.caps.MaxDepth {
		return refusePrecondition(followUpFilingCappedCode, fmt.Sprintf(
			"the new issue would sit deeper in Freeside-filed ancestry than the %s cap of %d",
			policyFollowUpFilingMaxDepth, view.caps.MaxDepth))
	}
	perRun, err := f.countFilings(ctx, filing, all,
		func(other approvedFiling) bool { return other.handle == filing.handle },
		func(domain.FollowUpFilingIntent) bool { return true })
	if err != nil {
		return err
	}
	if perRun >= view.caps.MaxPerRun {
		return refusePrecondition(followUpFilingCappedCode, fmt.Sprintf(
			"its run already has %d filings, the %s cap of %d", perRun, policyFollowUpFilingMaxPerRun, view.caps.MaxPerRun))
	}
	target, err := f.resolveTarget(ctx, view, true)
	var precondition *filingPreconditionError
	if errors.As(err, &precondition) {
		return refusePrecondition(followUpFilingRefusedCode, precondition.reason)
	}
	if err != nil {
		return err
	}
	if intent.PreDispatch == nil {
		issues, err := f.listBotIssues(ctx, target, intent)
		if err != nil {
			return err
		}
		var numbers []int
		for _, issue := range issues {
			if !issue.PullRequest && authoredBy(issue, target.bot) {
				numbers = append(numbers, issue.Number)
			}
		}
		if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			var err error
			intent, err = tx.RecordFollowUpFilingPreDispatch(
				ctx, filing.instanceID, numbers, target.bot.BotUserID, f.now())
			return err
		}); err != nil {
			return err
		}
	}
	// Build the request, token included, before the dispatch marker: once the
	// marker is durable the only thing left that can fail is the send, and a
	// failed send is unproven by rule.
	request, err := f.forge.newCreateIssueRequest(
		ctx, target.repo, view.params.Title.Text, view.params.Body.Text, view.params.Labels, target.milestone)
	if err != nil {
		return err
	}
	// The identity is read after the request holds its token, not taken from
	// the target: the token is minted separately, so a registration that
	// changed while the target was resolved or the set was listed would
	// otherwise send this create as the new App.
	target.bot, err = f.identity.Resolve(ctx, target.repo.path())
	if err != nil {
		return err
	}
	if fault := dispatchIdentityFault(intent.PreDispatch, target.bot); fault != "" {
		return refusePrecondition(followUpFilingRefusedCode, fault)
	}
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		intent, err = tx.StartFollowUpFilingAttempt(ctx, filing.instanceID, f.now())
		return err
	}); err != nil {
		return err
	}
	result, sendErr := f.forge.sendCreateIssue(request)
	class := classifyFollowUpFilingCreate(result, sendErr)
	// Returned-object boundary: a 201 is ledgered only when its issue would
	// also pass as a candidate. One that would not is unproven, and the
	// settled listing decides.
	if class == domain.FollowUpFilingResponseSuccess && !admits(*result.Issue, target.bot, intent) {
		class = domain.FollowUpFilingResponseUnproven
	}
	switch class {
	case domain.FollowUpFilingResponseSuccess:
		return f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.LedgerFollowUpFilingSuccess(ctx, filing.instanceID, result.Issue.Number, f.now())
			return err
		})
	case domain.FollowUpFilingResponseDefiniteRejection:
		return f.refuse(ctx, filing, domain.FollowUpFilingRefusalDefiniteRejection, followUpFilingRefusedCode,
			fmt.Sprintf("GitHub rejected the create request (HTTP %d)", result.Status), &class)
	case domain.FollowUpFilingResponseTransientRejection:
		if len(intent.Attempts) >= followUpFilingAttemptBound {
			return f.refuse(ctx, filing, domain.FollowUpFilingRefusalRetryBoundSpent, followUpFilingRefusedCode,
				retryBoundSpentReason, &class)
		}
		return f.recordResponse(ctx, filing.instanceID, class)
	case domain.FollowUpFilingResponseUnproven:
		return f.recordResponse(ctx, filing.instanceID, class)
	}
	return fmt.Errorf("attempt response class %q is not registered", class)
}

func (f *FollowUpFiler) recordResponse(
	ctx context.Context, id domain.ProposalInstanceID, class domain.FollowUpFilingResponseClass,
) error {
	return f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordFollowUpFilingResponse(ctx, id, class, f.now())
		return err
	})
}

// settle resolves an unproven create: it waits the settle interval, lists
// the App's issues, and adopts the single candidate or records residual
// ambiguity. It never sends another create. When the ledger does not name
// the App the create was sent as, or the repository's App is no longer that
// one, no listing can find that create's issue, so the filing ends ambiguous
// without one.
func (f *FollowUpFiler) settle(ctx context.Context, filing approvedFiling, view filingView, since time.Time) error {
	intent := *view.intent
	// No listing can be held to an identity the ledger does not name, so this
	// outcome waits on neither the settle interval nor the live target.
	if fault := dispatchIdentityMissing(intent.PreDispatch); fault != "" {
		return f.recordAmbiguous(ctx, filing, view, fault)
	}
	var repositoryAmbiguous bool
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		repositoryAmbiguous, err = tx.FollowUpFilingRepositoryAmbiguous(ctx, intent.RepositoryID)
		return err
	}); err != nil {
		return err
	}
	if repositoryAmbiguous {
		return f.recordAmbiguous(ctx, filing, view,
			"an earlier filing in the repository is ambiguous, so no issue there can be confirmed as this one")
	}
	if view.capsErr != nil {
		return view.capsErr
	}
	if f.now().Before(since.Add(view.caps.SettleInterval)) {
		return nil
	}
	target, err := f.resolveTarget(ctx, view, false)
	var candidates []int
	if err == nil {
		// A definite answer, not a listing failure: it does not wait out the
		// listing bound.
		if fault := dispatchIdentityFault(intent.PreDispatch, target.bot); fault != "" {
			delete(f.listingFailures, filing.instanceID)
			return f.recordAmbiguous(ctx, filing, view, fault)
		}
		candidates, err = f.candidates(ctx, view, target)
	}
	if err != nil {
		f.listingFailures[filing.instanceID]++
		if f.listingFailures[filing.instanceID] < followUpFilingListingBound {
			return err
		}
		delete(f.listingFailures, filing.instanceID)
		return f.recordAmbiguous(ctx, filing, view, fmt.Sprintf(
			"the listing of the App's issues could not complete in %d tries", followUpFilingListingBound))
	}
	delete(f.listingFailures, filing.instanceID)
	if len(candidates) != 1 {
		return f.recordAmbiguous(ctx, filing, view, fmt.Sprintf(
			"%d issues could be the one this filing created", len(candidates)))
	}
	err = f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.AdoptFollowUpFilingCandidate(ctx, filing.instanceID, candidates[0], f.now())
		return err
	})
	if errors.Is(err, domain.ErrFollowUpFilingAdoptionRefused) || errors.Is(err, domain.ErrFollowUpFilingIssueLedgered) {
		return f.recordAmbiguous(ctx, filing, view, "the ledger refused the only issue that could be this filing's")
	}
	return err
}

// candidates lists the issues that could be this intent's create: authored
// by the App's bot account in the target repository, created in the intent's
// window, outside the pre-dispatch set, and not ledgered to another filing.
// target is the live target, which the caller has held to the intent's
// dispatching identity.
func (f *FollowUpFiler) candidates(ctx context.Context, view filingView, target filingTarget) ([]int, error) {
	intent := *view.intent
	issues, err := f.listBotIssues(ctx, target, intent)
	if err != nil {
		return nil, err
	}
	var numbers []int
	err = f.store.Read(ctx, func(tx *store.ReadTx) error {
		for _, issue := range issues {
			if !admits(issue, target.bot, intent) {
				continue
			}
			filed, err := tx.FiledFollowUpIssue(ctx, intent.RepositoryID, issue.Number)
			if err != nil {
				return err
			}
			if filed == nil {
				numbers = append(numbers, issue.Number)
			}
		}
		return nil
	})
	return numbers, err
}

// refuse records a terminal refusal and its item in one transaction, so a
// refused filing never exists without the notice that explains it. response,
// when set, is the rejection the dispatched attempt just observed.
func (f *FollowUpFiler) refuse(
	ctx context.Context, filing approvedFiling, reason domain.FollowUpFilingRefusalReason,
	code, why string, response *domain.FollowUpFilingResponseClass,
) error {
	return f.store.Write(ctx, func(tx *store.WriteTx) error {
		if response != nil {
			if _, err := tx.RecordFollowUpFilingResponse(ctx, filing.instanceID, *response, f.now()); err != nil {
				return err
			}
		}
		if _, err := tx.RefuseFollowUpFiling(ctx, filing.instanceID, reason, f.now()); err != nil {
			return err
		}
		return f.putHealthItem(ctx, tx, filing.filingRef, code, fmt.Sprintf(
			"Follow-up issue for proposal instance %s was not filed: %s. No issue was created, and this proposal will not be retried.",
			filing.instanceID, why))
	})
}

// recordAmbiguous records residual ambiguity and raises its item in one
// transaction (plan §5.17: the outcome and the item are one step).
func (f *FollowUpFiler) recordAmbiguous(ctx context.Context, filing approvedFiling, view filingView, why string) error {
	return f.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.RecordFollowUpFilingAmbiguous(ctx, filing.instanceID, f.now()); err != nil {
			return err
		}
		return f.putHealthItem(ctx, tx, filing.filingRef, followUpFilingAmbiguousCode, fmt.Sprintf(
			"Follow-up issue filing for proposal instance %s in repository %s (ID %d) is ambiguous: %s. The create request may have committed, so no issue is recorded for this proposal and it will not be retried. Later filings in this repository record an issue only from a confirmed create response.",
			filing.instanceID, view.project.Repo, view.intent.RepositoryID, why))
	})
}

// reportUnreadable raises the notice for a filing whose durable rows no
// longer pass the store's gates, and returns the read error. Nothing more is
// sent for such a filing: every ledger write re-runs the same gates. The
// notice does not say whether a create was sent before, because the intent
// that would say so is behind the same gates and does not read.
func (f *FollowUpFiler) reportUnreadable(ctx context.Context, ref filingRef, cause error) error {
	f.holding[filingNotice{ref.instanceID, followUpFilingUnreadableCode}] = true
	err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return f.putHealthItem(ctx, tx, ref, followUpFilingUnreadableCode, fmt.Sprintf(
			"Follow-up issue filing for proposal instance %s is stopped: its stored proposal, approval, or ledger rows no longer pass validation, so nothing more is sent to GitHub for it. If a create request had already been sent, its outcome is not recorded and the issue may exist; check the repository before filing it by hand. If it had begun filing, later filings in the same repository wait until its rows are repaired.",
			ref.instanceID))
	})
	return errors.Join(fmt.Errorf("%w: %q: %w", errFollowUpFilingUnreadable, ref.instanceID, cause), err)
}

// putHealthItem opens one advisory, acknowledge-only system_health item on
// the proposal's own subject. Its ID is fixed by the filing and the code, so
// raising a notice that already exists writes nothing. A transient notice
// that was resolved is final, so the condition's return opens the next
// numbered one.
func (f *FollowUpFiler) putHealthItem(
	ctx context.Context, tx *store.WriteTx, ref filingRef, code, reason string,
) error {
	first := followUpFilingItemPrefix + string(ref.instanceID) + "/" + code
	id := domain.ItemID(first)
	for occurrence := 2; ; occurrence++ {
		existing, err := tx.GetAttentionItem(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			break
		}
		if err != nil {
			return err
		}
		if existing.Status == domain.StatusOpen || !slices.Contains(transientFilingNoticeCodes, code) {
			return nil
		}
		id = domain.ItemID(first + "/" + strconv.Itoa(occurrence))
	}
	names, err := tx.DisplayNamesFor(ctx, ref.projectID, ref.subject)
	if err != nil {
		return err
	}
	createdAt := f.now().UTC()
	advisory := domain.HealthPostureAdvisory
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: ref.projectID, Subject: ref.subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason:            reason,
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		// Advisory: publication and review continue; only this filing is affected.
		HealthDiagnostic: &domain.HealthDiagnostic{Code: code, Impairs: domain.ImpairedCapabilityNone},
		ItemVersion:      1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt, DisplayNames: names, Status: domain.StatusOpen, Posture: &advisory,
	}, nil)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}

// resolveClearedNotices resolves every open transient notice whose condition
// this pass did not see: the filing opened its intent, stopped failing, reads
// again, or finished. It is the only resolver because each condition ends in
// several places. The read comes first so the ordinary pass, with no notice
// to resolve, opens no write transaction.
func (f *FollowUpFiler) resolveClearedNotices(ctx context.Context) error {
	var cleared []domain.AttentionItem
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		cleared, err = f.clearedNotices(ctx, tx)
		return err
	}); err != nil || len(cleared) == 0 {
		return err
	}
	return f.store.Write(ctx, func(tx *store.WriteTx) error {
		// Recomputed inside the write, so each resolve advances the item's
		// current version, not the one the read saw.
		cleared, err := f.clearedNotices(ctx, &tx.ReadTx)
		if err != nil {
			return err
		}
		for _, item := range cleared {
			item.ItemVersion++
			item.Status = domain.StatusResolved
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	})
}

func (f *FollowUpFiler) clearedNotices(ctx context.Context, tx *store.ReadTx) ([]domain.AttentionItem, error) {
	items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return nil, err
	}
	var cleared []domain.AttentionItem
	for _, item := range items {
		if notice, ok := transientFilingNotice(item); ok && !f.holding[notice] {
			cleared = append(cleared, item)
		}
	}
	return cleared, nil
}

// transientFilingNotice reads the filing and code out of a transient notice
// this filer raised; ok is false for any other item.
func transientFilingNotice(item domain.AttentionItem) (filingNotice, bool) {
	rest, ok := strings.CutPrefix(string(item.ID), followUpFilingItemPrefix)
	if !ok {
		return filingNotice{}, false
	}
	for _, code := range transientFilingNoticeCodes {
		// The code is followed by nothing or by an occurrence number.
		if at := strings.LastIndex(rest, "/"+code); at >= 0 {
			return filingNotice{domain.ProposalInstanceID(rest[:at]), code}, true
		}
	}
	return filingNotice{}, false
}
