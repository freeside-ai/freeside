package store

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestRunUnderTheRetiredCheckStillObserves: a run held under the check plan
// revision 97 removed still observes, so the run listings that read every
// run's hold keep reading.
func TestRunUnderTheRetiredCheckStillObserves(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/retired-check.db", Options{})
	run := domain.Run{ID: "run-1", ProjectID: "project", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	agent := "implementer-agent"
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.RecordRunHold(ctx, domain.RunHoldObservation{
			RunID: run.ID, Reason: domain.HoldAdmissionPolicyRefused,
			Detail:          &domain.AdmissionRefusal{Check: domain.AdmissionCheckAgentPrompt, Agent: &agent},
			FirstObservedAt: at, LastObservedAt: at,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE run_hold_observations SET detail_check = 'agent_attended_mark'`); err != nil {
		t.Fatal(err)
	}
	var observed domain.RunObservation
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		observed, err = tx.ObserveRun(ctx, run.ID)
		return err
	}); err != nil {
		t.Fatalf("ObserveRun under the retired check: %v", err)
	}
	if observed.Hold == nil || observed.Hold.Reason != domain.HoldAdmissionPolicyRefused || observed.Hold.Detail != nil {
		t.Fatalf("observed hold = %+v, want admission_policy_refused with no detail", observed.Hold)
	}
}
