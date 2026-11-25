package daemoncompose

import (
	"context"
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
func (r *ActiveResourceReconciler) convergeRequiredCheck(
	ctx context.Context, itemID domain.ItemID, binding domain.ReadyItemPRBinding,
	check publish.RequiredCheckObservation, observedAt time.Time,
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
