//go:build darwin && cgo

package cordon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
)

// onTheManagerPath gives cfg the launch PATH and plants a decoy in each of
// its directories under cfg.Home, the fixture's fake home, so nothing is
// planted outside the test. A decoy stands for each program a launch's
// life starts outside the agent (sandbox-exec, the canary's cat, sleep,
// and the suite's launchctl and xcrun), for git, and for the agent CLI's
// own name. Each appends its path to the returned marker, which sits in
// the fake home so a decoy started inside a launch can write it too, and
// exits 97.
//
// A decoy catches only a start that resolves through the launch's PATH: a
// program os/exec starts by a bare name resolves through the starting
// process's own PATH, which for the daemon is not this one. A decoy also
// shadows its name only where no directory before it on the PATH holds
// that name, so a real git in /opt/homebrew/bin would hide the git decoy,
// and the CLI's own directory, first on the PATH, hides the CLI-name
// decoy; the control in TestHostLaunchRunsNoDecoyOnTheManagerPath shows
// the sandbox-exec decoy does shadow its name.
func onTheManagerPath(t *testing.T, cfg *Config) string {
	t.Helper()
	cfg.Path = LaunchPath(filepath.Dir(cfg.AgentCLI), cfg.Home)
	marker := filepath.Join(cfg.Home, ".local", "decoys-ran")
	script := fmt.Appendf(nil, "#!/bin/sh\nprintf '%%s\\n' \"$0\" >> '%s'\nexit 97\n", marker)
	names := []string{"git", "sandbox-exec", "cat", "sleep", "launchctl", "xcrun", filepath.Base(cfg.AgentCLI)}
	planted := 0
	for _, dir := range cfg.Path {
		if !strings.HasPrefix(dir, cfg.Home+"/") {
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			execfixture.Write(t, filepath.Join(dir, name), script, 0o700)
		}
		planted++
	}
	if planted != 3 {
		t.Fatalf("planted decoys in %d directories under the home, want 3: %q", planted, cfg.Path)
	}
	return marker
}

// requireNoDecoyRan fails the test if any decoy wrote the marker.
func requireNoDecoyRan(t *testing.T, marker string) {
	t.Helper()
	body, err := os.ReadFile(marker) //nolint:gosec // G304: the test's own marker in its fake home
	if errors.Is(err, fs.ErrNotExist) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("a decoy ran:\n%s", body)
}

// TestHostLaunchRunsNoDecoyOnTheManagerPath shows Freeside starts nothing
// through a launch's PATH, whose manager directories a run can write: a
// launch run to its end, a canceled launch, and a launch recovered after
// its daemon died run no decoy, from the daemon, the supervisor, or the
// run-end proof. A last launch whose agent runs sandbox-exec by name is
// the control: it runs the decoy, so a start that resolved through the
// PATH would have been seen.
func TestHostLaunchRunsNoDecoyOnTheManagerPath(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	marker := onTheManagerPath(t, &f.cfg)
	b := f.backend(t)

	got, err := b.Launch(t.Context(), hostRequest("decoy-exit", "", "exit", "0"))
	if err != nil || got.EndedBy != EndedByExited || got.ExitCode != 0 {
		t.Fatalf("Launch = %+v, %v, want exit 0 ended by exited", got, err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pidsFile := filepath.Join(layoutFor(f.cfg.RunsRoot, "decoy-cancel").worktree, "pids")
	go func() {
		awaitFile(t, pidsFile)
		cancel()
	}()
	got, err = b.Launch(ctx, hostRequest("decoy-cancel", "", "spawn", "chatter"))
	if err != nil || got.EndedBy != EndedByCanceled {
		t.Fatalf("Launch = %+v, %v, want ended by canceled", got, err)
	}

	const restart = "decoy-restart"
	supervisor, agent, survivors := killDaemonMidLaunch(t, f.cfg, restart)
	recoverCtx, cancelRecover := context.WithTimeout(t.Context(), time.Minute)
	defer cancelRecover()
	type recovered struct {
		res RecoveryResult
		err error
	}
	done := make(chan recovered, 1)
	go func() {
		res, err := f.backend(t).Recover(recoverCtx, RecoverRequest{RunID: restart, Export: exportRequest(restart)})
		done <- recovered{res, err}
	}()
	writeIn(t, layoutFor(f.cfg.RunsRoot, restart).worktree, "release", "")
	if r := <-done; r.err != nil || r.res.Outcome != RecoveryExported {
		t.Fatalf("Recover = %+v, %v, want exported", r.res, r.err)
	}
	requireDead(t, append([]int{supervisor, agent}, survivors...))
	requireNoDecoyRan(t, marker)

	runProbeLaunch(t, b, "decoy-control", []string{probeLine("any", "run", "sandbox-exec")})
	body, err := os.ReadFile(marker) //nolint:gosec // G304: the test's own marker in its fake home
	if err != nil {
		t.Fatalf("the control's decoy never ran: %v", err)
	}
	if want := filepath.Join(f.cfg.Home, ".cargo", "bin", "sandbox-exec") + "\n"; string(body) != want {
		t.Fatalf("the control ran %q, want the first decoy, %q", body, want)
	}
}

// TestFullRunsNoDecoyOnTheManagerPath shows the conformance suite's full
// pass and its pre-job probe start nothing through the launch's PATH: both
// pass with a decoy in each manager directory under the home, and no decoy
// runs.
func TestFullRunsNoDecoyOnTheManagerPath(t *testing.T) {
	t.Parallel()
	cfg := suiteFixture(t)
	marker := onTheManagerPath(t, &cfg)
	b, err := New(cfg, moduleSeeder{})
	if err != nil {
		t.Fatal(err)
	}
	s := testSuite(t, b, WithConformanceRecorder(&fakeRecorder{}))
	s.outside = &fakeOutside{}
	if err := s.Full(t.Context()); err != nil {
		t.Fatalf("Full: %v", err)
	}
	if err := s.ProbeBeforeJob(t.Context()); err != nil {
		t.Fatalf("ProbeBeforeJob: %v", err)
	}
	requireNoDecoyRan(t, marker)
}
