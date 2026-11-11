package engine

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestSubmissionStartsTheTaskAtItsOwnInstant: a submission that starts its
// task stamps the start with the run_submitted milestone's instant (#3039).
// The timeline orders same-instant events by kind, which puts the start below
// the submission; two clock reads microseconds apart would put it above.
func TestSubmissionStartsTheTaskAtItsOwnInstant(t *testing.T) {
	t.Parallel()
	t.Run("specification artifact", func(t *testing.T) {
		t.Parallel()
		f := newSpecificationFixture(t, true, 2)
		f.submit(t)
		requireTaskStartAtSubmission(t, f.store, "specification-run")
	})
	t.Run("issue subject", func(t *testing.T) {
		t.Parallel()
		r := newIssueSubjectReservation(t)
		if _, err := SubmitSpecificationRun(t.Context(), r.store, r.spec); err != nil {
			t.Fatal(err)
		}
		requireTaskStartAtSubmission(t, r.store, r.specificationRunID)
	})
	t.Run("issue subject after a released start", func(t *testing.T) {
		t.Parallel()
		r := newIssueSubjectReservation(t)
		// Label intake admitted the reserved run's start, then released it
		// while the card was undecided (#1770); submission starts it again.
		if err := r.store.Write(t.Context(), func(tx *store.WriteTx) error {
			run, err := tx.GetRun(t.Context(), r.specificationRunID)
			if err != nil {
				return err
			}
			admittedAt := time.Now().UTC().Add(-time.Hour)
			if err := tx.AdmitTaskStart(t.Context(), r.specificationRunID, admittedAt, 1); err != nil {
				return err
			}
			_, err = tx.ReleaseTaskStart(t.Context(), run.TaskID, admittedAt.Add(time.Minute))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := SubmitSpecificationRun(t.Context(), r.store, r.spec); err != nil {
			t.Fatal(err)
		}
		requireTaskStartAtSubmission(t, r.store, r.specificationRunID)
	})
	t.Run("direct production", func(t *testing.T) {
		t.Parallel()
		f := newSpecificationFixture(t, true, 2)
		const runID = domain.RunID("direct-production-run")
		policy, err := domain.NewResolvedPolicy(runID, f.policy.Keys)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SubmitProductionRun(t.Context(), f.store, ProductionRunSpec{
			RunID: runID, ProjectID: "project-1",
			SpecArtifactID: f.source.ID, PolicyArtifactID: f.policyArt.ID, ResolvedPolicy: policy,
			Publication: ProductionPublication{
				Title: "Implement the work item", Body: "Implements the submitted specification.",
				CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
			},
		}); err != nil {
			t.Fatal(err)
		}
		requireTaskStartAtSubmission(t, f.store, runID)
	})
}

// requireTaskStartAtSubmission fails unless the run's task holds a current
// start bound to the run, recorded at the run's run_submitted instant.
func requireTaskStartAtSubmission(t *testing.T, st *store.Store, runID domain.RunID) {
	t.Helper()
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), runID)
		if err != nil {
			return err
		}
		task, err := tx.GetTask(t.Context(), run.TaskID)
		if err != nil {
			return err
		}
		start := task.CurrentStart()
		if start == nil || start.RunID != runID || !domain.TaskWIP(task) {
			t.Fatalf("task start = %+v, want a held start bound to %q", start, runID)
		}
		milestones, err := tx.ListRunMilestones(t.Context(), runID)
		if err != nil {
			return err
		}
		for _, milestone := range milestones {
			if milestone.Kind != domain.MilestoneRunSubmitted {
				continue
			}
			if !milestone.RecordedAt.Equal(start.RecordedAt) {
				t.Fatalf("task started at %s, run submitted at %s; want one instant",
					start.RecordedAt.Format(time.RFC3339Nano), milestone.RecordedAt.Format(time.RFC3339Nano))
			}
			return nil
		}
		t.Fatalf("run %q carries no run_submitted milestone", runID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
