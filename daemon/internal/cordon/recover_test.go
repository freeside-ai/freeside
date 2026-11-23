package cordon

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// writingAChange is a clean agent's work: a change and its summary.
func writingAChange(t *testing.T) func(worktree string) {
	return func(wt string) {
		writeIn(t, wt, "src/change.txt", "the change\n")
		writeIn(t, wt, export.SummaryEvidencePath, "# Summary\n")
	}
}

// finished runs one fake launch to its end on host and returns its backend.
func finished(t *testing.T, cfg Config, host *fakeHost, id string, code int) *Backend {
	t.Helper()
	b := fakeBackend(t, cfg, &fakeLauncher{host: host, script: agentWriting(code, writingAChange(t))})
	if _, err := b.Launch(t.Context(), request(id)); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	return b
}

// unsupervised leaves id as a daemon that stopped before its supervisor
// started would: prepared and seeded, with its request written and nothing
// claimed. start receives the supervisor's spec and owns its files.
func unsupervised(t *testing.T, cfg Config, host *fakeHost, id string, start func(supervisorSpec)) {
	t.Helper()
	b := fakeBackend(t, cfg, &fakeLauncher{host: host})
	b.startSupervisor = func(spec supervisorSpec) (supervisorProcess, error) {
		start(spec)
		return nil, errors.New("the daemon stopped")
	}
	if _, err := b.Launch(t.Context(), request(id)); err == nil {
		t.Fatal("Launch returned no error with no supervisor")
	}
}

func closeSpec(spec supervisorSpec) {
	_ = spec.stderr.Close()
	if spec.secret != nil {
		_ = spec.secret.Close()
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// TestRecoverFinishesEveryInterruptedState has one row per state a restart
// can find a run in. leave builds the state with a first backend; a fresh
// backend on the same host then recovers it, and release, when set, runs
// once Recover is waiting.
func TestRecoverFinishesEveryInterruptedState(t *testing.T) {
	t.Parallel()
	const id = "run-recover"
	for _, row := range []struct {
		name  string
		leave func(t *testing.T, cfg Config, host *fakeHost) (release func())
		want  RecoveryOutcome
		// reason is a lost run's reason.
		reason string
		// proof is whether recovery ran the run-end proof.
		proof bool
	}{{
		name: "never claimed",
		leave: func(t *testing.T, cfg Config, host *fakeHost) func() {
			unsupervised(t, cfg, host, id, closeSpec)
			return nil
		},
		want: RecoveryLost, reason: "no supervisor started it",
	}, {
		name: "supervisor still running",
		leave: func(t *testing.T, cfg Config, host *fakeHost) func() {
			release := make(chan struct{})
			change := writingAChange(t)
			b := fakeBackend(t, cfg, &fakeLauncher{host: host, script: agentWriting(0, func(wt string) {
				<-release
				change(wt)
			})})
			launched := make(chan error, 1)
			go func() {
				_, err := b.Launch(t.Context(), request(id))
				launched <- err
			}()
			synctest.Wait()
			b.Detach()
			if err := <-launched; !errors.Is(err, ErrDetached) {
				t.Fatalf("Launch after Detach = %v, want ErrDetached", err)
			}
			return func() { close(release) }
		},
		want: RecoveryExported,
	}, {
		name: "supervisor gone without an end record",
		leave: func(t *testing.T, cfg Config, host *fakeHost) func() {
			finished(t, cfg, host, id, 0)
			if err := os.Remove(layoutFor(cfg.RunsRoot, id).ended); err != nil {
				t.Fatal(err)
			}
			host.set(201, true) // a survivor the proof must end
			return nil
		},
		want: RecoveryLost, reason: "its supervisor ended before it recorded an end", proof: true,
	}, {
		name: "clean end with an interrupted export",
		leave: func(t *testing.T, cfg Config, host *fakeHost) func() {
			finished(t, cfg, host, id, 0)
			writeIn(t, layoutFor(cfg.RunsRoot, id).export, "partial", "half an export")
			return nil
		},
		want: RecoveryExported,
	}, {
		name: "already exported",
		leave: func(t *testing.T, cfg Config, host *fakeHost) func() {
			if _, err := finished(t, cfg, host, id, 0).Export(t.Context(), exportRequest(id)); err != nil {
				t.Fatalf("Export: %v", err)
			}
			return nil
		},
		want: RecoveryExported,
	}, {
		name: "nonzero exit",
		leave: func(t *testing.T, cfg Config, host *fakeHost) func() {
			finished(t, cfg, host, id, 3)
			return nil
		},
		want: RecoveryFailed,
	}, {
		name: "cancellation marker over a clean exit",
		leave: func(t *testing.T, cfg Config, host *fakeHost) func() {
			if err := finished(t, cfg, host, id, 0).RequestCancellation(t.Context(), id); err != nil {
				t.Fatalf("RequestCancellation: %v", err)
			}
			return nil
		},
		want: RecoveryCanceled,
	}} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			l := layoutFor(cfg.RunsRoot, id)
			synctest.Test(t, func(t *testing.T) {
				host := newFakeHost()
				release := row.leave(t, cfg, host)
				launcher := &fakeLauncher{host: host}
				b := fakeBackend(t, cfg, launcher)
				type recovered struct {
					res RecoveryResult
					err error
				}
				done := make(chan recovered, 1)
				go func() {
					res, err := b.Recover(t.Context(), RecoverRequest{RunID: id, Export: exportRequest(id)})
					done <- recovered{res, err}
				}()
				synctest.Wait()
				if release != nil {
					release()
				}
				got := <-done
				if got.err != nil {
					t.Fatalf("Recover: %v", got.err)
				}
				if got.res.Outcome != row.want || got.res.Reason != row.reason {
					t.Fatalf("Recover = %s %q, want %s %q", got.res.Outcome, got.res.Reason, row.want, row.reason)
				}
				if alive := host.alive(); len(alive) != 0 {
					t.Fatalf("members alive after recovery: %v", alive)
				}
				// Recovery starts no process but the proof's canary.
				if proved := len(launcher.specs) == 1 && slices.Contains(launcher.specs[0].argv, canaryProgram); proved != row.proof || len(launcher.specs) > 1 {
					t.Fatalf("recovery started %d processes, want the proof's canary: %v", len(launcher.specs), row.proof)
				}
				if exported := exists(t, l.exportRecord); exported != (row.want == RecoveryExported) {
					t.Fatalf("export record present: %v, outcome %s", exported, got.res.Outcome)
				}
				if row.want == RecoveryExported {
					if got.res.Export.Dir != l.export ||
						!slices.ContainsFunc(got.res.Export.Manifest.Entries, func(e export.Entry) bool { return e.Path == "src/change.txt" }) {
						t.Fatalf("export = %+v, want the change in %s", got.res.Export, l.export)
					}
					if exists(t, filepath.Join(l.export, "partial")) {
						t.Fatal("the interrupted export's file survived")
					}
				}
			})
		})
	}
}

