package cordon

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"
)

func checkRequest(id string) CheckRequest {
	return CheckRequest{
		RunID: id, Repository: "owner/repo", BaseRef: "main", BaseSHA: "0123456789abcdef0123456789abcdef01234567",
		Argv: []string{"make", "test"}, MaxOutputBytes: 1 << 20,
	}
}

// checkWriting is a fake check command that writes out and err to its two
// streams, leaves a file in its worktree, and exits with code.
func checkWriting(out, err string, code int) agentScript {
	return func(spec processSpec, p *fakeProcess) {
		_, _ = io.WriteString(spec.stdout, out)
		_, _ = io.WriteString(spec.stderr, err)
		_ = os.WriteFile(filepath.Join(spec.dir, "left-behind"), nil, 0o600)
		p.exit(code)
	}
}

// TestCheckRunsItsCommandInAFreshSeededWorktree shows a check runs its
// argv under the rule file in a worktree seeded for it, with the launch's
// environment, no stdin, and no token, and that a second check of the same
// commit gets a run and a worktree of its own.
func TestCheckRunsItsCommandInAFreshSeededWorktree(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		l := &fakeLauncher{host: host, script: checkWriting("out\n", "err\n", 3)}
		seeder := &fakeSeeder{}
		b, err := New(cfg, seeder)
		if err != nil {
			t.Fatal(err)
		}
		useFakes(b, l)
		for i, id := range []string{"check-a", "check-b"} {
			got, err := b.Check(t.Context(), checkRequest(id))
			if err != nil {
				t.Fatalf("Check %s: %v", id, err)
			}
			if got.ExitCode != 3 || got.EndedBy != EndedByExited || got.Truncated || got.Reason != "" {
				t.Fatalf("result = %+v, want exit 3 ended by exited", got)
			}
			if string(got.Output) != "out\nerr\n" {
				t.Fatalf("output = %q, want both streams in one", got.Output)
			}
			if alive := host.alive(); len(alive) != 0 {
				t.Fatalf("members still alive after the check: %v", alive)
			}
			lay := layoutFor(cfg.RunsRoot, id)
			cmd := l.specs[2*i+1]
			if cmd.dir != lay.worktree {
				t.Fatalf("command dir = %s, want %s", cmd.dir, lay.worktree)
			}
			if want := confinedArgv(cfg, lay, "make", "test"); !slices.Equal(cmd.argv, want) {
				t.Fatalf("command argv = %q, want %q", cmd.argv, want)
			}
			if !slices.Equal(cmd.env, launchEnv(cfg, lay)) {
				t.Fatalf("command env = %q, want the launch's", cmd.env)
			}
			if cmd.stdin != nil || cmd.secret != nil || l.secrets[2*i+1] != nil {
				t.Fatal("the command got a stdin or a token pipe")
			}
			if stderr, err := os.ReadFile(lay.stderr); err != nil || len(stderr) != 0 {
				t.Fatalf("ctl/stderr = %q, %v; want it empty", stderr, err)
			}
		}
		if seeder.calls != 2 {
			t.Fatalf("seeded %d times, want once per check", seeder.calls)
		}
		if l.specs[3].dir == l.specs[1].dir {
			t.Fatal("the second check ran in the first's worktree")
		}
	})
}

