//go:build darwin

package cordon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// daemonStandInArg makes the test binary a daemon a restart test kills
// (daemonStandIn).
const daemonStandInArg = "cordon-daemon-stand-in"

// daemonStandIn launches the fake agent as run runID under the
// configuration at cfgPath, as a writer or, with check, as a check's
// command, and waits for the launch, as the daemon would.
func daemonStandIn(cfgPath, runID string, check bool) int {
	body, err := os.ReadFile(cfgPath) //nolint:gosec // G304: the test's own configuration file
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	var cfg Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	b, err := New(cfg, moduleSeeder{})
	switch {
	case err != nil:
	case check:
		_, err = b.Check(context.Background(), hostCheck(cfg, runID, "spawn", "release"))
	default:
		_, err = b.Launch(context.Background(), hostRequest(runID, "", "spawn", "release"))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// readPids reads the pids, one a line, the fake agent wrote at path.
func readPids(t *testing.T, path string) []int {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // G304: the fake agent's file in the test's worktree
	if err != nil {
		t.Fatal(err)
	}
	var pids []int
	for _, field := range strings.Fields(string(body)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			t.Fatal(err)
		}
		pids = append(pids, pid)
	}
	return pids
}

// claimedSupervisor returns the pid run id's claim names.
func claimedSupervisor(t *testing.T, l layout, id string) int {
	t.Helper()
	claim, err := readClaim(l, id)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Supervisor == nil {
		t.Fatal("the claim names no supervisor")
	}
	return claim.Supervisor.PID
}

// awaitDead waits for pid to be dead or a zombie.
func awaitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for processLive(t, pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%d is still alive", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startedAgent waits for the fake agent of run l to have spawned its
// survivors, and returns its pid and theirs.
func startedAgent(t *testing.T, l layout) (agent int, survivors []int) {
	t.Helper()
	pidsFile := filepath.Join(l.worktree, "pids")
	awaitFile(t, pidsFile)
	if t.Failed() {
		t.FailNow()
	}
	return readPids(t, filepath.Join(l.worktree, "agent.pid"))[0], readPids(t, pidsFile)
}

func requireDead(t *testing.T, pids []int) {
	t.Helper()
	for _, pid := range pids {
		if processLive(t, pid) {
			t.Errorf("%d is still alive", pid)
		}
	}
}

// killDaemonMidLaunch starts a daemon stand-in that launches run id under
// cfg, waits for the run's agent to spawn its survivors, and kills the
// stand-in with SIGKILL, as a crash or launchd would. It fails the test
// unless the run's supervisor and agent outlive the stand-in, and returns
// their pids and the survivors'.
func killDaemonMidLaunch(t *testing.T, cfg Config, id string) (supervisor, agent int, survivors []int) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var said bytes.Buffer
	daemon := exec.Command(self, daemonStandInArg, cfgPath, id) //nolint:gosec,noctx // G204: this test binary; ended by SIGKILL below
	daemon.Stdout, daemon.Stderr = &said, &said
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	l := layoutFor(cfg.RunsRoot, id)
	agent, survivors = startedAgent(t, l)
	_ = daemon.Process.Kill()
	_ = daemon.Wait()
	supervisor = claimedSupervisor(t, l, id)
	for _, pid := range []int{supervisor, agent} {
		if !processLive(t, pid) {
			t.Fatalf("%d died with the daemon; it said: %s", pid, said.String())
		}
	}
	return supervisor, agent, survivors
}

// TestHostLaunchOutlivesItsDaemon kills the daemon mid-launch with SIGKILL,
// as a crash or launchd would: the supervisor keeps the agent running, and
// a fresh backend adopts the run, waits for it, and exports it.
func TestHostLaunchOutlivesItsDaemon(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	const id = "restart"
	l := layoutFor(f.cfg.RunsRoot, id)
	supervisor, agent, survivors := killDaemonMidLaunch(t, f.cfg, id)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	type recovered struct {
		res RecoveryResult
		err error
	}
	done := make(chan recovered, 1)
	go func() {
		res, err := f.backend(t).Recover(ctx, RecoverRequest{RunID: id, Export: exportRequest(id)})
		done <- recovered{res, err}
	}()
	writeIn(t, l.worktree, "release", "")
	got := <-done
	if got.err != nil {
		t.Fatalf("Recover: %v", got.err)
	}
	if got.res.Outcome != RecoveryExported || got.res.EndedBy != EndedByExited || got.res.ExitCode != 0 {
		t.Fatalf("Recover = %+v, want exported after a clean exit", got.res)
	}
	if !slices.ContainsFunc(got.res.Export.Manifest.Entries, func(e export.Entry) bool { return e.Path == "release" }) {
		t.Fatalf("the export lacks the run's last change: %+v", got.res.Export.Manifest.Entries)
	}
	requireDead(t, append([]int{supervisor, agent}, survivors...))
}

// TestHostLaunchThatDiedWhileTheDaemonWasDownEndsLost kills a run's
// supervisor and agent while no daemon watches: recovery proves the
// survivors ended and ends the run lost, never exported.
func TestHostLaunchThatDiedWhileTheDaemonWasDownEndsLost(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	const id = "lost"
	l := layoutFor(f.cfg.RunsRoot, id)
	first := f.backend(t)
	launched := make(chan error, 1)
	go func() {
		_, err := first.Launch(t.Context(), hostRequest(id, "", "spawn", "release"))
		launched <- err
	}()
	agent, survivors := startedAgent(t, l)
	first.Detach()
	if err := <-launched; !errors.Is(err, ErrDetached) {
		t.Fatalf("Launch after Detach = %v, want ErrDetached", err)
	}
	supervisor := claimedSupervisor(t, l, id)
	for _, pid := range []int{supervisor, agent} {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		awaitDead(t, pid)
	}
	for _, pid := range survivors {
		if !processLive(t, pid) {
			t.Fatalf("survivor %d died with the agent, so the proof has nothing to end", pid)
		}
	}

	res, err := f.backend(t).Recover(t.Context(), RecoverRequest{RunID: id, Export: exportRequest(id)})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if res.Outcome != RecoveryLost || res.Reason != "its supervisor ended before it recorded an end" {
		t.Fatalf("Recover = %+v, want lost", res)
	}
	requireDead(t, survivors)
	for _, path := range []string{l.exportRecord, l.export} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s exists after a lost run: %v", path, err)
		}
	}
}
