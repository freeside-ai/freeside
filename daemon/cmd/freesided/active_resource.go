package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Active-resource reconciliation is a process cadence, not a durable
// schedule kind (plan §5.16). An immediate startup pass restores convergence;
// later passes use conditional requests through publish.Reconciler.
const (
	defaultActiveResourceInterval  = 15 * time.Minute
	operatorActiveResourceInterval = time.Minute
)

const activeResourceObservationFailureThreshold = 4

type activeResourceReconciler struct {
	store *store.Store
	pull  pullObserver
	issue issueObserver
	// review observes native (forge-hosted) review activity on the exact ready
	// PR; reviewers is the configured set of reviewer logins whose activity is
	// recorded. Both are optional: a composition that wires no native reviewer
	// (or an empty set) simply records no native evidence (plan §5.16, §7).
	review    nativeReviewObserver
	reviewers map[string]bool
	// reviewInvalidate drops the observer's cached validators for a PR when the
	// durable append of its native observations fails, so the next tick
	// re-fetches unconditionally instead of riding a 304 and permanently
	// dropping the un-persisted rows (issue #497). Optional: nil disables it,
	// which only forfeits the prompt retry a cache-eviction buys.
	reviewInvalidate func(repo string, number int)
	// evictConcluded drops every conditional-request cache entry owned by a
	// ready resource after the item leaves the open state. evictedConcluded
	// records whether the final, post-completion eviction has happened; both
	// are process-local optimizations.
	evictConcluded      func(domain.ReadyItemPRBinding, *int)
	evictedConcluded    map[domain.ItemID]bool
	observationFailures map[domain.ItemID]int
	now                 func() time.Time
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
	foreclosure        *completionForeclosure
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
	failures       []error
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
	declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, readyBinding.RunID)
	if errors.Is(err, store.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state.declaration = &declaration

	binding, err := tx.GetWorkUnitPRBinding(ctx, declaration.ID)
	if errors.Is(err, store.ErrNotFound) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if binding.Repo != readyBinding.Repo ||
		binding.RepositoryID != readyBinding.RepositoryID ||
		binding.PRNumber != readyBinding.PRNumber ||
		binding.BaseRef != readyBinding.BaseRef ||
		binding.HeadSHA != readyBinding.HeadSHA {
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
			state.foreclosure = &completionForeclosure{
				unitID: declaration.ID, binding: binding, pull: pull,
			}
			break
		}
	}
	return state, nil
}

// Run performs one startup pass and then re-arms its process-local timer from
// the pass result. An open ready-for-final-review item selects the
// operator-active interval; otherwise the background interval applies. A
// resource failure is reported and isolated to that item; enumeration or
// transaction failures stop the loop because continuing would silently omit
// durable work.
//
// This is the GitHub-facing loop, so it is where a recurring credential or
// rate-limit failure shows up. Reporting it at error severity with a
// timestamp is the whole point: as an unstructured stderr line every
// quarter hour, a persistent 401 was indistinguishable from noise.
func (r activeResourceReconciler) Run(
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
	operatorActive := false
	reconcile := func() (time.Duration, error) {
		result, err := r.Reconcile(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Cancellation mid-pass is shutdown, not a failure: the same
				// reading the engine and scheduler loops already take. The
				// select below logs the single stop record.
				return defaultInterval, nil
			}
			logger.Error("active resource pass failed", "error", err)
			return 0, err
		}
		for _, failure := range result.failures {
			// Isolated per-item failures: the pass converged around them, so
			// they are error severity without being loop-fatal.
			logger.Error("active resource observation failed", "error", failure)
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
			"failures", len(result.failures), "operator_active", result.operatorActive)
		return activeResourceInterval(
			result.operatorActive, defaultInterval, operatorActiveInterval,
		), nil
	}
	nextInterval, err := reconcile()
	if err != nil {
		return err
	}
	timer := time.NewTimer(nextInterval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			logger.Info("active resource reconciler stopped")
			return nil
		case <-r.store.ReadyItemCreated():
			// A new ready item must not wait through an idle interval before it
			// can engage the operator-active cadence. The store emits only after
			// commit; reconciliation remains authoritative if this wake is lost.
		case <-timer.C:
		}
		nextInterval, err = reconcile()
		if err != nil {
			return err
		}
		resetActiveResourceTimer(timer, nextInterval)
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

