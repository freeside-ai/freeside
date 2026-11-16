package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// leftContainerRuntime is a container runtime holding no builder and four
// containers: probe-left runs under reservation probe-kept; probe-done,
// stopped, carries probe-gone's label; and the registries for ports 5104 and
// 5105 are stopped and running.
const leftContainerRuntime = `#!/bin/sh
case "$1 $2" in
"list --all") printf '%s' '[{"id":"probe-left","configuration":{"id":"probe-left"}},{"id":"probe-done","configuration":{"id":"probe-done"}},{"id":"registry-stopped","configuration":{"id":"registry-stopped"}},{"id":"registry-running","configuration":{"id":"registry-running"}}]' ;;
"inspect probe-left") printf '%s' '[{"id":"probe-left","configuration":{"id":"probe-left","labels":{"ai.freeside.reservation":"probe-kept"}},"status":{"state":"running"}}]' ;;
"inspect probe-done") printf '%s' '[{"id":"probe-done","configuration":{"id":"probe-done","labels":{"ai.freeside.reservation":"probe-gone"}},"status":{"state":"stopped"}}]' ;;
"inspect registry-stopped") printf '%s' '[{"id":"registry-stopped","configuration":{"id":"registry-stopped","labels":{"ai.freeside.project-image.registry-port":"5104"}},"status":{"state":"stopped"}}]' ;;
"inspect registry-running") printf '%s' '[{"id":"registry-running","configuration":{"id":"registry-running","labels":{"ai.freeside.project-image.registry-port":"5105"}},"status":{"state":"running"}}]' ;;
"inspect "*) echo "Error: container not found: $2"; exit 1 ;;
*) exit 0 ;;
esac
`

// TestRecoverDaemonReservationsReleasesOnlyWhatItProves: a start clears the
// waits an earlier process left and releases each reservation whose
// containers it proves gone: a build whose builder is stopped and whose label
// marks no container, a registry the listing does not find or finds stopped,
// and a probe whose only container is stopped, which holds no memory. A proof
// pass whose container still runs, and a running registry, keep theirs.
func TestRecoverDaemonReservationsReleasesOnlyWhatItProves(t *testing.T) {
	t.Parallel()
	runtime := filepath.Join(t.TempDir(), "container")
	execfixture.Write(t, runtime, []byte(leftContainerRuntime), 0o755)
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	bound := ward.LaunchBound{BudgetMiB: 16384, RuntimeVersion: ward.RuntimeNone}
	// The earlier process reserved; this start holds none of its work.
	earlier := &engine.DaemonMemory{Store: st, Bound: bound, Now: time.Now}
	memory := &engine.DaemonMemory{Store: st, Bound: bound, Now: time.Now}
	for id, kind := range map[domain.InvocationID]domain.LaunchReservationKind{
		"image-build-left":                       domain.ReservationImageBuild,
		projectimage.RegistryReservationID(5103): domain.ReservationImageRegistry,
		projectimage.RegistryReservationID(5104): domain.ReservationImageRegistry,
		projectimage.RegistryReservationID(5105): domain.ReservationImageRegistry,
		"probe-kept":                             domain.ReservationProofPass,
		"probe-gone":                             domain.ReservationProbe,
	} {
		if _, err := earlier.TryReserve(t.Context(), engine.DaemonWork{
			Kind: kind, ID: id, Size: ward.ContainerSize{CPUs: 1, MemoryMiB: 1024}, VMs: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := memory.Store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordDaemonLaunchWait(t.Context(), "waiter", time.Now())
	}); err != nil {
		t.Fatal(err)
	}

	if err := recoverDaemonReservations(t.Context(), memory, nil, runtime); err != nil {
		t.Fatal(err)
	}
	open, err := memory.ListLeft(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var ids []domain.InvocationID
	for _, r := range open {
		ids = append(ids, r.InvocationID)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, []domain.InvocationID{projectimage.RegistryReservationID(5105), "probe-kept"}) {
		t.Fatalf("open after recovery = %q, want only the running registry and the proof pass with a running container", ids)
	}
	var waiting bool
	if err := memory.Store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, waiting, err = tx.LaunchWaitRefusedAt(t.Context(), "waiter")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if waiting {
		t.Fatal("a start kept an earlier process's wait")
	}
}

// labelRuntime is a container runtime holding one running container whose
// reservation label is the contents of the file label beside it, or no
// container while that file is absent.
const labelRuntime = `#!/bin/sh
label="$(dirname "$0")/label"
case "$1 $2" in
"list --all") if [ -f "$label" ]; then printf '%s' '[{"id":"left","configuration":{"id":"left"}}]'; else printf '[]'; fi ;;
"inspect left") printf '[{"id":"left","configuration":{"id":"left","labels":{"ai.freeside.reservation":"%s"}},"status":{"state":"running"}}]' "$(cat "$label")" ;;
*) exit 0 ;;
esac
`

func newWorkRunner(t *testing.T, budgetMiB int) (daemonWorkRunner, string) {
	t.Helper()
	dir := t.TempDir()
	runtime := filepath.Join(dir, "container")
	execfixture.Write(t, runtime, []byte(labelRuntime), 0o755)
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	return daemonWorkRunner{
		store: st, running: engine.NewRunningWork(), containerPath: runtime,
		memory: &engine.DaemonMemory{
			Store: st, Bound: ward.LaunchBound{BudgetMiB: budgetMiB, RuntimeVersion: ward.RuntimeNone}, Now: time.Now,
		},
	}, filepath.Join(dir, "label")
}

func passWork(id domain.InvocationID) engine.DaemonWork {
	return engine.DaemonWork{Kind: domain.ReservationProofPass, ID: id, Size: proofPassSize, VMs: proofPassVMs}
}

// TestDaemonWorkRunnerWaitsBeforeThePassRuns: a waiting proof pass runs
// nothing until its memory is granted, so the proof it would replace stands
// while it waits; once granted it runs under its reservation, counted as a
// running proof pass, and releases once no container with its label runs.
func TestDaemonWorkRunnerWaitsBeforeThePassRuns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		runner, _ := newWorkRunner(t, proofPassSize.MemoryMiB*proofPassVMs)
		holder, err := runner.memory.TryReserve(t.Context(), passWork("holder"))
		if err != nil || !holder.Held() {
			t.Fatalf("holder = %+v, %v", holder, err)
		}
		var (
			under   domain.InvocationID
			counted int
		)
		done := make(chan error, 1)
		go func() {
			done <- runner.run(t.Context(), domain.RestartBlockerProofPass, passWork("pass"), true,
				func(ctx context.Context) error {
					under, _ = ward.LaunchReservationFrom(ctx)
					counted = runner.running.Counts()[domain.RestartBlockerProofPass]
					return nil
				})
		}()
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if under != "" {
			t.Fatal("the pass ran while its memory was held by another")
		}
		if err := holder.Release(t.Context(), nil); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if under != "pass" || counted != 1 {
			t.Fatalf("pass ran under %q counted %d, want its reservation and one running pass", under, counted)
		}
		if left, err := runner.memory.ListLeft(t.Context()); err != nil || len(left) != 0 {
			t.Fatalf("left = %+v, %v; want the proved pass released", left, err)
		}
	})
}

