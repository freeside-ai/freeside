package macverify

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// declareProven publishes the backend's declaration as a passing pass would.
func declareProven(t *testing.T, b *Backend) {
	t.Helper()
	generation, err := b.beginConformanceProof(nil)
	if err != nil {
		t.Fatal(err)
	}
	if published, _ := b.concludeConformanceProof(generation, true, nil); !published || !b.proven.Load() {
		t.Fatal("could not declare the test backend proven")
	}
}

func TestRunPreJobProbeRunsTheSandboxModeFirst(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	if _, err := room.Run(t.Context(), t.TempDir(), []string{"true"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	_, prepares, runs := fake.snapshot()
	probe, req := prepares[0], runs[0]
	if probe.workdir != fake.probeDir || probe.caches != (jobCaches{}) || string(probe.profile) != testProfile {
		t.Fatalf("probe prepare = %+v, want the probe workspace under the configured profile with no cache", probe)
	}
	argv := req.Argv
	if !isProbeArgv(argv) || argv[3] != probeModeSandbox {
		t.Fatalf("probe argv = %q, want the sandbox mode", argv)
	}
	// A lone slot's foreign port is the one below its set.
	wantTail := append([]string{
		preJobResolverHost, preJobTCP, preJobUDP, "47000,47001,47002,47003", "46999",
	}, room.outsideDirs...)
	if !slices.Equal(argv[5:], wantTail) {
		t.Fatalf("probe targets = %q, want %q", argv[5:], wantTail)
	}
	if want := []string{"/private/tmp", "/Users/Shared"}; !slices.Equal(room.outsideDirs, want) {
		t.Fatalf("outside directories = %v, want %v", room.outsideDirs, want)
	}
}

// TestRunPreJobProbeFailsClosed fails each pre-job confinement and checks the
// command is never launched, the slot is quarantined, the declaration is
// withdrawn, and the error names the confinement and slot.
func TestRunPreJobProbeFailsClosed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		probe func(spec probeSpec) (string, int)
		want  Confinement
	}{
		"loopback reached": {
			probe: func(spec probeSpec) (string, int) { return failingReport(spec, checkLoopbackRefused), 1 },
			want:  ConfinementSandboxNetwork,
		},
		"TCP connected": {
			probe: func(spec probeSpec) (string, int) { return failingReport(spec, checkTCPRefused), 1 },
			want:  ConfinementSandboxNetwork,
		},
		"UDP sent": {
			probe: func(spec probeSpec) (string, int) { return failingReport(spec, checkUDPRefused), 1 },
			want:  ConfinementSandboxNetwork,
		},
		"resolver answered": {
			probe: func(spec probeSpec) (string, int) { return failingReport(spec, checkResolverEmpty), 1 },
			want:  ConfinementSandboxNetwork,
		},
		"outside write": {
			probe: func(spec probeSpec) (string, int) { return failingReport(spec, checkWriteDenied("/Users/Shared")), 1 },
			want:  ConfinementWriteConfinement,
		},
		"job directory unwritable": {
			probe: func(spec probeSpec) (string, int) { return failingReport(spec, checkWriteJobDir), 1 },
			want:  ConfinementWriteConfinement,
		},
		"report missing a check": {
			probe: func(spec probeSpec) (string, int) {
				return strings.Replace(passingReport(spec), "ok "+checkUDPRefused+"\n", "", 1), 0
			},
			want: ConfinementSandboxNetwork,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			fake.probe = func(req runRequest, _ []byte) (string, int) { return tc.probe(specFromArgv(req.Argv)) }
			recipeRan := false
			fake.command = func(runRequest, io.Writer, <-chan struct{}) runOutcome { recipeRan = true; return runOutcome{} }
			room := newTestRoom(t, fake)
			declareProven(t, room.backend)

			res, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
			var failure *ConformanceFailure
			if !errors.As(err, &failure) || failure.Confinement != tc.want || failure.Slot != "slot1" {
				t.Fatalf("Run = %+v, %v; want a %s failure on slot1", res, err, tc.want)
			}
			if !strings.Contains(err.Error(), string(tc.want)) || !strings.Contains(err.Error(), "slot1") {
				t.Fatalf("error %q does not name the confinement and slot", err)
			}
			if recipeRan || res.Output != nil {
				t.Fatal("the recipe command ran after a failed pre-job probe")
			}
			if calls, _, _ := fake.snapshot(); !slices.Equal(calls, probeJob("slot1")) {
				t.Fatalf("calls = %v, want only the probe job", calls)
			}
			if _, err := room.pool.lease(); !errors.Is(err, ErrNoFreeSlot) {
				t.Fatalf("lease after a failed probe = %v, want the slot quarantined", err)
			}
			if room.backend.proven.Load() || room.backend.Capabilities().Has(exec.CapCredentialFreeHostVerification) {
				t.Fatal("the declaration survived a failed pre-job probe")
			}
		})
	}
}