// endingHost is a host on which the supervisor records its end and exits
// just as recovery first finds it gone.
type endingHost struct {
	*fakeHost
	t     *testing.T
	l     layout
	ended []byte
}

func (h endingHost) same(p hostProcess) (bool, error) {
	if p.pid == fakeSupervisorPID {
		if err := os.WriteFile(h.l.ended, h.ended, 0o600); err != nil && !errors.Is(err, os.ErrExist) {
			h.t.Error(err)
		}
		return false, nil
	}
	return h.fakeHost.same(p)
}

// TestRecoverTakesAnEndRecordedAsItFindsTheSupervisorGone is the race
// between a supervisor's last write and recovery's check that it is alive:
// the supervisor's record is the run's end, not lost.
func TestRecoverTakesAnEndRecordedAsItFindsTheSupervisorGone(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	const id = "run-race"
	l := layoutFor(cfg.RunsRoot, id)
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		finished(t, cfg, host, id, 0)
		ended, err := os.ReadFile(l.ended)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(l.ended); err != nil {
			t.Fatal(err)
		}
		b := fakeBackend(t, cfg, &fakeLauncher{host: host})
		b.newHost = func() (hostProcesses, error) { return endingHost{fakeHost: host, t: t, l: l, ended: ended}, nil }
		res, err := b.Recover(t.Context(), RecoverRequest{RunID: id, Export: exportRequest(id)})
		if err != nil || res.Outcome != RecoveryExported || res.EndedBy != EndedByExited {
			t.Fatalf("Recover = %+v, %v; want the supervisor's clean end, exported", res, err)
		}
	})
}

// TestASupervisorStartedAfterRecoveryStartsNothing is a supervisor the
// daemon started just before it stopped, scheduled only after recovery
// claimed the run.
func TestASupervisorStartedAfterRecoveryStartsNothing(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	const id = "run-late"
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		var late supervisorSpec
		unsupervised(t, cfg, host, id, func(spec supervisorSpec) { late = spec })
		defer func() { _ = late.stderr.Close() }()
		res, err := fakeBackend(t, cfg, &fakeLauncher{host: host}).Recover(t.Context(), RecoverRequest{RunID: id, Export: exportRequest(id)})
		if err != nil || res.Outcome != RecoveryLost {
			t.Fatalf("Recover = %+v, %v; want lost", res, err)
		}
		launcher := &fakeLauncher{host: host}
		s := superviseEnv{
			l: layoutFor(cfg.RunsRoot, id), runID: id, host: host, start: launcher.start, secret: late.secret,
			self: hostProcess{pid: fakeSupervisorPID, start: fakeSupervisorPID, settled: true},
		}
		if status := s.supervise(t.Context(), late.stderr); status != supervisorDone {
			t.Fatalf("supervise = %d, want %d", status, supervisorDone)
		}
		if len(launcher.specs) != 0 {
			t.Fatalf("the late supervisor started %d processes", len(launcher.specs))
		}
	})
}

