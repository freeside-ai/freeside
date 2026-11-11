package ward

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func TestTaskCancellationVerificationJoinsAndFencesEveryCommand(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		runtime := newFakeRuntime(t)
		owned, err := NewVerificationOwnership(filepath.Join(t.TempDir(), "owned"), runtime)
		if err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		command := func(ctx context.Context, _ string, _ []string, _ int64) (verify.StepResult, error) {
			close(entered)
			<-ctx.Done()
			return verify.StepResult{}, ctx.Err()
		}
		room := newProjectImageRoom("container", verificationProjectImage(t, []string{"prepare"}), runtime, command, command, 1024, DefaultLaunchSize(LaunchVerification))
		if err := owned.Bind(room, "task", "run", "verify"); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := room.Run(t.Context(), t.TempDir(), []string{"verify"}); done <- err }()
		<-entered
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := owned.StopRun(ctx, "task", "run"); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("command did not cancel: %v", err)
		}
		if _, err := room.ReadRecipe(t.Context()); !errors.Is(err, context.Canceled) {
			t.Fatalf("post-stop launch: %v", err)
		}
	})
}

func TestTaskCancellationVerificationRestartRetainsUncertainty(t *testing.T) {
	t.Parallel()
	for _, joined := range []bool{false, true} {
		t.Run(map[bool]string{false: "interrupted-before-cid", true: "joined-before-cleanup"}[joined], func(t *testing.T) {
			runtime := newFakeRuntime(t)
			root := filepath.Join(t.TempDir(), "owned")
			owned, err := NewVerificationOwnership(root, runtime)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := newOwnershipLabel()
			if err != nil {
				t.Fatal(err)
			}
			_, _, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "verify"}, owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := finish(joined); err != nil {
				t.Fatal(err)
			}
			runtime.ctrs["owned"] = &fakeCtr{spec: ContainerSpec{Labels: []Label{owner}}, stopped: true, created: "owned-created"}
			runtime.ctrs["foreign"] = &fakeCtr{spec: ContainerSpec{Labels: []Label{{Key: "other", Value: "owner"}}}, stopped: true, created: "foreign-created"}
			restarted, err := NewVerificationOwnership(root, runtime)
			if err != nil {
				t.Fatal(err)
			}
			err = restarted.StopRun(t.Context(), "task", "run")
			if (err == nil) != joined {
				t.Fatalf("joined=%v, stop=%v", joined, err)
			}
			if runtime.ctrs["owned"] != nil || runtime.ctrs["foreign"] == nil {
				t.Fatal("cleanup did not preserve exact ownership")
			}
		})
	}
}

func TestTaskCancellationVerificationRejectsLyingDeleteAndDamagedBinding(t *testing.T) {
	t.Parallel()
	for _, omit := range []bool{false, true} {
		t.Run(map[bool]string{false: "listed", true: "omitted"}[omit], func(t *testing.T) {
			runtime := newFakeRuntime(t)
			owned, err := NewVerificationOwnership(filepath.Join(t.TempDir(), "owned"), runtime)
			if err != nil {
				t.Fatal(err)
			}
			owner, err := newOwnershipLabel()
			if err != nil {
				t.Fatal(err)
			}
			_, cid, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "verify"}, owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := finish(true); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cid, []byte("owned"), 0o600); err != nil {
				t.Fatal(err)
			}
			runtime.ctrs["owned"] = &fakeCtr{spec: ContainerSpec{Labels: []Label{owner}}, stopped: true, created: "owned-created"}
			runtime.onDeleteContainer = func(string) (bool, error) { return true, nil }
			if omit {
				runtime.onListContainers = func([]ContainerSummary) ([]ContainerSummary, error) { return nil, nil }
			}
			if err := owned.StopRun(t.Context(), "task", "run"); err == nil {
				t.Fatal("lying delete confirmed quiescence")
			}
			if err := owned.StopRun(t.Context(), "wrong-task", "run"); err == nil {
				t.Fatal("retargeted record authorized cleanup")
			}
		})
	}
}

func TestTaskCancellationCoverageSurvivesRestartAndEpochChange(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "coverage.json")
	coverage, err := OpenCancellationCoverage(path, "epoch", []domain.TaskID{"old"})
	if err != nil {
		t.Fatal(err)
	}
	if coverage.Covers("old", "epoch") == nil || coverage.Covers("new", "restored") == nil {
		t.Fatal("unknown ownership was accepted")
	}
	restored, err := OpenCancellationCoverage(path, "epoch", []domain.TaskID{"old", "new"})
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.Covers("new", "epoch"); err != nil {
		t.Fatal(err)
	}
}

