package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestUnprovableTaskStopFollowsTheCoverageRecord(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "freeside.db")
	storetest.Copy(t, path)
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	coverage, err := ward.OpenCancellationCoverage(
		filepath.Join(t.TempDir(), "task-runtime-coverage.json"), state.SyncEpoch, []domain.TaskID{"task-before-record"})
	if err != nil {
		t.Fatal(err)
	}
	if reason, err := unprovableTaskStop(ctx, st, coverage, "task-after-record"); err != nil || reason != "" {
		t.Fatalf("covered task reported unprovable: %q, %v", reason, err)
	}
	if reason, err := unprovableTaskStop(ctx, st, coverage, "task-before-record"); err != nil || !strings.HasPrefix(reason, "runtime coverage: ") {
		t.Fatalf("task that predates the record: %q, %v", reason, err)
	}
	if _, err := st.NewEpoch(ctx); err != nil {
		t.Fatal(err)
	}
	if reason, err := unprovableTaskStop(ctx, st, coverage, "task-after-record"); err != nil || !strings.HasPrefix(reason, "runtime coverage: ") {
		t.Fatalf("epoch that differs from the record's: %q, %v", reason, err)
	}
	// A reason ends retries for good, so a failed read must never produce one.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if reason, err := unprovableTaskStop(ctx, st, coverage, "task-before-record"); err == nil || reason != "" {
		t.Fatalf("failed epoch read reported a reason: %q, %v", reason, err)
	}
}

// saveCoverageTestTask saves a task, and a run for it when run is set, as
// a submission would; the run has no stage attempt.
func saveCoverageTestTask(t *testing.T, dbPath string, issue int, run domain.RunID) domain.TaskID {
	t.Helper()
	ctx := t.Context()
	var id domain.TaskID
	if err := withDeputyTestStore(ctx, dbPath, func(st *store.Store) error {
		return st.Write(ctx, func(tx *store.WriteTx) error {
			task, err := tx.GetOrCreateTask(ctx, "project", domain.SpecificationSource{
				Kind:         domain.SpecificationSourceIssueSubject,
				IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: issue},
			})
			if err != nil {
				return err
			}
			id = task.ID
			if run == "" {
				return nil
			}
			return tx.PutRun(ctx, domain.Run{
				ID: run, ProjectID: "project", TaskID: task.ID,
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
			})
		})
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestDeputyTaskCoverageCoversTasksSavedAfterIt pins #1920: the record a
// deputy start writes before its composition manifest covers a task saved
// afterwards, even one whose run has no stage attempt, while a task saved
// before it stays uncovered. A later open, as run() makes under the
// deputy's state directory, reads the same record without rewriting it.
//
//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
func TestDeputyTaskCoverageCoversTasksSavedAfterIt(t *testing.T) {
	ctx := t.Context()
	for _, savedBefore := range []bool{false, true} {
		_, paths, settings := deputyLeaseFixture(t)
		var before domain.TaskID
		if savedBefore {
			before = saveCoverageTestTask(t, paths.DB, 1, "")
		}
		if err := openDeputyTaskCoverage(ctx, paths, settings); err != nil {
			t.Fatal(err)
		}
		recordPath := filepath.Join(paths.StateRoot, taskRuntimeCoverageFile)
		written, err := os.ReadFile(recordPath) //nolint:gosec // G304: test-owned path
		if err != nil {
			t.Fatal(err)
		}
		var record ward.CancellationCoverage
		if err := json.Unmarshal(written, &record); err != nil {
			t.Fatal(err)
		}
		var wantUncovered []domain.TaskID
		if savedBefore {
			wantUncovered = []domain.TaskID{before}
		}
		if !slices.Equal(record.UncoveredTasks, wantUncovered) {
			t.Fatalf("savedBefore=%v: uncovered tasks %v, want %v", savedBefore, record.UncoveredTasks, wantUncovered)
		}
		// The lock is released, so the self-check's preflight can take it.
		lock, err := daemonlock.Acquire(paths.DB)
		if err != nil {
			t.Fatalf("savedBefore=%v: lock still held: %v", savedBefore, err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}

		after := saveCoverageTestTask(t, paths.DB, 2, "run-after-record")
		if err := openDeputyTaskCoverage(ctx, paths, settings); err != nil {
			t.Fatalf("savedBefore=%v: second start: %v", savedBefore, err)
		}
		cfg := deputyDaemonConfig(settings, paths, deputyLaunch{}, nil)
		if err := withDeputyTestStore(ctx, paths.DB, func(st *store.Store) error {
			coverage, err := openTaskRuntimeCoverage(ctx, st, cfg.StateDir)
			if err != nil {
				return err
			}
			if reason, err := unprovableTaskStop(ctx, st, coverage, after); err != nil || reason != "" {
				t.Errorf("savedBefore=%v: task saved after the record: %q, %v", savedBefore, reason, err)
			}
			if savedBefore {
				if reason, err := unprovableTaskStop(ctx, st, coverage, before); err != nil ||
					!strings.HasPrefix(reason, "runtime coverage: ") {
					t.Errorf("task saved before the record: %q, %v", reason, err)
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		reread, err := os.ReadFile(recordPath) //nolint:gosec // G304: test-owned path
		if err != nil || !bytes.Equal(reread, written) {
			t.Fatalf("savedBefore=%v: later opens rewrote the record: %s, %v", savedBefore, reread, err)
		}
	}
}

// TestDeputyTaskCoverageRefusesAHeldLock: a daemon holding the database
// fails the deputy start before it writes a record.
//
//nolint:paralleltest // Serial: deputyLeaseFixture sets HOME with t.Setenv.
func TestDeputyTaskCoverageRefusesAHeldLock(t *testing.T) {
	_, paths, settings := deputyLeaseFixture(t)
	lock, err := daemonlock.Acquire(paths.DB)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := openDeputyTaskCoverage(t.Context(), paths, settings); !errors.Is(err, daemonlock.ErrAlreadyRunning) {
		t.Fatalf("held lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.StateRoot, taskRuntimeCoverageFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("held lock wrote a record: %v", err)
	}
}

// TestRunDeputyOpensCoverageBeforeTheSelfCheck pins the start order that
// TestDeputyTaskCoverageCoversTasksSavedAfterIt relies on. runDeputy can't
// run in a test past the prod App authority check, so this reads its
// source: the record opens after the stale manifest is removed and before
// the self-check writes this start's manifest.
func TestRunDeputyOpensCoverageBeforeTheSelfCheck(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "deputy.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "runDeputy" {
			continue
		}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok {
				if ident, ok := call.Fun.(*ast.Ident); ok {
					switch ident.Name {
					case "removeIfPresent", "openDeputyTaskCoverage", "deputySelfCheck":
						calls = append(calls, ident.Name)
					}
				}
			}
			return true
		})
	}
	if want := []string{"removeIfPresent", "openDeputyTaskCoverage", "deputySelfCheck"}; !slices.Equal(calls, want) {
		t.Fatalf("runDeputy calls %v, want %v in that order", calls, want)
	}
}