// TestDaemonWorkRunnerRefusedAtOnceRunsNothing: a start-time pass refused
// for memory returns the refusal and runs nothing.
func TestDaemonWorkRunnerRefusedAtOnceRunsNothing(t *testing.T) {
	t.Parallel()
	runner, _ := newWorkRunner(t, proofPassSize.MemoryMiB*proofPassVMs)
	if _, err := runner.memory.TryReserve(t.Context(), passWork("holder")); err != nil {
		t.Fatal(err)
	}
	ran := false
	err := runner.run(t.Context(), domain.RestartBlockerProofPass, passWork("pass"), false,
		func(context.Context) error { ran = true; return nil })
	if !errors.Is(err, domain.ErrMachineCapacity) || ran {
		t.Fatalf("refused pass = %v, ran %t; want a capacity refusal and nothing run", err, ran)
	}
}

// TestDaemonWorkRunnerKeepsAReservationWhoseContainerRuns: a pass that left
// a container running under its label keeps its reservation, and a later
// sweep releases it once that container is gone.
func TestDaemonWorkRunnerKeepsAReservationWhoseContainerRuns(t *testing.T) {
	t.Parallel()
	runner, label := newWorkRunner(t, 16384)
	err := runner.run(t.Context(), domain.RestartBlockerProofPass, passWork("pass"), true,
		func(ctx context.Context) error {
			id, _ := ward.LaunchReservationFrom(ctx)
			return os.WriteFile(label, []byte(id), 0o600)
		})
	if err != nil {
		t.Fatal(err)
	}
	left, err := runner.memory.ListLeft(t.Context())
	if err != nil || len(left) != 1 || left[0].InvocationID != "pass" {
		t.Fatalf("left = %+v, %v; want the pass's reservation kept", left, err)
	}
	if err := os.Remove(label); err != nil {
		t.Fatal(err)
	}
	if err := sweepLeftReservations(t.Context(), runner.memory, runner.containerPath,
		domain.ReservationProofPass); err != nil {
		t.Fatal(err)
	}
	if left, err := runner.memory.ListLeft(t.Context()); err != nil || len(left) != 0 {
		t.Fatalf("left after the container went = %+v, %v", left, err)
	}
}

// TestScheduledConformanceSkipsAPassTheBudgetRefusesForGood: a scheduled
// proof pass whose memory the budget can never grant, after a lowered budget
// or on a runtime with no recorded overhead, runs nothing and fails no
// doctor pass, which would stop the daemon; any other failure that leaves
// no proof standing still fails it.
func TestScheduledConformanceSkipsAPassTheBudgetRefusesForGood(t *testing.T) {
	t.Parallel()
	suiteFailed := errors.New("suite failed")
	for name, tc := range map[string]struct {
		cause error
		want  error
	}{
		"never fits":       {cause: &domain.LaunchNeverFitsError{SizeMiB: 4096, OverheadMiB: 256, BudgetMiB: 2048}},
		"unknown overhead": {cause: &domain.RuntimeOverheadUnknownError{RuntimeVersion: "9.9.9"}},
		"suite failure":    {cause: suiteFailed, want: suiteFailed},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			image := domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("d", 64))
			ledger := newFakeWard(image)
			f := newConformanceFollower(followerConfig{
				Ledger: ledger,
				Prove: func(context.Context, domain.ImageRef) error {
					if tc.want != nil {
						// A pass that ran withdrew the image's proof first.
						ledger.publish(image, false)
					}
					return fmt.Errorf("reserve memory for the proof pass: %w", tc.cause)
				},
				Start: image, Now: time.Now, Logger: slog.New(slog.DiscardHandler),
			})
			if err := f.runScheduled(t.Context()); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("scheduled pass = %v, want %v", err, tc.want)
			}
		})
	}
}
