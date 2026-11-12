package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// checkDatabaseFixture is an ephemeral-tier live database and state
// directory under a temporary root.
type checkDatabaseFixture struct {
	dbPath, stateDir string
}

func newCheckDatabaseFixture(t *testing.T) checkDatabaseFixture {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "daemon")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return checkDatabaseFixture{dbPath: filepath.Join(stateDir, "freeside.db"), stateDir: stateDir}
}

func (f checkDatabaseFixture) args() []string {
	return []string{"-environment", "ephemeral", "-db", f.dbPath, "-state-dir", f.stateDir}
}

func (f checkDatabaseFixture) run(t *testing.T, ctx context.Context, seams checkDatabaseSeams) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := runCheckDatabaseCommand(ctx, f.args(), &out, io.Discard, seams)
	return out.String(), err
}

// requireNoCopy checks the copy's directory is gone.
func (f checkDatabaseFixture) requireNoCopy(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(f.stateDir, checkDatabaseDirName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("copy directory after the check: %v, want it removed", err)
	}
}

// writeLiveDatabaseAt writes a live database an older build left at version.
func writeLiveDatabaseAt(t *testing.T, path string, version int) {
	t.Helper()
	body, err := store.TemplateDatabaseAt(t.Context(), version)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestCheckDatabaseStoppedOlderSchema covers the case freesided snapshot
// cannot: no daemon runs, and this build adds a migration the live database
// lacks. The check copies without reading the schema, migrates the copy,
// and leaves the live file byte-for-byte unchanged.
func TestCheckDatabaseStoppedOlderSchema(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	head, err := store.CurrentSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	writeLiveDatabaseAt(t, f.dbPath, head-1)
	before, err := os.ReadFile(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.run(t, t.Context(), checkDatabaseSeams{})
	if err != nil {
		t.Fatalf("check-database: %v", err)
	}
	want := fmt.Sprintf("database check passed: schema %d to %d, ", head-1, head)
	if !strings.HasPrefix(out, want) || !strings.Contains(out, "tables not walked") {
		t.Fatalf("output = %q, want a pass from %d to %d", out, head-1, head)
	}
	after, err := os.ReadFile(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("the check changed the live database")
	}
	f.requireNoCopy(t)
	if _, err := os.Lstat(f.dbPath + ".daemon.lock"); err != nil {
		t.Fatalf("daemon lock file: %v", err)
	}
}

// TestCheckDatabaseThroughRunningDaemon: with a daemon running, the daemon
// writes the copy over the control socket, and the check still passes and
// cleans up.
func TestCheckDatabaseThroughRunningDaemon(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	seedTemplateStore(t, f.dbPath)
	h, err := run(t.Context(), nil, config{Environment: environmentEphemeral, DBPath: f.dbPath, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	out, err := f.run(t, t.Context(), checkDatabaseSeams{})
	if err != nil {
		t.Fatalf("check-database through the daemon: %v", err)
	}
	if !strings.HasPrefix(out, "database check passed: ") {
		t.Fatalf("output = %q, want a pass", out)
	}
	f.requireNoCopy(t)
}

// TestCheckDatabaseRepresentativeFixture runs the check over a store the
// representative fixture seeded, which fills runs, tasks, attention items,
// admissions, review records, and work units, so the walk's reads decode
// populated rows here and not only empty tables.
func TestCheckDatabaseRepresentativeFixture(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	seedTemplateStore(t, f.dbPath)
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: f.dbPath, ListenAddr: "127.0.0.1:0", SeedFixture: "representative",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	if out, err := f.run(t, t.Context(), checkDatabaseSeams{}); err != nil {
		t.Fatalf("check-database over the representative fixture: %v (output %q)", err, out)
	}
	f.requireNoCopy(t)
}

// TestCheckDatabaseNamesFailedMigration prints the failing file, never the
// migration's own error text.
func TestCheckDatabaseNamesFailedMigration(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	seedTemplateStore(t, f.dbPath)
	const secret = "migration-error-sentinel"
	_, err := f.run(t, t.Context(), checkDatabaseSeams{
		open: func(context.Context, string, store.Options) (*store.Store, error) {
			return nil, &store.MigrationError{Name: "0999_example.sql", Err: errors.New(secret)}
		},
	})
	if err == nil || !strings.Contains(err.Error(), "migration 0999_example.sql failed on the copy") {
		t.Fatalf("err = %v, want the failing migration named", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("err = %v, carries the migration's own error text", err)
	}
	f.requireNoCopy(t)
}

// TestCheckDatabaseNamesFailedKindsOnly tampers a stored row; the check
// fails naming the kind, and nothing from the row reaches its output.
func TestCheckDatabaseNamesFailedKindsOnly(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	seedTemplateStore(t, f.dbPath)
	const sentinel = "stored-row-sentinel-31d7"
	live, err := sql.Open("sqlite", "file:"+f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := live.ExecContext(t.Context(),
		`INSERT INTO projects (project_id, repository_id, body) VALUES (?, 1, ?)`,
		"p-"+sentinel, `{"`+sentinel); err != nil {
		t.Fatal(err)
	}
	if err := live.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	err = runCheckDatabaseCommand(t.Context(), f.args(), &stdout, &stderr, checkDatabaseSeams{})
	if err == nil || !strings.Contains(err.Error(), "failed to decode on the copy: project") {
		t.Fatalf("err = %v, want the project kind named", err)
	}
	for _, text := range []string{err.Error(), stdout.String(), stderr.String()} {
		if strings.Contains(text, sentinel) {
			t.Fatalf("output carries row content: %q", text)
		}
	}
	f.requireNoCopy(t)
}

// TestCheckDatabaseRemovesStaleCopy: a copy a killed run left behind is
// removed before the next copy is made.
func TestCheckDatabaseRemovesStaleCopy(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	seedTemplateStore(t, f.dbPath)
	stale := filepath.Join(f.stateDir, checkDatabaseDirName)
	if err := os.Mkdir(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "freeside.db"), []byte("left by a killed run"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run(t, t.Context(), checkDatabaseSeams{}); err != nil {
		t.Fatalf("check-database over a stale copy: %v", err)
	}
	f.requireNoCopy(t)
}

// TestCheckDatabaseCanceledAfterCopy: a signal after the copy is made ends
// the check with the context's error and removes the copy.
func TestCheckDatabaseCanceledAfterCopy(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	seedTemplateStore(t, f.dbPath)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var copied bool
	_, err := f.run(t, ctx, checkDatabaseSeams{
		open: func(ctx context.Context, path string, opts store.Options) (*store.Store, error) {
			info, err := os.Lstat(path)
			copied = err == nil && info.Mode().Perm() == 0o600
			cancel()
			return store.Open(ctx, path, opts)
		},
	})
	if !copied {
		t.Fatal("the check opened no owner-only copy")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	f.requireNoCopy(t)
}

func TestCheckDatabaseRejectsBadArguments(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	cases := map[string][]string{
		"missing environment": {"-db", f.dbPath, "-state-dir", f.stateDir},
		"missing db":          {"-environment", "ephemeral", "-state-dir", f.stateDir},
		"missing state dir":   {"-environment", "ephemeral", "-db", f.dbPath},
		"unknown tier":        {"-environment", "staging", "-db", f.dbPath, "-state-dir", f.stateDir},
		"extra argument":      append(f.args(), "extra"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := runCheckDatabaseCommand(t.Context(), args, io.Discard, io.Discard, checkDatabaseSeams{})
			if !errors.Is(err, errCheckDatabaseUsage) {
				t.Fatalf("check-database %v: err = %v, want a usage error", args, err)
			}
		})
	}
}

// TestCheckDatabaseNeedsALiveDatabase refuses a tier with no database and
// creates nothing beside the missing file.
func TestCheckDatabaseNeedsALiveDatabase(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	if _, err := f.run(t, t.Context(), checkDatabaseSeams{}); err == nil {
		t.Fatal("check-database with no live database succeeded")
	}
	entries, err := os.ReadDir(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("state directory holds %d entries, want none", len(entries))
	}
}

// TestCheckDatabaseContainsAPanic: a panic while reading the copy fails the
// check without printing the panic value, which can quote a stored row, and
// the copy is still removed.
func TestCheckDatabaseContainsAPanic(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	seedTemplateStore(t, f.dbPath)
	const sentinel = "panic-sentinel-5e02"
	out, err := f.run(t, t.Context(), checkDatabaseSeams{
		open: func(context.Context, string, store.Options) (*store.Store, error) {
			panic(sentinel)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "the check panicked") {
		t.Fatalf("err = %v, want the panic reported", err)
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(out, sentinel) {
		t.Fatalf("output carries the panic value: %v %q", err, out)
	}
	f.requireNoCopy(t)
}

// TestCheckDatabaseRefusesADatabaseInsideItsCopyDirectory: the sweep of a
// stale copy would delete a -db that lives where the copy goes.
func TestCheckDatabaseRefusesADatabaseInsideItsCopyDirectory(t *testing.T) {
	t.Parallel()
	f := newCheckDatabaseFixture(t)
	inside := filepath.Join(f.stateDir, checkDatabaseDirName, "freeside.db")
	if err := os.Mkdir(filepath.Dir(inside), 0o700); err != nil {
		t.Fatal(err)
	}
	seedTemplateStore(t, inside)
	err := runCheckDatabaseCommand(t.Context(),
		[]string{"-environment", "ephemeral", "-db", inside, "-state-dir", f.stateDir},
		io.Discard, io.Discard, checkDatabaseSeams{})
	if !errors.Is(err, errCheckDatabaseUsage) {
		t.Fatalf("err = %v, want a usage error", err)
	}
	if _, err := os.Stat(inside); err != nil {
		t.Fatalf("the named database after the refusal: %v", err)
	}
}