// An earlier room's unproven container blocks the invocation's proof after a
// later room of the same invocation runs clean, until a retried cleanup
// removes it; the proof leaves other invocations alone and fences nothing.
func TestVerificationInvocationAbsenceCoversEveryRoom(t *testing.T) {
	t.Parallel()
	runtime := newFakeRuntime(t)
	owned, err := NewVerificationOwnership(filepath.Join(t.TempDir(), "owned"), runtime)
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	runner := verificationRunner(t, runtime, make([]verify.StepResult, 8), &calls)
	boundRoom := func() *ProjectImageRoom {
		room := newProjectImageRoom("container", verificationProjectImage(t, []string{"prepare"}), runtime,
			runner, runner, verify.DefaultMaxRoomOutputBytes, DefaultLaunchSize(LaunchVerification))
		if err := owned.Bind(room, "task", "run", "verify"); err != nil {
			t.Fatal(err)
		}
		return room
	}
	stuck := true
	runtime.onDeleteContainer = func(id string) (bool, error) {
		if stuck && id == "verification-1" {
			return false, errors.New("runtime refused the delete")
		}
		return false, nil
	}
	if _, err := boundRoom().Run(t.Context(), t.TempDir(), []string{"verify"}); !errors.Is(err, ErrVerificationAbsenceUnproven) {
		t.Fatalf("first room = %v, want unproven", err)
	}
	if _, err := boundRoom().Run(t.Context(), t.TempDir(), []string{"verify"}); err != nil {
		t.Fatalf("second room = %v", err)
	}
	other, err := newOwnershipLabel()
	if err != nil {
		t.Fatal(err)
	}
	_, _, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "other"}, other)
	if err != nil {
		t.Fatal(err)
	}
	if err := finish(false); err != nil {
		t.Fatal(err)
	}
	runtime.ctrs["other"] = &fakeCtr{spec: ContainerSpec{Labels: []Label{other}}, stopped: true, created: "other-created"}

	if err := owned.ProveInvocationAbsent(t.Context(), "task", "run", "verify"); !errors.Is(err, ErrVerificationAbsenceUnproven) {
		t.Fatalf("proof with the first room's container listed = %v", err)
	}
	stuck = false
	if err := owned.ProveInvocationAbsent(t.Context(), "task", "run", "verify"); err != nil {
		t.Fatalf("proof after the retried cleanup = %v", err)
	}
	if runtime.ctrs["verification-1"] != nil || runtime.ctrs["other"] == nil {
		t.Fatal("proof did not reap exactly its invocation's containers")
	}
	if _, err := boundRoom().Run(t.Context(), t.TempDir(), []string{"verify"}); err != nil {
		t.Fatalf("room after the proof = %v, want no fence", err)
	}
}

func TestVerificationInvocationAbsenceFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// arrange leaves the run in the state under test and returns what
		// makes the proof pass afterwards, or nil when nothing should.
		arrange func(t *testing.T, owned *VerificationOwnership, runtime *fakeRuntime, dir string) func()
	}{
		{"live-call", func(t *testing.T, owned *VerificationOwnership, runtime *fakeRuntime, _ string) func() {
			owner, err := newOwnershipLabel()
			if err != nil {
				t.Fatal(err)
			}
			_, _, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "verify"}, owner)
			if err != nil {
				t.Fatal(err)
			}
			runtime.ctrs["live"] = &fakeCtr{spec: ContainerSpec{Labels: []Label{owner}}, stopped: true, created: "live-created"}
			return func() {
				if runtime.ctrs["live"] == nil {
					t.Fatal("proof reaped a live call's container")
				}
				if err := finish(true); err != nil {
					t.Fatal(err)
				}
				delete(runtime.ctrs, "live")
			}
		}},
		{"no-quiescence-proof", func(t *testing.T, owned *VerificationOwnership, _ *fakeRuntime, _ string) func() {
			owner, err := newOwnershipLabel()
			if err != nil {
				t.Fatal(err)
			}
			_, _, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "verify"}, owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := finish(false); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
		{"call-began-during-proof", func(t *testing.T, owned *VerificationOwnership, runtime *fakeRuntime, _ string) func() {
			begin := func() func(bool) error {
				owner, err := newOwnershipLabel()
				if err != nil {
					t.Fatal(err)
				}
				_, _, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "verify"}, owner)
				if err != nil {
					t.Fatal(err)
				}
				return finish
			}
			if err := begin()(true); err != nil {
				t.Fatal(err)
			}
			// The proof lists containers for the snapshot's record; a call that
			// begins and ends then is one the snapshot missed.
			runtime.onListContainers = func(list []ContainerSummary) ([]ContainerSummary, error) {
				runtime.onListContainers = nil
				if err := begin()(true); err != nil {
					t.Error(err)
				}
				return list, nil
			}
			return func() {}
		}},
		{"unreadable-record", func(t *testing.T, owned *VerificationOwnership, _ *fakeRuntime, dir string) func() {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "damaged.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime := newFakeRuntime(t)
			root := filepath.Join(t.TempDir(), "owned")
			owned, err := NewVerificationOwnership(root, runtime)
			if err != nil {
				t.Fatal(err)
			}
			settle := tc.arrange(t, owned, runtime, filepath.Join(root, verificationRunKey("run")))
			if err := owned.ProveInvocationAbsent(t.Context(), "task", "run", "verify"); !errors.Is(err, ErrVerificationAbsenceUnproven) {
				t.Fatalf("proof = %v, want unproven", err)
			}
			if settle == nil {
				return
			}
			settle()
			if err := owned.ProveInvocationAbsent(t.Context(), "task", "run", "verify"); err != nil {
				t.Fatalf("proof once settled = %v", err)
			}
		})
	}
}

