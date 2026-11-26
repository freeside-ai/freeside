package daemoncompose

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/keyedrun"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Active-resource reconciliation is a process cadence, not a durable
// schedule kind (plan §5.16). An immediate startup pass restores convergence;
// later passes use conditional requests through publish.Reconciler.
const (
	DefaultActiveResourceInterval  = 15 * time.Minute
	OperatorActiveResourceInterval = time.Minute
)

const activeResourceObservationFailureThreshold = 4

type ActiveResourceReconciler struct {
	Store *store.Store
	Pull  pullObserver
	issue issueObserver
	// Review observes native (forge-hosted) review activity on the exact ready
	// PR; reviewers is the configured set of reviewer logins whose activity is
	// recorded. Both are optional: a composition that wires no native reviewer
	// (or an empty set) simply records no native evidence (plan §5.16, §7).
	Review    NativeReviewObserver
	Reviewers map[string]bool
	// appBotIdentity names the App bot per repository, so the bot's own
	// conversation comments, which are its disposition replies, are never read
	// as review (#3127). Optional: without it no conversation comment becomes
	// an external finding.
	appBotIdentity appBotIdentitySource
	// handBackLabels names, per project, the label the landing batch puts on
	// a pull request it hands back for rework (issue #3160), read in the
	// observation's transaction from the run's project (#3209). Nil, or a
	// project that names none, turns hand-back intake off. The label is a
	// trigger, not an authority: the store re-proves the cycle from its own
	// records when it seals one.
	handBackLabels publish.HandBackLabelLookup
	// mergeBase reads a head's merge base with the target branch, the base a
	// hand-back cycle reviews against. Required when handBackLabels is set.
	mergeBase func(ctx context.Context, repo, baseRef, head string) (string, error)
	// readinessGate is the project readiness gate the three re-entry cycles
	// apply before they start (#3205); nil checks nothing. A refused start
	// writes nothing, so the next pass offers it again.
	readinessGate engine.ProjectReadinessGate
	// reviewInvalidate drops the observer's cached validators for a PR when the
	// durable append of its native observations fails, so the next tick
	// re-fetches unconditionally instead of riding a 304 and permanently
	// dropping the un-persisted rows (issue #497). Optional: nil disables it,
	// which only forfeits the prompt retry a cache-eviction buys.
	reviewInvalidate func(repo string, number int)
	// observeCheck reads the required check on a bound, open pull request's
	// head (#3598). Optional: nil reads nothing and the card shows no check.
	observeCheck requiredCheckObserver
	// readJobLog and excerpts read a failed required check's job logs and
	// keep their screened excerpts, for the check_failure cycle (#3598).
	// Optional: nil turns the cycle's trigger off.
	readJobLog jobLogReader
	excerpts   engine.ArtifactStore
	// evictConcluded drops every conditional-request cache entry owned by a
	// ready resource after the item leaves the open state.
	evictConcluded func(domain.ReadyItemPRBinding, *int)
	// The process-local maps below are a forProject copy's view of its
	// project's activeResourceProjectState, which documents them.
	evictedConcluded         map[domain.ItemID]bool
	observationFailures      map[domain.ItemID]int
	handBackGapSeen          map[domain.ItemID]string
	reactionsRefusalReported map[domain.ItemID]bool
	// projects holds each project's process-local state and runs each
	// project's items and each repository's tip watch apart. Reconcile
	// creates it when it is nil; Run creates it before its passes start.
	projects *activeResourceProjects
	// tipWatches name the branches whose tips each pass reads whether or not
	// a bound PR moved them, one per served repository, so a merge made
	// outside Freeside still reaches that repository's project-image upkeep
	// (plan §5.7, Project-image upkeep, Triggers). observeTip reads a tip;
	// upkeepTipAdvance is the tip-advance entry point, which runs inside the
	// pass and so must hand the tip off and return, not do upkeep's work. No
	// watches watch nothing; any watch needs both functions.
	tipWatches       []repositoryTipWatch
	observeTip       func(ctx context.Context, repo, branch string) (string, error)
	upkeepTipAdvance func(context.Context, domain.RepositoryTipObservation) error
	Now              func() time.Time
}

// repositoryTipWatch is a served repository and the branch tasks run
// against, which is the tip a project image must stay admissible at.
type repositoryTipWatch struct {
	Repo         string
	RepositoryID int64
	Branch       string
}

// activeResourceProjects runs each project's items on its own goroutine and
// each repository's tip watch on its own, so one project's stalled GitHub
// call holds up only that project's pull requests and never another
// project's or repository's (#3210). A project or repository whose previous
// run has not returned is skipped by the next pass, which never waits on it.
type activeResourceProjects struct {
	mu    sync.Mutex
	state map[domain.ProjectID]*activeResourceProjectState
	runs  keyedrun.Group[domain.ProjectID]
	tips  keyedrun.Group[int64]
}

// activeResourceProjectState is one project's process-local memory across
// passes. Only that project's run touches it, and keyedrun runs one at a
// time per project, so it needs no lock of its own.
type activeResourceProjectState struct {
	// evictedConcluded records whether the final, post-completion eviction of
	// a concluded item's caches has happened; a process-local optimization.
	evictedConcluded    map[domain.ItemID]bool
	observationFailures map[domain.ItemID]int
	// handBackGapSeen records, per ready item, the head the last pass saw
	// wait under the hand-back label for an admitted comment, so the notice
	// is raised only when the next pass still sees it (convergeHandBackWait).
	// Process-local: a restart delays a notice by one pass.
	handBackGapSeen map[domain.ItemID]string
	// reactionsRefusalReported records the open ready items whose refused
	// reactions read (publish.PullReviewObservation.ReactionsUnavailable) a
	// pass already reported, so the refusal is reported once per item rather
	// than every pass (#2717). An item leaves it when a pass reads its
	// reactions again or when it leaves the open state. Process-local: a
	// restart reports a standing refusal once more.
	reactionsRefusalReported map[domain.ItemID]bool
}

// stateFor returns project's state, creating it on first use.
func (p *activeResourceProjects) stateFor(project domain.ProjectID) *activeResourceProjectState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if state, ok := p.state[project]; ok {
		return state
	}
	if p.state == nil {
		p.state = map[domain.ProjectID]*activeResourceProjectState{}
	}
	state := &activeResourceProjectState{
		evictedConcluded:         map[domain.ItemID]bool{},
		observationFailures:      map[domain.ItemID]int{},
		handBackGapSeen:          map[domain.ItemID]string{},
		reactionsRefusalReported: map[domain.ItemID]bool{},
	}
	p.state[project] = state
	return state
}

// forProject is the reconciler as one project's run sees it: its own
// process-local maps. The project's hand-back label is read per observation
// (handBackLabels), not here.
func (r *ActiveResourceReconciler) forProject(project domain.ProjectID) *ActiveResourceReconciler {
	state := r.projects.stateFor(project)
	scoped := *r
	scoped.evictedConcluded = state.evictedConcluded
	scoped.observationFailures = state.observationFailures
	scoped.handBackGapSeen = state.handBackGapSeen
	scoped.reactionsRefusalReported = state.reactionsRefusalReported
	return &scoped
}

type activeResourceObservation struct {
	itemID         domain.ItemID
	binding        domain.ReadyItemPRBinding
	completionOnly bool
	// pull is nil when no completion sweep is needed or when the first
	// lifecycle observation has a foreign repository/PR identity. A foreign
	// pull is never constructed as a fact or persisted; only an open item's
	// readiness withdrawal crosses the commit boundary (plan §7, issue #514).
	pull        *domain.PullMergeFact
	issue       *domain.IssueStateFact
	completion  *domain.WorkUnitCompletion
	completed   bool
	exactClosed bool
	conclude    bool
	material    bool
	// invalidation is set when the observed pull diverges from the ready
	// item's binding (head, target ref, or identity), so the clean review
	// pass no longer describes the live candidate (plan §7, issue #496). The
	// commit supersedes the still-open item and records this fact.
	invalidation *domain.ReadinessInvalidation
	// nativeObservations are normalized native-review observations to append as
	// best-effort extra evidence (plan §5.16, §7; issue #497). nativeErr holds
	// an isolated native-observe failure: it is reported but never blocks the
	// pull/issue facts, completion, or invalidation handling, and the next tick
	// retries.
	nativeObservations []domain.NativeReviewObservation
	nativeErr          error
	// reviewRead reports that this pass read the review activity without
	// error; reactionsUnavailable then reports whether GitHub refused its
	// reactions read. Both hold on an unchanged (NotModified) read too.
	reviewRead           bool
	reactionsUnavailable bool
	// externalFindings are the same fetch's review activity as external
	// findings, one per counted review body and inline comment whoever wrote
	// it (plan §5.19; issue #524). externalSkipped names the activity that
	// could not be a valid finding.
	externalFindings []domain.Finding
	externalSkipped  []error
	// check is the required-check reading on the bound head, read only while
	// boundOpen; nil when no read ran or the base requires no check.
	// checkErr isolates a failed read as nativeErr does.
	check    *publish.RequiredCheckObservation
	checkErr error
	// boundOpen reports that the pass saw the pull request open and still the
	// bound one, the only state in which its review activity is read and an
	// external finding may start a cycle.
	boundOpen bool
	// handBack reports that the pass saw the run's pull request open,
	// unmerged, on its bound target, and carrying the hand-back label. Its
	// conversation comments then bind to the observed head, which a hand-back
	// cycle adopts, and the external review trigger waits.
	handBack    bool
	foreclosure *completionForeclosure
	// held marks an observation of a publication hold's pull request, read
	// through the held binding because the run has no ready item (issue #531).
	// It is always completion-only: it records facts and work-unit completion
	// and leaves the hold item and the publication task alone.
	held bool
	// bindingAbsent reports a hold whose binding the engine has not recorded
	// yet. The hold is skipped until a later pass; nothing was observed.
	bindingAbsent bool
}

