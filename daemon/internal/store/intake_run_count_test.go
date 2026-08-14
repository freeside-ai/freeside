package store_test

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestCountActiveProjectRuns proves the WIP-cap projection counts exactly the
// non-final runs of one project, using the same milestone-derived conclusion
// (domain.ConcludeRun) the run-observation surface reports: pending runs count,
// published/blocked/failed runs do not, and another project's runs never leak
// into the count.
func TestCountActiveProjectRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})

	putRun := func(tx *store.WriteTx, id, project string) {
		if err := tx.PutRun(ctx, domain.Run{
			ID: domain.RunID(id), ProjectID: domain.ProjectID(project),
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		}); err != nil {
			t.Fatalf("put run %s: %v", id, err)
		}
	}
	milestone := func(tx *store.WriteTx, runID string, m domain.RunMilestone) {
		m.RunID = domain.RunID(runID)
		m.RecordedAt = intakeStoreTS
		if err := tx.AppendRunMilestone(ctx, m); err != nil {
			t.Fatalf("append milestone %s to %s: %v", m.Kind, runID, err)
		}
	}
	inv := domain.InvocationID("inv-1")
	completed := domain.ObservedStatusCompleted
	failed := domain.ObservedStatusFailed
	blockReason := domain.HoldPublicationEnvironment

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		// project-1: two active, three concluded.
		putRun(tx, "run-submitted", "project-1")
		milestone(tx, "run-submitted", domain.RunMilestone{Kind: domain.MilestoneRunSubmitted, InvocationID: &inv})

		putRun(tx, "run-completed-stage", "project-1")
		milestone(tx, "run-completed-stage", domain.RunMilestone{
			Kind: domain.MilestoneTerminalRecorded, InvocationID: &inv, Terminal: &completed,
		})

		putRun(tx, "run-published", "project-1")
		milestone(tx, "run-published", domain.RunMilestone{Kind: domain.MilestonePublicationReady, InvocationID: &inv})

		putRun(tx, "run-blocked", "project-1")
		milestone(tx, "run-blocked", domain.RunMilestone{
			Kind: domain.MilestonePublicationBlocked, InvocationID: &inv, Reason: &blockReason,
		})

		putRun(tx, "run-failed", "project-1")
		milestone(tx, "run-failed", domain.RunMilestone{
			Kind: domain.MilestoneTerminalRecorded, InvocationID: &inv, Terminal: &failed,
		})

		// project-2: an active run that must not leak into project-1's count.
		putRun(tx, "run-other", "project-2")
		milestone(tx, "run-other", domain.RunMilestone{Kind: domain.MilestoneRunSubmitted, InvocationID: &inv})
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	assertCount := func(project string, want int) {
		var got int
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, err = tx.CountActiveProjectRuns(ctx, domain.ProjectID(project))
			return err
		}); err != nil {
			t.Fatalf("count active runs for %s: %v", project, err)
		}
		if got != want {
			t.Errorf("active runs for %s = %d, want %d", project, got, want)
		}
	}
	assertCount("project-1", 2)
	assertCount("project-2", 1)
	assertCount("project-unknown", 0)
}
