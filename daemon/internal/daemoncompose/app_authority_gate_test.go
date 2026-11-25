package daemoncompose

import (
	"bufio"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// gateHolderEnv makes this test binary a holder that dies without cleanup:
// its value is "<step>:<state dir>:<lease root>", where step names how far
// it got before it reports "held" and waits to be killed.
const gateHolderEnv = "FREESIDE_TEST_GATE_HOLDER"

const (
	// holderLock: prod's first step, the lock.
	holderLock = "lock"
	// holderLease: a deputy's first step, the lease, before its lock.
	holderLease = "lease"
	// holderLeaseAndLock: a deputy holding both, as during its run.
	holderLeaseAndLock = "lease+lock"
)

var gateHolderKeep []any

func TestGateHolderProcess(t *testing.T) {
	t.Parallel()
	spec := os.Getenv(gateHolderEnv)
	if spec == "" {
		t.Skip("gate holder subprocess only")
	}
	step, rest, _ := strings.Cut(spec, ":")
	stateDir, leaseRoot, _ := strings.Cut(rest, ":")
	fail := func(err error) {
		_, _ = os.Stdout.WriteString("error: " + err.Error() + "\n")
		os.Exit(3)
	}
	if step == holderLease || step == holderLeaseAndLock {
		lease, err := acquireGateTestRig(leaseRoot, os.Getpid())
		if err != nil {
			fail(err)
		}
		gateHolderKeep = append(gateHolderKeep, lease)
	}
	if step == holderLock || step == holderLeaseAndLock {
		lock, err := publish.AcquireAuthorityWriterLock(stateDir)
		if err != nil {
			fail(err)
		}
		gateHolderKeep = append(gateHolderKeep, lock)
	}
	_, _ = os.Stdout.WriteString("held\n")
	time.Sleep(time.Hour) // hold until killed; nothing is ever closed
}

func acquireGateTestRig(leaseRoot string, pid int) (*daemonlock.RigLease, error) {
	stateRoot := filepath.Join(filepath.Dir(leaseRoot), "deputy-state")
	return daemonlock.AcquireRig(daemonlock.RigAcquireConfig{
		Owner:     daemonlock.RigOwner{User: "operator", Host: "rig-host", PID: pid},
		StateRoot: stateRoot, DatabasePath: filepath.Join(stateRoot, "freeside.db"),
		ListenAddress: "127.0.0.1:8677", SeedRoot: filepath.Join(filepath.Dir(leaseRoot), "seed"),
		LeaseRoot: leaseRoot,
	})
}

// startGateHolder runs a holder subprocess through step and returns a kill
// that ends it the way a crash would.
func startGateHolder(t *testing.T, step, stateDir, leaseRoot string) (kill func()) {
	t.Helper()
	cmd := osexec.Command(os.Args[0], "-test.run=^TestGateHolderProcess$") //nolint:gosec // G204: this test binary with a fixed argument
	cmd.Env = append(os.Environ(), gateHolderEnv+"="+step+":"+stateDir+":"+leaseRoot)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("holder %s reported %q, %v", step, line, err)
	}
	return func() {
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
	}
}

// gateDirs is one machine's App authority state directory and production
// rig lease root.
func gateDirs(t *testing.T) (stateDir, leaseRoot string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	stateDir, leaseRoot = filepath.Join(root, "app-state"), filepath.Join(root, "rig-locks")
	for _, dir := range []string{stateDir, leaseRoot} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return stateDir, leaseRoot
}

// requireLockFree proves no holder is left: a fresh acquisition succeeds.
func requireLockFree(t *testing.T, stateDir string) {
	t.Helper()
	lock, err := publish.AcquireAuthorityWriterLock(stateDir)
	if err != nil {
		t.Fatalf("the App authority writer lock is still held: %v", err)
	}
	_ = lock.Close()
}

func requireGate(t *testing.T, stateDir, leaseRoot string, want ExecutionState) *publish.AuthorityWriterLock {
	t.Helper()
	state, lock, err := gateProdAppAuthority(stateDir, leaseRoot)
	if err != nil {
		t.Fatal(err)
	}
	if state != want {
		t.Fatalf("gate = %s, want %s", state, want)
	}
	if (lock != nil) != want.Runs() {
		t.Fatalf("gate %s returned lock %v", state, lock)
	}
	if lock != nil {
		t.Cleanup(func() { _ = lock.Close() })
		if err := lock.Covers(stateDir); err != nil {
			t.Fatal(err)
		}
	}
	return lock
}