// TestRunPreJobProbeOperationalErrorKeepsDeclaration fails the probe for a
// reason that says nothing about confinement: the command fails, the slot is
// released, and the declaration stands.
func TestRunPreJobProbeOperationalErrorKeepsDeclaration(t *testing.T) {
	t.Parallel()
	for name, setup := range map[string]func(*fakeLauncher, *Room) context.Context{
		"prepare failed": func(f *fakeLauncher, _ *Room) context.Context {
			f.prepareErr = errors.New("slot-run: slot slot1 owns a live process")
			return context.Background()
		},
		"probe not staged": func(_ *fakeLauncher, r *Room) context.Context {
			r.probeWorkspace = func() (string, error) { return "", errors.New("stage probe: disk full") }
			return context.Background()
		},
		"canceled during the probe": func(f *fakeLauncher, _ *Room) context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			f.probe = func(req runRequest, _ []byte) (string, int) {
				cancel()
				return "", -1
			}
			return ctx
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fake := newFakeLauncher()
			room := newTestRoom(t, fake)
			ctx := setup(fake, room)
			declareProven(t, room.backend)

			_, err := room.Run(ctx, t.TempDir(), []string{"true"})
			if err == nil || errors.Is(err, ErrConformance) || errors.Is(err, ErrAbsenceUnproven) {
				t.Fatalf("Run error = %v, want an operational failure", err)
			}
			if prepares, _ := fake.recipeJobs(); len(prepares) != 0 {
				t.Fatal("the recipe was prepared after the probe failed")
			}
			if !room.backend.proven.Load() {
				t.Fatal("an operational probe failure withdrew the declaration")
			}
			if _, err := room.pool.lease(); err != nil {
				t.Fatalf("slot not released: %v", err)
			}
		})
	}
}

// TestRunPreJobProbeTimeoutIsOperational lets the probe outlive
// probeTimeout, which also covers prepare: a slow host is not a broken
// confinement, so the declaration stands.
func TestRunPreJobProbeTimeoutIsOperational(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		fake := newFakeLauncher()
		fake.probe = func(req runRequest, _ []byte) (string, int) {
			time.Sleep(probeTimeout + time.Second)
			return "", -1
		}
		room := newTestRoom(t, fake)
		declareProven(t, room.backend)
		_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
		if err == nil || errors.Is(err, ErrConformance) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run = %v, want an operational timeout", err)
		}
		if prepares, _ := fake.recipeJobs(); len(prepares) != 0 || !room.backend.proven.Load() {
			t.Fatalf("recipe prepared %d times, declared %v", len(prepares), room.backend.proven.Load())
		}
	})
}

// TestRunPreJobProbeAbsenceUnprovenKeepsTheSlotQuarantined: a probe job whose
// slot cannot be proved empty leaves the slot quarantined, runs no recipe,
// and keeps the declaration, since a lingering process says nothing about
// confinement.
func TestRunPreJobProbeAbsenceUnprovenKeepsTheSlotQuarantined(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	fake.live = func(int) bool { return true }
	room := newTestRoom(t, fake)
	declareProven(t, room.backend)
	_, err := room.Run(t.Context(), t.TempDir(), []string{"true"})
	if !errors.Is(err, ErrAbsenceUnproven) || errors.Is(err, ErrConformance) {
		t.Fatalf("Run = %v, want ErrAbsenceUnproven", err)
	}
	if prepares, _ := fake.recipeJobs(); len(prepares) != 0 {
		t.Fatal("the recipe was prepared after the probe's slot stayed occupied")
	}
	if _, err := room.pool.lease(); !errors.Is(err, ErrNoFreeSlot) {
		t.Fatalf("lease = %v, want the slot quarantined", err)
	}
	if !room.backend.proven.Load() {
		t.Fatal("an unproven absence withdrew the declaration")
	}
}

