package engine

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// V1TaskSubmitter composes new commands the way the daemon did before client
// submissions froze recipe v2, so a test can record a v1 command and prove it
// replays unchanged through the current submitter.
type V1TaskSubmitter struct{ *TaskSubmitter }

func (s V1TaskSubmitter) SubmitTask(ctx context.Context, tx *store.WriteTx, in signet.TaskSubmissionInput) (signet.TaskSubmissionResult, error) {
	return s.submit(ctx, tx, in, clientPublicationRecipeV1)
}
