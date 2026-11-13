package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const undoDrainBuild domain.Digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

// undoUpdateFixture is a drained database, its checkpoint, and the
// checkpoint's sha256. The command's own resume and lift cases are covered
// against the files in internal/store; this pins the command's flags, exit
// codes, and lock.
type undoUpdateFixture struct {
	db, checkpoint, sha256 string
}

func newUndoUpdateFixture(t *testing.T, liveWrites ...string) undoUpdateFixture {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	db := filepath.Join(dir, "freeside.db")
	st, err := store.Open(ctx, db, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordUpdateDrainTransition(ctx, domain.UpdateDrainTransition{
			State: domain.UpdateDrainDraining, Build: undoDrainBuild,
			OccurredAt: time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	checkpoint := filepath.Join(dir, "checkpoint.db")
	if err := store.CopyDatabaseFile(ctx, db, checkpoint); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", db)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range liveWrites {
		if _, err := raw.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(checkpoint) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return undoUpdateFixture{db: db, checkpoint: checkpoint, sha256: hex.EncodeToString(sum[:])}
}

func (f undoUpdateFixture) args() []string {
	return []string{"-db", f.db, "-checkpoint", f.checkpoint, "-sha256", f.sha256}
}

func TestUndoUpdateCommandRestores(t *testing.T) {
	t.Parallel()
	f := newUndoUpdateFixture(t, `CREATE TABLE newer_build_table (id INTEGER PRIMARY KEY)`)
	var out bytes.Buffer
	if err := runUndoUpdateCommand(t.Context(), f.args(), &out, io.Discard); err != nil {
		t.Fatalf("undo-update: %v", err)
	}
	if !strings.Contains(out.String(), "restored "+f.db+" from the checkpoint taken ") {
		t.Fatalf("output = %q", out.String())
	}
	if _, err := os.Lstat(f.checkpoint); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("checkpoint after the undo: %v, want it deleted", err)
	}
	err := runUndoUpdateCommand(t.Context(), f.args(), io.Discard, io.Discard)
	if !errors.Is(err, store.ErrNoPendingUpdateUndo) || undoUpdateExitCode(err) != undoUpdateExitRefused {
		t.Fatalf("second undo-update: err = %v, want no pending undo with exit 1", err)
	}
}

func TestUndoUpdateCommandExitCodes(t *testing.T) {
	t.Parallel()
	lifted := `INSERT INTO update_drain_transitions (state, build, forced, occurred_at) VALUES ('lifted', '` +
		string(undoDrainBuild) + `', 0, '2026-11-13T09:01:00Z')`
	cases := []struct {
		name  string
		args  func(undoUpdateFixture) []string
		live  []string
		code  int
		lock  bool
		match string
	}{
		{name: "lifted", live: []string{lifted}, code: undoUpdateExitLifted, match: "nothing was restored"},
		{name: "daemon running", lock: true, code: undoUpdateExitRefused, match: "stop the daemon first"},
		{
			name: "sha256 mismatch",
			args: func(f undoUpdateFixture) []string {
				return []string{"-db", f.db, "-checkpoint", f.checkpoint, "-sha256", strings.Repeat("0", 64)}
			},
			code: undoUpdateExitRefused, match: "not the recorded",
		},
		{
			name:  "missing flag",
			args:  func(f undoUpdateFixture) []string { return []string{"-db", f.db, "-checkpoint", f.checkpoint} },
			code:  undoUpdateExitUsage,
			match: "required",
		},
		{
			name:  "positional argument",
			args:  func(f undoUpdateFixture) []string { return append(f.args(), "extra") },
			code:  undoUpdateExitUsage,
			match: "nothing else",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newUndoUpdateFixture(t, tc.live...)
			if tc.lock {
				lock, err := daemonlock.Acquire(f.db)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = lock.Close() })
			}
			args := f.args()
			if tc.args != nil {
				args = tc.args(f)
			}
			before, err := os.ReadFile(f.db)
			if err != nil {
				t.Fatal(err)
			}
			err = runUndoUpdateCommand(t.Context(), args, io.Discard, io.Discard)
			if err == nil {
				t.Fatal("undo-update succeeded")
			}
			if got := undoUpdateExitCode(err); got != tc.code {
				t.Fatalf("exit code = %d, want %d (err %v)", got, tc.code, err)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.match)
			}
			after, err := os.ReadFile(f.db)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("the live database changed on a refusal")
			}
			if _, err := os.Stat(f.checkpoint); err != nil {
				t.Fatalf("checkpoint after a refusal: %v, want it kept", err)
			}
		})
	}
}