type completionForeclosure struct {
	unitID  domain.WorkUnitID
	binding domain.WorkUnitPRBinding
	pull    domain.PullMergeFact
}

type completionRecoveryState struct {
	declaration *domain.WorkUnitDeclaration
	binding     *domain.WorkUnitPRBinding
	completed   bool
	foreclosure *completionForeclosure
}

type activeResourceReconcileResult struct {
	operatorActive bool
	Failures       []error
	// Skipped names review activity a pass read and could not store as an
	// external finding. Nothing retries it: the activity itself is unusable.
	Skipped []error
	// reactionsRefused names each ready item whose reactions read GitHub
	// refused, once per item until the reactions read again. The reviews and
	// comments were still observed, so it is a warning, not a failure.
	reactionsRefused []error
}

func validateObservedPullIdentityCoordinates(repositoryID int64, prNumber int) error {
	if repositoryID <= 0 {
		return fmt.Errorf("returned repository_id %d: %w", repositoryID, domain.ErrNonPositive)
	}
	if prNumber <= 0 {
		return fmt.Errorf("returned pr_number %d: %w", prNumber, domain.ErrNonPositive)
	}
	return nil
}

func loadCompletionRecoveryState(
	ctx context.Context, tx *store.ReadTx, readyBinding domain.ReadyItemPRBinding,
) (completionRecoveryState, error) {
	state := completionRecoveryState{}
	successor, err := tx.CurrentPublicationSuccessor(ctx, readyBinding.RunID)
	if err != nil {
		return state, err
	}
	if successor != nil && successor.ReadyItemID() != readyBinding.ItemID {
		published, err := tx.PublishedProductionReadyItemID(ctx, readyBinding.RunID)
		if err != nil {
			return state, err
		}
		// Sealing a successor does not replace the PR's published head.
		// Retire its predecessor's completion watch only after publication.
		if published != readyBinding.ItemID {
			return state, nil
		}
	}
	declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, readyBinding.RunID)
	if errors.Is(err, store.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.declaration = &declaration

	binding, err := tx.EffectiveWorkUnitPRBinding(ctx, declaration.ID)
	if errors.Is(err, store.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	covers, err := tx.WorkUnitBindingCovers(ctx, binding, readyBinding.HeadSHA)
	if err != nil {
		return state, err
	}
	if binding.Repo != readyBinding.Repo ||
		binding.RepositoryID != readyBinding.RepositoryID ||
		binding.PRNumber != readyBinding.PRNumber ||
		binding.BaseRef != readyBinding.BaseRef || !covers {
		return state, fmt.Errorf("work-unit binding %s disagrees with ready resource %s",
			binding.UnitID, readyBinding.ItemID)
	}
	state.binding = &binding

	if _, err := tx.GetWorkUnitCompletion(ctx, declaration.ID); err == nil {
		state.completed = true
		return state, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return state, err
	}

	pulls, err := tx.ListPullMergeFacts(ctx, binding.RepositoryID, binding.PRNumber)
	if err != nil {
		return state, err
	}
	// A merged pull cannot later reopen or change its head or base. Preserve
	// the newest foreclosing proof even if a contradictory later fact is
	// appended, rather than letting an impossible transition resurrect polling
	// or authorize completion after restart.
	for i := len(pulls) - 1; i >= 0; i-- {
		pull := pulls[i]
		if pull.Merged && (pull.HeadSHA != binding.HeadSHA || pull.BaseRef != binding.BaseRef) {
			pending, err := rebaseProofPending(ctx, tx, successor, declaration.ID, pull)
			if err != nil {
				return state, err
			}
			if pending {
				break
			}
			state.foreclosure = &completionForeclosure{
				unitID: declaration.ID, binding: binding, pull: pull,
			}
			break
		}
	}
	return state, nil
}

// rebaseProofPending reports whether a merge of a head other than the
// effective one may still be completion rather than foreclosure: a lander
// rebased and merged (issue #2129), the head_changed cycle for exactly the
// merged head has not finished, and it has recorded no verdict for that
// head. The cycle proves the rebase or ends on the merge in its first pass
// that reaches the proof, so the wait lasts as long as that cycle runs; a
// cycle whose pass keeps failing fails the publication lane loudly on every
// pass, and the hold lasts as long as that failure does. Once the cycle
// finishes, a proof moves the effective head onto the merged one and a
// refusal or a missing proof forecloses as before.
func rebaseProofPending(
	ctx context.Context, tx *store.ReadTx, successor *domain.PublicationSuccessor,
	unitID domain.WorkUnitID, pull domain.PullMergeFact,
) (bool, error) {
	if successor == nil || successor.Reentry == nil ||
		successor.Reentry.Reason != domain.ReadinessInvalidationHeadChanged ||
		successor.Reentry.HeadSHA != pull.HeadSHA {
		return false, nil
	}
	if _, err := tx.GetWorkUnitRebaseProof(ctx, unitID, pull.HeadSHA); err == nil {
		return false, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	task, err := tx.GetOutbox(ctx, successor.TaskKey())
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return !task.Dispatched(), nil
}

// Run performs one startup pass and then one pass per interval. An open
// ready-for-final-review item selects the operator-active interval;
// otherwise the background interval applies. A resource failure is reported
// and isolated to that item; enumeration or transaction failures stop the
// loop because continuing would silently omit durable work.
//
// Each pass runs on its own goroutine, so a pass waiting on a stalled
// project never delays the next tick; that pass skips the busy project
// (#3210). A wake for a new ready item starts a pass at once, and a pass that
// was already running when the wake arrived starts one more as it returns,
// since its project may have been skipped by the pass the wake started.
//
// This is the GitHub-facing loop, so it is where a recurring credential or
// rate-limit failure shows up. Reporting it at error severity with a
// timestamp is the whole point: as an unstructured stderr line every
// quarter hour, a persistent 401 was indistinguishable from noise.
func (r ActiveResourceReconciler) Run(
	ctx context.Context, defaultInterval, operatorActiveInterval time.Duration,
	logger *slog.Logger,
) error {
	if defaultInterval <= 0 {
		return fmt.Errorf("default active resource interval %s must be positive", defaultInterval)
	}
	if operatorActiveInterval <= 0 {
		return fmt.Errorf("operator-active resource interval %s must be positive", operatorActiveInterval)
	}
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	logger = logger.With("subsystem", "active-resource")
	logger.Info("active resource reconciler started",
		"default_interval", defaultInterval,
		"operator_active_interval", operatorActiveInterval,
	)
	if r.projects == nil {
		r.projects = &activeResourceProjects{}
	}
	ctx, cancel := context.WithCancel(ctx)
	// Every pass returns before Run does: cancel runs first, then the wait.
	var passes sync.WaitGroup
	defer passes.Wait()
	defer cancel()
	type passOutcome struct {
		// seq is the pass's place in start order, from 1.
		seq    int
		result activeResourceReconcileResult
		err    error
	}
	outcomes := make(chan passOutcome)
	// started counts the passes started; wokeAt is started when the latest
	// wake arrived, so a pass with seq at or below it was running then.
	started, wokeAt := 0, 0
	start := func() {
		started++
		seq := started
		passes.Go(func() {
			result, err := r.Reconcile(ctx)
			select {
			case outcomes <- passOutcome{seq: seq, result: result, err: err}:
			case <-ctx.Done():
			}
		})
	}
	operatorActive := false
	report := func(result activeResourceReconcileResult, err error) error {
		// Logged before the pass error is handled: noteReactionsRefusal has
		// already marked these items reported, so dropping them with a failed
		// pass would silence a standing refusal until the reactions read again.
		for _, refused := range result.reactionsRefused {
			logger.Warn("native review reactions unavailable", "error", refused)
		}
		if err != nil {
			if ctx.Err() != nil {
				// Cancellation mid-pass is shutdown, not a failure: the same
				// reading the engine and scheduler loops already take. The
				// select below logs the single stop record.
				return nil
			}
			logger.Error("active resource pass failed", "error", err)
			return err
		}
		for _, failure := range result.Failures {
			// Isolated per-item failures: the pass converged around them, so
			// they are error severity without being loop-fatal.
			logger.Error("active resource observation failed", "error", failure)
		}
		for _, skip := range result.Skipped {
			logger.Warn("external review activity skipped", "error", skip)
		}
		if result.operatorActive != operatorActive {
			operatorActive = result.operatorActive
			if operatorActive {
				logger.Info("operator-active cadence engaged", "interval", operatorActiveInterval)
			} else {
				logger.Info("operator-active cadence released", "interval", defaultInterval)
			}
		}
		// Per-pass at debug. This loop runs forever, including once a minute
		// while an operator is active; an info record per pass is a log an
		// operator stops reading, and the records that matter drown in it.
		logger.Debug("active resource pass complete",
			"failures", len(result.Failures), "operator_active", result.operatorActive)
		return nil
	}
	interval := func() time.Duration {
		return activeResourceInterval(operatorActive, defaultInterval, operatorActiveInterval)
	}
	// An unconfigured reconciler has no store to wake it; its first pass
	// reports the missing configuration as a failed pass.
	var wakes <-chan struct{}
	if r.Store != nil {
		wakes = r.Store.ReadyItemCreated()
	}
	start()
	timer := time.NewTimer(interval())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("active resource reconciler stopped")
			return nil
		case outcome := <-outcomes:
			wasActive := operatorActive
			if err := report(outcome.result, outcome.err); err != nil {
				return err
			}
			if operatorActive != wasActive {
				resetActiveResourceTimer(timer, interval())
			}
			if outcome.seq <= wokeAt {
				start()
			}
			continue
		case <-wakes:
			// A new ready item must not wait through an idle interval before it
			// can engage the operator-active cadence. The store emits only after
			// commit; reconciliation remains authoritative if this wake is lost.
			wokeAt = started
		case <-timer.C:
		}
		start()
		resetActiveResourceTimer(timer, interval())
	}
}

