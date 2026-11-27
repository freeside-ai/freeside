//go:build darwin && cgo

package cordon_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/cordon"
	"github.com/freeside-ai/freeside/daemon/internal/cordonhost"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// The host launcher's contract suite: the same launch, check,
// cancellation, recovery, and export cases run at the backend seam against *cordon.Backend in this
// process and against a cordonhost.Client talking to a served backend over
// a Unix socket, so a launch behind the launcher ends, fails, and recovers
// as a local one does. It sits in this package's tests, not cordonhost's,
// because a real launch runs the test binary as its agent and supervisor,
// which this package's TestMain dispatches (fakeagent_darwin_test.go).

const (
	testBuild = "contract-test-build"
	// serveVerb makes the test binary a launcher in a process of its own,
	// which the restart test kills.
	serveVerb = "cordonhost-serve"
)

func init() { cordon.RegisterTestVerb(serveVerb, serveForTest) }

// seam is the part of the backend seam the shared cases drive.
type seam interface {
	Launch(context.Context, cordon.LaunchRequest) (cordon.LaunchResult, error)
	Check(context.Context, cordon.CheckRequest) (cordon.CheckResult, error)
	Recover(context.Context, cordon.RecoverRequest) (cordon.RecoveryResult, error)
	Export(context.Context, cordon.ExportRequest) (cordon.ExportResult, error)
	AuthenticateReleasedExport(ctx context.Context, runID, dir string) error
	RecoverCheck(context.Context, cordon.CheckRecoverRequest) (cordon.CheckResult, error)
	Detach()
}

// opener readies a seam over cfg's runs root, seeding on the daemon's side
// with seeder, and returns how to open one; a second open is a restarted
// daemon's.
type opener func(t *testing.T, cfg cordon.Config, seeder cordon.Seeder) func() seam

