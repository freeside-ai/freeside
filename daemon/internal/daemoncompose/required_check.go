package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// requiredCheckObserver reads a head's required check against its base
// branch's rules (publish.Reconciler.ReconcileRequiredCheck).
type requiredCheckObserver func(ctx context.Context, repo, baseRef, head string) (publish.RequiredCheckObservation, error)

// convergeRequiredCheck writes the pass's required-check reading onto the
// open ready item, at its next version, when the reading differs from the
// one the card shows (#3598). Every write advances the client-visible
// revision and stales a command prepared against the old version, so an
// unchanged reading writes nothing. The transaction re-reads the item and
// writes only while it is still open on the head the pass read.
//
// An infrastructure failure under MaxRequiredCheckReruns also enqueues one
// rerun intent per failed run attempt, in the same transaction that raises
// the card's rerun count, unless the pull request carries the hand-back
// label (the lander owns it then) or an intent for one of those attempts
// already exists (it was rerun, or the rerun is pending).
func (r *ActiveResourceReconciler) convergeRequiredCheck(
	ctx context.Context, itemID domain.ItemID, binding domain.ReadyItemPRBinding,
	check publish.RequiredCheckObservation, handBack bool, observedAt time.Time,
) error {
	return r.Store.Write(ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if err != nil {
			return err
		}
		if item.Status != domain.StatusOpen || item.PRHeadSHA != binding.HeadSHA {
			return nil
		}
		next := requiredCheckReading(check, binding.HeadSHA, item.RequiredCheck, observedAt)
		if !handBack && rerunAllowed(next) {
			enqueued, err := enqueueCheckReruns(ctx, tx, itemID, binding, check.FailedRuns)
			if err != nil {
				return err
			}
			if enqueued {
				next.Reruns++
			}
		}
		if sameRequiredCheckReading(item.RequiredCheck, next) {
			return nil
		}
		item.RequiredCheck = &next
		item.ItemVersion++
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return fmt.Errorf("write required check: %w", err)
		}
		return nil
	})
}

func rerunAllowed(reading domain.RequiredCheck) bool {
	return reading.State == domain.RequiredCheckFailed && reading.FailureClass != nil &&
		*reading.FailureClass == domain.CheckFailureInfrastructure &&
		reading.Reruns < domain.MaxRequiredCheckReruns
}

// enqueueCheckReruns enqueues a rerun intent for each failed run attempt and
// reports whether it did. An existing intent for any of them means this
// failure was already handled, so it enqueues none.
func enqueueCheckReruns(
	ctx context.Context, tx *store.WriteTx, itemID domain.ItemID, binding domain.ReadyItemPRBinding,
	runs []publish.CheckRun,
) (bool, error) {
	type intent struct {
		key     string
		payload []byte
	}
	intents := make([]intent, 0, len(runs))
	for _, run := range runs {
		key, payload, err := publish.CheckRerunIntent(itemID, binding.Repo, binding.PRNumber, binding.HeadSHA, run)
		if err != nil {
			return false, err
		}
		_, err = tx.GetOutbox(ctx, key)
		if err == nil {
			return false, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
		intents = append(intents, intent{key: key, payload: payload})
	}
	for _, in := range intents {
		if _, _, err := tx.EnqueueOutbox(ctx, in.key, publish.CheckRerunKind, in.payload); err != nil {
			return false, err
		}
	}
	return len(intents) > 0, nil
}

// requiredCheckReading is the card's reading of an observation on head. The
// rerun count carries over from the stored reading, which is always on the
// same head: a moved head invalidates the item before it is read.
func requiredCheckReading(
	check publish.RequiredCheckObservation, head string, stored *domain.RequiredCheck, observedAt time.Time,
) domain.RequiredCheck {
	reading := domain.RequiredCheck{
		State: check.State, FailureClass: check.FailureClass, HeadSHA: head, ObservedAt: observedAt.UTC(),
	}
	if stored != nil && stored.HeadSHA == head {
		reading.Reruns = stored.Reruns
	}
	for _, job := range check.FailedJobs {
		reading.FailedJobs = append(reading.FailedJobs, job.Name)
	}
	return reading
}

// sameRequiredCheckReading compares what the card shows, ignoring when it
// was read.
func sameRequiredCheckReading(stored *domain.RequiredCheck, next domain.RequiredCheck) bool {
	if stored == nil {
		return false
	}
	sameClass := (stored.FailureClass == nil) == (next.FailureClass == nil) &&
		(stored.FailureClass == nil || *stored.FailureClass == *next.FailureClass)
	return stored.State == next.State && sameClass && stored.HeadSHA == next.HeadSHA &&
		stored.Reruns == next.Reruns && slices.Equal(stored.FailedJobs, next.FailedJobs)
}
