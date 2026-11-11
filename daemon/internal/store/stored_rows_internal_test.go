package store

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// TestStoredRowKindsCoverEveryTable is the ratchet on the decode walk: every
// table in a migrated schema is covered by exactly one kind or listed as not
// walked with a reason, and every name either list carries is a real table.
func TestStoredRowKindsCoverEveryTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	rows, err := st.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	schema := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		schema[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	covered := map[string]string{}
	names := map[string]bool{}
	for _, kind := range storedRowKinds {
		if names[kind.name] {
			t.Errorf("kind %q is registered twice", kind.name)
		}
		names[kind.name] = true
		for _, table := range kind.tables {
			if other, ok := covered[table]; ok {
				t.Errorf("table %s is covered by both %q and %q", table, other, kind.name)
			}
			covered[table] = kind.name
		}
	}
	for table, reason := range notWalkedTables {
		if reason == "" {
			t.Errorf("not-walked table %s has no reason", table)
		}
		if kind, ok := covered[table]; ok {
			t.Errorf("table %s is both walked by %q and listed as not walked", table, kind)
		}
	}
	for table := range schema {
		if _, ok := covered[table]; ok {
			continue
		}
		if _, ok := notWalkedTables[table]; ok {
			continue
		}
		t.Errorf("table %s is neither walked by a stored-row kind nor listed in notWalkedTables", table)
	}
	for table, kind := range covered {
		if !schema[table] {
			t.Errorf("kind %q covers %s, which is not in the migrated schema", kind, table)
		}
	}
	for table := range notWalkedTables {
		if !schema[table] {
			t.Errorf("notWalkedTables lists %s, which is not in the migrated schema", table)
		}
	}
}

// TestDecodeStoredRowsEmptyDatabase runs every kind against a freshly
// migrated database, so a walk's own query (a renamed column, a dropped
// table) fails here rather than as a false refusal during an update.
func TestDecodeStoredRowsEmptyDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	for _, kind := range storedRowKinds {
		if err := kind.walk(ctx, st); err != nil {
			t.Errorf("kind %q: %v", kind.name, err)
		}
	}
	report, err := st.DecodeStoredRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failed) != 0 {
		t.Fatalf("Failed = %v, want none", report.Failed)
	}
	if report.Walked != len(storedRowKinds) || report.NotWalked != len(notWalkedTables) {
		t.Fatalf("report = %+v, want %d walked and %d not walked", report, len(storedRowKinds), len(notWalkedTables))
	}
}

// TestDecodeStoredRowsReportsKindsOnly corrupts rows of two kinds: the report
// names both, in registry order, and carries nothing from the rows.
func TestDecodeStoredRowsReportsKindsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	const sentinel = "stored-row-sentinel-8c1f"
	for _, statement := range []string{
		`INSERT INTO projects (project_id, repository_id, body) VALUES ('p-` + sentinel + `', 1, '{"` + sentinel + `')`,
		`INSERT INTO trust_profiles (profile_digest, repo, recorded_at, body) VALUES ('sha256:` + sentinel + `', 'o/r', '2026-11-11T00:00:00Z', '` + sentinel + `')`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	report, err := st.DecodeStoredRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"project", "trust profile"}
	if !slices.Equal(report.Failed, want) {
		t.Fatalf("Failed = %v, want %v", report.Failed, want)
	}
	for _, name := range report.Failed {
		if strings.Contains(name, sentinel) {
			t.Fatalf("report carries row content: %q", name)
		}
	}
}

// TestDecodeStoredRowsStopsOnCancel returns the context's error, not a
// report, so a canceled check never reads as a pass or a decode failure.
func TestDecodeStoredRowsStopsOnCancel(t *testing.T) {
	t.Parallel()
	st := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.DecodeStoredRows(ctx); err == nil {
		t.Fatal("DecodeStoredRows on a canceled context returned no error")
	}
}

// TestStoredRowKindPanicFailsTheKind: a read that panics on a stored row
// fails its kind instead of crashing the check before it cleans up.
func TestStoredRowKindPanicFailsTheKind(t *testing.T) {
	t.Parallel()
	kind := storedRowKind{name: "panicking", walk: func(context.Context, *Store) error {
		panic("stored-row-sentinel-panic")
	}}
	err := kind.run(context.Background(), nil)
	if err == nil {
		t.Fatal("a panicking walk returned no error")
	}
	if strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("err = %v, carries the panic value", err)
	}
}

// TestDecodeStoredRowsReadsFiledFollowUpIssues: an intent's reconstruction
// only counts its filed issues, so the walk must decode each filed issue by
// its own key. The row is written with foreign keys off, as a damaged store
// could hold it.
func TestDecodeStoredRowsReadsFiledFollowUpIssues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	conn, err := st.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`PRAGMA foreign_keys = OFF`,
		`INSERT INTO follow_up_filed_issues (repository_id, issue_number, instance_id, body) VALUES (1, 7, 'instance-1', '{"broken')`,
		`PRAGMA foreign_keys = ON`,
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	// The store reads on one connection, so the pinned one goes back first.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := st.DecodeStoredRows(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.Failed, []string{"follow-up filing"}) {
		t.Fatalf("Failed = %v, want the follow-up filing kind", report.Failed)
	}
}
