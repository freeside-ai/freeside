package engine

import (
	"context"
	"fmt"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// productionPublicationProgress names how far a forward-route publication
// task got before this pass, from the durable facts read after import. The
// continuation route returns before they are read. It is computed on each pass and never stored, and it describes the
// state read at the start of the pass: verification writes a checkpoint later
// in the same pass, and nothing after verification may read progress. The
// zero value "" is invalid.
//
// The committed publication outcome and the review state stay out. The
// outcome needs a composed candidate to read, and the review state has its
// own enum, productionReviewState.
type productionPublicationProgress string

const (
	// productionPublicationProgressUnverified has no verification
	// checkpoint: the pass verifies.
	productionPublicationProgressUnverified productionPublicationProgress = "unverified"
	// productionPublicationProgressVerified has a checkpoint but no ready
	// item and no finalized publication intent: the pass skips verification
	// and goes to review.
	productionPublicationProgressVerified productionPublicationProgress = "verified"
	// productionPublicationProgressIntentFinalized has a checkpoint and a
	// finalized publication intent but no ready item: the pass recovers the
	// committed outcome, and goes forward when there is none.
	productionPublicationProgressIntentFinalized productionPublicationProgress = "intent_finalized"
	// productionPublicationProgressReady has a checkpoint and a ready item:
	// the pass recovers the ready outcome and always returns.
	productionPublicationProgressReady productionPublicationProgress = "ready"
)

var allProductionPublicationProgresses = []productionPublicationProgress{
	productionPublicationProgressUnverified,
	productionPublicationProgressVerified,
	productionPublicationProgressIntentFinalized,
	productionPublicationProgressReady,
}

func (p productionPublicationProgress) valid() bool {
	switch p {
	case productionPublicationProgressUnverified, productionPublicationProgressVerified,
		productionPublicationProgressIntentFinalized, productionPublicationProgressReady:
		return true
	default:
		return false
	}
}

// hasCheckpoint reports whether a verification checkpoint existed when
// progress was read.
func (p productionPublicationProgress) hasCheckpoint() bool {
	switch p {
	case productionPublicationProgressUnverified:
		return false
	case productionPublicationProgressVerified, productionPublicationProgressIntentFinalized,
		productionPublicationProgressReady:
		return true
	}
	return false
}

// productionPublicationProgressOf maps the three durable facts to progress;
// the first match wins. A ready item without a checkpoint is no state and
// returns false. Without a checkpoint the finalized intent isn't read, so
// that fact is ignored there.
func productionPublicationProgressOf(
	checkpointFound, readyExists, finalizedIntent bool,
) (productionPublicationProgress, bool) {
	switch {
	case readyExists && !checkpointFound:
		return "", false
	case !checkpointFound:
		return productionPublicationProgressUnverified, true
	case readyExists:
		return productionPublicationProgressReady, true
	case finalizedIntent:
		return productionPublicationProgressIntentFinalized, true
	default:
		return productionPublicationProgressVerified, true
	}
}

// readPublicationProgress reads the verification checkpoint, the ready item,
// and, only when a checkpoint exists, whether the publication intent was
// finalized, in that order. It refuses a ready item without a checkpoint and
// a checkpoint whose import account disagrees with the reconstruction. Every
// read is its own read transaction and nothing here writes.
func (w *productionPublicationWorkflow) readPublicationProgress(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	imported importer.Result,
) (productionPublicationProgress, productionVerificationCheckpoint, error) {
	checkpoint, found, err := w.loadCheckpoint(ctx, task, binding)
	if err != nil {
		return "", productionVerificationCheckpoint{}, err
	}
	readyExists, err := w.hasReadyItemRecord(ctx, task)
	if err != nil {
		return "", productionVerificationCheckpoint{}, err
	}
	finalizedIntent := false
	if found {
		if !reflect.DeepEqual(checkpoint.Imported, imported) {
			return "", productionVerificationCheckpoint{}, fmt.Errorf(
				"verification checkpoint import account disagrees with reconstruction: %w",
				domain.ErrParentKeyMismatch)
		}
		finalizedIntent, err = w.hasFinalizedPublicationIntent(ctx, task.PublicationID)
		if err != nil {
			return "", productionVerificationCheckpoint{}, err
		}
	}
	progress, ok := productionPublicationProgressOf(found, readyExists, finalizedIntent)
	if !ok {
		return "", productionVerificationCheckpoint{}, fmt.Errorf(
			"ready item %q has no verification checkpoint: %w",
			task.readyItemID(), domain.ErrParentKeyMismatch,
		)
	}
	return progress, checkpoint, nil
}
