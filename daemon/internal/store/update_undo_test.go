package store_test

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const newerDrainBuild domain.Digest = "sha256:3333333333333333333333333333333333333333333333333333333333333333"

// undoFixture is the state an installer leaves when an updated daemon fails:
// a checkpoint taken while the old daemon was drained, and a live database
// the new build changed afterwards.
type undoFixture struct {
	db, checkpoint, sha256 string
	// localBackup is the encrypted local backup checkpoint the failed
	// build's maintenance pass wrote; its bytes are opaque to the undo.
	localBackup     string
	checkpointEpoch string
	drain           domain.UpdateDrainTransition
}

// newerBuildWrites is what the failed build did to the live database after
// the checkpoint, as SQL run on it.
type newerBuildWrites []string

// migrated is a newer build's migration: a table this binary doesn't know
// and a schema_migrations row past this binary's head.
func migrated(t *testing.T) newerBuildWrites {
	t.Helper()
	head, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	return newerBuildWrites{
		`CREATE TABLE newer_build_table (id INTEGER PRIMARY KEY)`,
		`INSERT INTO schema_migrations (version, name, digest, applied_at) VALUES (` +
			strconv.Itoa(head+1) + `, '9999_newer.sql', 'sha256:00', '2026-11-13T00:00:00Z')`,
	}
}

func drainRowSQL(state domain.UpdateDrainState, build domain.Digest, at time.Time) string {
	return `INSERT INTO update_drain_transitions (state, build, forced, occurred_at) VALUES ('` +
		string(state) + `', '` + string(build) + `', 0, '` + at.Format(time.RFC3339Nano) + `')`
}

