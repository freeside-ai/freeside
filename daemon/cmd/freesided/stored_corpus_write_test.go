package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
)

// storedCorpusOutEnv names the corpus directory TestWriteStoredCorpus writes
// a fixture set into. Unset, the producer skips.
const storedCorpusOutEnv = "FREESIDE_STORED_CORPUS_OUT"

// storedCorpusCommand is the command manifests record. It names the output
// by placeholder, so no fixture carries a path from the producing machine.
const storedCorpusCommand = storedCorpusOutEnv +
	"=<corpus> go test ./cmd/freesided -run '^TestWriteStoredCorpus$' -count=1"

// storedCorpusCapture is a stopped scenario daemon's root: its database and
// the task and run the case's resume continues.
type storedCorpusCapture struct {
	dbPath string
	task   domain.TaskID
	run    domain.RunID
}

// storedCorpusProducers plays each case on its own scenario daemon to the
// state it captures, then closes the daemon.
var storedCorpusProducers = map[string]func(t *testing.T) storedCorpusCapture{
	"spec-approval": func(t *testing.T) storedCorpusCapture {
		t.Helper()
		played := storedCorpusCases["spec-approval"].scenario()
		cfg := scenarioConfig(t, &played)
		h := startDaemon(t, cfg)
		submitted := submitScenario(t, h, cfg)
		waitForScenarioItem(t, h.Attention, scenarioSpecApprovalItem(submitted.RunID))
		closeStoredCorpusDaemon(t, h)
		return storedCorpusCapture{dbPath: cfg.DBPath, task: submitted.TaskID, run: submitted.RunID}
	},
	"quota-park": func(t *testing.T) storedCorpusCapture {
		t.Helper()
		played := storedCorpusCases["quota-park"].scenario()
		cfg := scenarioConfig(t, &played)
		h := startDaemon(t, cfg)
		submitted := submitAndApproveScenario(t, h, cfg)
		first := waitForScenarioRun(t, h.Store, submitted.RunID)
		parked, err := engine.ProductionAttemptRunID(first.CampaignID, 2)
		if err != nil {
			t.Fatal(err)
		}
		if hold := waitForScenarioHold(t, h.Store, parked); hold.Reason != domain.HoldQuotaWindowExhausted {
			t.Fatalf("parked hold = %+v, want %s", hold, domain.HoldQuotaWindowExhausted)
		}
		// The hold can land before the engine records the refused
		// attempt's terminal; capture the settled state.
		waitForStoredCorpusTerminal(t, h, submitted.TaskID, first.ID)
		closeStoredCorpusDaemon(t, h)
		return storedCorpusCapture{dbPath: cfg.DBPath, task: submitted.TaskID, run: parked}
	},
	"publication-pending": func(t *testing.T) storedCorpusCapture {
		t.Helper()
		played := storedCorpusCases["publication-pending"].scenario()
		// The hook holds the lane at its first review request until the
		// daemon is stopping, so the task stays pending and verified, with
		// nothing sent to the reviewer or the forge. It returns only after
		// stop cancels the daemon's context: an error returned while the
		// daemon runs is a crash seam the lane answers with a durable stop.
		// A later stop can't be moved to another root: the review request's
		// authority binds the review workspace's absolute path, and the
		// resume's re-gate rebuilds that path under the new root.
		ctx, stop := context.WithCancel(t.Context())
		t.Cleanup(stop)
		reached := make(chan struct{})
		var once sync.Once
		played.PublicationTransitionHook = func(transition engine.DurableTransition, side engine.DurableTransitionSide) error {
			if transition != engine.DurableTransitionReviewRequest || side != engine.DurableTransitionBefore {
				return nil
			}
			once.Do(func() { close(reached) })
			<-ctx.Done()
			return errors.New("stored-state corpus: daemon stopped before the review request")
		}
		cfg := scenarioConfig(t, &played)
		seedTemplateStore(t, cfg.DBPath)
		h, err := run(ctx, nil, cfg)
		if err != nil {
			t.Fatal(err)
		}
		submitted := submitAndApproveScenario(t, h, cfg)
		select {
		case <-reached:
		case <-time.After(scenarioWaitBound):
			t.Fatalf("run %s did not reach the review request within %s", submitted.RunID, scenarioWaitBound)
		}
		timeline, err := h.Attention.GetTaskTimeline(t.Context(), submitted.TaskID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range timeline.Events {
			if event.Kind == domain.TaskEventPROpened {
				t.Fatalf("task %s opened a pull request before the stop", submitted.TaskID)
			}
		}
		assertNoDurableStop(t, h)
		// Cancel before Close: the hook's error then reaches a lane that is
		// shutting down, which records no stop.
		stop()
		if err := h.Close(); err != nil {
			t.Fatalf("close daemon: %v", err)
		}
		return storedCorpusCapture{dbPath: cfg.DBPath, task: submitted.TaskID, run: submitted.RunID}
	},
}

// TestWriteStoredCorpus writes one fixture set, every known case, into the
// corpus directory storedCorpusOutEnv names. It refuses a daemon tree with
// uncommitted changes, so every fixture names the real build that wrote it.
// The recipe is in daemon/README.md (Stored-State Corpus).
func TestWriteStoredCorpus(t *testing.T) {
	t.Parallel()
	out := os.Getenv(storedCorpusOutEnv)
	if out == "" {
		t.Skip(storedCorpusOutEnv + " is unset")
	}
	if names := slices.Sorted(maps.Keys(storedCorpusProducers)); !slices.Equal(names, slices.Sorted(maps.Keys(storedCorpusCases))) {
		t.Fatalf("producer cases %v differ from the corpus test's cases", names)
	}
	if dirty := storedCorpusGit(t, "status", "--porcelain", "--", "."); dirty != "" {
		t.Fatalf("daemon tree has uncommitted changes; commit them so the fixture names its build:\n%s", dirty)
	}
	commit := storedCorpusGit(t, "rev-parse", "HEAD")
	day := storedCorpusGit(t, "show", "-s", "--format=%cd", "--date=format:%Y%m%d", "HEAD")
	set := filepath.Join(out, day+"-"+commit[:12])

	for _, name := range slices.Sorted(maps.Keys(storedCorpusProducers)) {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(set, name)
			//nolint:gosec // the operator names the output directory
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("fixture %s already exists or can't be checked (%v); every fixture stays as written", dir, err)
			}
			capture := storedCorpusProducers[name](t)
			writeStoredCorpusFixture(t, dir, capture, storedCorpusFixture{
				BuildCommit: commit, Case: name, ProducedAt: time.Now().UTC().Truncate(time.Second),
				ProducedBy: storedCorpusCommand, TaskID: capture.task, RunID: capture.run,
			})
		})
	}
}

