package signet_test

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestReadRunObservationTagsAMissingStatusRecord pins that a status milestone
// whose export or outcome row is missing fails ReadRunObservation as an
// integrity fault, the same as one its record contradicts. The store reports
// the missing row as ErrNotFound, not a parent-key mismatch, so the read has
// to tag it itself for readers outside signet to classify it.
func TestReadRunObservationTagsAMissingStatusRecord(t *testing.T) {
	t.Parallel()
	for name, milestone := range map[string]domain.RunMilestone{
		"execution_export_recorded": {Kind: domain.MilestoneExecutionExportRecorded},
		"execution_outcome_recorded": {
			Kind: domain.MilestoneExecutionOutcomeRecorded, Outcome: ptr(domain.ExecutionOutcomeFailed),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f := newCorpusFixture(t)
			runID := domain.RunID("run-1")
			invocation := domain.InvocationID("inv-1")
			run := corpusRun(runID, corpusAttempt(runID, invocation))
			f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) })
			milestone.RunID, milestone.InvocationID, milestone.RecordedAt = runID, &invocation, f.at
			f.appendMilestone(t, milestone)

			err := f.store.Read(ctx, func(tx *store.ReadTx) error {
				_, err := signet.ReadRunObservation(ctx, tx, run)
				return err
			})
			if !errors.Is(err, signet.ErrRunObservationIntegrity) || !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("ReadRunObservation = %v, want ErrRunObservationIntegrity wrapping ErrNotFound", err)
			}
		})
	}
}
