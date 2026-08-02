package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Active-resource reconciliation is a process cadence, not a durable
// schedule kind (plan §5.16). An immediate startup pass restores convergence;
// later passes use conditional requests through publish.Reconciler.
const defaultActiveResourceInterval = 15 * time.Minute

type activeResourceReconciler struct {
	store *store.Store
	pull  pullObserver
	issue issueObserver
	now   func() time.Time
}

type activeResourceObservation struct {
	itemID      domain.ItemID
	binding     domain.ReadyItemPRBinding
	pull        domain.PullMergeFact
	issue       *domain.IssueStateFact
	completion  *domain.WorkUnitCompletion
	completed   bool
	exactClosed bool
	conclude    bool
	material    bool
}

// Run performs one startup pass and then polls on a plain ticker. A resource
// failure is reported and isolated to that item; enumeration or transaction
// failures stop the loop because continuing would silently omit durable work.
func (r activeResourceReconciler) Run(
	ctx context.Context, interval time.Duration, report func(error),
) error {
	if interval <= 0 {
		return fmt.Errorf("active resource interval %s must be positive", interval)
	}
	if report == nil {
		report = func(error) {}
	}
	reconcile := func() error {
		failures, err := r.Reconcile(ctx)
		if err != nil {
			return err
		}
		for _, failure := range failures {
			report(failure)
		}
		return nil
	}
	if err := reconcile(); err != nil {
		return err
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := reconcile(); err != nil {
				return err
			}
		}
	}
}

// Reconcile makes one independent pass over every active ready item.
// Per-resource observation failures remain retryable and do not prevent a
// healthy sibling from converging in the same pass.
func (r activeResourceReconciler) Reconcile(ctx context.Context) ([]error, error) {
	if r.store == nil || r.pull == nil || r.now == nil {
		return nil, errors.New("active resource reconciler is not fully configured")
	}
	var snapshots []store.Snapshotted[domain.AttentionItem]
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		snapshots, err = tx.ListAttentionItems(ctx)
		return err
	}); err != nil {
		return nil, fmt.Errorf("list active ready resources: %w", err)
	}
	failures := make([]error, 0)
	for _, snapshot := range snapshots {
		item := snapshot.Value
		if item.Type != domain.AttentionReadyForFinalReview {
			continue
		}
		if item.Status != domain.StatusOpen {
			if err := r.settleSchedules(ctx, item.ID, r.now().UTC()); err != nil {
				return failures, fmt.Errorf("settle ready resource %s: %w", item.ID, err)
			}
			continue
		}
		observation, err := r.observe(ctx, item, r.now().UTC())
		if err != nil {
			failures = append(failures, fmt.Errorf("reconcile ready resource %s: %w", item.ID, err))
			continue
		}
		if !observation.material && !observation.conclude && observation.completion == nil {
			continue
		}
		if err := r.commit(ctx, observation); err != nil {
			return failures, fmt.Errorf("commit ready resource %s: %w", item.ID, err)
		}
	}
	return failures, nil
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
	var (
		binding     domain.ReadyItemPRBinding
		declaration *domain.WorkUnitDeclaration
		unitBinding *domain.WorkUnitPRBinding
		completed   bool
	)
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding, err = tx.GetReadyItemPRBinding(ctx, item.ID)
		if err != nil {
			return err
		}
		d, err := tx.GetWorkUnitDeclarationByRun(ctx, binding.RunID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		b, err := tx.GetWorkUnitPRBinding(ctx, d.ID)
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
	observed, err := r.pull(ctx, binding.Repo, binding.PRNumber)
	if err != nil {
		return activeResourceObservation{}, fmt.Errorf("observe pull %s#%d: %w", binding.Repo, binding.PRNumber, err)
	}
	if observed.Number != binding.PRNumber {
		return activeResourceObservation{}, fmt.Errorf("observe pull %s#%d returned number %d",
			binding.Repo, binding.PRNumber, observed.Number)
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
	observation := activeResourceObservation{
		itemID: item.ID, binding: binding, pull: pullFact,
		completed: completed, exactClosed: exact && pullFact.State == domain.PullRequestClosed,
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
		if issueObserved.Number != *declaration.BoundIssue {
			return activeResourceObservation{}, fmt.Errorf("observe issue %s#%d returned number %d",
				binding.Repo, *declaration.BoundIssue, issueObserved.Number)
		}
		recheck, err := r.pull(ctx, binding.Repo, binding.PRNumber)
		if err != nil {
			return activeResourceObservation{}, fmt.Errorf("re-verify repository identity %s: %w", binding.Repo, err)
		}
		if recheck.Number != binding.PRNumber || recheck.BaseRepoID != pullFact.RepositoryID {
			return activeResourceObservation{}, fmt.Errorf("repository %s changed identity during reconciliation", binding.Repo)
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
	if observation.exactClosed {
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
		if _, err := tx.AppendPullMergeFact(ctx, observation.pull); err != nil {
			return err
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
		item, err := tx.GetAttentionItem(ctx, observation.itemID)
		if err != nil {
			return err
		}
		if observation.conclude && item.Status == domain.StatusOpen {
			item.Status = domain.StatusResolved
			item.ItemVersion++
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		if item.Status == domain.StatusOpen && !observation.conclude {
			return nil
		}
		return concludePublicationSchedules(ctx, tx, item.ID, observation.pull.ObservedAt)
	})
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