// storedCorpusGit runs git on the daemon tree the test was built from, with
// any inherited GIT_* variable dropped so it reads this checkout.
func storedCorpusGit(t *testing.T, args ...string) string {
	t.Helper()
	//nolint:gosec // fixed git subcommands from this file
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", "../.."}, args...)...)
	cmd.Env = gitenv.Scrubbed()
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(output))
}

// waitForStoredCorpusTerminal waits for the run's terminal milestone,
// whatever its outcome.
func waitForStoredCorpusTerminal(t *testing.T, h *daemon, task domain.TaskID, run domain.RunID) {
	t.Helper()
	deadline := time.Now().Add(scenarioWaitBound)
	for {
		timeline, err := h.Attention.GetTaskTimeline(t.Context(), task)
		if err != nil {
			t.Fatalf("task timeline: %v", err)
		}
		for _, event := range timeline.Events {
			if event.RunID != nil && *event.RunID == run &&
				event.Milestone != nil && event.Milestone.Kind == domain.MilestoneTerminalRecorded {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s recorded no terminal within %s", run, scenarioWaitBound)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func closeStoredCorpusDaemon(t *testing.T, h *daemon) {
	t.Helper()
	assertNoDurableStop(t, h)
	if err := h.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// writeStoredCorpusFixture compacts the closed daemon's database into the
// fixture directory with the trees beside it and the manifest. Unconsumed
// pairing codes, which every start mints, are deleted from the database
// first. Every check runs before anything is written, so a refusal leaves no
// partial fixture: a database that still holds a pairing code or device
// credential, or a database or file whose bytes name the producer's
// temporary root or $HOME.
func writeStoredCorpusFixture(t *testing.T, dir string, capture storedCorpusCapture, fixture storedCorpusFixture) {
	t.Helper()
	root := filepath.Dir(capture.dbPath)
	forbidden := storedCorpusForbiddenPaths(root)
	database := compactStoredCorpusDatabase(t, capture.dbPath, forbidden, &fixture)
	refuseStoredCorpusPaths(t, "database", database, forbidden)
	files := map[string][]byte{}
	for _, tree := range storedCorpusTrees {
		readStoredCorpusTree(t, root, filepath.FromSlash(tree), files)
	}
	for rel, data := range files {
		refuseStoredCorpusPaths(t, rel, data, forbidden)
	}

	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(database); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	files[storedCorpusDatabase] = compressed.Bytes()
	// HTML escaping would turn the command's <corpus> placeholder into
	// \u003ccorpus\u003e.
	var manifest bytes.Buffer
	enc := json.NewEncoder(&manifest)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fixture); err != nil {
		t.Fatal(err)
	}
	files[storedCorpusManifest] = manifest.Bytes()
	for _, rel := range slices.Sorted(maps.Keys(files)) {
		target := filepath.Join(dir, rel)
		//nolint:gosec // the operator names the output directory
		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			t.Fatal(err)
		}
		writeNewFile(t, target, files[rel])
	}
	t.Logf("wrote %s: %d files, database %d bytes compressed", dir, len(files), compressed.Len())
}

// compactStoredCorpusDatabase deletes the unconsumed pairing codes, checks
// no credential row remains and no stored value names a forbidden path,
// records the applied migrations in the manifest, and returns a VACUUM INTO
// copy of the database.
func compactStoredCorpusDatabase(t *testing.T, dbPath string, forbidden []string, fixture *storedCorpusFixture) []byte {
	t.Helper()
	ctx := t.Context()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // the compact copy is what the fixture keeps
	if _, err := db.ExecContext(ctx, "DELETE FROM pairing_codes WHERE consumed_at IS NULL"); err != nil {
		t.Fatalf("delete unconsumed pairing codes: %v", err)
	}
	for _, table := range []string{"pairing_codes", "device_credentials"} {
		var n int
		if err := db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("captured database holds %d %s rows; a fixture holds none", n, table)
		}
	}
	refuseStoredCorpusValues(t, db, forbidden)
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&fixture.SchemaVersion); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	compact := filepath.Join(t.TempDir(), "compact.db")
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", compact); err != nil {
		t.Fatalf("compact database: %v", err)
	}
	database, err := os.ReadFile(compact) //nolint:gosec // test-owned path
	if err != nil {
		t.Fatal(err)
	}
	return database
}

