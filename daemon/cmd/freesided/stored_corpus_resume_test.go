package main

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// storedCorpusDir is the committed corpus.
const storedCorpusDir = "testdata/stored-corpus"

// TestStoredCorpusResumes starts the current scenario daemon on a copy of
// every corpus fixture. The start migrates the copy and runs every start
// pass; a sweep then reads every task timeline, run, run timeline, and
// attention item through the signet service; and the case's resume carries
// its in-flight work to a published pull request. The committed fixture is
// never opened for writing.
func TestStoredCorpusResumes(t *testing.T) {
	t.Parallel()
	fixtures := storedCorpusFixtureDirs(t)
	if len(fixtures) == 0 {
		t.Fatalf("stored-state corpus %s holds no fixture", storedCorpusDir)
	}
	for _, dir := range fixtures {
		rel, err := filepath.Rel(storedCorpusDir, dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Run(filepath.ToSlash(rel), func(t *testing.T) {
			t.Parallel()
			fixture := readStoredCorpusFixture(t, dir)
			known, ok := storedCorpusCases[fixture.Case]
			if !ok {
				t.Fatalf("fixture %s names case %q, which the corpus test does not know", dir, fixture.Case)
			}
			played := known.scenario()
			cfg := scenarioConfig(t, &played)
			copyStoredCorpusFixture(t, dir, cfg.DBPath)
			assertStoredCorpusCopy(t, cfg.DBPath, fixture)

			h := startDaemon(t, cfg)
			sweepStoredHistory(t, h)
			known.resume(t, h, fixture)
			assertNoDurableStop(t, h)
			if err := h.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
		})
	}
}

// storedCorpusFixtureDirs lists every <build>/<case> directory in the corpus.
// A build directory with no case, or a stray file, fails the test, so nothing
// sits in the corpus unchecked.
func storedCorpusFixtureDirs(t *testing.T) []string {
	t.Helper()
	builds, err := os.ReadDir(storedCorpusDir)
	if err != nil {
		t.Fatalf("read stored-state corpus: %v", err)
	}
	var dirs []string
	for _, build := range builds {
		if !build.IsDir() || !storedCorpusBuildName.MatchString(build.Name()) {
			t.Fatalf("corpus entry %q is not a <YYYYMMDD>-<sha12> fixture-set directory", build.Name())
		}
		cases, err := os.ReadDir(filepath.Join(storedCorpusDir, build.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if len(cases) == 0 {
			t.Fatalf("fixture set %s holds no case", build.Name())
		}
		for _, c := range cases {
			if !c.IsDir() {
				t.Fatalf("corpus entry %s/%s is not a case directory", build.Name(), c.Name())
			}
			dirs = append(dirs, filepath.Join(storedCorpusDir, build.Name(), c.Name()))
		}
	}
	return dirs
}

var (
	storedCorpusBuildName = regexp.MustCompile(`^[0-9]{8}-[0-9a-f]{12}$`)
	storedCorpusCommit    = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// readStoredCorpusFixture decodes and checks the fixture's manifest against
// the directory that holds it.
func readStoredCorpusFixture(t *testing.T, dir string) storedCorpusFixture {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, storedCorpusManifest)) //nolint:gosec // a committed corpus path
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var fixture storedCorpusFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode manifest %s: %v", dir, err)
	}
	build := filepath.Base(filepath.Dir(dir))
	switch {
	case fixture.Case != filepath.Base(dir):
		t.Fatalf("manifest case %q, directory %q", fixture.Case, filepath.Base(dir))
	case !storedCorpusCommit.MatchString(fixture.BuildCommit) || build[len(build)-12:] != fixture.BuildCommit[:12]:
		t.Fatalf("manifest build_commit %q does not match fixture set %s", fixture.BuildCommit, build)
	case fixture.SchemaVersion <= 0 || fixture.ProducedAt.IsZero() || fixture.ProducedBy == "" ||
		fixture.TaskID == "" || fixture.RunID == "":
		t.Fatalf("manifest %s is incomplete: %+v", dir, fixture)
	}
	return fixture
}

// copyStoredCorpusFixture lays the fixture down at dbPath: a fresh topic
// key first, as a first start would write it, then the decompressed
// database and a copy of each tree the fixture carries. No key or
// checkpoint is committed, so the start produces its own backup key and
// checkpoint.
func copyStoredCorpusFixture(t *testing.T, dir, dbPath string) {
	t.Helper()
	if _, err := loadOrCreateTopicKey(dbPath, false); err != nil {
		t.Fatalf("create topic key: %v", err)
	}
	compressed, err := os.Open(filepath.Join(dir, storedCorpusDatabase)) //nolint:gosec // a committed corpus path
	if err != nil {
		t.Fatal(err)
	}
	defer compressed.Close() //nolint:errcheck // read-only
	reader, err := gzip.NewReader(compressed)
	if err != nil {
		t.Fatalf("open %s: %v", storedCorpusDatabase, err)
	}
	database, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress %s: %v", storedCorpusDatabase, err)
	}
	writeNewFile(t, dbPath, database)
	root := filepath.Dir(dbPath)
	for _, tree := range storedCorpusTrees {
		from := filepath.Join(dir, filepath.FromSlash(tree))
		if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			t.Fatal(err)
		}
		if err := os.CopyFS(filepath.Join(root, filepath.FromSlash(tree)), os.DirFS(from)); err != nil {
			t.Fatalf("copy %s: %v", tree, err)
		}
	}
}