func resetActiveResourceTimer(timer *time.Timer, interval time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(interval)
}

func activeResourceInterval(
	operatorActive bool, defaultInterval, operatorActiveInterval time.Duration,
) time.Duration {
	if operatorActive {
		return operatorActiveInterval
	}
	return defaultInterval
}

// noteReactionsRefusal reports a ready item's refused reactions read once:
// the first pass that sees the refusal adds it to result, later refused
// passes stay quiet, and a pass that reads the reactions again re-arms the
// report. A pass whose review read failed or didn't run changes nothing.
func (r *ActiveResourceReconciler) noteReactionsRefusal(
	itemID domain.ItemID, observation activeResourceObservation, result *activeResourceReconcileResult,
) {
	if !observation.reviewRead {
		return
	}
	if !observation.reactionsUnavailable {
		delete(r.reactionsRefusalReported, itemID)
		return
	}
	if r.reactionsRefusalReported[itemID] {
		return
	}
	if r.reactionsRefusalReported == nil {
		r.reactionsRefusalReported = make(map[domain.ItemID]bool)
	}
	r.reactionsRefusalReported[itemID] = true
	binding := observation.binding
	result.reactionsRefused = append(result.reactionsRefused, fmt.Errorf(
		"ready resource %s: github refused the reactions read on %s#%d (status 403); "+
			"reviews and review comments are still observed, the clean-pass reaction is unknown",
		itemID, binding.Repo, binding.PRNumber))
}

// listAttentionItems lists every attention item with its stored version.
func (r *ActiveResourceReconciler) listAttentionItems(
	ctx context.Context,
) ([]store.Snapshotted[domain.AttentionItem], error) {
	var snapshots []store.Snapshotted[domain.AttentionItem]
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		snapshots, err = tx.ListAttentionItems(ctx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("list active ready resources: %w", err)
	}
	return snapshots, nil
}

// activeResourceItems are the items a pass observes: every ready item, and
// every open publication hold that carries a pull request for a run with no
// ready item. A run with a ready item is observed through that item's
// binding alone, whatever the ready item's status, so one pull request is
// never polled twice. A held successor therefore keeps the watch its
// predecessor's ready item already has, no more: its own held binding is not
// read.
func activeResourceItems(snapshots []store.Snapshotted[domain.AttentionItem]) []domain.AttentionItem {
	readyRuns := make(map[domain.RunID]bool)
	for _, snapshot := range snapshots {
		if item := snapshot.Value; item.Type == domain.AttentionReadyForFinalReview && item.Subject.RunID != nil {
			readyRuns[*item.Subject.RunID] = true
		}
	}
	var observed []domain.AttentionItem
	for _, snapshot := range snapshots {
		item := snapshot.Value
		switch {
		case item.Type == domain.AttentionReadyForFinalReview:
		case item.Type == domain.AttentionPublishBlocked && item.Status == domain.StatusOpen &&
			item.PRReference != nil && item.Subject.RunID != nil && !readyRuns[*item.Subject.RunID]:
		default:
			continue
		}
		observed = append(observed, item)
	}
	return observed
}

// Reconcile makes one independent pass over every active ready item, and over
// every open publication hold that carries a pull request for a run with no
// ready item, and reads each watched repository's tip. Each project's items
// and each repository's tip run on their own goroutine; a project or
// repository whose previous run has not returned is skipped, and the pass
// waits only for the runs it started (#3210).
// Per-resource observation failures remain retryable and do not prevent a
// healthy sibling from converging in the same pass. A project's fatal
// failure stops that project's run, and the pass returns it joined with any
// other project's.
func (r *ActiveResourceReconciler) Reconcile(
	ctx context.Context,
) (activeResourceReconcileResult, error) {
	result := activeResourceReconcileResult{}
	if r.Store == nil || r.Pull == nil || r.Now == nil {
		return result, errors.New("active resource reconciler is not fully configured")
	}
	if len(r.tipWatches) != 0 && (r.observeTip == nil || r.upkeepTipAdvance == nil) {
		return result, errors.New("active resource reconciler tip watch is not fully configured")
	}
	if r.projects == nil {
		r.projects = &activeResourceProjects{}
	}
	snapshots, err := r.listAttentionItems(ctx)
	if err != nil {
		return result, err
	}
	result.Failures = make([]error, 0)
	observed := activeResourceItems(snapshots)
	var (
		mu    sync.Mutex
		fatal []error
		done  []<-chan struct{}
	)
	for _, watch := range r.tipWatches {
		done = append(done, r.projects.tips.Go(watch.RepositoryID, func() {
			if err := r.observeRepositoryTip(ctx, watch); err != nil {
				mu.Lock()
				defer mu.Unlock()
				result.Failures = append(result.Failures,
					fmt.Errorf("observe repository tip %s %s: %w", watch.Repo, watch.Branch, err))
			}
		}))
	}
	for _, part := range keyedrun.Partition(observed, func(item domain.AttentionItem) domain.ProjectID {
		return item.ProjectID
	}) {
		project := r.forProject(part.Key)
		done = append(done, r.projects.runs.Go(part.Key, func() {
			projectResult, err := project.reconcileProject(ctx, part.Key)
			mu.Lock()
			defer mu.Unlock()
			result.operatorActive = result.operatorActive || projectResult.operatorActive
			result.Failures = append(result.Failures, projectResult.Failures...)
			result.Skipped = append(result.Skipped, projectResult.Skipped...)
			result.reactionsRefused = append(result.reactionsRefused, projectResult.reactionsRefused...)
			if err != nil {
				fatal = append(fatal, fmt.Errorf("project %s: %w", part.Key, err))
			}
		}))
	}
	for _, ch := range done {
		if ch != nil {
			<-ch
		}
	}
	return result, errors.Join(fatal...)
}

