package ward

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestRigReviewRecoveryPreservesInterruptedOutcomeWithoutLaunch(t *testing.T) {
	backend, rt, cfg, launch, journal := testCodexReviewLifecycle(t)
	leaser, err := NewRuntimeCodexReviewVolumeLeaser(rt)
	if err != nil {
		t.Fatal(err)
	}
	cfg.VolumeLifecycleLeaser = leaser
	process, err := backend.CodexReview(t.Context(), cfg, launch)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Close() })
	// Match the live failure: the harness already removed the container,
	// leaving its journal, three volumes and network for recovery.
	if err := rt.StopContainer(t.Context(), journal.intent.ReviewContainer); err != nil {
		t.Fatal(err)
	}
	if err := rt.DeleteContainer(t.Context(), journal.intent.ReviewContainer); err != nil {
		t.Fatal(err)
	}
	rt.calls = nil
	resources := codexReviewRuntimeResourceNames(launch.RunID, codexReviewNames(launch.RunID))
	for range 2 {
		if err := RecoverCodexReviewResources(t.Context(), rt, journal, backend.cfg.ExportRoot, resources); err != nil {
			t.Fatal(err)
		}
	}
	if journal.intent.State != CodexReviewIntentClosed || !journal.ready[launch.RunID] {
		t.Fatal("interrupted review did not close with a durable ready outcome")
	}
	outcome := journal.outcomes[launch.RunID]
	if outcome.FailureClass != domain.ReviewFailureTransient || !outcome.AbortRequired {
		t.Fatalf("lost review outcome = %#v", outcome)
	}
	for _, volume := range resources.Volumes {
		if _, exists := rt.vols[volume]; exists {
			t.Errorf("retained volume %s", volume)
		}
	}
	for _, network := range resources.Networks {
		if _, exists := rt.nets[network]; exists {
			t.Errorf("retained network %s", network)
		}
	}
	for _, call := range rt.calls {
		if strings.HasPrefix(call, "create-") || strings.HasPrefix(call, "start-container ") || strings.HasPrefix(call, "export") {
			t.Fatalf("cleanup launched or exported: %s", call)
		}
	}
}

func TestRigReviewRecoveryPreparationAndOwnership(t *testing.T) {
	for _, mode := range []string{"owned", "foreign", "partial-failure", "outside-rig"} {
		t.Run(mode, func(t *testing.T) {
			fx := testCodexReviewPrepRecoveryFixture(t)
			resources := codexReviewRuntimeResourceNames(fx.launch.RunID, fx.names)
			switch mode {
			case "foreign":
				fx.rt.vols[fx.names.shadowVolume].created = "replaced"
			case "partial-failure":
				fx.rt.onDeleteVolume = func(name string) (bool, error) {
					if name == fx.names.shadowVolume {
						return true, errors.New("runtime unavailable")
					}
					return false, nil
				}
			case "outside-rig":
				resources = RuntimeResourceNames{}
			}
			fx.rt.calls = nil
			err := RecoverCodexReviewResources(t.Context(), fx.rt, fx.journal, fx.backend.cfg.ExportRoot, resources)
			if mode == "outside-rig" {
				if err != nil || len(fx.rt.calls) != 0 || fx.journal.intent.State == CodexReviewIntentClosed {
					t.Fatalf("unrelated intent changed: %v, calls=%v", err, fx.rt.calls)
				}
				return
			}
			if mode == "foreign" || mode == "partial-failure" {
				if err == nil || fx.journal.intent.State == CodexReviewIntentClosed {
					t.Fatal("failed cleanup closed the journal")
				}
				if _, exists := fx.rt.vols[fx.names.shadowVolume]; !exists {
					t.Fatal("unproven volume removed")
				}
				if mode == "foreign" {
					return
				}
				fx.rt.onDeleteVolume = nil
				err = RecoverCodexReviewResources(t.Context(), fx.rt, fx.journal, fx.backend.cfg.ExportRoot, resources)
			}
			if err != nil {
				t.Fatal(err)
			}
			if fx.journal.intent.State != CodexReviewIntentClosed {
				t.Fatal("preparation intent still open")
			}
			if len(fx.rt.vols) != 0 {
				t.Fatalf("recovery retained volumes: %v", fx.rt.vols)
			}
		})
	}
}

func TestRigReviewRecoveryLeavesUnrelatedOutcomeUntouched(t *testing.T) {
	fx := testCodexReviewPrepRecoveryFixture(t)
	fx.journal.intent.State = CodexReviewIntentClosed
	fx.journal.outcomes = map[string]CodexReviewSourceOutcome{fx.launch.RunID: {
		InvocationID: domain.InvocationID(fx.launch.RunID), FailureClass: domain.ReviewFailureTransient,
		Failure: "unrelated interrupted review",
	}}
	if err := RecoverCodexReviewResources(t.Context(), fx.rt, fx.journal, fx.backend.cfg.ExportRoot, RuntimeResourceNames{}); err != nil {
		t.Fatal(err)
	}
	if fx.journal.ready[fx.launch.RunID] {
		t.Fatal("unrelated outcome made ready")
	}
}

func TestRigReviewRecoveryOrphanWorkspace(t *testing.T) {
	backend, rt, _, launch, journal := testCodexReviewLifecycle(t)
	resources := codexReviewWorkspaceRuntimeResourceNames(launch.RunID)
	if err := RecoverCodexReviewResources(context.Background(), rt, journal, backend.cfg.ExportRoot, resources); err != nil {
		t.Fatal(err)
	}
	if _, exists := rt.vols[launch.WorkspaceVolume]; exists {
		t.Fatal("orphan workspace retained")
	}
	if journal.workspaceBinding.SourceRunID != "" {
		t.Fatal("orphan binding retained")
	}
}