// Reconcile makes one independent pass over every active ready item.
// Per-resource observation failures remain retryable and do not prevent a
// healthy sibling from converging in the same pass.
func (r *activeResourceReconciler) Reconcile(
	ctx context.Context,
) (activeResourceReconcileResult, error) {
	result := activeResourceReconcileResult{}
	if r.store == nil || r.pull == nil || r.now == nil {
		return result, errors.New("active resource reconciler is not fully configured")
	}
	var snapshots []store.Snapshotted[domain.AttentionItem]
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		snapshots, err = tx.ListAttentionItems(ctx)
		return err
	}); err != nil {
		return result, fmt.Errorf("list active ready resources: %w", err)
	}
	result.failures = make([]error, 0)
	for _, snapshot := range snapshots {
		item := snapshot.Value
		if item.Type != domain.AttentionReadyForFinalReview {
			continue
		}
		if item.Status != domain.StatusOpen {
			delete(r.observationFailures, item.ID)
			if err := r.convergeObservationHealth(ctx, snapshots, item, true); err != nil {
				return result, fmt.Errorf("resolve ready resource observation health %s: %w", item.ID, err)
			}
			observation, err := r.observeCompletionOnly(ctx, item, r.now().UTC())
			if err != nil {
				result.failures = append(result.failures,
					fmt.Errorf("recover ready resource completion %s: %w", item.ID, err))
			} else {
				if observation.material || observation.completion != nil {
					if err := r.commit(ctx, observation); err != nil {
						return result, fmt.Errorf("commit ready resource completion %s: %w", item.ID, err)
					}
				}
				if observation.foreclosure != nil {
					if err := r.convergeCompletionForeclosure(ctx, item, *observation.foreclosure); err != nil {
						return result, fmt.Errorf("surface ready resource completion foreclosure %s: %w", item.ID, err)
					}
				}
			}
			if err := r.evictConcludedResource(ctx, item.ID); err != nil {
				result.failures = append(result.failures,
					fmt.Errorf("evict concluded ready resource %s: %w", item.ID, err))
			}
			if err := r.settleSchedules(ctx, item.ID, r.now().UTC()); err != nil {
				return result, fmt.Errorf("settle ready resource %s: %w", item.ID, err)
			}
			continue
		}
		observation, err := r.observe(ctx, item, r.now().UTC())
		if err != nil {
			result.operatorActive = true
			result.failures = append(result.failures,
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
			result.failures = append(result.failures,
				fmt.Errorf("reconcile ready resource %s: %w", item.ID, observation.nativeErr))
		}
		if observation.material || observation.conclude ||
			observation.completion != nil || observation.invalidation != nil {
			if err := r.commit(ctx, observation); err != nil {
				return result, fmt.Errorf("commit ready resource %s: %w", item.ID, err)
			}
			if err := r.evictConcludedResource(ctx, item.ID); err != nil {
				result.failures = append(result.failures,
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
				result.failures = append(result.failures,
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
	}
	return result, nil
}

func completionForeclosureItemID(unitID domain.WorkUnitID) domain.ItemID {
	digest := sha256.Sum256([]byte(unitID))
	return domain.ItemID(fmt.Sprintf("completion-foreclosed-%x", digest[:]))
}

// convergeCompletionForeclosure surfaces the terminal completion refusal once.
// The deterministic ID is checked across every item status so acknowledging the
// notice does not cause a later reconciliation pass to resurrect it.
func (r *activeResourceReconciler) convergeCompletionForeclosure(
	ctx context.Context, ready domain.AttentionItem, foreclosure completionForeclosure,
) error {
	itemID := completionForeclosureItemID(foreclosure.unitID)
	var exists bool
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
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
	createdAt := r.now().UTC()
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	var displayNames *domain.DisplayNames
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
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
	return signet.NewService(r.store).PutItem(ctx, item)
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
func (r *activeResourceReconciler) convergeObservationHealth(
	ctx context.Context,
	snapshots []store.Snapshotted[domain.AttentionItem],
	ready domain.AttentionItem,
	healthy bool,
) error {
	existing := activeResourceObservationHealthItems(snapshots, ready.ID)
	if (healthy && len(existing) == 0) || (!healthy && len(existing) == 1) {
		return nil
	}
	attention := signet.NewService(r.store)
	if healthy {
		for _, item := range existing {
			item.ItemVersion++
			item.Status = domain.StatusResolved
			if err := attention.PutItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}
	if len(existing) > 0 {
		for _, duplicate := range existing[1:] {
			duplicate.ItemVersion++
			duplicate.Status = domain.StatusResolved
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
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
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
	createdAt := r.now().UTC()
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

func (r *activeResourceReconciler) evictConcludedResource(
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
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
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
func (r activeResourceReconciler) commitNativeReview(
	ctx context.Context, observations []domain.NativeReviewObservation,
) error {
	return r.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, observation := range observations {
			if _, err := tx.AppendNativeReviewObservation(ctx, observation); err != nil {
				return err
			}
		}
		return nil
	})
}

func (r activeResourceReconciler) settleSchedules(
	ctx context.Context, itemID domain.ItemID, settledAt time.Time,
) error {
	ids := publicationScheduleIDs(itemID)
	hasArmed := false
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
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
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
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

func (r activeResourceReconciler) observe(
	ctx context.Context, item domain.AttentionItem, observedAt time.Time,
) (activeResourceObservation, error) {
	return r.observeReadyResource(ctx, item, observedAt, false)
}

// observeCompletionOnly reuses the ready-resource trust gates and fact
// construction without applying readiness, review, or item-lifecycle
// semantics. It remains eligible after the item concludes until completion is
// durable; a closed pull remains retryable because GitHub permits reopening it.
func (r activeResourceReconciler) observeCompletionOnly(
	ctx context.Context, item domain.AttentionItem, observedAt time.Time,
) (activeResourceObservation, error) {
	return r.observeReadyResource(ctx, item, observedAt, true)
}

func (r activeResourceReconciler) observeReadyResource(
	ctx context.Context, item domain.AttentionItem, observedAt time.Time, completionOnly bool,
) (activeResourceObservation, error) {
	var (
		binding     domain.ReadyItemPRBinding
		declaration *domain.WorkUnitDeclaration
		unitBinding *domain.WorkUnitPRBinding
		completed   bool
		foreclosure *completionForeclosure
	)
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding, err = tx.GetReadyItemPRBinding(ctx, item.ID)
		if err != nil {
			return err
		}
		if completionOnly {
			state, err := loadCompletionRecoveryState(ctx, tx, binding)
			if err != nil {
				return err
			}
			declaration, unitBinding = state.declaration, state.binding
			completed, foreclosure = state.completed, state.foreclosure
			return nil
		}
		d, err := tx.GetWorkUnitDeclarationByRun(ctx, binding.RunID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		b, err := tx.GetWorkUnitPRBinding(ctx, d.ID)
		if completionOnly && errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if b.Repo != binding.Repo || b.RepositoryID != binding.RepositoryID ||
			b.PRNumber != binding.PRNumber || b.BaseRef != binding.BaseRef ||
			b.HeadSHA != binding.HeadSHA {
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
	}
	if completionOnly && (declaration == nil || unitBinding == nil || completed || foreclosure != nil) {
		return observation, nil
	}
	observed, err := r.pull(ctx, binding.Repo, binding.PRNumber)
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
	if !completionOnly && r.review != nil && exact && pullFact.State == domain.PullRequestOpen {
		reviewObs, err := r.review(ctx, binding.Repo, binding.PRNumber)
		switch {
		case err != nil:
			observation.nativeErr = fmt.Errorf("observe native review %s#%d: %w", binding.Repo, binding.PRNumber, err)
		case !reviewObs.NotModified:
			observation.nativeObservations = buildNativeReviewObservations(reviewObs, binding, r.reviewers, observedAt)
		}
	}
	if declaration != nil && unitBinding != nil && !completed && exact && pullFact.Merged &&
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
		recheck, err := r.pull(ctx, binding.Repo, binding.PRNumber)
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
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
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
		return activeResourceObservation{}, err
	}
	return observation, nil
}

func (r activeResourceReconciler) commit(ctx context.Context, observation activeResourceObservation) error {
	return r.store.Write(ctx, func(tx *store.WriteTx) error {
		binding, err := tx.GetReadyItemPRBinding(ctx, observation.itemID)
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
			unitBinding, err := tx.GetWorkUnitPRBinding(ctx, observation.completion.UnitID)
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
			if _, err := tx.GetWorkUnitCompletion(ctx, completion.UnitID); errors.Is(err, store.ErrNotFound) {
				if err := tx.RecordWorkUnitCompletion(ctx, completion); err != nil {
					return err
				}
			} else if err != nil {
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
				item.Status = domain.StatusSuperseded
				item.ReadinessInvalidation = observation.invalidation
				item.ItemVersion++
				if err := tx.PutAttentionItem(ctx, item); err != nil {
					return err
				}
			case observation.conclude:
				item.Status = domain.StatusResolved
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
		engine.PublicationWatchScheduleID(domain.ScheduleBaseAdvanceWatch, itemID),
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