// reconcileProject is one project's share of a pass: its ready items and
// the publication holds the pass observes. r is the project's forProject
// copy. It lists the items again rather than reading the pass's list, since
// the project's previous run may have outlived the pass that listed them and
// written versions that list predates; a convergence that read the older
// version would refuse it as stale and stop the loop.
func (r *ActiveResourceReconciler) reconcileProject(
	ctx context.Context, project domain.ProjectID,
) (activeResourceReconcileResult, error) {
	result := activeResourceReconcileResult{}
	snapshots, err := r.listAttentionItems(ctx)
	if err != nil {
		return result, err
	}
	items := slices.DeleteFunc(activeResourceItems(snapshots), func(item domain.AttentionItem) bool {
		return item.ProjectID != project
	})
	for _, item := range items {
		if item.Type == domain.AttentionPublishBlocked {
			if err := r.reconcileHeldResource(ctx, item, &result); err != nil {
				return result, err
			}
			continue
		}
		if item.Status != domain.StatusOpen {
			delete(r.observationFailures, item.ID)
			delete(r.reactionsRefusalReported, item.ID)
			if err := r.convergeObservationHealth(ctx, snapshots, item, true); err != nil {
				return result, fmt.Errorf("resolve ready resource observation health %s: %w", item.ID, err)
			}
			observation, err := r.observeCompletionOnly(ctx, item, r.Now().UTC())
			if err != nil {
				result.Failures = append(result.Failures,
					fmt.Errorf("recover ready resource completion %s: %w", item.ID, err))
			} else {
				if observation.material || observation.completion != nil {
					if err := r.commit(ctx, observation); err != nil {
						return result, fmt.Errorf("commit ready resource completion %s: %w", item.ID, err)
					}
				}
				var gap *engine.HandBackWait
				known := true
				if observation.handBack {
					gap, known = r.intakeHandBack(ctx, item.ID, observation, &result)
				}
				if known {
					if err := r.convergeHandBackWait(ctx, snapshots, item, gap); err != nil {
						result.Failures = append(result.Failures,
							fmt.Errorf("converge hand-back notice %s: %w", item.ID, err))
					}
				}
				if observation.foreclosure != nil {
					if err := r.convergeCompletionForeclosure(ctx, item, *observation.foreclosure); err != nil {
						return result, fmt.Errorf("surface ready resource completion foreclosure %s: %w", item.ID, err)
					}
				}
			}
			if err := r.evictConcludedResource(ctx, item.ID); err != nil {
				result.Failures = append(result.Failures,
					fmt.Errorf("evict concluded ready resource %s: %w", item.ID, err))
			}
			if err := r.settleSchedules(ctx, item.ID, r.Now().UTC()); err != nil {
				return result, fmt.Errorf("settle ready resource %s: %w", item.ID, err)
			}
			continue
		}
		observation, err := r.observe(ctx, item, r.Now().UTC())
		if err != nil {
			result.operatorActive = true
			result.Failures = append(result.Failures,
				fmt.Errorf("reconcile ready resource %s: %w", item.ID, err))
			if r.observationFailures == nil {
				r.observationFailures = make(map[domain.ItemID]int)
			}
			r.observationFailures[item.ID]++
			if r.observationFailures[item.ID] >= activeResourceObservationFailureThreshold {
				if healthErr := r.convergeObservationHealth(ctx, snapshots, item, false); healthErr != nil {
					return result, fmt.Errorf(
						"raise ready resource observation health %s: %w", item.ID, healthErr)
				}
			}
			continue
		}
		delete(r.observationFailures, item.ID)
		if err := r.convergeObservationHealth(ctx, snapshots, item, true); err != nil {
			return result, fmt.Errorf("resolve ready resource observation health %s: %w", item.ID, err)
		}
		// A native-observe failure is isolated: it is reported but never blocks
		// the pull/issue commit below, and the next tick retries (plan §5.16).
		if observation.nativeErr != nil {
			result.Failures = append(result.Failures,
				fmt.Errorf("reconcile ready resource %s: %w", item.ID, observation.nativeErr))
		}
		if observation.checkErr != nil {
			result.Failures = append(result.Failures,
				fmt.Errorf("reconcile ready resource %s: %w", item.ID, observation.checkErr))
		}
		r.noteReactionsRefusal(item.ID, observation, &result)
		if observation.material || observation.conclude ||
			observation.completion != nil || observation.invalidation != nil {
			if err := r.commit(ctx, observation); errors.Is(err, domain.ErrProjectNotReady) {
				// The head-change re-entry this commit starts was refused
				// because the project is not ready (#3205). The commit wrote
				// nothing, so the next pass observes the same change and
				// tries again; like a refused hand-back or external review,
				// it is isolated and the pass goes on to the next resource.
				result.Failures = append(result.Failures,
					fmt.Errorf("commit ready resource %s: %w", item.ID, err))
			} else if err != nil {
				return result, fmt.Errorf("commit ready resource %s: %w", item.ID, err)
			} else if err := r.evictConcludedResource(ctx, item.ID); err != nil {
				result.Failures = append(result.Failures,
					fmt.Errorf("evict concluded ready resource %s: %w", item.ID, err))
			}
		}
		if !observation.conclude && observation.invalidation == nil {
			result.operatorActive = true
		}
		// Native observations commit in their own daemon-internal transaction,
		// so a native-store failure is isolated from the pull/issue facts and
		// collected as a retryable failure rather than stopping the pass.
		if len(observation.nativeObservations) > 0 {
			if err := r.commitNativeReview(ctx, observation.nativeObservations); err != nil {
				result.Failures = append(result.Failures,
					fmt.Errorf("record native review %s: %w", item.ID, err))
				// The observer already advanced its ETags on the fetch, so a 304
				// on the next tick would suppress the rebuild and strand these
				// un-persisted rows. Evict the cache so the retry re-fetches
				// unconditionally and rebuilds them (issue #497).
				if r.reviewInvalidate != nil {
					r.reviewInvalidate(observation.binding.Repo, observation.binding.PRNumber)
				}
			}
		}
		// The required check commits in its own transaction too, and only on
		// a pass that left the item open on its bound head.
		checkRecorded := false
		if observation.check != nil && observation.invalidation == nil && !observation.conclude {
			if err := r.convergeRequiredCheck(ctx, item.ID, observation.binding, *observation.check,
				observation.handBack, observation.pull.ObservedAt); err != nil {
				result.Failures = append(result.Failures,
					fmt.Errorf("record required check %s: %w", item.ID, err))
			} else {
				checkRecorded = true
			}
		}
		// External findings ride the same fetch and are isolated the same way:
		// their own transaction, a collected failure, and an evicted cache so
		// the retry rebuilds them.
		result.Skipped = append(result.Skipped, observation.externalSkipped...)
		if len(observation.externalFindings) > 0 {
			if err := r.commitExternalFindings(ctx, observation); err != nil {
				result.Failures = append(result.Failures,
					fmt.Errorf("record external review %s: %w", item.ID, err))
				r.dropReviewCache(observation.binding)
			}
		}
		// An admitted external finding on the published head withdraws
		// readiness and starts a review cycle (issue #524). The trigger reads
		// the store, not this pass's fetch, so it runs on every pass: it also
		// picks up a finding stored before a crash and a reviewer the owner
		// listed after their comment arrived. A failure is isolated like the
		// intake's and retried on the next pass.
		// A handed-back pull request starts a hand-back cycle instead: the
		// comment that hands it back is not a review of the published head.
		var gap *engine.HandBackWait
		known := true
		if observation.handBack && observation.invalidation == nil && !observation.conclude {
			var err error
			gap, known, err = r.startHandBack(ctx, observation.binding.RunID, observation.binding.Repo, *observation.pull)
			if err != nil {
				result.Failures = append(result.Failures, fmt.Errorf("start hand-back %s: %w", item.ID, err))
			}
		} else if observation.boundOpen && observation.invalidation == nil && !observation.conclude {
			if err := r.startExternalReview(ctx, item.ID, observation.pull.ObservedAt); err != nil {
				result.Failures = append(result.Failures,
					fmt.Errorf("start external review %s: %w", item.ID, err))
			}
			// A code failure of the required check starts a check_failure
			// cycle (#3598), from the reading this pass just recorded: the
			// engine matches the record's jobs to the ones the card names.
			// An external review that started first superseded the item, so
			// the probe finds nothing due.
			if checkRecorded {
				if err := r.startCheckFailure(ctx, item.ID, observation.binding, *observation.check,
					observation.pull.ObservedAt); err != nil {
					result.Failures = append(result.Failures,
						fmt.Errorf("start check failure %s: %w", item.ID, err))
				}
			}
		}
		if known {
			if err := r.convergeHandBackWait(ctx, snapshots, item, gap); err != nil {
				result.Failures = append(result.Failures,
					fmt.Errorf("converge hand-back notice %s: %w", item.ID, err))
			}
		}
	}
	return result, nil
}

// observeRepositoryTip records the watched branch's tip and hands a tip
// upkeep has not handled yet to upkeepTipAdvance. It compares the read SHA
// with the stored row instead of trusting a 304, because image upkeep's tip
// read shares the ref cache: a tip it read first comes back NotModified
// here. UpkeepTipSHA advances only after the entry point succeeds, so every
// failure leaves the tip pending and the next pass retries it; a crash
// between that success and the write repeats one call.
//
// Each write reads the row and writes it in one transaction and changes only
// its own fields, so it never overwrites a field another writer set.
func (r *ActiveResourceReconciler) observeRepositoryTip(ctx context.Context, watch repositoryTipWatch) error {
	tip, err := r.observeTip(ctx, watch.Repo, watch.Branch)
	if err != nil {
		return fmt.Errorf("read tip: %w", err)
	}
	var pending domain.RepositoryTipObservation
	if err := r.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		pending, err = recordRepositoryTip(ctx, tx, watch, tip, r.Now().UTC())
		return err
	}); err != nil {
		return fmt.Errorf("record tip: %w", err)
	}
	if pending.TipSHA == pending.UpkeepTipSHA {
		return nil
	}
	if err := r.upkeepTipAdvance(ctx, pending); err != nil {
		return fmt.Errorf("hand tip %s to image upkeep: %w", pending.TipSHA, err)
	}
	if err := r.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		current, found, err := tx.GetRepositoryTipObservation(ctx, watch.RepositoryID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("tip observation vanished")
		}
		// Another writer advanced UpkeepTipSHA during the call; its value
		// stands. Otherwise record the handled tip even if the tip moved
		// meanwhile, which leaves the newer one pending for the next pass.
		if current.UpkeepTipSHA != pending.UpkeepTipSHA {
			return nil
		}
		current.UpkeepTipSHA = pending.TipSHA
		return tx.RecordRepositoryTipObservation(ctx, current)
	}); err != nil {
		return fmt.Errorf("record upkeep tip %s: %w", pending.TipSHA, err)
	}
	return nil
}

// recordRepositoryTip stores tip as watch's observed tip unless the stored
// row already names it, and returns the row. It keeps UpkeepTipSHA, so a tip
// upkeep has not handled stays pending. The tip watch and the driver's start
// both write through it.
func recordRepositoryTip(
	ctx context.Context, tx *store.InternalTx, watch repositoryTipWatch, tip string, now time.Time,
) (domain.RepositoryTipObservation, error) {
	stored, found, err := tx.GetRepositoryTipObservation(ctx, watch.RepositoryID)
	if err != nil {
		return domain.RepositoryTipObservation{}, err
	}
	if found && stored.TipSHA == tip && stored.Branch == watch.Branch && stored.Repository == watch.Repo {
		return stored, nil
	}
	stored = domain.RepositoryTipObservation{
		RepositoryID: watch.RepositoryID, Repository: watch.Repo, Branch: watch.Branch,
		TipSHA: tip, ObservedAt: now, UpkeepTipSHA: stored.UpkeepTipSHA,
	}
	if err := tx.RecordRepositoryTipObservation(ctx, stored); err != nil {
		return domain.RepositoryTipObservation{}, err
	}
	return stored, nil
}

