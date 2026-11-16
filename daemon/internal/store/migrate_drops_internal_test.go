package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestMigrationsDeclareDrops applies every embedded migration in order and
// fails when one removes a table, column, index, trigger, or view it does not
// declare on a -- drop: line, or declares one it does not remove. A renumbered
// migration keeps its declarations, so the check holds across a rebase.
func TestMigrationsDeclareDrops(t *testing.T) {
	t.Parallel()
	db := openRaw(t)
	if err := checkDeclaredDrops(context.Background(), db, migrations.FS); err != nil {
		t.Fatal(err)
	}
}

func TestDeclaredDropFixture(t *testing.T) {
	t.Parallel()
	const create = "CREATE TABLE t (id INTEGER PRIMARY KEY, b TEXT, g INTEGER GENERATED ALWAYS AS (id + 1) VIRTUAL) STRICT;"
	cases := []struct {
		name    string
		drop    string
		wantErr string
	}{
		{
			name:    "undeclared drop",
			drop:    "ALTER TABLE t DROP COLUMN b;",
			wantErr: "removes undeclared column t.b",
		},
		{
			name:    "undeclared generated column drop",
			drop:    "ALTER TABLE t DROP COLUMN g;",
			wantErr: "removes undeclared column t.g",
		},
		{
			name: "declared drop",
			drop: "-- drop: column t.b\nALTER TABLE t DROP COLUMN b;",
		},
		{
			name:    "declared drop still present",
			drop:    "-- drop: column t.b\nSELECT 1;",
			wantErr: "declares column t.b dropped but it was not removed",
		},
		{
			name: "table drop covers its columns",
			drop: "-- drop: table t\nDROP TABLE t;",
		},
		{
			name:    "malformed declaration",
			drop:    "-- drop: col t.b\nALTER TABLE t DROP COLUMN b;",
			wantErr: `unknown kind "col"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := mapFS(map[string]string{"0001_t.sql": create, "0002_drop.sql": tc.drop})
			err := checkDeclaredDrops(context.Background(), openRaw(t), fsys)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("checkDeclaredDrops: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("checkDeclaredDrops passed, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("checkDeclaredDrops error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// schemaName is one named schema object. A column is written table.column,
// and so is a view's column.
type schemaName struct {
	kind string
	name string
}

func (n schemaName) String() string { return n.kind + " " + n.name }

var dropKinds = []string{"table", "column", "index", "trigger", "view"}

// dropDirective finds every line that looks like a declaration, so a
// misspelled one is reported rather than read as an ordinary comment.
var dropDirective = regexp.MustCompile(`(?im)^[ \t]*--[ \t]*drop[ \t]*:(.*)$`)

// declaredDrops parses the -- drop: <kind> <name> lines of one migration.
func declaredDrops(body string) ([]schemaName, error) {
	var drops []schemaName
	for _, m := range dropDirective.FindAllStringSubmatch(body, -1) {
		fields := strings.Fields(m[1])
		if len(fields) != 2 {
			return nil, fmt.Errorf("%q: want -- drop: <kind> <name>", strings.TrimSpace(m[0]))
		}
		n := schemaName{kind: fields[0], name: fields[1]}
		if !slices.Contains(dropKinds, n.kind) {
			return nil, fmt.Errorf("%q: unknown kind %q, want one of %s", strings.TrimSpace(m[0]), n.kind, strings.Join(dropKinds, ", "))
		}
		if isColumn := n.kind == "column"; isColumn != (strings.Count(n.name, ".") == 1) {
			return nil, fmt.Errorf("%q: a column is written table.column and no other name has a dot", strings.TrimSpace(m[0]))
		}
		drops = append(drops, n)
	}
	return drops, nil
}

// checkDeclaredDrops applies the migrations in fsys to an empty database one
// file at a time and compares the names each one removes against its -- drop:
// lines. It reports every mismatch rather than the first. The baseline
// creates every name it holds and removes none.
func checkDeclaredDrops(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	history, err := loadMigrationHistory(fsys)
	if err != nil {
		return err
	}
	var problems []error
	if _, err := db.ExecContext(ctx, createSchemaMigrations); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	before, err := schemaNames(ctx, db)
	if err != nil {
		return err
	}
	for i, file := range history.files {
		body, err := fs.ReadFile(fsys, file)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", file, err)
		}
		declared, err := declaredDrops(string(body))
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", file, err))
		}
		if err := applyMigration(ctx, db, fsys, history.baseline+i, file); err != nil {
			return err
		}
		after, err := schemaNames(ctx, db)
		if err != nil {
			return err
		}
		expected := make(map[schemaName]bool)
		for _, n := range declared {
			expected[n] = true
		}
		lost := make(map[schemaName]bool)
		for n := range before {
			if !after[n] {
				lost[n] = true
			}
		}
		for n := range lost {
			if !expected[n] && !columnOfLostRelation(n, lost) {
				problems = append(problems, fmt.Errorf("%s removes undeclared %s: add a -- drop: %s line", file, n, n))
			}
		}
		for n := range expected {
			if !lost[n] {
				problems = append(problems, fmt.Errorf("%s declares %s dropped but it was not removed", file, n))
			}
		}
		before = after
	}
	return errors.Join(problems...)
}

// columnOfLostRelation reports whether n is a column of a table or view that
// was itself removed, so declaring the table or view covers its columns.
func columnOfLostRelation(n schemaName, lost map[schemaName]bool) bool {
	if n.kind != "column" {
		return false
	}
	relation, _, _ := strings.Cut(n.name, ".")
	return lost[schemaName{kind: "table", name: relation}] || lost[schemaName{kind: "view", name: relation}]
}

// schemaNames lists the named schema objects and the columns of every table
// and view. It reads table_xinfo because table_info omits generated columns,
// which would let a migration drop one undeclared. SQLite's own objects are left out: sqlite_autoindex_* names are
// derived from constraint position, so a table rebuild can renumber one
// without changing the schema.
func schemaNames(ctx context.Context, db *sql.DB) (map[schemaName]bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.type, m.name, p.name
		FROM sqlite_schema AS m
		LEFT JOIN pragma_table_xinfo(m.name) AS p ON m.type IN ('table', 'view')
		WHERE m.type IN ('table', 'index', 'trigger', 'view')
		  AND m.name NOT LIKE 'sqlite\_%' ESCAPE '\'
		  AND m.name <> 'schema_migrations'`)
	if err != nil {
		return nil, fmt.Errorf("list schema names: %w", err)
	}
	defer func() { _ = rows.Close() }()
	names := make(map[schemaName]bool)
	for rows.Next() {
		var (
			kind, name string
			column     sql.NullString
		)
		if err := rows.Scan(&kind, &name, &column); err != nil {
			return nil, fmt.Errorf("list schema names: %w", err)
		}
		names[schemaName{kind: kind, name: name}] = true
		if column.Valid {
			names[schemaName{kind: "column", name: name + "." + column.String}] = true
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list schema names: %w", err)
	}
	return names, nil
}
