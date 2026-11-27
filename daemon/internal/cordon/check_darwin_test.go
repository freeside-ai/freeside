//go:build darwin

package cordon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hostCheck is a check whose command is the fake agent in mode args.
func hostCheck(cfg Config, id string, args ...string) CheckRequest {
	r := checkRequest(id)
	r.Argv = append([]string{cfg.AgentCLI, fakeAgentArg}, args...)
	return r
}

// TestHostCheckResolvesABareCommandName runs a recipe-shaped command by
// name: sandbox-exec finds it through the launch's PATH.
func TestHostCheckResolvesABareCommandName(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	req := checkRequest("check-bare")
	req.Argv = []string{"go", "version"}
	got, err := f.backend(t).Check(t.Context(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.ExitCode != 0 || got.EndedBy != EndedByExited || !strings.HasPrefix(string(got.Output), "go version go") {
		t.Fatalf("Check = exit %d %s, output %q", got.ExitCode, got.EndedBy, got.Output)
	}
}

// TestHostCheckCannotSetTheSandboxsParameters gives a check an argv that
// would be sandbox-exec options: a later -D overrides an earlier one, so
// HOME=/nonexistent would reopen the owner's secrets. The "--" before the
// program makes "-D" the program's name, which doesn't exist.
func TestHostCheckCannotSetTheSandboxsParameters(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	secret := standIn(t, filepath.Join(f.cfg.Home, ".ssh/id_ed25519"))
	req := checkRequest("check-options")
	req.Argv = []string{"-D", "HOME=/nonexistent", "/bin/cat", secret}
	got, err := f.backend(t).Check(t.Context(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.ExitCode == 0 || bytes.Contains(got.Output, []byte("stand-in")) {
		t.Fatalf("Check = exit %d, output %q, want the program -D not found", got.ExitCode, got.Output)
	}
}

// TestHostCheckKeepsItsStreamsInOrder alternates lines between stdout and
// stderr: the output holds them in the order the command wrote them.
func TestHostCheckKeepsItsStreamsInOrder(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	req := checkRequest("check-order")
	req.Argv = []string{"/bin/sh", "-c", "i=0; while [ $i -lt 200 ]; do echo out $i; echo err $i >&2; i=$((i+1)); done"}
	got, err := f.backend(t).Check(t.Context(), req)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	var want strings.Builder
	for i := range 200 {
		fmt.Fprintf(&want, "out %d\nerr %d\n", i, i)
	}
	if got.ExitCode != 0 || string(got.Output) != want.String() {
		t.Fatalf("Check = exit %d, output out of order:\n%s", got.ExitCode, got.Output)
	}
}

// TestHostCheckHasNoTokenDescriptor shows a real check's command has no
// pipe on descriptor 3 and no variable naming one.
func TestHostCheckHasNoTokenDescriptor(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	got, err := f.backend(t).Check(t.Context(), hostCheck(f.cfg, "check-fd3", "fd3"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if want := "fd3 pipe false\ntoken-env \"\"\n"; got.ExitCode != 0 || string(got.Output) != want {
		t.Fatalf("Check = exit %d, output %q, want %q", got.ExitCode, got.Output, want)
	}
}

// TestHostCheckEndsEverySurvivor shows a check returns only once the
// detached children its command left are dead, as a writer's launch does.
func TestHostCheckEndsEverySurvivor(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	const id = "check-survivors"
	got, err := f.backend(t).Check(t.Context(), hostCheck(f.cfg, id, "spawn", "exit"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if got.EndedBy != EndedByExited {
		t.Fatalf("EndedBy = %s, want exited", got.EndedBy)
	}
	pids := readPids(t, filepath.Join(layoutFor(f.cfg.RunsRoot, id).worktree, "pids"))
	if len(pids) != 3 {
		t.Fatalf("pids = %v, want three", pids)
	}
	requireDead(t, pids)
}

// probeFileSeeder seeds the module and writes lines to ./probes, where the
// fake agent's "probe file" mode reads them.
type probeFileSeeder struct{ lines []string }

func (s probeFileSeeder) FetchBaseWorktree(ctx context.Context, repo, baseRef, baseSHA, dir string) error {
	if err := (moduleSeeder{}).FetchBaseWorktree(ctx, repo, baseRef, baseSHA, dir); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "probes"), []byte(strings.Join(s.lines, "")), 0o600)
}

// TestHostCheckRunsUnderTheWritersDenials runs a few of the writer's denial
// probes as a check's command: a secret read, a protected-root write, and a
// LaunchAgents write.
func TestHostCheckRunsUnderTheWritersDenials(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	home := f.cfg.Home
	if err := os.MkdirAll(filepath.Join(home, "Library/LaunchAgents"), 0o700); err != nil {
		t.Fatal(err)
	}
	const id = "check-denials"
	probes := []string{
		probeLine("deny", "read", standIn(t, filepath.Join(home, ".ssh/id_ed25519"))),
		probeLine("deny", "write", filepath.Join(f.daemonRoot, "planted")),
		probeLine("deny", "write", filepath.Join(home, "Library/LaunchAgents/com.example.agent.plist")),
		probeLine("allow", "write", filepath.Join(layoutFor(f.cfg.RunsRoot, id).worktree, "written")),
	}
	b, err := New(f.cfg, probeFileSeeder{lines: probes})
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Check(t.Context(), hostCheck(f.cfg, id, "probe", "file"))
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	ok := strings.Count(string(got.Output), "ok ")
	if got.ExitCode != 0 || ok != len(probes) {
		t.Fatalf("%d of %d probes ok, exit %d:\n%s", ok, len(probes), got.ExitCode, got.Output)
	}
	rules, err := os.ReadFile(layoutFor(f.cfg.RunsRoot, id).rules)
	if err != nil || !bytes.Equal(rules, RenderRules(f.cfg)) {
		t.Fatalf("the check ran rules other than the configuration's: %v", err)
	}
}

// TestHostCheckOutlivesItsDaemon kills the daemon mid-check with SIGKILL:
// the supervisor keeps the command running, and a fresh backend's
// RecoverCheck waits for it and returns its end, never exporting it.
func TestHostCheckOutlivesItsDaemon(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	const id = "check-restart"
	l := layoutFor(f.cfg.RunsRoot, id)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	body, err := json.Marshal(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	var said bytes.Buffer
	daemon := exec.Command(f.cfg.AgentCLI, daemonStandInArg, cfgPath, id, "check") //nolint:gosec,noctx // G204: this test binary; ended by SIGKILL below
	daemon.Stdout, daemon.Stderr = &said, &said
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	agent, survivors := startedAgent(t, l)
	_ = daemon.Process.Kill()
	_ = daemon.Wait()
	supervisor := claimedSupervisor(t, l, id)
	if !processLive(t, supervisor) || !processLive(t, agent) {
		t.Fatalf("the check died with the daemon; it said: %s", said.String())
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	type recovered struct {
		res CheckResult
		err error
	}
	done := make(chan recovered, 1)
	go func() {
		res, err := f.backend(t).RecoverCheck(ctx, CheckRecoverRequest{RunID: id, MaxOutputBytes: 1 << 20})
		done <- recovered{res, err}
	}()
	writeIn(t, l.worktree, "release", "")
	got := <-done
	if got.err != nil {
		t.Fatalf("RecoverCheck: %v", got.err)
	}
	if got.res.EndedBy != EndedByExited || got.res.ExitCode != 0 || !strings.Contains(string(got.res.Output), "working") {
		t.Fatalf("RecoverCheck = %+v, want a clean exit with its output", got.res)
	}
	if exists(t, l.export) {
		t.Fatal("recovering a check exported it")
	}
	requireDead(t, append([]int{supervisor, agent}, survivors...))
}