func TestCheckRefusesAnInvalidRequestBeforeCreatingAnything(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		l := &fakeLauncher{host: newFakeHost()}
		seeder := &fakeSeeder{}
		b, err := New(cfg, seeder)
		if err != nil {
			t.Fatal(err)
		}
		useFakes(b, l)
		for name, edit := range map[string]func(*CheckRequest){
			"run id":      func(r *CheckRequest) { r.RunID = "../escape" },
			"repository":  func(r *CheckRequest) { r.Repository = "" },
			"base ref":    func(r *CheckRequest) { r.BaseRef = "" },
			"base commit": func(r *CheckRequest) { r.BaseSHA = "" },
			"no argv":     func(r *CheckRequest) { r.Argv = nil },
			"empty name":  func(r *CheckRequest) { r.Argv = []string{"", "x"} },
			"NUL":         func(r *CheckRequest) { r.Argv = []string{"make", "te\x00st"} },
			"no bound":    func(r *CheckRequest) { r.MaxOutputBytes = 0 },
		} {
			req := checkRequest("check-bad")
			edit(&req)
			if _, err := b.Check(t.Context(), req); err == nil {
				t.Errorf("Check with a bad %s succeeded", name)
			}
		}
		if len(l.specs) != 0 || seeder.calls != 0 {
			t.Fatalf("started %d processes and seeded %d times", len(l.specs), seeder.calls)
		}
		if exists(t, filepath.Join(cfg.RunsRoot, "check-bad")) {
			t.Fatal("a refused check left its directory")
		}
	})
}

func TestCheckOutputKeepsTheTailPastTheBound(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		out := strings.Repeat("a", 100) + "the end"
		l := &fakeLauncher{host: newFakeHost(), script: checkWriting(out, "!", 0)}
		req := checkRequest("check-tail")
		req.MaxOutputBytes = 8
		got, err := fakeBackend(t, cfg, l).Check(t.Context(), req)
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if !got.Truncated || string(got.Output) != "the end!" {
			t.Fatalf("output = %q truncated %v, want the last 8 bytes, truncated", got.Output, got.Truncated)
		}
		// Output of exactly the bound is whole.
		l.script = checkWriting("1234567", "8", 0)
		req.RunID = "check-whole"
		if got, err = fakeBackend(t, cfg, l).Check(t.Context(), req); err != nil {
			t.Fatalf("Check: %v", err)
		}
		if got.Truncated || string(got.Output) != "12345678" {
			t.Fatalf("output = %q truncated %v, want all 8 bytes, whole", got.Output, got.Truncated)
		}
	})
}

// TestSupervisorRefusesATokenPipeTheKindDoesNotTake hands a check's
// supervisor a pipe on the token's descriptor, and a writer's none: each
// claims nothing and starts nothing.
func TestSupervisorRefusesATokenPipeTheKindDoesNotTake(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		kind   launchKind
		secret []byte
		want   string
	}{
		{kindCheck, []byte("not for a check"), "takes no token"},
		{kindWriter, nil, "needs the token's pipe"},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			synctest.Test(t, func(t *testing.T) {
				l := &fakeLauncher{host: newFakeHost(), script: checkWriting("", "", 0)}
				b := fakeBackend(t, cfg, l)
				lay, err := b.claimLaunch("run-pipe")
				if err != nil {
					t.Fatal(err)
				}
				if err := writeExclusive(lay.prompt, nil); err != nil {
					t.Fatal(err)
				}
				_, err = b.supervise(t.Context(), lay, supervisorRequest{
					RunID: "run-pipe", Kind: tc.kind,
					Program: supervisedProcess{Argv: confinedArgv(cfg, lay, "make"), Dir: lay.runRoot, Env: launchEnv(cfg, lay)},
				}, tc.secret)
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("supervise = %v, want %q", err, tc.want)
				}
				if len(l.specs) != 0 || exists(t, lay.claim) || exists(t, lay.ended) {
					t.Fatalf("started %d processes; claim %v, end %v", len(l.specs), exists(t, lay.claim), exists(t, lay.ended))
				}
			})
		})
	}
}

func TestCheckCancellationEndsEveryMember(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		ctx, cancel := context.WithCancel(t.Context())
		l := &fakeLauncher{host: host, script: func(processSpec, *fakeProcess) { cancel() }}
		got, err := fakeBackend(t, cfg, l).Check(ctx, checkRequest("check-cancel"))
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if got.EndedBy != EndedByCanceled {
			t.Fatalf("EndedBy = %s, want canceled", got.EndedBy)
		}
		if sent := l.agent.sent(); !slices.Equal(sent, []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL}) {
			t.Fatalf("signals = %v, want SIGTERM then SIGKILL", sent)
		}
		if alive := host.alive(); len(alive) != 0 {
			t.Fatalf("members still alive: %v", alive)
		}
	})
}