func inProcess(t *testing.T, cfg cordon.Config, seeder cordon.Seeder) func() seam {
	return func() seam {
		b, err := cordon.New(cfg, seeder)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
}

func served(t *testing.T, cfg cordon.Config, seeder cordon.Seeder) func() seam {
	h := newServedHost(t, cfg, seeder)
	return func() seam { return h.client(t, &records{}) }
}

// TestTheSeamsKeepOneContract runs each case at both seams. In both, a
// launch reports its end only once the end proof has passed, so every
// process it started is dead when it returns.
func TestTheSeamsKeepOneContract(t *testing.T) {
	t.Parallel()
	for name, open := range map[string]opener{"in process": inProcess, "served": served} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			t.Run("clean exit", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				got, err := open(t, cfg, moduleSeeder{})().Launch(t.Context(), cordon.HostRequest("clean", "", "spawn", "exit"))
				if err != nil {
					t.Fatalf("Launch: %v", err)
				}
				if got.EndedBy != cordon.EndedByExited || got.ExitCode != 0 {
					t.Fatalf("Launch = %+v, want a clean exit", got)
				}
				requireSurvivorsDead(t, cfg, "clean")
			})
			t.Run("failed exit", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				got, err := open(t, cfg, moduleSeeder{})().Launch(t.Context(), cordon.HostRequest("failed", "", "exit", "3", "the", "failure"))
				if err != nil {
					t.Fatalf("Launch: %v", err)
				}
				if got.EndedBy != cordon.EndedByExited || got.ExitCode != 3 {
					t.Fatalf("Launch = %+v, want exit 3", got)
				}
				requireTranscript(t, got.TranscriptPath, "the failure\n")
			})
			t.Run("cancellation", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				launcher := open(t, cfg, moduleSeeder{})()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() {
					awaitFile(t, filepath.Join(cordon.Worktree(cfg, "canceled"), "pids"))
					cancel()
				}()
				got, err := launcher.Launch(ctx, cordon.HostRequest("canceled", "", "spawn", "chatter"))
				if err != nil {
					t.Fatalf("Launch: %v", err)
				}
				if got.EndedBy != cordon.EndedByCanceled {
					t.Fatalf("Launch = %+v, want canceled", got)
				}
				requireSurvivorsDead(t, cfg, "canceled")
			})
			t.Run("stall", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				cfg.StallLimit = 2 * time.Second
				got, err := open(t, cfg, moduleSeeder{})().Launch(t.Context(), cordon.HostRequest("stalled", "", "spawn", "silent"))
				if err != nil {
					t.Fatalf("Launch: %v", err)
				}
				if got.EndedBy != cordon.EndedByStalled {
					t.Fatalf("Launch = %+v, want stalled", got)
				}
				requireSurvivorsDead(t, cfg, "stalled")
			})
			t.Run("recovery after detach", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				seams := open(t, cfg, moduleSeeder{})
				first := seams()
				launched := make(chan error, 1)
				go func() {
					_, err := first.Launch(t.Context(), cordon.HostRequest("detached", "", "spawn", "release", "3"))
					launched <- err
				}()
				agent := startedAgent(t, cfg, "detached")
				first.Detach()
				if err := <-launched; !errors.Is(err, cordon.ErrDetached) {
					t.Fatalf("Launch after Detach = %v, want ErrDetached", err)
				}
				if !cordon.ProcessLive(t, agent) {
					t.Fatal("the agent died with the detach")
				}
				got := recoverReleased(t, seams(), cfg, "detached")
				requireFailedRecovery(t, got)
				requireSurvivorsDead(t, cfg, "detached")
			})
			t.Run("clean exit exports", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				base, baseSHA := gitBase(t)
				s := open(t, cfg, cordon.GitSeeder(base))()
				// The line is the change the import carries and, printed, a
				// transcript the importer reads as JSONL.
				req := cordon.HostRequest("exported", "", "write", "result.json", `{"type":"result"}`)
				req.BaseSHA = baseSHA
				if got, err := s.Launch(t.Context(), req); err != nil || got.ExitCode != 0 {
					t.Fatalf("Launch = %+v, %v", got, err)
				}
				res, err := s.Export(t.Context(), cordon.ExportRequestFor("exported"))
				if err != nil {
					t.Fatalf("Export: %v", err)
				}
				if res.BaseSHA != baseSHA {
					t.Fatalf("exported from %s, want %s", res.BaseSHA, baseSHA)
				}
				if err := s.AuthenticateReleasedExport(t.Context(), "exported", res.Dir); err != nil {
					t.Fatalf("AuthenticateReleasedExport: %v", err)
				}
				// The backend's own export of the worktree, which on the served
				// seam stays on the launcher's side.
				want := importedTree(t, base, cordon.ExportDir(cfg, "exported"), baseSHA)
				if got := importedTree(t, base, res.Dir, baseSHA); got != want {
					t.Fatalf("imported tree %s, want the in-process export's %s", got, want)
				}
			})
			t.Run("recovery after detach exports", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				seams := open(t, cfg, moduleSeeder{})
				first := seams()
				launched := make(chan error, 1)
				go func() {
					_, err := first.Launch(t.Context(), cordon.HostRequest("detached", "", "spawn", "release", "0"))
					launched <- err
				}()
				startedAgent(t, cfg, "detached")
				first.Detach()
				if err := <-launched; !errors.Is(err, cordon.ErrDetached) {
					t.Fatalf("Launch after Detach = %v, want ErrDetached", err)
				}
				s := seams()
				got := recoverReleased(t, s, cfg, "detached")
				if got.Outcome != cordon.RecoveryExported || got.EndedBy != cordon.EndedByExited || got.Export.Dir == "" {
					t.Fatalf("Recover = %+v, want an export", got)
				}
				if err := s.AuthenticateReleasedExport(t.Context(), "detached", got.Export.Dir); err != nil {
					t.Fatalf("AuthenticateReleasedExport: %v", err)
				}
				requireSurvivorsDead(t, cfg, "detached")
			})
			t.Run("check output", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				got, err := open(t, cfg, moduleSeeder{})().Check(t.Context(), cordon.HostCheck(cfg, "check-failed", "exit", "3", "the", "failure"))
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				if got.EndedBy != cordon.EndedByExited || got.ExitCode != 3 || string(got.Output) != "the failure\n" {
					t.Fatalf("Check = %+v, want exit 3 with its output", got)
				}
			})
			t.Run("check cancellation", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				checker := open(t, cfg, moduleSeeder{})()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() {
					awaitFile(t, filepath.Join(cordon.Worktree(cfg, "check-canceled"), "pids"))
					cancel()
				}()
				got, err := checker.Check(ctx, cordon.HostCheck(cfg, "check-canceled", "spawn", "chatter"))
				if err != nil {
					t.Fatalf("Check: %v", err)
				}
				if got.EndedBy != cordon.EndedByCanceled {
					t.Fatalf("Check = %+v, want canceled", got)
				}
				requireSurvivorsDead(t, cfg, "check-canceled")
			})
			t.Run("check recovery after detach", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				seams := open(t, cfg, moduleSeeder{})
				first := seams()
				checked := make(chan error, 1)
				go func() {
					_, err := first.Check(t.Context(), cordon.HostCheck(cfg, "check-detached", "spawn", "release", "3"))
					checked <- err
				}()
				command := startedAgent(t, cfg, "check-detached")
				first.Detach()
				if err := <-checked; !errors.Is(err, cordon.ErrDetached) {
					t.Fatalf("Check after Detach = %v, want ErrDetached", err)
				}
				if !cordon.ProcessLive(t, command) {
					t.Fatal("the command died with the detach")
				}
				got := recoverReleasedCheck(t, seams(), cfg, "check-detached")
				if got.EndedBy != cordon.EndedByExited || got.ExitCode != 3 || !bytes.Contains(got.Output, []byte("working\n")) {
					t.Fatalf("RecoverCheck = %+v, want exit 3 with the run's output", got)
				}
				requireSurvivorsDead(t, cfg, "check-detached")
			})
			t.Run("recover check of no run", func(t *testing.T) {
				t.Parallel()
				cfg := cordon.HostConfig(t)
				_, err := open(t, cfg, moduleSeeder{})().RecoverCheck(t.Context(), cordon.CheckRecoverRequest{RunID: "never", MaxOutputBytes: 1 << 10})
				if !errors.Is(err, cordon.ErrNoRun) {
					t.Fatalf("RecoverCheck = %v, want ErrNoRun", err)
				}
			})
		})
	}
}

