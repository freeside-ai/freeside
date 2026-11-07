package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The publication lane is the 1B consumer of the §5.16 watch and deadline
// kinds: a durably ready_for_final_review item arms the PR-checks deadline
// and the review-wait threshold, both bound to the item and its version.
// Nothing watches the target branch: a base advance after handoff starts
// nothing (plan §5.16, revision 110). Arming is convergent (scheduler
// Converge semantics) and runs on the same reconcile passes that ensure the
// item, so a crash between the item and its watches heals on the next pass;
// the watches resolve themselves with recorded proof once the item
// concludes (fire-time subject validation).
//
// The cadences are trusted configuration, not policy or proposal input;
// their 1B values are fixed here and shared by both publication lanes.
const (
	// DefaultPRChecksDeadline bounds how long required checks may stay
	// unconcluded on a published PR before the deadline fires.
	DefaultPRChecksDeadline = 30 * time.Minute
	// DefaultReviewWaitThreshold bounds how long a published PR may wait for
	// its concluding decision before the threshold fires.
	DefaultReviewWaitThreshold = 24 * time.Hour
)

// PublicationWatchDeadline is the fixed duration a publication watch kind
// arms with, so the arming code and the deadline handler that records an
// elapse read one source. It reports false for every other schedule kind.
func PublicationWatchDeadline(kind domain.ScheduleKind) (time.Duration, bool) {
	switch kind {
	case domain.SchedulePRChecksDeadline:
		return DefaultPRChecksDeadline, true
	case domain.ScheduleReviewWaitThreshold:
		return DefaultReviewWaitThreshold, true
	case domain.ScheduleInstallationPoll, domain.ScheduleDoctor, domain.ScheduleJanitor, domain.ScheduleImageUpkeep:
	}
	return 0, false
}

// PublicationWatchScheduleID is the deterministic identity of one watch
// kind bound to one item.
func PublicationWatchScheduleID(kind domain.ScheduleKind, itemID domain.ItemID) domain.ScheduleID {
	return domain.ScheduleID(fmt.Sprintf("schedule-%s-%s", kind, itemID))
}

// armPublicationWatches converges the two deadline schedules for one live
// ready_for_final_review item. A concluded item arms nothing: its watches
// either never existed or resolve with recorded proof at their next fire.
func armPublicationWatches(
	ctx context.Context,
	st *store.Store,
	item domain.AttentionItem,
	now time.Time,
) error {
	now = now.UTC()
	return st.Write(ctx, func(tx *store.WriteTx) error {
		current, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return fmt.Errorf("arm publication watches item %s: %w", item.ID, err)
		}
		if current.Status != domain.StatusOpen {
			return nil
		}
		if current.Subject.Type != domain.SubjectRun || current.Subject.RunID == nil ||
			current.Subject.ID != domain.SubjectID(*current.Subject.RunID) {
			return fmt.Errorf("arm publication watches item %s has no exact run binding",
				current.ID)
		}
		run, err := tx.GetRun(ctx, *current.Subject.RunID)
		if err != nil {
			return fmt.Errorf("arm publication watches run %s: %w", *current.Subject.RunID, err)
		}
		policy, err := tx.GetResolvedPolicy(ctx, run.ID)
		if err != nil {
			return fmt.Errorf("arm publication watches policy %s: %w", run.ID, err)
		}
		if run.ProjectID != current.ProjectID || policy.RunID != run.ID ||
			policy.Digest != run.PolicyDigest {
			return fmt.Errorf("arm publication watches item %s authority mismatch",
				current.ID)
		}
		itemID, version := current.ID, current.ItemVersion
		runID, policyDigest := run.ID, policy.Digest
		subject := domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		}
		checksFireAt := now.Add(DefaultPRChecksDeadline)
		reviewFireAt := now.Add(DefaultReviewWaitThreshold)
		inputs := []domain.ScheduleInput{
			{
				ID:        PublicationWatchScheduleID(domain.SchedulePRChecksDeadline, current.ID),
				ProjectID: current.ProjectID, Kind: domain.SchedulePRChecksDeadline,
				Subject: subject, RunID: &runID, PolicyDigest: &policyDigest,
				CreatedAt: now, FireAt: &checksFireAt,
			},
			{
				ID:        PublicationWatchScheduleID(domain.ScheduleReviewWaitThreshold, current.ID),
				ProjectID: current.ProjectID, Kind: domain.ScheduleReviewWaitThreshold,
				Subject: subject, RunID: &runID, PolicyDigest: &policyDigest,
				CreatedAt: now, FireAt: &reviewFireAt,
			},
		}
		for _, in := range inputs {
			_, err := tx.GetSchedule(ctx, in.ID)
			switch {
			case errors.Is(err, store.ErrNotFound):
			case err != nil:
				return fmt.Errorf("arm publication watch %s: %w", in.ID, err)
			default:
				// Present: an armed watch keeps its original deadline, clock,
				// and subject expectation (this ensure re-runs each reconcile
				// pass with a fresh now, which must not re-derive them); a
				// concluded generation is history, and re-arming is the
				// handlers' stale-event move, never this ensure's.
				continue
			}
			desired, err := domain.NewSchedule(in)
			if err != nil {
				return fmt.Errorf("arm publication watch %s: %w", in.ID, err)
			}
			if err := scheduler.Converge(ctx, tx, desired, now); err != nil {
				return fmt.Errorf("arm publication watch %s: %w", in.ID, err)
			}
		}
		return nil
	})
}