func TestCheckStallLimitEndsASilentCommand(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		l := &fakeLauncher{host: host, obeyTerm: true, script: func(spec processSpec, _ *fakeProcess) {
			time.Sleep(cfg.StallLimit / 2)
			_, _ = io.WriteString(spec.stderr, "tick\n")
		}}
		start := time.Now()
		got, err := fakeBackend(t, cfg, l).Check(t.Context(), checkRequest("check-stall"))
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		if got.EndedBy != EndedByStalled || string(got.Output) != "tick\n" {
			t.Fatalf("result = %+v, want stalled with its output", got)
		}
		if elapsed := time.Since(start); elapsed < cfg.StallLimit*3/2 {
			t.Fatalf("stalled after %v, before the limit passed silent", elapsed)
		}
		if alive := host.alive(); len(alive) != 0 {
			t.Fatalf("members still alive: %v", alive)
		}
	})
}

// TestOnlyAWritersRunExports shows Export and Recover refuse a check's
// run for good, even after a clean exit with a base commit.
func TestOnlyAWritersRunExports(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		b := fakeBackend(t, cfg, &fakeLauncher{host: host, script: checkWriting("", "", 0)})
		if got, err := b.Check(t.Context(), checkRequest("check-export")); err != nil || got.ExitCode != 0 {
			t.Fatalf("Check = %+v, %v", got, err)
		}
		if _, err := b.Export(t.Context(), exportRequest("check-export")); !errors.Is(err, ErrNotExportable) {
			t.Fatalf("Export = %v, want ErrNotExportable", err)
		}
		_, err := fakeBackend(t, cfg, &fakeLauncher{host: host}).Recover(t.Context(),
			RecoverRequest{RunID: "check-export", Export: exportRequest("check-export")})
		if !errors.Is(err, ErrNotExportable) {
			t.Fatalf("Recover = %v, want ErrNotExportable", err)
		}
		l := layoutFor(cfg.RunsRoot, "check-export")
		if exists(t, l.export) || exists(t, l.exportRecord) {
			t.Fatal("a refused export left its directory or record")
		}
	})
}

type failingSeeder struct{}

func (failingSeeder) FetchBaseWorktree(context.Context, string, string, string, string) error {
	return errors.New("the fetch failed")
}