func TestStageProbeCopiesTheDaemonBinaryAndRestagesAMissingCopy(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	daemon := filepath.Join(t.TempDir(), "freesided")
	if err := os.WriteFile(daemon, []byte("daemon build A"), 0o700); err != nil { //nolint:gosec // G306: stands in for an executable
		t.Fatal(err)
	}
	digest, err := fileDigest(daemon)
	if err != nil {
		t.Fatal(err)
	}
	room.backend.daemonPath, room.backend.daemonDigest = daemon, digest

	dir, err := room.backend.stageProbe()
	if err != nil {
		t.Fatalf("stageProbe: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	probe := filepath.Join(dir, probeExecutable)
	info, err := os.Stat(probe)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("staged probe = %v, %v; want mode 0755", info, err)
	}
	if canonical, _ := filepath.EvalSymlinks(dir); canonical != dir {
		t.Fatalf("probe workspace %s is not canonical (%s)", dir, canonical)
	}
	if again, err := room.backend.stageProbe(); err != nil || again != dir {
		t.Fatalf("second stageProbe = %s, %v; want the same workspace", again, err)
	}

	// A copy that differs is replaced, as is one the temporary-file sweep
	// removed.
	if err := os.WriteFile(probe, []byte("tampered"), 0o755); err != nil { //nolint:gosec // G306: the probe is executable
		t.Fatal(err)
	}
	restaged, err := room.backend.stageProbe()
	if err != nil {
		t.Fatalf("restage after tampering: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(restaged) })
	if got, _ := os.ReadFile(filepath.Join(restaged, probeExecutable)); !bytes.Equal(got, []byte("daemon build A")) { //nolint:gosec // G304: the test's own staged file
		t.Fatalf("restaged probe = %q", got)
	}
	if err := os.RemoveAll(restaged); err != nil {
		t.Fatal(err)
	}
	if again, err := room.backend.stageProbe(); err != nil || again == restaged {
		t.Fatalf("restage after removal = %s, %v; want a fresh workspace", again, err)
	} else {
		t.Cleanup(func() { _ = os.RemoveAll(again) })
	}
}

func TestStageProbeRefusesADaemonBinaryChangedOnDisk(t *testing.T) {
	t.Parallel()
	fake := newFakeLauncher()
	room := newTestRoom(t, fake)
	daemon := filepath.Join(t.TempDir(), "freesided")
	if err := os.WriteFile(daemon, []byte("daemon build A"), 0o700); err != nil { //nolint:gosec // G306: stands in for an executable
		t.Fatal(err)
	}
	digest, err := fileDigest(daemon)
	if err != nil {
		t.Fatal(err)
	}
	room.backend.daemonPath, room.backend.daemonDigest = daemon, digest
	if err := os.WriteFile(daemon, []byte("daemon build B"), 0o700); err != nil { //nolint:gosec // G306: stands in for an executable
		t.Fatal(err)
	}

	if _, err := room.backend.stageProbe(); err == nil || !strings.Contains(err.Error(), "restart the daemon") {
		t.Fatalf("stageProbe = %v, want a refusal naming the restart", err)
	}
	if room.backend.probeDir != "" {
		t.Fatalf("a refused stage left workspace %s", room.backend.probeDir)
	}
}

// TestNewRoomStagesTheRunningBinary checks the production staging path end
// to end: the running test binary, which answers the probe verb (TestMain),
// is what lands in the workspace.
func TestNewRoomStagesTheRunningBinary(t *testing.T) {
	t.Parallel()
	b, err := New(Config{
		ExecutorRoot: "/var/freeside/macverify", SandboxProfile: DefaultSandboxProfile(),
		PacketFilterRules: []byte("rules"), Slots: []string{"slot1"}, Toolchain: testToolchain,
		PortBase: DefaultPortBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	room, err := NewRoom(b, RoomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := room.probeWorkspace()
	if err != nil {
		t.Fatalf("probeWorkspace: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	other, err := NewRoom(b, RoomOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if shared, err := other.probeWorkspace(); err != nil || shared != dir {
		t.Fatalf("a second room staged %s (%v), want the backend's one copy %s", shared, err, dir)
	}
	digest, err := fileDigest(filepath.Join(dir, probeExecutable))
	if err != nil || digest != b.daemonDigest || !strings.HasSuffix(b.daemonIdentity, "@"+digest) {
		t.Fatalf("staged digest %s (%v), backend %s / %s", digest, err, b.daemonDigest, b.daemonIdentity)
	}
}