// TestAServedExportWaitsForTheEndAndLastsUntilRelease asks a launcher to
// export a run still running and a run it never launched: each is refused
// as the backend's ErrNoEndRecord and the launcher writes no export. Once
// the run ends clean it exports, and its directory on the launcher and the
// client's receipt both last until ReleaseFinished removes them.
func TestAServedExportWaitsForTheEndAndLastsUntilRelease(t *testing.T) {
	t.Parallel()
	cfg := cordon.HostConfig(t)
	h := newServedHost(t, cfg, moduleSeeder{})
	client := h.client(t, &records{})
	launched := make(chan error, 1)
	go func() {
		got, err := client.Launch(t.Context(), cordon.HostRequest("running", "", "spawn", "release", "0"))
		if err == nil && got.ExitCode != 0 {
			err = fmt.Errorf("exit %d", got.ExitCode)
		}
		launched <- err
	}()
	startedAgent(t, cfg, "running")
	for _, runID := range []string{"running", "never"} {
		if _, err := client.Export(t.Context(), cordon.ExportRequestFor(runID)); !errors.Is(err, cordon.ErrNoEndRecord) {
			t.Fatalf("exporting %s = %v, want ErrNoEndRecord", runID, err)
		}
		if _, err := os.Lstat(cordon.ExportDir(cfg, runID)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a refused export of %s wrote one: %v", runID, err)
		}
	}
	if entries, err := os.ReadDir(h.exports); err != nil || len(entries) != 0 {
		t.Fatalf("refused exports left %v, %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(cordon.Worktree(cfg, "running"), "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := <-launched; err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := client.Export(t.Context(), cordon.ExportRequestFor("running")); err != nil {
		t.Fatalf("Export: %v", err)
	}
	receipt := filepath.Join(h.exports, "running")
	for _, dir := range []string{cordon.RunDir(cfg, "running"), receipt} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("before release: %v", err)
		}
	}
	if err := client.ReleaseFinished(t.Context(), []string{"running"}); err != nil {
		t.Fatalf("ReleaseFinished: %v", err)
	}
	for _, dir := range []string{cordon.RunDir(cfg, "running"), receipt} {
		if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("after release %s: %v", dir, err)
		}
	}
}