// TestRecoverCheckFinishesEveryInterruptedState has one row per state a
// restart can find a check in; a fresh backend on the same host recovers
// each.
func TestRecoverCheckFinishesEveryInterruptedState(t *testing.T) {
	t.Parallel()
	const id = "check-recover"
	checked := func(t *testing.T, cfg Config, host *fakeHost, code int) *Backend {
		t.Helper()
		b := fakeBackend(t, cfg, &fakeLauncher{host: host, script: checkWriting("ran\n", "", code)})
		if _, err := b.Check(t.Context(), checkRequest(id)); err != nil {
			t.Fatalf("Check: %v", err)
		}
		return b
	}
	for _, row := range []struct {
		name    string
		leave   func(t *testing.T, cfg Config, host *fakeHost)
		endedBy EndedBy
		code    int
		output  string
		reason  string
		proof   bool
	}{{
		name: "never claimed",
		leave: func(t *testing.T, cfg Config, host *fakeHost) {
			b := fakeBackend(t, cfg, &fakeLauncher{host: host})
			b.startSupervisor = func(spec supervisorSpec) (supervisorProcess, error) {
				closeSpec(spec)
				return nil, errors.New("the daemon stopped")
			}
			if _, err := b.Check(t.Context(), checkRequest(id)); err == nil {
				t.Fatal("Check returned no error with no supervisor")
			}
		},
		endedBy: EndedByLost, code: -1, reason: "no supervisor started it",
	}, {
		// A daemon that stopped between the claim and the request.
		name: "request never written",
		leave: func(t *testing.T, cfg Config, host *fakeHost) {
			b, err := New(cfg, failingSeeder{})
			if err != nil {
				t.Fatal(err)
			}
			useFakes(b, &fakeLauncher{host: host})
			if _, err := b.Check(t.Context(), checkRequest(id)); err == nil {
				t.Fatal("Check returned no error with a failed seed")
			}
		},
		endedBy: EndedByLost, code: -1, reason: "no supervisor started it",
	}, {
		name: "supervisor gone without an end record",
		leave: func(t *testing.T, cfg Config, host *fakeHost) {
			checked(t, cfg, host, 0)
			if err := os.Remove(layoutFor(cfg.RunsRoot, id).ended); err != nil {
				t.Fatal(err)
			}
			host.set(201, true) // a survivor the proof must end
		},
		endedBy: EndedByLost, code: -1, output: "ran\n", reason: "its supervisor ended before it recorded an end", proof: true,
	}, {
		name:    "recorded end",
		leave:   func(t *testing.T, cfg Config, host *fakeHost) { checked(t, cfg, host, 3) },
		endedBy: EndedByExited, code: 3, output: "ran\n",
	}, {
		name: "cancellation marker over a clean exit",
		leave: func(t *testing.T, cfg Config, host *fakeHost) {
			if err := checked(t, cfg, host, 0).RequestCancellation(t.Context(), id); err != nil {
				t.Fatalf("RequestCancellation: %v", err)
			}
		},
		endedBy: EndedByCanceled, output: "ran\n",
	}} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			synctest.Test(t, func(t *testing.T) {
				host := newFakeHost()
				row.leave(t, cfg, host)
				launcher := &fakeLauncher{host: host}
				got, err := fakeBackend(t, cfg, launcher).RecoverCheck(t.Context(), CheckRecoverRequest{RunID: id, MaxOutputBytes: 1 << 20})
				if err != nil {
					t.Fatalf("RecoverCheck: %v", err)
				}
				if got.EndedBy != row.endedBy || got.ExitCode != row.code || string(got.Output) != row.output || got.Reason != row.reason {
					t.Fatalf("RecoverCheck = %+v, want %s exit %d output %q reason %q", got, row.endedBy, row.code, row.output, row.reason)
				}
				if alive := host.alive(); len(alive) != 0 {
					t.Fatalf("members alive after recovery: %v", alive)
				}
				if proved := len(launcher.specs) == 1; proved != row.proof || len(launcher.specs) > 1 {
					t.Fatalf("recovery started %d processes, want the proof's canary: %v", len(launcher.specs), row.proof)
				}
				if exists(t, layoutFor(cfg.RunsRoot, id).export) {
					t.Fatal("recovering a check exported it")
				}
			})
		})
	}
}

func TestRecoverCheckRefusesAWritersRun(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		finished(t, cfg, host, "run-writer", 0)
		before, err := os.ReadFile(layoutFor(cfg.RunsRoot, "run-writer").ended)
		if err != nil {
			t.Fatal(err)
		}
		_, err = fakeBackend(t, cfg, &fakeLauncher{host: host}).RecoverCheck(t.Context(),
			CheckRecoverRequest{RunID: "run-writer", MaxOutputBytes: 1 << 20})
		if err == nil || !strings.Contains(err.Error(), "not a check") {
			t.Fatalf("RecoverCheck of a writer's run = %v, want a refusal", err)
		}
		if after, err := os.ReadFile(layoutFor(cfg.RunsRoot, "run-writer").ended); err != nil || !bytes.Equal(before, after) {
			t.Fatalf("the writer's end record changed: %v", err)
		}
	})
}

func TestLaunchKindRegistration(t *testing.T) {
	t.Parallel()
	for _, k := range allLaunchKinds {
		if !k.valid() {
			t.Fatalf("%q is registered but invalid", k)
		}
	}
	if launchKind("").valid() {
		t.Fatal("the zero launchKind is valid")
	}
}
