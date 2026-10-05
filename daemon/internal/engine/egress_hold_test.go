package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A writer whose egress profile the composition cannot enforce holds its own
// run and nothing else. The dispatch pass returns no error, so the reconcile
// loop keeps running; it reaches the intent queued behind the refused one;
// and the refused intent stays pending with a typed hold and no attempt or
// admission recorded.
//
// The fixture's policy requests the default profile, so the composition is
// narrowed to make that profile unenforceable. The refusal is the one a
// provider_registry opt-in draws from the production composition.
func TestUnenforceableEgressHoldsOneRunAndThePassContinues(t *testing.T) {
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	driver := f.newDriver(t)
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 0,
		specify.Output{Specification: &specify.Specification{
			Summary: "ready", Body: "# Specification\n\nImplement.",
			Addressals: []specify.Addressal{},
		}}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	if result, err := engine.Reconcile(ctx); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("specification reconcile = %+v, %v", result, err)
	}
	engine.admission.environment.OperatingMode = domain.ModeUnattended
	engine.admission.environment.EnforceableEgressProfiles = []domain.EgressProfile{
		domain.EgressCleanVerification,
	}

	// An undecodable production marker for a later run. The pass quarantines
	// it only if it goes on past the refused intent.
	laterRun := domain.RunID("zz-later-run")
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, domain.Run{
			ID: laterRun, ProjectID: "project-1",
			SpecDigest:   domain.Digest("sha256:" + strings.Repeat("a", 64)),
			PolicyDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
			Stages: []domain.Stage{{
				ID: productionStageID(laterRun), RunID: laterRun,
				Name: productionStageName, Attempts: []domain.Attempt{},
			}},
		})
	}); err != nil {
		t.Fatalf("seed later run: %v", err)
	}
	seedProductionMarker(t, ctx, f.store, laterRun, productionRequestJSON(
		`"version":"freeside.production-invocation/v3",`+
			`"invocation_id":"inv-implement-zz-later-run",`+
			`"run_id":"zz-later-run","stage_id":"implement-zz-later-run"`,
	))

	// The refusal repeats on every pass; none of them may fail the loop.
	for pass := range 3 {
		if started, err := engine.dispatchPendingInvocations(ctx); err != nil || started != 0 {
			t.Fatalf("dispatch pass %d = %d started, err %v; want a quiet hold", pass, started, err)
		}
	}
	requireQuarantineItem(t, ctx, f.store, laterRun)

	implementationID := productionInvocationID("implementation-run")
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		hold, found, err := tx.GetRunHold(ctx, "implementation-run")
		if err != nil {
			return err
		}
		if !found || hold.Reason != domain.HoldAdmissionPolicyRefused {
			t.Errorf("run hold = %+v, found %t; want %q", hold, found, domain.HoldAdmissionPolicyRefused)
		}
		entry, err := tx.GetOutbox(ctx, string(implementationID))
		if err != nil {
			return err
		}
		if entry.Dispatched() {
			t.Error("the refused intent was marked dispatched")
		}
		run, err := tx.GetRun(ctx, "implementation-run")
		if err != nil {
			return err
		}
		if attemptRecorded(run, implementationID) {
			t.Error("the refused intent recorded an attempt")
		}
		if _, err := tx.GetExecutionAdmissionRecord(ctx, implementationID); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("admission record err = %v, want none recorded", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