func TestGateProdAppAuthorityTakesAFreeAuthority(t *testing.T) {
	t.Parallel()
	stateDir, leaseRoot := gateDirs(t)
	requireGate(t, stateDir, leaseRoot, ExecutionConfigured)
	// prod first, then a deputy: the deputy takes its lease, then is
	// refused the lock, so it never opens the authority beside prod.
	lease, err := acquireGateTestRig(leaseRoot, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Close() }()
	if _, err := publish.AcquireAuthorityWriterLock(stateDir); !errors.Is(err, publish.ErrAuthorityWriterLockHeld) {
		t.Fatalf("deputy beside an executing prod: %v, want ErrAuthorityWriterLockHeld", err)
	}
}

func TestGateProdAppAuthorityDeclinesBesideADeputy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// lease and lock are what the deputy holds when prod starts.
		lease, lock bool
		want        ExecutionState
	}{
		{name: "deputy holds the lock", lock: true, want: ExecutionAuthorityInUse},
		{name: "deputy holds the lease, not yet the lock", lease: true, want: ExecutionRigLeasePresent},
		{name: "deputy holds both", lease: true, lock: true, want: ExecutionAuthorityInUse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stateDir, leaseRoot := gateDirs(t)
			if tc.lease {
				lease, err := acquireGateTestRig(leaseRoot, os.Getpid())
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lease.Close() })
			}
			var deputyLock *publish.AuthorityWriterLock
			if tc.lock {
				var err error
				if deputyLock, err = publish.AcquireAuthorityWriterLock(stateDir); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = deputyLock.Close() })
			}
			requireGate(t, stateDir, leaseRoot, tc.want)
			if deputyLock == nil {
				// prod released the lock it took, so the deputy's next
				// step, the lock, succeeds.
				requireLockFree(t, stateDir)
			} else if _, err := publish.AcquireAuthorityWriterLock(stateDir); !errors.Is(err,
				publish.ErrAuthorityWriterLockHeld) {
				// The kernel still holds the deputy's lock: prod's
				// attempt did not take it over or drop it.
				t.Fatalf("prod disturbed the deputy's lock: %v, want ErrAuthorityWriterLockHeld", err)
			}
		})
	}
}

// TestGateProdAppAuthorityAfterACrash kills a holder between each pair of
// steps of each order. The kernel drops a dead holder's lock, so nothing it
// took keeps prod from the authority except a lease manifest, which stays
// until freesided rig recover, and keeps prod driverless meanwhile.
func TestGateProdAppAuthorityAfterACrash(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		step string
		want ExecutionState
	}{
		{step: holderLock, want: ExecutionConfigured},
		{step: holderLease, want: ExecutionRigLeasePresent},
		{step: holderLeaseAndLock, want: ExecutionRigLeasePresent},
	} {
		t.Run(tc.step, func(t *testing.T) {
			t.Parallel()
			stateDir, leaseRoot := gateDirs(t)
			kill := startGateHolder(t, tc.step, stateDir, leaseRoot)
			if tc.step != holderLease {
				requireGate(t, stateDir, leaseRoot, ExecutionAuthorityInUse)
			}
			kill()
			lock := requireGate(t, stateDir, leaseRoot, tc.want)
			_ = lock.Close()
			requireLockFree(t, stateDir)
		})
	}
}

func TestGateProdAppAuthorityFailsOnAnUnusableDirectory(t *testing.T) {
	t.Parallel()
	stateDir, leaseRoot := gateDirs(t)
	if _, _, err := gateProdAppAuthority(filepath.Join(stateDir, "missing"), leaseRoot); err == nil {
		t.Fatal("missing state directory accepted")
	}
	file := filepath.Join(leaseRoot, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A lease root that cannot be read leaves prod unable to tell whether a
	// deputy runs, so the start fails and no lock stays held.
	if _, _, err := gateProdAppAuthority(stateDir, filepath.Join(file, "below")); err == nil {
		t.Fatal("unreadable lease root accepted")
	}
	requireLockFree(t, stateDir)
}

func TestExecutionStatesRegistered(t *testing.T) {
	t.Parallel()
	if ExecutionState("").valid() {
		t.Fatal("zero execution state is valid")
	}
	for _, state := range AllExecutionStates {
		if !state.valid() {
			t.Fatalf("%s is not valid", state)
		}
		code, reason := executionStateDiagnostic(state, "detail")
		if state.Runs() != (code == "") || (code == "") != (reason == "") {
			t.Fatalf("%s: code %q, reason %q", state, code, reason)
		}
	}
}