// newUndoFixture checkpoints a drained store, then applies writes to the
// live file with a WAL connection that is never checkpointed, and copies the
// file and its -wal while that connection is open: the live database is left
// as a crashed daemon leaves it, with the newer build's writes only in the
// -wal.
func newUndoFixture(t *testing.T, writes newerBuildWrites) undoFixture {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	base := filepath.Join(dir, "base.db")
	storetest.Copy(t, base)
	s, err := store.Open(ctx, base, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	drain := domain.UpdateDrainTransition{State: domain.UpdateDrainDraining, Build: drainBuild, OccurredAt: admissionEpoch}
	recordDrain(t, s, drain)
	state, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "update-checkpoint")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(stateDir, "freeside.db")
	if err := store.CopyDatabaseFile(ctx, base, checkpoint); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", base)
	if err != nil {
		t.Fatal(err)
	}
	raw.SetMaxOpenConns(1)
	for _, statement := range append([]string{`PRAGMA journal_mode=WAL`, `PRAGMA wal_autocheckpoint=0`}, writes...) {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	live := filepath.Join(dir, "freeside.db")
	copyFile(t, base, live)
	if _, err := os.Stat(base + "-wal"); err == nil {
		copyFile(t, base+"-wal", live+"-wal")
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	localBackupDir := live + ".checkpoints"
	if err := os.Mkdir(localBackupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	localBackup := filepath.Join(localBackupDir, "latest.backup")
	if err := os.WriteFile(localBackup, []byte("the failed build's checkpoint"), 0o600); err != nil {
		t.Fatal(err)
	}
	return undoFixture{
		db: live, checkpoint: checkpoint, sha256: fileSHA256(t, checkpoint),
		localBackup: localBackup, checkpointEpoch: state.SyncEpoch, drain: drain,
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	data, err := os.ReadFile(from) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, data, 0o600); err != nil { //nolint:gosec // G703: a path in the test's own temporary directory
		t.Fatal(err)
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// fileDigests records the bytes of the database files an undo may touch, so
// a refusal can be shown to change nothing. The -shm file is left out: a
// read-only reader of a WAL database creates it.
func (f undoFixture) fileDigests(t *testing.T) map[string]string {
	t.Helper()
	digests := map[string]string{}
	for _, path := range []string{
		f.db, f.db + "-wal", f.checkpoint, f.localBackup,
		f.db + ".update-failed", f.db + ".update-failed-wal", f.db + ".update-restore",
	} {
		if _, err := os.Stat(path); err == nil {
			digests[path] = fileSHA256(t, path)
		}
	}
	return digests
}

func requireUnchanged(t *testing.T, f undoFixture, before map[string]string) {
	t.Helper()
	after := f.fileDigests(t)
	if len(after) != len(before) {
		t.Fatalf("files after a refusal = %v, want %v", after, before)
	}
	for path, digest := range before {
		if after[path] != digest {
			t.Fatalf("%s changed on a refusal", path)
		}
	}
}

func requireAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: err = %v, want it absent", path, err)
		}
	}
}

// requireRestored checks the live database is the checkpoint again: this
// binary opens it at its own schema, the newer build's table is gone, the
// drain the checkpoint held is still in force, the epoch is new, and every
// file the undo set aside, staged, or restored from is gone, along with the
// failed build's local backup checkpoint.
func requireRestored(t *testing.T, f undoFixture, result store.UpdateUndoResult) {
	t.Helper()
	ctx := t.Context()
	head, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != head {
		t.Fatalf("restored schema version = %d, want %d", result.SchemaVersion, head)
	}
	requireAbsent(t, f.checkpoint, f.db+"-wal", f.db+"-journal",
		f.db+".update-failed", f.db+".update-failed-wal", f.db+".update-failed-shm",
		f.db+".update-failed-journal", f.db+".update-restore", f.localBackup)
	info, err := os.Stat(f.db)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("restored database mode = %v, want 0600", mode)
	}
	s, err := store.OpenExisting(ctx, f.db, store.Options{})
	if err != nil {
		t.Fatalf("OpenExisting on the restored database: %v", err)
	}
	defer func() { _ = s.Close() }()
	state, err := s.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.SyncEpoch == f.checkpointEpoch || state.SyncEpoch == "" {
		t.Fatalf("restored sync_epoch = %q, want a new one (checkpoint's was %q)", state.SyncEpoch, f.checkpointEpoch)
	}
	if drain := drainInForce(t, s); drain == nil || *drain != f.drain {
		t.Fatalf("restored drain = %+v, want the checkpoint's %+v", drain, f.drain)
	}
}

// TestUndoUpdateRestoresMigratedDatabase: a build that migrated and crashed
// before lifting the drain is undone, though its migration lives only in a
// -wal the restore must not replay.
func TestUndoUpdateRestoresMigratedDatabase(t *testing.T) {
	t.Parallel()
	f := newUndoFixture(t, migrated(t))
	if _, err := os.Stat(f.db + "-wal"); err != nil {
		t.Fatalf("fixture has no -wal: %v", err)
	}
	if _, err := store.OpenExisting(t.Context(), f.db, store.Options{}); err == nil {
		t.Fatal("the migrated live database opened at this binary's schema; the fixture did not migrate it")
	}
	result, err := store.UndoUpdate(t.Context(), f.db, f.checkpoint, f.sha256)
	if err != nil {
		t.Fatalf("UndoUpdate: %v", err)
	}
	if result.Resumed {
		t.Fatal("a fresh undo reported Resumed")
	}
	requireRestored(t, f, result)

	if _, err := store.UndoUpdate(t.Context(), f.db, f.checkpoint, f.sha256); !errors.Is(err, store.ErrNoPendingUpdateUndo) {
		t.Fatalf("second UndoUpdate: err = %v, want ErrNoPendingUpdateUndo", err)
	}
}

// TestUndoUpdateRefuses: each case either shows the drain was lifted or
// leaves a lift unruled out, and refuses with every file as it was.
func TestUndoUpdateRefuses(t *testing.T) {
	t.Parallel()
	later := admissionEpoch.Add(time.Minute)
	cases := []struct {
		name    string
		writes  func(*testing.T) newerBuildWrites
		sha256  func(undoFixture) string
		lifted  bool
		message string
	}{
		{
			name: "drain lifted",
			writes: func(t *testing.T) newerBuildWrites {
				return append(migrated(t), drainRowSQL(domain.UpdateDrainLifted, newerDrainBuild, later))
			},
			lifted:  true,
			message: "lifted",
		},
		{
			name: "later draining row",
			writes: func(*testing.T) newerBuildWrites {
				return newerBuildWrites{
					`INSERT INTO update_drain_transitions (state, build, forced, occurred_at) VALUES ('draining', '` +
						string(drainBuild) + `', 1, '` + later.Format(time.RFC3339Nano) + `')`,
				}
			},
			message: "cannot be ruled out",
		},
		{
			name: "live log unreadable",
			writes: func(*testing.T) newerBuildWrites {
				return newerBuildWrites{`DROP TABLE update_drain_transitions`}
			},
			message: "update_drain_transitions",
		},
		{
			name: "live log lost the checkpoint's row",
			writes: func(*testing.T) newerBuildWrites {
				return newerBuildWrites{`DELETE FROM update_drain_transitions`}
			},
			message: "does not descend",
		},
		{
			name:    "checkpoint sha256 mismatch",
			writes:  migrated,
			sha256:  func(undoFixture) string { return strings.Repeat("0", 64) },
			message: "not the recorded",
		},
		{
			name:    "malformed sha256",
			writes:  migrated,
			sha256:  func(undoFixture) string { return "abc" },
			message: "64 hex digits",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUndoFixture(t, tc.writes(t))
			want := f.sha256
			if tc.sha256 != nil {
				want = tc.sha256(f)
			}
			before := f.fileDigests(t)
			_, err := store.UndoUpdate(t.Context(), f.db, f.checkpoint, want)
			if err == nil {
				t.Fatal("UndoUpdate succeeded")
			}
			if errors.Is(err, store.ErrUpdateDrainLifted) != tc.lifted {
				t.Fatalf("err = %v, want ErrUpdateDrainLifted %v", err, tc.lifted)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.message)
			}
			requireUnchanged(t, f, before)
		})
	}
}