// assertStoredCorpusCopy checks, before any start touches the copy, that it
// holds no pairing code or device credential and that its applied
// migrations match the manifest.
func assertStoredCorpusCopy(t *testing.T, dbPath string, fixture storedCorpusFixture) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close() //nolint:errcheck // the checks below report what matters
	for _, table := range []string{"pairing_codes", "device_credentials"} {
		var n int
		if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Fatalf("fixture holds %d %s rows, want none", n, table)
		}
	}
	var applied int
	if err := db.QueryRowContext(t.Context(), "SELECT count(*) FROM schema_migrations").Scan(&applied); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if applied != fixture.SchemaVersion {
		t.Fatalf("fixture has %d migrations applied, manifest says %d", applied, fixture.SchemaVersion)
	}
}

// sweepStoredHistory reads every task timeline, run, run timeline, and
// attention item through the signet service, as a client would. It takes
// the IDs from the store, not from the service's listings, because a
// listing leaves out a run it can't project (signet's ListRuns) and the
// sweep must reach exactly those. It fails on the first read that errors,
// and on a store with no task or run, which would make the sweep prove
// nothing.
func sweepStoredHistory(t *testing.T, h *daemon) {
	t.Helper()
	ctx := t.Context()
	var (
		tasks []domain.TaskID
		runs  []domain.RunID
		items []domain.ItemID
	)
	if err := h.Store.Read(ctx, func(tx *store.ReadTx) error {
		listedTasks, err := tx.ListTasks(ctx)
		if err != nil {
			return err
		}
		for _, task := range listedTasks {
			tasks = append(tasks, task.Value.ID)
		}
		listedRuns, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		for _, run := range listedRuns {
			runs = append(runs, run.Value.ID)
		}
		listedItems, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		for _, item := range listedItems {
			items = append(items, item.Value.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("list stored history: %v", err)
	}
	for _, id := range tasks {
		if _, err := h.Attention.GetTaskTimeline(ctx, id); err != nil {
			t.Fatalf("task %s timeline: %v", id, err)
		}
	}
	for _, id := range runs {
		if _, err := h.Attention.GetRun(ctx, id); err != nil {
			t.Fatalf("run %s: %v", id, err)
		}
		if _, err := h.Attention.GetRunTimeline(ctx, id); err != nil {
			t.Fatalf("run %s timeline: %v", id, err)
		}
	}
	for _, id := range items {
		if _, err := h.Attention.GetAttentionItem(ctx, id); err != nil {
			t.Fatalf("attention item %s: %v", id, err)
		}
	}
	if len(tasks) == 0 || len(runs) == 0 {
		t.Fatalf("history sweep read %d tasks and %d runs; a fixture holds at least one of each", len(tasks), len(runs))
	}
}
