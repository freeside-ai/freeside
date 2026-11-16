package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// migrationName pins the file naming rule: NNNN_description.sql, NNNN
// contiguous from the first file's version (see daemon/migrations).
var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// squashedHistoryFile lists, one "version name digest" line each, the rows a
// database recorded for the migration files a baseline replaced.
const squashedHistoryFile = "squashed_history.txt"

// lastUnsquashedCommit is a main commit whose build still migrates a database
// below the baseline: the commit the squash branched from. Later main commits
// before the squash merged can too; BelowBaselineError names this one.
const lastUnsquashedCommit = "b7bd02f71538ef66414ee1874ef378ca9dbf3a0f"

const createSchemaMigrations = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    digest     TEXT NOT NULL,
    applied_at TEXT NOT NULL
) STRICT;
`

// migrate applies every pending migration from fsys in order, one transaction
// per file with its schema_migrations row inserted inside it, so a failing
// migration rolls back completely and leaves the database at the prior
// version. Re-running against an up-to-date database is a no-op. Applied
// migrations are pinned by name and content digest: files are immutable once
// applied, and a renamed or rewritten file is a hard error, not a silent
// divergence between existing and fresh databases. The whole recorded history
// is checked before any file applies, so a refused database is left as it was.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	if _, err := db.ExecContext(ctx, createSchemaMigrations); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	history, err := loadMigrationHistory(fsys)
	if err != nil {
		return err
	}
	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}
	next, err := history.validate(fsys, applied)
	if err != nil {
		return err
	}
	for i := next; i < len(history.files); i++ {
		if err := applyMigration(ctx, db, fsys, history.baseline+i, history.files[i]); err != nil {
			return err
		}
	}
	return nil
}

// requireHead checks db's recorded history against the embedded one, as
// migrate does, and requires it at head without applying anything. It is
// the rule OpenReadOnly and OpenExisting share.
func requireHead(ctx context.Context, db *sql.DB) error {
	history, err := loadMigrationHistory(migrations.FS)
	if err != nil {
		return err
	}
	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}
	if _, err := history.validate(migrations.FS, applied); err != nil {
		return err
	}
	if version := lastVersion(applied); version != history.head() {
		return &SchemaMismatchError{Database: version, Binary: history.head()}
	}
	return nil
}

// migrationHistory is an embedded schema history. Files run contiguously
// from baseline. When baseline is above 1, the first file is a squash of the
// files before it, and squashed holds the rows 1 through baseline that a
// database which applied those files recorded.
type migrationHistory struct {
	files    []string
	baseline int
	squashed []appliedMigration
}

// head is the version of the last file, zero for an empty history.
func (h migrationHistory) head() int { return h.baseline + len(h.files) - 1 }

// validate checks a database's recorded history against h and returns the
// index in h.files of the first file still to apply. It accepts three
// histories, and refuses every other:
//
//   - empty: every file applies, and the database records the baseline to
//     head;
//   - starting at the baseline with the baseline file's own row, as a
//     database created from the squash records;
//   - starting at 1 and reaching at least the baseline, with rows 1 through
//     the baseline equal to the squashed list by name and digest, as a
//     database created before the squash records.
//
// Rows above the baseline match the files by name and digest either way. A
// history that starts at 1 and stops below the baseline is a
// BelowBaselineError: the files that would migrate it are gone.
func (h migrationHistory) validate(fsys fs.FS, applied []appliedMigration) (int, error) {
	if len(applied) == 0 {
		return 0, nil
	}
	last := lastVersion(applied)
	if last > h.head() {
		return 0, fmt.Errorf("database at schema version %d but this binary's head is %d: binary older than database", last, h.head())
	}
	rest := applied
	switch first := applied[0].version; {
	case h.baseline > 1 && first == 1:
		for i, a := range applied[:min(len(applied), h.baseline)] {
			if want := h.squashed[i]; a.name != want.name || a.digest != want.digest {
				return 0, fmt.Errorf("applied migration %d is %q with digest %s but the squashed history records %q with digest %s: migration history diverged",
					a.version, a.name, a.digest, want.name, want.digest)
			}
		}
		if last < h.baseline {
			return 0, &BelowBaselineError{Database: last, Baseline: h.baseline}
		}
		// The squashed rows stand for the baseline file itself.
		rest = applied[h.baseline:]
	case first != h.baseline:
		return 0, fmt.Errorf("schema_migrations starts at version %d, want 1 or the baseline %d: migration history diverged", first, h.baseline)
	}
	for _, a := range rest {
		name := h.files[a.version-h.baseline]
		if name != a.name {
			return 0, fmt.Errorf("applied migration %d is %q but the embedded file is %q: migration history diverged", a.version, a.name, name)
		}
		digest, err := fileDigest(fsys, name)
		if err != nil {
			return 0, err
		}
		if digest != a.digest {
			return 0, fmt.Errorf("applied migration %q has digest %s but the embedded file has %s: migration content rewritten", a.name, a.digest, digest)
		}
	}
	return last - h.baseline + 1, nil
}

// BelowBaselineError refuses a database whose recorded history stops below
// the migration baseline. The files that would migrate it were squashed into
// the baseline, so this build can't; a build from lastUnsquashedCommit can.
// The store returns it before writing anything. It carries versions only,
// never a stored value.
type BelowBaselineError struct {
	Database, Baseline int
}

func (e *BelowBaselineError) Error() string {
	return fmt.Sprintf("database schema version %d is below migration baseline %d, which this build can't migrate: "+
		"open it first with freesided built from main at %s, which migrates it to %d or later",
		e.Database, e.Baseline, lastUnsquashedCommit, e.Baseline)
}

// fileDigest is the content pin recorded with each applied migration.
func fileDigest(fsys fs.FS, name string) (string, error) {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", fmt.Errorf("read migration %q: %w", name, err)
	}
	return contentaddr.Sum(body), nil
}

// loadMigrationHistory lists and validates the migration files and the
// squashed history: every name matches the naming rule, the versions run
// contiguously from the first file's with no gaps or duplicates, and when
// that first version is above 1 the squashed list holds exactly one row for
// each version from 1 through it. A history that starts at 0001 needs no
// list.
func loadMigrationHistory(fsys fs.FS) (migrationHistory, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return migrationHistory{}, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	history := migrationHistory{files: names, baseline: 1}
	for i, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return migrationHistory{}, err
		}
		if i == 0 {
			history.baseline = version
		}
		if want := history.baseline + i; version != want {
			return migrationHistory{}, fmt.Errorf("migration %q has version %d, want contiguous version %d", name, version, want)
		}
	}
	if history.baseline > 1 {
		history.squashed, err = squashedHistory(fsys, history.baseline)
		if err != nil {
			return migrationHistory{}, err
		}
	}
	return history, nil
}

// migrationVersion parses the version from a migration file name.
func migrationVersion(name string) (int, error) {
	m := migrationName.FindStringSubmatch(name)
	if m == nil {
		return 0, fmt.Errorf("migration %q does not match NNNN_description.sql", name)
	}
	version, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("migration %q: %w", name, err)
	}
	return version, nil
}

// squashedHistory reads the squashed list and requires it to hold versions 1
// through baseline in order, each naming a migration file of that version
// with a canonical digest.
func squashedHistory(fsys fs.FS, baseline int) ([]appliedMigration, error) {
	body, err := fs.ReadFile(fsys, squashedHistoryFile)
	if err != nil {
		return nil, fmt.Errorf("migrations start at version %d, so %s must list 1 through it: %w", baseline, squashedHistoryFile, err)
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != baseline {
		return nil, fmt.Errorf("%s has %d lines, want one for each version 1 through %d", squashedHistoryFile, len(lines), baseline)
	}
	rows := make([]appliedMigration, len(lines))
	for i, line := range lines {
		fields := strings.Split(line, " ")
		if len(fields) != 3 {
			return nil, fmt.Errorf("%s line %d is %q, want \"version name digest\"", squashedHistoryFile, i+1, line)
		}
		version, err := strconv.Atoi(fields[0])
		if err != nil || version != i+1 {
			return nil, fmt.Errorf("%s line %d has version %q, want %d", squashedHistoryFile, i+1, fields[0], i+1)
		}
		if named, err := migrationVersion(fields[1]); err != nil || named != version {
			return nil, fmt.Errorf("%s line %d names %q, want a migration file of version %d", squashedHistoryFile, i+1, fields[1], version)
		}
		if _, ok := contentaddr.Parse(fields[2]); !ok {
			return nil, fmt.Errorf("%s line %d has digest %q, want a canonical content address", squashedHistoryFile, i+1, fields[2])
		}
		rows[i] = appliedMigration{version: version, name: fields[1], digest: fields[2]}
	}
	return rows, nil
}

// appliedMigration is one recorded schema_migrations row.
type appliedMigration struct {
	version int
	name    string
	digest  string
}

// lastVersion is the highest recorded version, zero for an empty history.
func lastVersion(applied []appliedMigration) int {
	if len(applied) == 0 {
		return 0
	}
	return applied[len(applied)-1].version
}

// appliedMigrations returns the recorded migrations ordered by version,
// verifying the versions are contiguous from the first. Where the history
// may start is migrationHistory.validate's rule.
func appliedMigrations(ctx context.Context, db *sql.DB) ([]appliedMigration, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, name, digest FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var applied []appliedMigration
	for rows.Next() {
		var a appliedMigration
		if err := rows.Scan(&a.version, &a.name, &a.digest); err != nil {
			return nil, fmt.Errorf("read schema_migrations: %w", err)
		}
		if len(applied) > 0 && a.version != lastVersion(applied)+1 {
			return nil, fmt.Errorf("schema_migrations has version %d after %d: history not contiguous", a.version, lastVersion(applied))
		}
		applied = append(applied, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

// MigrationError is a failure applying one migration file. Its text is the
// one migrate has always returned, so logs keep their shape; Name lets a
// caller report which file failed without printing Err, whose text can carry
// stored values a migration read.
type MigrationError struct {
	Name string
	Err  error
}

func (e *MigrationError) Error() string {
	return fmt.Sprintf("migration %q: %v", e.Name, e.Err)
}

func (e *MigrationError) Unwrap() error { return e.Err }

// applyMigration runs one migration file and records its version, the one its
// name carries, in the same transaction. Migration files must not contain BEGIN/COMMIT.
func applyMigration(ctx context.Context, db *sql.DB, fsys fs.FS, version int, name string) error {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("read migration %q: %w", name, err)
	}
	digest := contentaddr.Sum(body)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return &MigrationError{Name: name, Err: fmt.Errorf("begin: %w", err)}
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return &MigrationError{Name: name, Err: err}
	}
	appliedAt := formatTime(time.Now())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, digest, applied_at) VALUES (?, ?, ?, ?)`,
		version, name, digest, appliedAt); err != nil {
		return &MigrationError{Name: name, Err: fmt.Errorf("record version: %w", err)}
	}
	if err := tx.Commit(); err != nil {
		return &MigrationError{Name: name, Err: fmt.Errorf("commit: %w", err)}
	}
	return nil
}

// SchemaVersion reports the highest applied migration version, zero for a
// database with no migrations applied.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var version int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	if err != nil {
		return 0, fmt.Errorf("schema version: %w", err)
	}
	return version, nil
}