// storedCorpusForbiddenPaths is the producer's temporary root, as given and
// with symlinks resolved, and $HOME.
func storedCorpusForbiddenPaths(root string) []string {
	paths := []string{root}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != root {
		paths = append(paths, resolved)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, home)
	}
	return paths
}

func refuseStoredCorpusPaths(t *testing.T, what string, data []byte, forbidden []string) {
	t.Helper()
	for _, path := range forbidden {
		if bytes.Contains(data, []byte(path)) {
			t.Fatalf("%s names %s, a path on the producing machine; find what stores it before relaxing this", what, path)
		}
	}
}

// refuseStoredCorpusValues refuses a stored value, in any column of any
// table, that names a forbidden path. The scan of the file's bytes alone
// isn't enough: a value too long for its page continues on overflow pages,
// so a path can straddle a page pointer and never appear whole in the file.
func refuseStoredCorpusValues(t *testing.T, db *sql.DB, forbidden []string) {
	t.Helper()
	ctx := t.Context()
	rows, err := db.QueryContext(ctx, "SELECT name FROM sqlite_schema WHERE type = 'table'")
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("list tables: %v", err)
		}
		tables = append(tables, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	for _, table := range tables {
		refuseStoredCorpusTable(t, db, table, forbidden)
	}
}

func refuseStoredCorpusTable(t *testing.T, db *sql.DB, table string, forbidden []string) {
	t.Helper()
	quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
	rows, err := db.QueryContext(t.Context(), "SELECT * FROM "+quoted) //nolint:gosec // the name comes from sqlite_schema, quoted
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	defer rows.Close() //nolint:errcheck // rows.Err reports a failed read
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
	values := make([]any, len(columns))
	targets := make([]any, len(columns))
	for i := range values {
		targets[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(targets...); err != nil {
			t.Fatalf("read %s: %v", table, err)
		}
		for i, value := range values {
			var data []byte
			switch v := value.(type) {
			case string:
				data = []byte(v)
			case []byte:
				data = v
			default:
				continue
			}
			refuseStoredCorpusPaths(t, table+"."+columns[i], data, forbidden)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s: %v", table, err)
	}
}

// readStoredCorpusTree adds every file under root/tree to files, keyed by
// its path relative to root. A missing tree adds nothing.
func readStoredCorpusTree(t *testing.T, root, tree string, files map[string][]byte) {
	t.Helper()
	err := filepath.WalkDir(filepath.Join(root, tree), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, err := os.ReadFile(path) //nolint:gosec // test-owned path
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = data
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("read %s: %v", tree, err)
	}
}