// startExternalReview starts an external review cycle for a ready item when
// one is due. The cycle starts in its own transaction: the store seals its
// authority only against an item that is already superseded, so the
// supersession, the authority, and the task commit together or not at all,
// and the item stays open when anything fails. The transaction is opened
// only when a cycle is due, because every write transaction advances the
// client-visible revision.
func (r ActiveResourceReconciler) startExternalReview(
	ctx context.Context, itemID domain.ItemID, observedAt time.Time,
) error {
	var due bool
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		due, err = engine.ExternalReviewReentryDue(ctx, tx, itemID)
		return err
	}); err != nil || !due {
		return err
	}
	return r.Store.Write(ctx, func(tx *store.WriteTx) error {
		started, err := engine.StartExternalReviewReentry(ctx, tx, itemID, r.readinessGate)
		if err != nil || !started {
			return err
		}
		// The item is no longer ready, so its watches end with it.
		return concludePublicationSchedules(ctx, tx, itemID, observedAt)
	})
}

// intakeHandBack stores a handed-back pull request's review activity and
// starts the hand-back cycle for a run whose ready item already ended. Each
// failure is isolated like the open path's and retried on the next pass. It
// returns startHandBack's gap and whether the pass learned it; a failed
// store of the activity leaves it unknown.
func (r *ActiveResourceReconciler) intakeHandBack(
	ctx context.Context, itemID domain.ItemID, observation activeResourceObservation,
	result *activeResourceReconcileResult,
) (*engine.HandBackWait, bool) {
	if observation.nativeErr != nil {
		result.Failures = append(result.Failures, fmt.Errorf("hand-back %s: %w", itemID, observation.nativeErr))
	}
	result.Skipped = append(result.Skipped, observation.externalSkipped...)
	if len(observation.externalFindings) > 0 {
		if err := r.commitExternalFindings(ctx, observation); err != nil {
			result.Failures = append(result.Failures, fmt.Errorf("record hand-back review %s: %w", itemID, err))
			r.dropReviewCache(observation.binding)
			return nil, false
		}
	}
	gap, known, err := r.startHandBack(ctx, observation.binding.RunID, observation.binding.Repo, *observation.pull)
	if err != nil {
		result.Failures = append(result.Failures, fmt.Errorf("start hand-back %s: %w", itemID, err))
	}
	return gap, known
}

// startHandBack starts a hand-back cycle for the run when one is due on the
// observed head (engine.StartHandBackReentry). Like startExternalReview it
// opens a write transaction only when a cycle is due, and the supersession
// of an open ready item, the authority, and the task commit together.
//
// When no cycle is due it returns why the head waits for a comment, nil when
// it does not (engine.HandBackCommentGap). known reports whether the pass
// learned that: it is false only when the read that decides it failed. A
// cycle that is due has no gap, whether or not it then starts.
func (r ActiveResourceReconciler) startHandBack(
	ctx context.Context, runID domain.RunID, repo string, pull domain.PullMergeFact,
) (gap *engine.HandBackWait, known bool, err error) {
	var due bool
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if due, err = engine.HandBackReentryDue(ctx, tx, runID, pull.HeadSHA); err != nil || due {
			return err
		}
		gap, err = engine.HandBackCommentGap(ctx, tx, runID, pull.HeadSHA)
		return err
	}); err != nil {
		return nil, false, err
	}
	if !due {
		return gap, true, nil
	}
	if r.mergeBase == nil {
		return nil, true, errors.New("hand-back intake has no merge-base reader")
	}
	base, err := r.mergeBase(ctx, repo, pull.BaseRef, pull.HeadSHA)
	if err != nil {
		return nil, true, fmt.Errorf("read merge base: %w", err)
	}
	return nil, true, r.Store.Write(ctx, func(tx *store.WriteTx) error {
		superseded, started, err := engine.StartHandBackReentry(ctx, tx, runID, pull.HeadSHA, base, r.readinessGate)
		if err != nil || !started || superseded == "" {
			return err
		}
		// The superseded item is no longer ready, so its watches end with it.
		return concludePublicationSchedules(ctx, tx, superseded, pull.ObservedAt)
	})
}

// publishedPullOpen reports whether binding is its run's published ready
// item and its pull request was last recorded open and unmerged: the only
// concluded item a hand-back can still reach.
func publishedPullOpen(ctx context.Context, tx *store.ReadTx, binding domain.ReadyItemPRBinding) (bool, error) {
	published, err := tx.PublishedProductionReadyItemID(ctx, binding.RunID)
	if err != nil || published != binding.ItemID {
		return false, err
	}
	latest, err := tx.LatestPullMergeFact(ctx, binding.RepositoryID, binding.PRNumber)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return latest.State == domain.PullRequestOpen && !latest.Merged, nil
}

// reconcileHeldResource observes the pull request behind an open publication
// hold. A held run has no readiness to invalidate or conclude, so the pass
// only records what it sees: the pull request fact, and for a declared work
// unit the issue fact and completion. The hold item stays open and the
// publication task stays queued; ending a held run is the engine's decision.
// Observation failures are isolated to this hold, like a ready item's.
func (r *ActiveResourceReconciler) reconcileHeldResource(
	ctx context.Context, item domain.AttentionItem, result *activeResourceReconcileResult,
) error {
	observation, err := r.observeReadyResource(ctx, item, r.Now().UTC(), true, true)
	if err != nil {
		result.Failures = append(result.Failures,
			fmt.Errorf("reconcile held resource %s: %w", item.ID, err))
		return nil
	}
	if observation.bindingAbsent {
		return nil
	}
	if observation.material || observation.completion != nil {
		if err := r.commit(ctx, observation); err != nil {
			return fmt.Errorf("commit held resource %s: %w", item.ID, err)
		}
	}
	if observation.foreclosure != nil {
		if err := r.convergeCompletionForeclosure(ctx, item, *observation.foreclosure); err != nil {
			return fmt.Errorf("surface held resource completion foreclosure %s: %w", item.ID, err)
		}
	}
	return nil
}

func completionForeclosureItemID(unitID domain.WorkUnitID) domain.ItemID {
	digest := sha256.Sum256([]byte(unitID))
	return domain.ItemID(fmt.Sprintf("completion-foreclosed-%x", digest[:]))
}

// convergeCompletionForeclosure surfaces the terminal completion refusal once.
// The deterministic ID is checked across every item status so acknowledging the
// notice does not cause a later reconciliation pass to resurrect it.
func (r *ActiveResourceReconciler) convergeCompletionForeclosure(
	ctx context.Context, ready domain.AttentionItem, foreclosure completionForeclosure,
) error {
	itemID := completionForeclosureItemID(foreclosure.unitID)
	var exists bool
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, itemID)
		switch {
		case err == nil:
			exists = true
			return nil
		case errors.Is(err, store.ErrNotFound):
			return nil
		default:
			return err
		}
	}); err != nil {
		return err
	}
	if exists {
		return nil
	}

	posture := domain.HealthPostureAdvisory
	createdAt := r.Now().UTC()
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	var displayNames *domain.DisplayNames
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		displayNames, err = tx.DisplayNamesFor(ctx, ready.ProjectID, subject)
		return err
	}); err != nil {
		return err
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: ready.ProjectID,
		Subject: subject,
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Work unit %s cannot record completion: merged %s#%d has base %s and head %s; the bound candidate has base %s and head %s",
			foreclosure.unitID,
			foreclosure.binding.Repo,
			foreclosure.binding.PRNumber,
			foreclosure.pull.BaseRef,
			foreclosure.pull.HeadSHA,
			foreclosure.binding.BaseRef,
			foreclosure.binding.HeadSHA,
		),
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "completion_foreclosed", Impairs: domain.ImpairedCapabilityNone,
		},
		DisplayNames: displayNames,
		ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt,
		Posture:   &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return err
	}
	return signet.NewService(r.Store).PutItem(ctx, item)
}

func activeResourceObservationHealthPrefix(itemID domain.ItemID) string {
	digest := sha256.Sum256([]byte(itemID))
	return fmt.Sprintf("active-resource-observation-%x-", digest[:])
}

func activeResourceObservationHealthItems(
	snapshots []store.Snapshotted[domain.AttentionItem], itemID domain.ItemID,
) []domain.AttentionItem {
	prefix := activeResourceObservationHealthPrefix(itemID)
	items := make([]domain.AttentionItem, 0, 1)
	for _, snapshot := range snapshots {
		item := snapshot.Value
		if item.Type == domain.AttentionSystemHealth &&
			item.Status == domain.StatusOpen &&
			strings.HasPrefix(string(item.ID), prefix) {
			items = append(items, item)
		}
	}
	return items
}

// convergeObservationHealth raises one advisory health item after persistent
// failure and resolves every open incident item once the resource recovers or
// is no longer active. The error itself is deliberately not persisted: remote
// response text is untrusted, while the ready item ID is a trusted local
// coordinate an operator can use to inspect the logs.
func (r *ActiveResourceReconciler) convergeObservationHealth(
	ctx context.Context,
	snapshots []store.Snapshotted[domain.AttentionItem],
	ready domain.AttentionItem,
	healthy bool,
) error {
	existing := activeResourceObservationHealthItems(snapshots, ready.ID)
	if (healthy && len(existing) == 0) || (!healthy && len(existing) == 1) {
		return nil
	}
	attention := signet.NewService(r.Store)
	if healthy {
		for _, item := range existing {
			item, err := item.ConcludeByDaemon(domain.ConcludedByDaemon, r.Now().UTC(),
				"Resolved when the pull request could be observed again or its watch ended.", nil)
			if err != nil {
				return err
			}
			item.ItemVersion++
			if err := attention.PutItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}
	if len(existing) > 0 {
		for _, duplicate := range existing[1:] {
			duplicate, err := duplicate.ConcludeByDaemon(domain.ConcludedByDaemon, r.Now().UTC(),
				fmt.Sprintf("Resolved as a duplicate of the open observation item %s.", existing[0].ID), nil)
			if err != nil {
				return err
			}
			duplicate.ItemVersion++
			if err := attention.PutItem(ctx, duplicate); err != nil {
				return err
			}
		}
		return nil
	}
	var (
		state        store.ServerState
		displayNames *domain.DisplayNames
	)
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		state, err = tx.ServerState(ctx)
		if err != nil {
			return err
		}
		displayNames, err = tx.DisplayNamesFor(ctx, ready.ProjectID, subject)
		return err
	}); err != nil {
		return err
	}
	posture := domain.HealthPostureAdvisory
	createdAt := r.Now().UTC()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID(fmt.Sprintf(
			"%s%d", activeResourceObservationHealthPrefix(ready.ID), state.Revision+1,
		)),
		ProjectID: ready.ProjectID,
		Subject:   subject,
		Type:      domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Active-resource observation for %s failed for %d consecutive passes",
			ready.ID,
			activeResourceObservationFailureThreshold,
		),
		RequestedDecision: []domain.Action{
			domain.ActionRunDoctor,
			domain.ActionAcknowledge,
			domain.ActionStopUnattended,
		},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "active_resource_observation_failed", Impairs: domain.ImpairedCapabilityRunVisibility,
		},
		DisplayNames: displayNames,
		ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &createdAt,
		Posture:   &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return err
	}
	return attention.PutItem(ctx, item)
}

