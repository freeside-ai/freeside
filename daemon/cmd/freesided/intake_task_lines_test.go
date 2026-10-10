package main

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestIntakeAutoStartSetsNoTaskLine holds intake to the operator-only rule
// (plan §5.4, Admitted Agents): choosing an agent chooses which credential
// runs, so an issue, a label, or repository content never sets a task line.
// A proposal admitted from a labeled issue and started by the daemon creates
// a task with no line for any role.
func TestIntakeAutoStartSetsNoTaskLine(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	init := intakeInitiatorFor(t, domain.InitiatorModeAutoStart, domain.ProvenanceOverride, 1)
	r := f.reconciler([]intakeInitiator{init}, labeledOpen(7), nil)

	r.reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Refusal != nil {
		t.Fatalf("unexpected refusal %q", o.Refusal.Reason)
	}
	if !f.started(t, o.Admission.Subject.SpecificationRunID) {
		t.Fatal("authorized auto_start under cap must start specification")
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), o.Admission.Subject.SpecificationRunID)
		if err != nil {
			return err
		}
		lines, err := tx.TaskLines(t.Context(), run.TaskID)
		if err != nil {
			return err
		}
		if len(lines) != 0 {
			t.Fatalf("intake-started task %s has task lines %+v", run.TaskID, lines)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the started task's lines: %v", err)
	}
}