// TestCheckUpdateUndo pins the lift rule on the logs alone.
func TestCheckUpdateUndo(t *testing.T) {
	t.Parallel()
	entry := func(id int64, state domain.UpdateDrainState, minute int) store.UpdateDrainLogEntry {
		return store.UpdateDrainLogEntry{ID: id, Transition: domain.UpdateDrainTransition{
			State: state, Build: drainBuild, OccurredAt: admissionEpoch.Add(time.Duration(minute) * time.Minute),
		}}
	}
	draining, lifted := domain.UpdateDrainDraining, domain.UpdateDrainLifted
	history := []store.UpdateDrainLogEntry{entry(1, draining, 0), entry(2, lifted, 1), entry(3, draining, 2)}
	cases := []struct {
		name             string
		checkpoint, live []store.UpdateDrainLogEntry
		allowed, isLift  bool
	}{
		{name: "same log", checkpoint: history, live: history, allowed: true},
		{name: "lifted after", checkpoint: history, live: append(history[:3:3], entry(4, lifted, 3)), isLift: true},
		{name: "drained again after", checkpoint: history, live: append(history[:3:3], entry(4, draining, 3))},
		{name: "empty checkpoint log", checkpoint: nil, live: history},
		{name: "checkpoint not drained", checkpoint: history[:2], live: history[:2]},
		{name: "live lacks the row", checkpoint: history, live: history[:2]},
		{name: "live row differs", checkpoint: history, live: append(history[:2:2], entry(3, draining, 9))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := store.CheckUpdateUndo(tc.checkpoint, tc.live)
			if (err == nil) != tc.allowed {
				t.Fatalf("CheckUpdateUndo: err = %v, want allowed %v", err, tc.allowed)
			}
			if errors.Is(err, store.ErrUpdateDrainLifted) != tc.isLift {
				t.Fatalf("CheckUpdateUndo: err = %v, want ErrUpdateDrainLifted %v", err, tc.isLift)
			}
		})
	}
}

// TestUndoUpdateResumes: a run killed at each point of the swap is finished
// by the next run, which never repeats the drain check against a database it
// has already set aside.
func TestUndoUpdateResumes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		interrupt func(*testing.T, undoFixture)
	}{
		{
			name: "after setting the database aside",
			interrupt: func(t *testing.T, f undoFixture) {
				renameFile(t, f.db, f.db+".update-failed")
			},
		},
		{
			name: "after setting the -wal aside",
			interrupt: func(t *testing.T, f undoFixture) {
				renameFile(t, f.db, f.db+".update-failed")
				renameFile(t, f.db+"-wal", f.db+".update-failed-wal")
			},
		},
		{
			name: "after the swap, before finalizing",
			interrupt: func(t *testing.T, f undoFixture) {
				renameFile(t, f.db, f.db+".update-failed")
				renameFile(t, f.db+"-wal", f.db+".update-failed-wal")
				if err := store.CopyDatabaseFile(t.Context(), f.checkpoint, f.db); err != nil {
					t.Fatal(err)
				}
				execSQL(t, f.db, `UPDATE server_state SET sync_epoch = 'restored' WHERE id = 1`)
			},
		},
		{
			name: "after removing the set-aside database",
			interrupt: func(t *testing.T, f undoFixture) {
				if _, err := store.UndoUpdate(t.Context(), f.db, f.checkpoint, f.sha256); err != nil {
					t.Fatal(err)
				}
				// Put the checkpoint back as though the run had stopped
				// before deleting it; the restored database stays.
				if err := store.CopyDatabaseFile(t.Context(), f.db, f.checkpoint); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUndoFixture(t, migrated(t))
			tc.interrupt(t, f)
			// The last case recreated the checkpoint from the restored file.
			f.sha256 = fileSHA256(t, f.checkpoint)
			result, err := store.UndoUpdate(t.Context(), f.db, f.checkpoint, f.sha256)
			if err != nil {
				t.Fatalf("resumed UndoUpdate: %v", err)
			}
			requireRestored(t, f, result)
		})
	}
}

