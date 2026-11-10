package daemoncompose

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// workUnitCompletedMilestones returns the run's work_unit_completed
// milestones as the observation reader reconstructs them.
func workUnitCompletedMilestones(t *testing.T, st *store.Store, runID domain.RunID) []domain.RunMilestone {
	t.Helper()
	var out []domain.RunMilestone
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		observation, err := tx.ObserveRun(context.Background(), runID)
		if err != nil {
			return err
		}
		for _, milestone := range observation.Milestones {
			if milestone.Kind == domain.MilestoneWorkUnitCompleted {
				out = append(out, milestone)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("observe %s: %v", runID, err)
	}
	return out
}

// assertCompletionMilestone pins the completion transaction's observation
// mirror: exactly one work_unit_completed milestone, on the run's publication
// invocation, at the completion record's instant.
func assertCompletionMilestone(t *testing.T, st *store.Store, runID domain.RunID, recordedAt time.Time) {
	t.Helper()
	milestones := workUnitCompletedMilestones(t, st, runID)
	if len(milestones) != 1 {
		t.Fatalf("run %s work_unit_completed milestones = %d, want 1", runID, len(milestones))
	}
	got := milestones[0]
	if got.InvocationID == nil || *got.InvocationID != domain.ProductionPublicationInvocationID(runID) ||
		!got.RecordedAt.Equal(recordedAt) {
		t.Fatalf("run %s milestone = %+v, want publication invocation at %s", runID, got, recordedAt)
	}
}