func (r *ActiveResourceReconciler) evictConcludedResource(
	ctx context.Context, itemID domain.ItemID,
) error {
	if r.evictConcluded == nil {
		return nil
	}
	if final, ok := r.evictedConcluded[itemID]; ok && final {
		return nil
	}
	var (
		binding          domain.ReadyItemPRBinding
		boundIssue       *int
		concluded        bool
		found            bool
		recoveryComplete = true
	)
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if err != nil {
			return err
		}
		if item.Status == domain.StatusOpen {
			return nil
		}
		concluded = true
		binding, err = tx.GetReadyItemPRBinding(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		state, err := loadCompletionRecoveryState(ctx, tx, binding)
		if err != nil {
			return err
		}
		if state.declaration == nil {
			return nil
		}
		boundIssue = state.declaration.BoundIssue
		if state.binding == nil {
			recoveryComplete = false
			return nil
		}
		if !state.completed && state.foreclosure == nil {
			recoveryComplete = false
		}
		return nil
	}); err != nil {
		return err
	}
	if !concluded || !found {
		return nil
	}
	if _, alreadyEvicted := r.evictedConcluded[itemID]; alreadyEvicted && !recoveryComplete {
		return nil
	}
	r.evictConcluded(binding, boundIssue)
	if r.evictedConcluded == nil {
		r.evictedConcluded = make(map[domain.ItemID]bool)
	}
	r.evictedConcluded[itemID] = recoveryComplete
	return nil
}

// commitNativeReview appends the pass's native review observations in a single
// daemon-internal transaction. Each append coalesces an unchanged
// re-observation, so duplicate observations converge idempotently under
// retries (issue #497).
func (r ActiveResourceReconciler) commitNativeReview(
	ctx context.Context, observations []domain.NativeReviewObservation,
) error {
	return r.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, observation := range observations {
			if _, err := tx.AppendNativeReviewObservation(ctx, observation); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r ActiveResourceReconciler) settleSchedules(
	ctx context.Context, itemID domain.ItemID, settledAt time.Time,
) error {
	ids := publicationScheduleIDs(itemID)
	hasArmed := false
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		for _, id := range ids {
			schedule, err := tx.GetSchedule(ctx, id)
			switch {
			case errors.Is(err, store.ErrNotFound):
				continue
			case err != nil:
				return err
			case !schedule.Status.Terminal():
				hasArmed = true
			}
		}
		return nil
	}); err != nil || !hasArmed {
		return err
	}
	return r.Store.Write(ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if err != nil {
			return err
		}
		if item.Status == domain.StatusOpen {
			return nil
		}
		return concludePublicationSchedules(ctx, tx, itemID, settledAt)
	})
}

func (r ActiveResourceReconciler) observe(
	ctx context.Context, item domain.AttentionItem, observedAt time.Time,
) (activeResourceObservation, error) {
	return r.observeReadyResource(ctx, item, observedAt, false, false)
}

// observeCompletionOnly reuses the ready-resource trust gates and fact
// construction without applying readiness, review, or item-lifecycle
// semantics. It remains eligible after the item concludes until completion is
// durable; a closed pull remains retryable because GitHub permits reopening it.
func (r ActiveResourceReconciler) observeCompletionOnly(
	ctx context.Context, item domain.AttentionItem, observedAt time.Time,
) (activeResourceObservation, error) {
	return r.observeReadyResource(ctx, item, observedAt, true, false)
}