// gitBase is a repository holding one commit, and that commit.
func gitBase(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	cordon.Git(t, base, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(base, "README"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cordon.Git(t, base, "add", "README")
	cordon.Git(t, base, "commit", "-q", "-m", "base")
	return base, cordon.Git(t, base, "rev-parse", "HEAD")
}

// importedTree imports the export in dir into a fresh clone of base and
// returns the tree the importer committed, which must change something.
func importedTree(t *testing.T, base, dir, baseSHA string) string {
	t.Helper()
	checkout := filepath.Join(t.TempDir(), "checkout")
	cordon.Git(t, "", "clone", "-q", base, checkout)
	got, err := importer.Import(t.Context(), dir, checkout, importer.Options{BaseSHA: baseSHA, CommitDate: time.Unix(1_700_000_000, 0)})
	if err != nil {
		t.Fatalf("Import %s: %v", dir, err)
	}
	if len(got.Changes) == 0 {
		t.Fatalf("Import %s changed nothing", dir)
	}
	return cordon.Git(t, checkout, "rev-parse", got.CommitSHA+"^{tree}")
}

// TestAServedLaunchSeedsTheExactBaseAndCarriesNoToken launches through the
// launcher with the base seeded on the client's side: the run starts at
// the exact base commit, the serving backend's own seeder is never called,
// the token reaches the agent on its descriptor, and neither the agent's
// argv and environment nor any file the launch or the launcher leaves, nor
// the launcher's log, holds it.
func TestAServedLaunchSeedsTheExactBaseAndCarriesNoToken(t *testing.T) {
	t.Parallel()
	cfg := cordon.HostConfig(t)
	base := t.TempDir()
	cordon.Git(t, base, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(base, "file"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cordon.Git(t, base, "add", "file")
	cordon.Git(t, base, "commit", "-q", "-m", "base")
	baseSHA := cordon.Git(t, base, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(base, "file"), []byte("later\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cordon.Git(t, base, "commit", "-q", "-am", "later")

	h := newServedHost(t, cfg, cordon.GitSeeder(base))
	client := h.client(t, &records{})
	req := cordon.HostRequest("witness", "the prompt", "witness")
	req.BaseSHA = baseSHA
	got, err := client.Launch(t.Context(), req)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	// A clean exit's transcript stays on the launcher's side.
	transcript, err := os.ReadFile(cordon.Transcript(cfg, "witness"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ExitCode != 0 {
		t.Fatalf("exit %d:\n%s", got.ExitCode, transcript)
	}
	token := []byte(cordon.FixtureToken)
	if want := fmt.Sprintf("token %d %x\n", len(token), sha256.Sum256(token)); !bytes.Contains(transcript, []byte(want)) {
		t.Fatalf("the agent did not read the token on its descriptor:\n%s", transcript)
	}
	if !bytes.Contains(transcript, []byte("arg "+cordon.FixtureAgentArg+"\n")) || !bytes.Contains(transcript, []byte("env HOME=")) {
		t.Fatalf("the transcript lacks the agent's argv or environment:\n%s", transcript)
	}
	if at := bytes.Index(transcript, token); at >= 0 {
		t.Fatalf("the agent's argv or environment holds the token at offset %d", at)
	}
	if head := cordon.Git(t, cordon.Worktree(cfg, "witness"), "rev-parse", "HEAD"); head != baseSHA {
		t.Fatalf("the run starts at %s, want the base %s", head, baseSHA)
	}
	for _, dir := range []string{cfg.RunsRoot, h.seedRoot, h.scratch} {
		requireNoToken(t, dir, token)
	}
	if at := bytes.Index([]byte(h.log.String()), token); at >= 0 {
		t.Fatalf("the launcher's log holds the token at offset %d", at)
	}
	if h.ownSeeds.calls.Load() != 0 {
		t.Fatal("the serving backend's own seeder was called")
	}
}

// TestAServedRefusalIsTheBackendsTypedFailure holds an approval on the
// launcher's side: the hello says so, and a launch, a check, and a full
// pass come back as the backend's own *cordon.ConformanceFailure, its
// check named, with no run directory claimed.
func TestAServedRefusalIsTheBackendsTypedFailure(t *testing.T) {
	t.Parallel()
	cfg := cordon.PinnedConfig(t)
	h := newServedHost(t, cfg, moduleSeeder{}, func(b *cordon.Backend) {
		cordon.WithHostFacts(b, cordon.HostFacts{Accessibility: true, DesktopSession: true})
	})
	client := h.client(t, &records{})
	hello, err := client.Hello(t.Context())
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if hello.Facts == nil || !hello.Facts.Accessibility || hello.Facts.ScreenRecording {
		t.Fatalf("the hello's facts = %+v, want accessibility alone", hello.Facts)
	}
	_, err = client.Launch(t.Context(), cordon.HostRequest("refused", "", "exit", "0"))
	requireApprovalRefusal(t, "Launch", err)
	_, err = client.Check(t.Context(), cordon.HostCheck(cfg, "refused-check", "exit", "0"))
	requireApprovalRefusal(t, "Check", err)
	for _, id := range []string{"refused", "refused-check"} {
		if _, err := os.Stat(cordon.Worktree(cfg, id)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("a refused launch claimed %s's directory: %v", id, err)
		}
	}
	requireApprovalRefusal(t, "Full", client.Full(t.Context()))
}

func requireApprovalRefusal(t *testing.T, op string, err error) {
	t.Helper()
	var failure *cordon.ConformanceFailure
	if !errors.As(err, &failure) || failure.Check != cordon.CheckApprovals || !strings.Contains(failure.Reason, "accessibility") {
		t.Fatalf("%s = %v, want the %q failure naming accessibility", op, err, cordon.CheckApprovals)
	}
}

// TestAServedPassRecordsBoundToTheHello runs a full pass on the launcher:
// each record its suite writes reaches the client's recorder, bound to the
// hello's configuration digest, and the class is declared there. A record
// the client can't keep fails the next pass and leaves the class
// undeclared on the launcher.
func TestAServedPassRecordsBoundToTheHello(t *testing.T) {
	t.Parallel()
	cfg := cordon.SuiteConfig(t)
	h := newServedHost(t, cfg, moduleSeeder{})
	rec := &records{}
	client := h.client(t, rec)
	hello, err := client.Hello(t.Context())
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if err := client.Full(t.Context()); err != nil {
		t.Fatalf("Full: %v", err)
	}
	kept := rec.kept()
	var outcomes []domain.ConformanceOutcome
	for _, r := range kept {
		outcomes = append(outcomes, r.Outcome)
		if r.ConfigurationDigest != hello.ConfigurationDigest {
			t.Errorf("a %s record names digest %s, want the hello's %s", r.Outcome, r.ConfigurationDigest, hello.ConfigurationDigest)
		}
	}
	if want := []domain.ConformanceOutcome{domain.ConformanceSuperseded, domain.ConformancePassed}; !slices.Equal(outcomes, want) {
		t.Fatalf("recorded %v, want %v", outcomes, want)
	}
	if !declares(h.backend) {
		t.Fatal("a recorded pass does not declare the class on the launcher")
	}
	rec.fail(errors.New("the store is full"))
	if err := client.Full(t.Context()); !errors.Is(err, cordon.ErrConformanceRecord) {
		t.Fatalf("Full with a failing recorder = %v, want ErrConformanceRecord", err)
	}
	if declares(h.backend) {
		t.Fatal("a pass whose record was not kept left the class declared")
	}
}

// TestAKilledLauncherLeavesItsRunForTheNext kills a launcher in its own
// process with SIGKILL mid-launch: the client's launch reads as detached,
// the run keeps running, and a restarted launcher recovers it and reports
// its failed end with its transcript. The kill is direct, not through
// launchctl, so it does not show what launchd does to a job's processes;
// the README's AbandonProcessGroup is what keeps them.
func TestAKilledLauncherLeavesItsRunForTheNext(t *testing.T) {
	t.Parallel()
	cfg := cordon.HostConfig(t)
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortDir(t), "l.sock")
	seedRoot := filepath.Join(t.TempDir(), "seeds")
	launcher := startLauncher(t, cfgPath, socket, seedRoot)
	first := newClient(t, socket, moduleSeeder{}, &records{})
	launched := make(chan error, 1)
	go func() {
		_, err := first.Launch(t.Context(), cordon.HostRequest("restart", "", "spawn", "release", "3"))
		launched <- err
	}()
	agent := startedAgent(t, cfg, "restart")
	_ = launcher.Process.Kill()
	_ = launcher.Wait()
	if err := <-launched; !errors.Is(err, cordon.ErrDetached) {
		t.Fatalf("Launch across the kill = %v, want ErrDetached", err)
	}
	if !cordon.ProcessLive(t, agent) {
		t.Fatal("the agent died with its launcher")
	}
	startLauncher(t, cfgPath, socket, seedRoot)
	got := recoverReleased(t, newClient(t, socket, moduleSeeder{}, &records{}), cfg, "restart")
	requireFailedRecovery(t, got)
	requireSurvivorsDead(t, cfg, "restart")
}

// startLauncher runs serveVerb in its own process and waits until it
// accepts; the test's cleanup kills it.
func startLauncher(t *testing.T, cfgPath, socket, seedRoot string) *exec.Cmd {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var said syncBuffer
	cmd := exec.Command(self, serveVerb, cfgPath, socket, seedRoot) //nolint:gosec,noctx // G204: this test binary; killed below or at cleanup
	cmd.Stdout, cmd.Stderr = &said, &said
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	deadline := time.Now().Add(30 * time.Second)
	for {
		conn, err := net.Dial("unix", socket)
		if err == nil {
			_ = conn.Close()
			return cmd
		}
		if time.Now().After(deadline) {
			t.Fatalf("the launcher never accepted: %v; it said: %s", err, said.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// serveForTest serves a backend over the configuration at args[0] on the
// socket args[1], staging seeds under args[2], until it is killed. Its
// backend's own seeder is the launcher's, which refuses.
func serveForTest(args []string) int {
	if len(args) != 3 {
		return 2
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fail := func(err error) int {
		logger.Error("test launcher", "error", err)
		return 1
	}
	body, err := os.ReadFile(args[0])
	if err != nil {
		return fail(err)
	}
	var cfg cordon.Config
	if err := json.Unmarshal(body, &cfg); err != nil {
		return fail(err)
	}
	b, err := cordon.New(cfg, cordonhost.NoForgeSeeder{})
	if err != nil {
		return fail(err)
	}
	suite, err := cordon.NewSuite(b, cordon.WithConformanceRecorder(cordonhost.ConnectionRecorder()))
	if err != nil {
		return fail(err)
	}
	server, err := cordonhost.NewServer(cordonhost.ServerConfig{
		Build: testBuild, Backend: b, Suite: suite, Account: cordonhost.Account{Name: "runner", UID: os.Getuid()},
		SeedRoot: args[2], Logger: logger,
	})
	if err != nil {
		return fail(err)
	}
	// The killed launcher's socket is still there.
	if err := os.Remove(args[1]); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fail(err)
	}
	ln, err := net.Listen("unix", args[1])
	if err != nil {
		return fail(err)
	}
	return fail(server.Serve(ln))
}

// servedHost is a launcher served in this process.
type servedHost struct {
	backend           *cordon.Backend
	socket            string
	seedRoot, scratch string
	// exports is every client's ExportRoot, so a fresh client finds the
	// receipts an earlier one kept.
	exports      string
	clientSeeder cordon.Seeder
	log          *syncBuffer
	ownSeeds     *countingSeeder
}

func newServedHost(t *testing.T, cfg cordon.Config, clientSeeder cordon.Seeder, configure ...func(*cordon.Backend)) *servedHost {
	t.Helper()
	h := &servedHost{
		socket: filepath.Join(shortDir(t), "l.sock"), seedRoot: filepath.Join(t.TempDir(), "seeds"),
		scratch: t.TempDir(), exports: t.TempDir(), clientSeeder: clientSeeder, log: &syncBuffer{}, ownSeeds: &countingSeeder{},
	}
	b, err := cordon.New(cfg, h.ownSeeds)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range configure {
		c(b)
	}
	h.backend = b
	suite := cordon.HostSuite(t, b, cordon.WithConformanceRecorder(cordonhost.ConnectionRecorder()))
	server, err := cordonhost.NewServer(cordonhost.ServerConfig{
		Build: testBuild, Backend: b, Suite: suite, Account: cordonhost.Account{Name: "runner", UID: os.Getuid()},
		SeedRoot: h.seedRoot, Logger: slog.New(slog.NewTextHandler(h.log, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", h.socket)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- server.Serve(ln) }()
	t.Cleanup(func() {
		server.Close()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})
	return h
}

// client is a fresh client of h, as a restarted daemon would make; each
// keeps its failed runs' transcripts under h's one scratch root.
func (h *servedHost) client(t *testing.T, rec cordon.ConformanceRecorder) *cordonhost.Client {
	t.Helper()
	c, err := cordonhost.NewClient(cordonhost.ClientConfig{
		Socket: h.socket, Build: testBuild, Seeder: h.clientSeeder, Recorder: rec, ScratchRoot: h.scratch,
		ExportRoot: h.exports,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Detach)
	return c
}

func newClient(t *testing.T, socket string, seeder cordon.Seeder, rec cordon.ConformanceRecorder) *cordonhost.Client {
	t.Helper()
	c, err := cordonhost.NewClient(cordonhost.ClientConfig{
		Socket: socket, Build: testBuild, Seeder: seeder, Recorder: rec, ScratchRoot: t.TempDir(),
		ExportRoot: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Detach)
	return c
}

// moduleSeeder seeds a worktree holding one file.
type moduleSeeder struct{}

func (moduleSeeder) FetchBaseWorktree(_ context.Context, _, _, _, dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "README"), []byte("seeded\n"), 0o600)
}

// countingSeeder is a served backend's own seeder, which a launch behind
// the launcher must never call.
type countingSeeder struct{ calls atomic.Int64 }

func (s *countingSeeder) FetchBaseWorktree(context.Context, string, string, string, string) error {
	s.calls.Add(1)
	return errors.New("the serving backend's own seeder was called")
}

// records is a client's recorder.
type records struct {
	mu      sync.Mutex
	records []domain.BackendConformance
	err     error
}

func (r *records) RecordBackendConformance(_ context.Context, record domain.BackendConformance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.records = append(r.records, record)
	return nil
}

func (r *records) kept() []domain.BackendConformance {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.records)
}

func (r *records) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

func declares(b *cordon.Backend) bool {
	return slices.Equal(b.Capabilities().Snapshot(), domain.NewCapabilitySnapshot(domain.CordonedCapabilities()...))
}

// startedAgent waits for runID's fake agent to start its survivors and
// returns the agent's pid.
func startedAgent(t *testing.T, cfg cordon.Config, runID string) int {
	t.Helper()
	worktree := cordon.Worktree(cfg, runID)
	awaitFile(t, filepath.Join(worktree, "pids"))
	return readPids(t, filepath.Join(worktree, "agent.pid"))[0]
}

// recoverReleased recovers runID through s while its agent, started with
// spawn release 3, is released to exit.
func recoverReleased(t *testing.T, s seam, cfg cordon.Config, runID string) cordon.RecoveryResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	type recovered struct {
		res cordon.RecoveryResult
		err error
	}
	done := make(chan recovered, 1)
	go func() {
		res, err := s.Recover(ctx, cordon.RecoverRequest{RunID: runID, Export: cordon.ExportRequestFor(runID)})
		done <- recovered{res, err}
	}()
	if err := os.WriteFile(filepath.Join(cordon.Worktree(cfg, runID), "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("Recover: %v", got.err)
	}
	return got.res
}

// recoverReleasedCheck recovers runID's check through s while its
// command, started with spawn release 3, is released to exit.
func recoverReleasedCheck(t *testing.T, s seam, cfg cordon.Config, runID string) cordon.CheckResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	type recovered struct {
		res cordon.CheckResult
		err error
	}
	done := make(chan recovered, 1)
	go func() {
		res, err := s.RecoverCheck(ctx, cordon.CheckRecoverRequest{RunID: runID, MaxOutputBytes: 1 << 20})
		done <- recovered{res, err}
	}()
	if err := os.WriteFile(filepath.Join(cordon.Worktree(cfg, runID), "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("RecoverCheck: %v", got.err)
	}
	return got.res
}

func requireFailedRecovery(t *testing.T, got cordon.RecoveryResult) {
	t.Helper()
	if got.Outcome != cordon.RecoveryFailed || got.EndedBy != cordon.EndedByExited || got.ExitCode != 3 {
		t.Fatalf("Recover = %+v, want failed with exit 3", got)
	}
	transcript, err := os.ReadFile(got.TranscriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(transcript, []byte("working\n")) {
		t.Fatalf("the recovered transcript lacks the run's output: %q", transcript)
	}
}

func requireTranscript(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path) //nolint:gosec // G304: a transcript the launch under test wrote
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

// requireSurvivorsDead reads the pids runID's fake agent wrote and fails
// on any still alive.
func requireSurvivorsDead(t *testing.T, cfg cordon.Config, runID string) {
	t.Helper()
	pids := readPids(t, filepath.Join(cordon.Worktree(cfg, runID), "pids"))
	if len(pids) != 3 {
		t.Fatalf("pids = %v, want three", pids)
	}
	for _, pid := range pids {
		if cordon.ProcessLive(t, pid) {
			t.Errorf("survivor %d is alive after the launch ended", pid)
		}
	}
}

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

func awaitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%s never appeared", path)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// requireNoToken fails the test when any file under dir holds token. It
// names the file and the offset, never the bytes around them.
func requireNoToken(t *testing.T, dir string, token []byte) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		body, err := os.ReadFile(path) //nolint:gosec // G304: a file under the test's own directories
		if err != nil {
			return err
		}
		if at := bytes.Index(body, token); at >= 0 {
			t.Errorf("%s holds the token at offset %d", path, at)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("walk %s: %v", dir, err)
	}
}

// shortDir is a directory short enough to hold a Unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	created, err := os.MkdirTemp("", "cc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(created) })
	dir, err := filepath.EvalSymlinks(created)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
