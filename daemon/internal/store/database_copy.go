package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
)

// CopyDatabaseFile writes a consistent copy of the SQLite database at src to
// a new owner-only file at dst without reading src's schema. It is the copy
// for a database this binary may not be able to open: OpenExisting refuses
// any migration history other than this binary's head, so a build that adds
// a migration could not otherwise copy the database it is about to migrate.
//
// The copy carries every credential the database holds (device credentials,
// pairing codes), so dst is created exclusively at mode 0600 before VACUUM
// INTO fills it, as writeSnapshot does: O_EXCL refuses an existing file and
// the 0600 create closes the window a loose umask would open. On failure the
// file this call created is removed, never a pre-existing one.
//
// The caller must hold the daemon lock on src, so no daemon writes it during
// the copy and no exclusive-locking daemon holds it open.
func CopyDatabaseFile(ctx context.Context, src, dst string) error {
	db, err := openSchemaAgnostic(src)
	if err != nil {
		return fmt.Errorf("copy database: %w", err)
	}
	defer func() { _ = db.Close() }()

	file, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // caller-owned copy path
	if err != nil {
		return fmt.Errorf("copy database: create %s: %w", dst, err)
	}
	if err := file.Close(); err != nil {
		return errors.Join(fmt.Errorf("copy database: create %s: %w", dst, err), os.Remove(dst))
	}
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		return errors.Join(fmt.Errorf("copy database into %s: %w", dst, err), os.Remove(dst))
	}
	// VACUUM INTO may replace the file under the umask; restrict it again.
	if err := os.Chmod(dst, 0o600); err != nil {
		return errors.Join(fmt.Errorf("copy database: restrict %s: %w", dst, err), os.Remove(dst))
	}
	return nil
}

// ReadSchemaVersion reports the highest migration version the database at
// path records, zero when it records none, without opening it as a Store, so
// it reads a database at any schema.
func ReadSchemaVersion(ctx context.Context, path string) (int, error) {
	db, err := openSchemaAgnostic(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = db.Close() }()
	var ledgers int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).
		Scan(&ledgers); err != nil {
		return 0, fmt.Errorf("schema version: %w", err)
	}
	if ledgers == 0 {
		return 0, nil
	}
	var version int
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return 0, fmt.Errorf("schema version: %w", err)
	}
	return version, nil
}

// openSchemaAgnostic opens an existing database read-only on one plain
// connection, never creating it and never checking its migrations. No
// query_only pragma: it would refuse VACUUM INTO, which writes only its
// target.
func openSchemaAgnostic(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}