// observeReadyResource observes the pull request an item is bound to. held
// reads a publication hold's binding instead of a ready item's and requires
// completionOnly: both bindings carry the same store-proven coordinates, so
// everything after the read is shared.
func (r ActiveResourceReconciler) observeReadyResource(
	ctx context.Context, item domain.AttentionItem, observedAt time.Time, completionOnly, held bool,
) (activeResourceObservation, error) {
	var (
		binding       domain.ReadyItemPRBinding
		declaration   *domain.WorkUnitDeclaration
		unitBinding   *domain.WorkUnitPRBinding
		completed     bool
		foreclosure   *completionForeclosure
		bindingAbsent bool
		mergeRecorded bool
		reviewFetched bool
		// handBackWatch keeps the pull read going for a run's published,
		// concluded ready item whose pull request was last seen open, so a
		// hand-back after readiness ended still reaches the run.
		handBackWatch bool
		// handBackLabel is the run's project's hand-back label, empty for a
		// held resource or a project that names none.
		handBackLabel string
	)
	if held && !completionOnly {
		return activeResourceObservation{}, errors.New("a held resource is observed for completion only")
	}
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if held {
			var heldBinding domain.HeldItemPRBinding
			heldBinding, err = tx.GetHeldItemPRBinding(ctx, item.ID)
			// Only a missing row is absence. A binding that exists but cannot
			// be proven reads as an inconsistent row and is reported.
			if errors.Is(err, store.ErrNotFound) {
				bindingAbsent = true
				return nil
			}
			binding = domain.ReadyItemPRBinding(heldBinding)
		} else {
			binding, err = tx.GetReadyItemPRBinding(ctx, item.ID)
		}
		if err != nil {
			return err
		}
		if !held && r.handBackLabels != nil {
			run, err := tx.GetRun(ctx, binding.RunID)
			if err != nil {
				return err
			}
			if handBackLabel, err = r.handBackLabels(ctx, tx, run.ProjectID); err != nil {
				return err
			}
		}
		if completionOnly {
			state, err := loadCompletionRecoveryState(ctx, tx, binding)
			if err != nil {
				return err
			}
			declaration, unitBinding = state.declaration, state.binding
			completed, foreclosure = state.completed, state.foreclosure
			if handBackLabel != "" {
				if handBackWatch, err = publishedPullOpen(ctx, tx, binding); err != nil {
					return err
				}
			}
			if held {
				latest, err := tx.LatestPullMergeFact(ctx, binding.RepositoryID, binding.PRNumber)
				switch {
				case err == nil:
					mergeRecorded = latest.Merged
				case !errors.Is(err, store.ErrNotFound):
					return err
				}
			}
			return nil
		}
		d, err := tx.GetWorkUnitDeclarationByRun(ctx, binding.RunID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		b, err := tx.EffectiveWorkUnitPRBinding(ctx, d.ID)
		if completionOnly && errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		covers, err := tx.WorkUnitBindingCovers(ctx, b, binding.HeadSHA)
		if err != nil {
			return err
		}
		if b.Repo != binding.Repo || b.RepositoryID != binding.RepositoryID ||
			b.PRNumber != binding.PRNumber || b.BaseRef != binding.BaseRef || !covers {
			return fmt.Errorf("work-unit binding %s disagrees with ready resource %s",
				b.UnitID, binding.ItemID)
		}
		declaration, unitBinding = &d, &b
		if _, err := tx.GetWorkUnitCompletion(ctx, d.ID); err == nil {
			completed = true
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return nil
	}); err != nil {
		return activeResourceObservation{}, err
	}
	observation := activeResourceObservation{
		itemID: item.ID, binding: binding, completionOnly: completionOnly,
		completed: completed, foreclosure: foreclosure,
		held: held, bindingAbsent: bindingAbsent,
	}
	if bindingAbsent {
		return observation, nil
	}
	if completionOnly && (completed || foreclosure != nil) {
		return observation, nil
	}
	if completionOnly && (declaration == nil || unitBinding == nil) {
		// A concluded ready item already recorded its pull request's end, so
		// only a declared unit's completion is left to observe. A hold has
		// recorded nothing yet: keep polling until its merge is on record. A
		// closed pull request can reopen, so only a merge stops the polling.
		if (!held || mergeRecorded) && !handBackWatch {
			return observation, nil
		}
	}
	observed, err := r.Pull(ctx, binding.Repo, binding.PRNumber)
	if err != nil {
		return activeResourceObservation{}, fmt.Errorf("observe pull %s#%d: %w", binding.Repo, binding.PRNumber, err)
	}
	if err := validateObservedPullIdentityCoordinates(observed.BaseRepoID, observed.Number); err != nil {
		return activeResourceObservation{}, fmt.Errorf("observe pull %s#%d: %w",
			binding.Repo, binding.PRNumber, err)
	}
	// Readiness is a standing claim about one immutable repository/PR binding.
	// When the successful observation channel no longer reaches that identity,
	// withdraw the claim before constructing or validating any foreign pull
	// fact. The mismatch proves only that readiness is no longer verifiable; it
	// gives no authority over the foreign object (plan §7, issue #514).
	if observed.BaseRepoID != binding.RepositoryID || observed.Number != binding.PRNumber {
		if completionOnly {
			return activeResourceObservation{}, fmt.Errorf(
				"observe pull %s#%d returned identity %d#%d",
				binding.Repo, binding.PRNumber, observed.BaseRepoID, observed.Number,
			)
		}
		return activeResourceObservation{
			itemID: item.ID, binding: binding,
			invalidation: readinessIdentityInvalidationFor(
				binding, observed.BaseRepoID, observed.Number, observedAt,
			),
		}, nil
	}
	pullFact := domain.PullMergeFact{
		Repo: binding.Repo, RepositoryID: observed.BaseRepoID,
		PRNumber: observed.Number, State: domain.PullRequestState(observed.State),
		Merged: observed.Merged, MergeCommitSHA: observed.MergeCommitSHA,
		BaseRef: observed.BaseRef, HeadSHA: observed.HeadSHA, ObservedAt: observedAt,
	}
	if err := pullFact.Validate(); err != nil {
		return activeResourceObservation{}, fmt.Errorf("observe pull %s#%d: %w", binding.Repo, binding.PRNumber, err)
	}
	exact := pullFact.RepositoryID == binding.RepositoryID &&
		pullFact.PRNumber == binding.PRNumber && pullFact.BaseRef == binding.BaseRef &&
		pullFact.HeadSHA == binding.HeadSHA
	observation.pull = &pullFact
	observation.exactClosed = exact && pullFact.State == domain.PullRequestClosed
	// A pull that is provably this PR (matching repository and number) but no
	// longer matches its head or base ref invalidates the ready pass. Identity
	// divergence already returned above without constructing a pull fact.
	// exactClosed (the resolve path below) requires full exactness, so an
	// invalidation and a conclusion are mutually exclusive by construction.
	if !completionOnly && !exact && pullFact.RepositoryID == binding.RepositoryID && pullFact.PRNumber == binding.PRNumber {
		observation.invalidation = readinessInvalidationFor(binding, pullFact)
	}
	// Native review activity is observed only while the live candidate is the
	// bound one (exact and still open): a diverged pull is invalidating and a
	// closed pull is concluding, and observation stops once the item leaves
	// ready (plan §5.16). The observer's failure is isolated into nativeErr so
	// it never blocks the pull/issue facts; on success, unchanged activity
	// (a 304 across all sub-resources) yields nothing to record.
	observation.boundOpen = !completionOnly && exact && pullFact.State == domain.PullRequestOpen
	observation.handBack = handBackLabel != "" && slices.Contains(observed.Labels, handBackLabel) &&
		pullFact.State == domain.PullRequestOpen && !pullFact.Merged && pullFact.BaseRef == binding.BaseRef &&
		(handBackWatch || !completionOnly)
	// A conversation comment binds to the head the pass observed. That is the
	// ready item's own head while it is bound, and under a hand-back after
	// readiness ended it is the head the hand-back adopts.
	conversationHead := binding.HeadSHA
	readReview := observation.boundOpen
	if completionOnly && observation.handBack {
		conversationHead, readReview = pullFact.HeadSHA, true
	}
	if observation.boundOpen && r.observeCheck != nil {
		check, err := r.observeCheck(ctx, binding.Repo, binding.BaseRef, binding.HeadSHA)
		switch {
		case err != nil:
			observation.checkErr = fmt.Errorf("observe required check %s#%d: %w", binding.Repo, binding.PRNumber, err)
		case !check.None:
			observation.check = &check
		}
	}
	if readReview && r.Review != nil {
		reviewObs, err := r.Review(ctx, binding.Repo, binding.PRNumber)
		if err == nil {
			observation.reviewRead = true
			observation.reactionsUnavailable = reviewObs.ReactionsUnavailable
		}
		switch {
		case err != nil:
			observation.nativeErr = fmt.Errorf("observe native review %s#%d: %w", binding.Repo, binding.PRNumber, err)
		case !reviewObs.NotModified:
			reviewFetched = true
			// Native evidence describes a ready item's live candidate, so a
			// hand-back read after readiness ended records none.
			if observation.boundOpen {
				observation.nativeObservations = buildNativeReviewObservations(reviewObs, binding, r.Reviewers, observedAt)
			}
			botUserID, identityErr := r.conversationBotUserID(ctx, binding.Repo, reviewObs)
			if identityErr != nil {
				// The reviews and inline comments are still stored. The
				// conversation comments wait for the bot's identity; the evicted
				// cache makes the next pass read them again.
				observation.nativeErr = fmt.Errorf("observe native review %s#%d: %w", binding.Repo, binding.PRNumber, identityErr)
				r.dropReviewCache(binding)
			}
			observation.externalFindings, observation.externalSkipped = buildExternalFindings(
				reviewObs, binding, conversationHead, botUserID)
		}
	}
	// Completion compares the unit's effective head, which a proven rebase
	// moves off the ready item's head (issue #2129), so the issue that
	// completes the unit is observed against that head, not the ready one.
	unitExact := unitBinding != nil && pullFact.RepositoryID == unitBinding.RepositoryID &&
		pullFact.PRNumber == unitBinding.PRNumber && pullFact.BaseRef == unitBinding.BaseRef &&
		pullFact.HeadSHA == unitBinding.HeadSHA
	if declaration != nil && unitBinding != nil && !completed && unitExact && pullFact.Merged &&
		declaration.CompletionCriterion == domain.CompletionBoundIssueClosedByMergedPR &&
		declaration.BoundIssue != nil {
		if r.issue == nil {
			return activeResourceObservation{}, errors.New("bound-issue completion has no issue observer")
		}
		issueObserved, err := r.issue(ctx, binding.Repo, *declaration.BoundIssue)
		if err != nil {
			return activeResourceObservation{}, fmt.Errorf("observe issue %s#%d: %w",
				binding.Repo, *declaration.BoundIssue, err)
		}
		recheck, err := r.Pull(ctx, binding.Repo, binding.PRNumber)
		if err != nil {
			return activeResourceObservation{}, fmt.Errorf("re-verify repository identity %s: %w", binding.Repo, err)
		}
		if err := validateObservedPullIdentityCoordinates(recheck.BaseRepoID, recheck.Number); err != nil {
			return activeResourceObservation{}, fmt.Errorf("re-verify repository identity %s: %w",
				binding.Repo, err)
		}
		if recheck.Number != binding.PRNumber || recheck.BaseRepoID != binding.RepositoryID {
			// The exact pull observed at the start of the pass remains a valid fact,
			// but the intervening issue observation cannot support completion after
			// the path stops resolving to the bound identity. Withdraw readiness and
			// persist neither the issue observation nor completion (issue #514).
			observation.invalidation = readinessIdentityInvalidationFor(
				binding, recheck.BaseRepoID, recheck.Number, observedAt,
			)
		} else {
			if issueObserved.Number != *declaration.BoundIssue {
				return activeResourceObservation{}, fmt.Errorf("observe issue %s#%d returned number %d",
					binding.Repo, *declaration.BoundIssue, issueObserved.Number)
			}
			issueFact := domain.IssueStateFact{
				Repo: binding.Repo, RepositoryID: pullFact.RepositoryID,
				IssueNumber: issueObserved.Number, State: domain.IssueState(issueObserved.State),
				ClosedByCommitSHA: issueObserved.ClosedByCommitSHA, ObservedAt: observedAt,
			}
			if err := issueFact.Validate(); err != nil {
				return activeResourceObservation{}, fmt.Errorf("observe issue %s#%d: %w",
					binding.Repo, *declaration.BoundIssue, err)
			}
			observation.issue = &issueFact
		}
	}
	if declaration != nil && unitBinding != nil {
		if completion, ok := domain.EvaluateWorkUnitCompletion(
			*declaration, *unitBinding, pullFact, observation.issue,
		); ok {
			observation.completion = &completion
		}
	}
	// An unmerged close can never satisfy a merge criterion. A merged PR is
	// conclusive only when the declared criterion is also durable: GitHub may
	// expose the merge before its automatic issue-closing side effect, so the
	// bound-issue resource must stay active until that second observation lands.
	if !completionOnly && observation.exactClosed {
		observation.conclude = !pullFact.Merged || declaration == nil ||
			observation.completed || observation.completion != nil
	}
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		latest, err := tx.LatestPullMergeFact(ctx, pullFact.RepositoryID, pullFact.PRNumber)
		switch {
		case errors.Is(err, store.ErrNotFound):
			observation.material = true
		case err != nil:
			return err
		case pullFact.MaterialChangeFrom(latest):
			observation.material = true
		}
		if observation.issue == nil {
			return nil
		}
		latestIssue, err := tx.LatestIssueStateFact(ctx, observation.issue.RepositoryID, observation.issue.IssueNumber)
		switch {
		case errors.Is(err, store.ErrNotFound):
			observation.material = true
		case err != nil:
			return err
		case observation.issue.MaterialChangeFrom(latestIssue):
			observation.material = true
		}
		return nil
	}); err != nil {
		// The observation is discarded with the review activity it fetched.
		if reviewFetched {
			r.dropReviewCache(binding)
		}
		return activeResourceObservation{}, err
	}
	return observation, nil
}