// TestVerificationInvocationContainersAbsence: the operator-confirmed proof
// passes an unjoined record once its containers are gone, where the strict
// proof refuses it as unjoined, and fails closed on everything else the
// strict proof does.
func TestVerificationInvocationContainersAbsence(t *testing.T) {
	t.Parallel()
	unjoined := func(t *testing.T, owned *VerificationOwnership) Label {
		t.Helper()
		owner, err := newOwnershipLabel()
		if err != nil {
			t.Fatal(err)
		}
		_, _, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "verify"}, owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := finish(false); err != nil {
			t.Fatal(err)
		}
		return owner
	}
	for _, tc := range []struct {
		name    string
		arrange func(t *testing.T, owned *VerificationOwnership, runtime *fakeRuntime, dir string)
		proved  bool
	}{
		{"unjoined-containers-gone", func(t *testing.T, owned *VerificationOwnership, _ *fakeRuntime, _ string) {
			unjoined(t, owned)
		}, true},
		{"container-present", func(t *testing.T, owned *VerificationOwnership, runtime *fakeRuntime, _ string) {
			owner := unjoined(t, owned)
			runtime.ctrs["owned"] = &fakeCtr{spec: ContainerSpec{Labels: []Label{owner}}, stopped: true, created: "owned-created"}
			runtime.onDeleteContainer = func(string) (bool, error) {
				return false, errors.New("runtime refused the delete")
			}
		}, false},
		{"live-call", func(t *testing.T, owned *VerificationOwnership, _ *fakeRuntime, _ string) {
			owner, err := newOwnershipLabel()
			if err != nil {
				t.Fatal(err)
			}
			_, _, finish, err := owned.begin(t.Context(), verificationOwner{Version: 1, TaskID: "task", RunID: "run", InvocationID: "verify"}, owner)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = finish(true) })
		}, false},
		{"unreadable-record", func(t *testing.T, _ *VerificationOwnership, _ *fakeRuntime, dir string) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "damaged.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtime := newFakeRuntime(t)
			root := filepath.Join(t.TempDir(), "owned")
			owned, err := NewVerificationOwnership(root, runtime)
			if err != nil {
				t.Fatal(err)
			}
			tc.arrange(t, owned, runtime, filepath.Join(root, verificationRunKey("run")))
			err = owned.ProveInvocationContainersAbsent(t.Context(), "task", "run", "verify")
			if tc.proved != (err == nil) || (err != nil && !errors.Is(err, ErrVerificationAbsenceUnproven)) {
				t.Fatalf("container proof = %v, want proved %v", err, tc.proved)
			}
			if errors.Is(err, ErrVerificationCommandUnjoined) {
				t.Fatalf("container proof refused for an unjoined command: %v", err)
			}
			if tc.name == "unjoined-containers-gone" {
				strict := owned.ProveInvocationAbsent(t.Context(), "task", "run", "verify")
				if !errors.Is(strict, ErrVerificationAbsenceUnproven) || !errors.Is(strict, ErrVerificationCommandUnjoined) {
					t.Fatalf("strict proof = %v, want unproven as unjoined", strict)
				}
			}
		})
	}
	if err := (&VerificationOwnership{}).ProveInvocationContainersAbsent(t.Context(), "task", "run", ""); !errors.Is(err, domain.ErrEmptyID) {
		t.Fatalf("proof without an invocation = %v", err)
	}
}

func TestVerificationInvocationAbsenceWithoutRecords(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "owned")
	owned, err := NewVerificationOwnership(root, newFakeRuntime(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.ProveInvocationAbsent(t.Context(), "task", "run", "verify"); err != nil {
		t.Fatalf("proof for a run with no records = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, verificationRunKey("run"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("proof created the run directory: %v", err)
	}
	if err := owned.ProveInvocationAbsent(t.Context(), "task", "run", ""); !errors.Is(err, domain.ErrEmptyID) {
		t.Fatalf("proof without an invocation = %v", err)
	}
}