// TestRecoveryRefusesAnotherRunsClaim shows a record names only its own
// run: a claim naming another run stops recovery before it acts.
func TestRecoveryRefusesAnotherRunsClaim(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	const id = "run-mine"
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		unsupervised(t, cfg, host, id, closeSpec)
		l := layoutFor(cfg.RunsRoot, id)
		if err := writeRecord(l.claim, supervisorClaim{Version: claimVersion, RunID: "run-other"}); err != nil {
			t.Fatal(err)
		}
		launcher := &fakeLauncher{host: host}
		if _, err := fakeBackend(t, cfg, launcher).Recover(t.Context(), RecoverRequest{RunID: id, Export: exportRequest(id)}); err == nil {
			t.Fatal("Recover accepted another run's claim")
		}
		if exists(t, l.ended) || len(launcher.specs) != 0 {
			t.Fatal("recovery acted on another run's claim")
		}
	})
}

// TestCancellationSignalsOnlyTheClaimedSupervisor shows a pid whose start
// time isn't the claim's is never signaled: the claimed supervisor is gone
// and its pid reused. The marker is still written.
func TestCancellationSignalsOnlyTheClaimedSupervisor(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	const id = "run-reused"
	synctest.Test(t, func(t *testing.T) {
		host := newFakeHost()
		unsupervised(t, cfg, host, id, closeSpec)
		l := layoutFor(cfg.RunsRoot, id)
		stale := &claimedProcess{PID: fakeSupervisorPID, Start: fakeSupervisorPID + 1}
		if won, err := claimRun(l, id, stale); err != nil || !won {
			t.Fatalf("claim: %v, %v", won, err)
		}
		host.setSupervisor(fakeSupervisorPID, func() {})
		defer host.setSupervisor(fakeSupervisorPID, nil)
		if err := fakeBackend(t, cfg, &fakeLauncher{host: host}).RequestCancellation(t.Context(), id); err != nil {
			t.Fatalf("RequestCancellation: %v", err)
		}
		if sent := host.sigterms(); len(sent) != 0 {
			t.Fatalf("signaled %v, want none", sent)
		}
		if !exists(t, l.cancel) {
			t.Fatal("no cancellation marker")
		}
	})
}

func TestReleaseFinishedRemovesOnlyFinishedRuns(t *testing.T) {
	t.Parallel()
	const id = "run-release"
	for _, row := range []struct {
		name    string
		leave   func(t *testing.T, cfg Config, host *fakeHost)
		removed bool
	}{{
		name:    "finished",
		leave:   func(t *testing.T, cfg Config, host *fakeHost) { finished(t, cfg, host, id, 0) },
		removed: true,
	}, {
		name:  "no end record",
		leave: func(t *testing.T, cfg Config, host *fakeHost) { unsupervised(t, cfg, host, id, closeSpec) },
	}, {
		name: "supervisor alive after its end record",
		leave: func(t *testing.T, cfg Config, host *fakeHost) {
			finished(t, cfg, host, id, 0)
			host.setSupervisor(fakeSupervisorPID, func() {})
		},
	}} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			cfg := testConfig(t)
			synctest.Test(t, func(t *testing.T) {
				host := newFakeHost()
				row.leave(t, cfg, host)
				if err := fakeBackend(t, cfg, &fakeLauncher{host: host}).ReleaseFinished(t.Context(), []string{id}); err != nil {
					t.Fatalf("ReleaseFinished: %v", err)
				}
				if gone := !exists(t, layoutFor(cfg.RunsRoot, id).dir); gone != row.removed {
					t.Fatalf("run directory removed: %v, want %v", gone, row.removed)
				}
			})
		})
	}
}

func TestReleaseFinishedRefusesABadRunID(t *testing.T) {
	t.Parallel()
	cfg := testConfig(t)
	outside := filepath.Join(filepath.Dir(cfg.RunsRoot), "outside")
	if err := os.Mkdir(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	b := fakeBackend(t, cfg, &fakeLauncher{host: newFakeHost()})
	if err := b.ReleaseFinished(t.Context(), []string{"../outside"}); err == nil {
		t.Fatal("ReleaseFinished accepted a run id outside the runs root")
	}
	if !exists(t, outside) {
		t.Fatal("ReleaseFinished removed a directory outside the runs root")
	}
}

func TestRecoveryOutcomeRegistration(t *testing.T) {
	t.Parallel()
	for _, o := range AllRecoveryOutcomes {
		if !o.valid() {
			t.Fatalf("%q is registered but invalid", o)
		}
	}
	if RecoveryOutcome("").valid() {
		t.Fatal("the zero RecoveryOutcome is valid")
	}
}
