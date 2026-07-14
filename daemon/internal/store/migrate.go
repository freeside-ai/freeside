package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"time"
)

// migrationName pins the file naming rule: NNNN_description.sql, NNNN
// contiguous from 0001 (see daemon/migrations).
var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

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
// divergence between existing and fresh databases.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	if _, err := db.ExecContext(ctx, createSchemaMigrations); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	names, err := migrationFiles(fsys)
	if err != nil {
		return err
	}
	applied, err := appliedMigrations(ctx, db)
	if err != nil {
		return err
	}
	if len(applied) > len(names) {
		return fmt.Errorf("database at schema version %d but only %d migrations known: binary older than database", len(applied), len(names))
	}
	for i, a := range applied {
		if names[i] != a.name {
			return fmt.Errorf("applied migration %d is %q but the embedded file is %q: migration history diverged", i+1, a.name, names[i])
		}
		digest, err := fileDigest(fsys, names[i])
		if err != nil {
			return err
		}
		if digest != a.digest {
			return fmt.Errorf("applied migration %q has digest %s but the embedded file has %s: migration content rewritten", a.name, a.digest, digest)
		}
	}
	for i := len(applied); i < len(names); i++ {
		if err := applyMigration(ctx, db, fsys, i+1, names[i]); err != nil {
			return err
		}
	}
	return nil
}

// fileDigest is the content pin recorded with each applied migration.
func fileDigest(fsys fs.FS, name string) (string, error) {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", fmt.Errorf("read migration %q: %w", name, err)
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(body)), nil
}

// migrationFiles lists and validates the migration files: every name matches
// the naming rule and the versions run 1, 2, 3, ... with no gaps or
// duplicates.
func migrationFiles(fsys fs.FS) ([]string, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	for i, name := range names {
		m := migrationName.FindStringSubmatch(name)
		if m == nil {
			return nil, fmt.Errorf("migration %q does not match NNNN_description.sql", name)
		}
		var version int
		if _, err := fmt.Sscanf(m[1], "%d", &version); err != nil {
			return nil, fmt.Errorf("migration %q: %w", name, err)
		}
		if version != i+1 {
			return nil, fmt.Errorf("migration %q has version %d, want contiguous version %d", name, version, i+1)
		}
	}
	return names, nil
}

// appliedMigration is one recorded schema_migrations row.
type appliedMigration struct {
	name   string
	digest string
}

// appliedMigrations returns the recorded migrations ordered by version,
// verifying the versions are contiguous from 1.
func appliedMigrations(ctx context.Context, db *sql.DB) ([]appliedMigration, error) {
	rows, err := db.QueryContext(ctx, `SELECT version, name, digest FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var applied []appliedMigration
	for rows.Next() {
		var (
			version int
			a       appliedMigration
		)
		if err := rows.Scan(&version, &a.name, &a.digest); err != nil {
			return nil, fmt.Errorf("read schema_migrations: %w", err)
		}
		if version != len(applied)+1 {
			return nil, fmt.Errorf("schema_migrations has version %d after %d applied: history not contiguous", version, len(applied))
		}
		applied = append(applied, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

// applyMigration runs one migration file and records its version in the same
// transaction. Migration files must not contain BEGIN/COMMIT.
func applyMigration(ctx context.Context, db *sql.DB, fsys fs.FS, version int, name string) error {
	body, err := fs.ReadFile(fsys, name)
	if err != nil {
		return fmt.Errorf("read migration %q: %w", name, err)
	}
	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(body))
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migration %q: begin: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("migration %q: %w", name, err)
	}
	appliedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, digest, applied_at) VALUES (?, ?, ?, ?)`,
		version, name, digest, appliedAt); err != nil {
		return fmt.Errorf("migration %q: record version: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migration %q: commit: %w", name, err)
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