// dropReviewCache evicts the review observer's validators for a pull request
// whose fetched activity was not stored. The observer advanced its ETags on
// the fetch, so without this the next pass would be answered 304 and the
// unstored activity would stay unread until the pull request changed again.
func (r ActiveResourceReconciler) dropReviewCache(binding domain.ReadyItemPRBinding) {
	if r.reviewInvalidate != nil {
		r.reviewInvalidate(binding.Repo, binding.PRNumber)
	}
}

func (r ActiveResourceReconciler) commit(ctx context.Context, observation activeResourceObservation) error {
	return r.Store.Write(ctx, func(tx *store.WriteTx) error {
		var binding domain.ReadyItemPRBinding
		var err error
		if observation.held {
			var heldBinding domain.HeldItemPRBinding
			heldBinding, err = tx.GetHeldItemPRBinding(ctx, observation.itemID)
			binding = domain.ReadyItemPRBinding(heldBinding)
		} else {
			binding, err = tx.GetReadyItemPRBinding(ctx, observation.itemID)
		}
		if err != nil {
			return err
		}
		if binding != observation.binding {
			return fmt.Errorf("ready resource binding changed during reconciliation: %w", store.ErrImmutableConflict)
		}
		if observation.pull != nil {
			if _, err := tx.AppendPullMergeFact(ctx, *observation.pull); err != nil {
				return err
			}
		}
		if observation.issue != nil {
			if _, err := tx.AppendIssueStateFact(ctx, *observation.issue); err != nil {
				return err
			}
		}
		if observation.completion != nil {
			// Appends coalesce an observation that repeats the latest material
			// state. Derive from the rows visible after those appends, not the
			// poll's timestamps, so a shared deterministic PR yields a completion
			// the reconstruction gate can reproduce exactly.
			declaration, err := tx.GetWorkUnitDeclaration(ctx, observation.completion.UnitID)
			if err != nil {
				return err
			}
			unitBinding, err := tx.EffectiveWorkUnitPRBinding(ctx, observation.completion.UnitID)
			if err != nil {
				return err
			}
			persistedPull, err := tx.LatestPullMergeFact(ctx, unitBinding.RepositoryID, unitBinding.PRNumber)
			if err != nil {
				return err
			}
			var persistedIssue *domain.IssueStateFact
			if declaration.CompletionCriterion == domain.CompletionBoundIssueClosedByMergedPR {
				if declaration.BoundIssue == nil {
					return errors.New("bound-issue completion criterion has no bound issue")
				}
				issue, err := tx.LatestIssueStateFact(ctx, unitBinding.RepositoryID, *declaration.BoundIssue)
				if err != nil {
					return err
				}
				persistedIssue = &issue
			}
			completion, ok := domain.EvaluateWorkUnitCompletion(
				declaration, unitBinding, persistedPull, persistedIssue,
			)
			if !ok {
				return errors.New("persisted resource facts do not support observed work-unit completion")
			}
			persisted, err := tx.GetWorkUnitCompletion(ctx, completion.UnitID)
			if errors.Is(err, store.ErrNotFound) {
				if err := tx.RecordWorkUnitCompletion(ctx, completion); err != nil {
					return err
				}
				persisted = completion
			} else if err != nil {
				return err
			}
			if err := appendWorkUnitCompletedMilestone(ctx, tx, declaration.RunID, persisted); err != nil {
				return err
			}
			// Record the completion as a task lifecycle fact (issue #1318 D3).
			if err := tx.RecordTaskCompletion(ctx, declaration.RunID, persisted.UnitID, time.Now().UTC()); err != nil {
				return err
			}
		}
		if observation.completionOnly {
			return nil
		}
		item, err := tx.GetAttentionItem(ctx, observation.itemID)
		if err != nil {
			return err
		}
		if item.Status == domain.StatusOpen {
			switch {
			case observation.invalidation != nil:
				// The pass no longer describes the live candidate: supersede the
				// item and record why in the same transaction, so the staleness is
				// item-visible and the version bump stales any command prepared
				// against the old ready claim (plan §7, issue #496).
				item.ReadinessInvalidation = observation.invalidation
				item, err = item.ConcludeByDaemon(domain.ConcludedBySupersession, time.Now().UTC(),
					readinessInvalidationConclusion(observation.invalidation.Reason), nil)
				if err != nil {
					return err
				}
				item.ItemVersion++
				if err := tx.PutAttentionItem(ctx, item); err != nil {
					return err
				}
				// A head change re-enters verification and review in this
				// transaction (issue #502); any other invalidation, and a unit
				// this pass just completed, starts nothing and stays with a
				// person. A handed-back pull request's new head waits for the
				// hand-back cycle instead, which adopts it and also hands it
				// off (issue #3160).
				if observation.handBack {
					break
				}
				if _, err := engine.StartReadinessReentry(ctx, tx, item, r.readinessGate); err != nil {
					return err
				}
			case observation.conclude:
				item, err = item.ConcludeByDaemon(domain.ConcludedByDaemon, time.Now().UTC(),
					"Resolved when the pull request was merged or closed.", nil)
				if err != nil {
					return err
				}
				item.ItemVersion++
				if err := tx.PutAttentionItem(ctx, item); err != nil {
					return err
				}
			default:
				// Still open and neither concluded nor invalidated: the pull fact
				// (and any issue fact) is recorded, but the schedules stay armed.
				return nil
			}
		}
		settledAt := time.Time{}
		if observation.pull != nil {
			settledAt = observation.pull.ObservedAt
		} else if observation.invalidation != nil {
			settledAt = observation.invalidation.ObservedAt
		}
		if settledAt.IsZero() {
			return errors.New("active resource commit has no observation timestamp")
		}
		return concludePublicationSchedules(ctx, tx, item.ID, settledAt)
	})
}

func readinessIdentityInvalidationFor(
	binding domain.ReadyItemPRBinding, repositoryID int64, prNumber int,
	observedAt time.Time,
) *domain.ReadinessInvalidation {
	return &domain.ReadinessInvalidation{
		Reason:     domain.ReadinessInvalidationIdentityChanged,
		Bound:      fmt.Sprintf("%d#%d", binding.RepositoryID, binding.PRNumber),
		Observed:   fmt.Sprintf("%d#%d", repositoryID, prNumber),
		ObservedAt: observedAt,
	}
}

// readinessInvalidationFor derives the readiness-invalidation fact for a ready
// item whose observed pull is provably this PR (the caller has matched
// repository and number) but no longer matches its binding, naming the target
// ref before the head. The caller establishes divergence, so the default is
// unreachable and returns nil rather than a fact that would fail validation.
func readinessInvalidationFor(
	binding domain.ReadyItemPRBinding, pull domain.PullMergeFact,
) *domain.ReadinessInvalidation {
	inv := &domain.ReadinessInvalidation{ObservedAt: pull.ObservedAt}
	switch {
	case pull.BaseRef != binding.BaseRef:
		inv.Reason = domain.ReadinessInvalidationRetargeted
		inv.Bound = binding.BaseRef
		inv.Observed = pull.BaseRef
	case pull.HeadSHA != binding.HeadSHA:
		inv.Reason = domain.ReadinessInvalidationHeadChanged
		inv.Bound = binding.HeadSHA
		inv.Observed = pull.HeadSHA
	default:
		return nil
	}
	return inv
}

func publicationScheduleIDs(itemID domain.ItemID) []domain.ScheduleID {
	return []domain.ScheduleID{
		engine.PublicationWatchScheduleID(domain.SchedulePRChecksDeadline, itemID),
		engine.PublicationWatchScheduleID(domain.ScheduleReviewWaitThreshold, itemID),
	}
}

func concludePublicationSchedules(
	ctx context.Context, tx *store.WriteTx, itemID domain.ItemID, concludedAt time.Time,
) error {
	for _, id := range publicationScheduleIDs(itemID) {
		schedule, err := tx.GetSchedule(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			continue
		case err != nil:
			return err
		case schedule.Status.Terminal():
			continue
		}
		concluded, err := schedule.Concluded(
			domain.ScheduleResolved, domain.ResolutionSubjectConcluded, concludedAt,
		)
		if err != nil {
			return err
		}
		if err := tx.PutSchedule(ctx, concluded); err != nil {
			return err
		}
		if err := tx.DeleteScheduleTimer(ctx, id); err != nil {
			return err
		}
		pending, err := tx.ListPendingScheduleOccurrences(ctx)
		if err != nil {
			return err
		}
		for _, occurrence := range pending {
			if occurrence.ScheduleID != id {
				continue
			}
			if _, err := tx.ConsumeScheduleOccurrence(
				ctx, occurrence.ScheduleID, occurrence.Generation,
				occurrence.NominalFireAt, domain.OutcomeConditionNoLongerApplies,
				concludedAt.UTC(),
			); err != nil {
				return err
			}
		}
	}
	return nil
}

// readinessInvalidationConclusion is the closed card's sentence for a ready
// item superseded because its pass stopped describing the live candidate.
func readinessInvalidationConclusion(reason domain.ReadinessInvalidationReason) string {
	switch reason {
	case domain.ReadinessInvalidationHeadChanged:
		return "Superseded because the pull request head changed after the review passed."
	case domain.ReadinessInvalidationBaseAdvanced:
		return "Superseded because the target branch advanced past the base the review ran against."
	case domain.ReadinessInvalidationRetargeted:
		return "Superseded because the pull request was retargeted at another branch."
	case domain.ReadinessInvalidationIdentityChanged:
		return "Superseded because the observed pull request no longer matches the one this item names."
	}
	return "Superseded because the review pass no longer describes the pull request."
}