// TestUndoUpdateRefusesForeignSetAside: a set-aside database that is not
// this undo's (no checkpoint, or a live database never restored from it) is
// never deleted or swapped.
func TestUndoUpdateRefusesForeignSetAside(t *testing.T) {
	t.Parallel()
	t.Run("live database not restored", func(t *testing.T) {
		t.Parallel()
		f := newUndoFixture(t, migrated(t))
		copyFile(t, f.checkpoint, f.db+".update-failed")
		before := f.fileDigests(t)
		if _, err := store.UndoUpdate(t.Context(), f.db, f.checkpoint, f.sha256); err == nil ||
			!strings.Contains(err.Error(), "not this undo's") {
			t.Fatalf("UndoUpdate: err = %v, want a refusal of the foreign set-aside file", err)
		}
		requireUnchanged(t, f, before)
	})
	// An undo killed between its two renames releases the daemon lock with
	// nothing at the live path; a daemon starting then creates an empty
	// database there. A fresh sync_epoch alone must not read as a finished
	// swap, or finalizing would delete the set-aside real data.
	t.Run("live database a daemon created mid-swap", func(t *testing.T) {
		t.Parallel()
		f := newUndoFixture(t, migrated(t))
		renameFile(t, f.db, f.db+".update-failed")
		renameFile(t, f.db+"-wal", f.db+".update-failed-wal")
		created, err := store.Open(t.Context(), f.db, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if err := created.Close(); err != nil {
			t.Fatal(err)
		}
		before := f.fileDigests(t)
		if _, err := store.UndoUpdate(t.Context(), f.db, f.checkpoint, f.sha256); err == nil ||
			!strings.Contains(err.Error(), "not this undo's") {
			t.Fatalf("UndoUpdate: err = %v, want a refusal that keeps the set-aside database", err)
		}
		// Reading a WAL database that was closed cleanly recreates an empty
		// -wal, which holds nothing.
		if info, err := os.Stat(f.db + "-wal"); err == nil && info.Size() == 0 {
			if err := os.Remove(f.db + "-wal"); err != nil {
				t.Fatal(err)
			}
		}
		requireUnchanged(t, f, before)
	})
	// A restored copy some daemon reopened after the swap carries a later
	// drain row, so it is no longer the copy the undo staged.
	t.Run("restored database reopened", func(t *testing.T) {
		t.Parallel()
		f := newUndoFixture(t, migrated(t))
		renameFile(t, f.db, f.db+".update-failed")
		renameFile(t, f.db+"-wal", f.db+".update-failed-wal")
		copyFile(t, f.checkpoint, f.db)
		execSQL(t, f.db, `UPDATE server_state SET sync_epoch = 'reopened' WHERE id = 1`)
		execSQL(t, f.db, drainRowSQL(domain.UpdateDrainLifted, f.drain.Build, admissionEpoch.Add(time.Hour)))
		before := f.fileDigests(t)
		if _, err := store.UndoUpdate(t.Context(), f.db, f.checkpoint, f.sha256); err == nil ||
			!strings.Contains(err.Error(), "not this undo's") {
			t.Fatalf("UndoUpdate: err = %v, want a refusal that keeps the set-aside database", err)
		}
		requireUnchanged(t, f, before)
	})
	t.Run("no checkpoint", func(t *testing.T) {
		t.Parallel()
		f := newUndoFixture(t, migrated(t))
		copyFile(t, f.checkpoint, f.db+".update-failed")
		if err := os.Remove(f.checkpoint); err != nil {
			t.Fatal(err)
		}
		before := f.fileDigests(t)
		if _, err := store.UndoUpdate(t.Context(), f.db, f.checkpoint, f.sha256); err == nil ||
			errors.Is(err, store.ErrNoPendingUpdateUndo) {
			t.Fatalf("UndoUpdate: err = %v, want a refusal that is not ErrNoPendingUpdateUndo", err)
		}
		requireUnchanged(t, f, before)
	})
}

func renameFile(t *testing.T, from, to string) {
	t.Helper()
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
}

func execSQL(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
}
