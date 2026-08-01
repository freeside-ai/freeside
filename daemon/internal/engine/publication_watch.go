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
// kinds: a durably ready_for_final_review item arms the PR-checks deadline,
// the review-wait threshold, and the base-advance staleness watch, all
// bound to the item and its version. Arming is convergent (scheduler
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
	// DefaultBaseAdvanceInterval is the base-advance staleness watch's
	// recurring observation cadence.
	DefaultBaseAdvanceInterval = 15 * time.Minute
)

// PublicationWatchScheduleID is the deterministic identity of one watch
// kind bound to one item.
func PublicationWatchScheduleID(kind domain.ScheduleKind, itemID domain.ItemID) domain.ScheduleID {
	return domain.ScheduleID(fmt.Sprintf("schedule-%s-%s", kind, itemID))
}

// armPublicationWatches converges the three watch schedules for one live
// ready_for_final_review item. A concluded item arms nothing: its watches
// either never existed or resolve with recorded proof at their next fire.
func armPublicationWatches(
	ctx context.Context,
	st *store.Store,
	item domain.AttentionItem,
	repo, baseRef, admittedBaseSHA string,
	now time.Time,
) error {
	if item.Status != domain.StatusOpen {
		return nil
	}
	now = now.UTC()
	itemID := item.ID
	version := item.ItemVersion
	subject := domain.ScheduleSubject{
		Type:   domain.ScheduleSubjectAttentionItem,
		ItemID: &itemID, ItemVersion: &version,
	}
	checksFireAt := now.Add(DefaultPRChecksDeadline)
	reviewFireAt := now.Add(DefaultReviewWaitThreshold)
	watchInterval := int64(DefaultBaseAdvanceInterval / time.Second)
	inputs := []domain.ScheduleInput{
		{
			ID:        PublicationWatchScheduleID(domain.SchedulePRChecksDeadline, item.ID),
			ProjectID: item.ProjectID, Kind: domain.SchedulePRChecksDeadline,
			Subject: subject, CreatedAt: now, FireAt: &checksFireAt,
		},
		{
			ID:        PublicationWatchScheduleID(domain.ScheduleReviewWaitThreshold, item.ID),
			ProjectID: item.ProjectID, Kind: domain.ScheduleReviewWaitThreshold,
			Subject: subject, CreatedAt: now, FireAt: &reviewFireAt,
		},
		{
			ID:        PublicationWatchScheduleID(domain.ScheduleBaseAdvanceWatch, item.ID),
			ProjectID: item.ProjectID, Kind: domain.ScheduleBaseAdvanceWatch,
			Subject: subject, CreatedAt: now, IntervalSeconds: &watchInterval,
			BaseWatch: &domain.ScheduleBaseWatch{
				Repo: repo, BaseRef: baseRef, AdmittedBaseSHA: admittedBaseSHA,
			},
		},
	}
	return st.Write(ctx, func(tx *store.WriteTx) error {
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
			first := now
			if desired.IntervalSeconds != nil {
				first = now.Add(time.Duration(*desired.IntervalSeconds) * time.Second)
			}
			if err := scheduler.Converge(ctx, tx, desired, first); err != nil {
				return fmt.Errorf("arm publication watch %s: %w", in.ID, err)
			}
		}
		return nil
	})
}
