package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
)

// safeTableName is the SQLite identifier shape restorableTables enforces
// before a name is interpolated into a copy statement: table and column names
// cannot be bound as parameters, so the copy quotes the name and this guard
// proves it is a plain identifier (defense in depth; the names already come
// from the trusted schema in sqlite_master).
var safeTableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Checkpoint writes a consistent snapshot of the live database to path: a
// standalone SQLite file carrying the schema, every row, and the current
// sync_epoch/revision. This is the local-only development checkpoint plan
// §5.10 permits first; the encrypted, digest-bound BackupCheckpoint is
// deferred. VACUUM INTO refuses to overwrite an existing file, so the caller
// supplies a fresh path.
//
// The snapshot carries every row, including device credentials and pairing
// codes, and a checkpoint is a portable artifact meant to be copied for
// restore, so it is chmodded owner-only: the file must not rely on its parent
// directory's mode alone (which a copy or a later relaxed mode would drop).
// Enforcement fails closed.
func (s *Store) Checkpoint(ctx context.Context, path string) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// VACUUM cannot run inside a transaction; it takes the write lock itself
	// and produces a fully consistent copy.
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("checkpoint into %s: %w", path, err)
	}
	// VACUUM INTO honours the umask, so the file can land group/world-readable;
	// restrict it to the owner before it is handed back. The interim
	// group-readable window is not exposed: the file lives in the owner-only
	// checkpoint directory the caller validated.
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("checkpoint: restrict %s: %w", path, err)
	}
	return nil
}

// Restore replaces live state with the checkpoint at path and, in the same
// transaction, issues a fresh sync_epoch. A restore rolls the database back to
// an earlier history, so revision and every entity_version regress to their
// checkpoint values; the new epoch is what forces clients to discard caches
// built on the pre-restore world (plan §5.14, §5.10). Because revisions
// compare only within an epoch, the lower post-restore revision is never
// ambiguous against a client's higher pre-restore cursor.
//
// Rotation is not a separate step a caller can forget: it commits atomically
// with the data copy on a single exclusive connection, so the first instant
// any client can read restored data the epoch is already fresh. Returns the
// post-restore ServerState.
//
// Local-only constraint: Restore copies rows, not DDL, so the checkpoint must
// have been produced at this schema version; a mismatch fails closed rather
// than leaving data that predates the live schema.
func (s *Store) Restore(ctx context.Context, path string) (state ServerState, err error) {
	epoch, err := randomEpoch()
	if err != nil {
		return ServerState{}, err
	}

	// A single pinned connection for the whole operation: ATTACH must run
	// outside a transaction but on the same connection that later reads the
	// attached database, and MaxOpenConns(1) already serializes every other
	// reader and writer behind this one.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ServerState{}, fmt.Errorf("restore: %w", err)
	}
	defer func() { _ = conn.Close() }()

	// Connection-state cleanup (detach, re-enable foreign keys) must run even
	// if the request context is cancelled mid-restore. The store keeps a
	// single pooled connection whose session state modernc never resets, so a
	// skipped detach would leave the checkpoint attached (bricking later
	// restores) and a skipped re-enable would leave foreign keys off for every
	// later query. Bind cleanup to a non-cancellable context, and surface a
	// cleanup failure as the returned error rather than silently poisoning the
	// connection.
	cleanupCtx := context.WithoutCancel(ctx)

	// Attach read-only so the copy can never mutate the checkpoint or spill
	// -wal/-shm sidecars next to it.
	src := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro"
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS restore_src`, src); err != nil {
		return ServerState{}, fmt.Errorf("restore: attach %s: %w", path, err)
	}
	defer func() {
		if _, derr := conn.ExecContext(cleanupCtx, `DETACH DATABASE restore_src`); derr != nil && err == nil {
			err = fmt.Errorf("restore: detach: %w", derr)
		}
	}()

	if err := checkSchemaMatch(ctx, conn); err != nil {
		return ServerState{}, err
	}
	tables, err := restorableTables(ctx, conn)
	if err != nil {
		return ServerState{}, err
	}

	// Suspend foreign-key enforcement for the wholesale table-by-table copy so
	// it needs no dependency ordering; the checkpoint is an internally
	// consistent VACUUM INTO snapshot, so the restored state is consistent by
	// construction. foreign_keys is a no-op inside a transaction and must be
	// toggled here, before BEGIN, and restored (above) before the pooled
	// connection is reused.
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return ServerState{}, fmt.Errorf("restore: suspend foreign keys: %w", err)
	}
	defer func() {
		if _, ferr := conn.ExecContext(cleanupCtx, `PRAGMA foreign_keys = ON`); ferr != nil && err == nil {
			err = fmt.Errorf("restore: re-enable foreign keys: %w", ferr)
		}
	}()

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return ServerState{}, fmt.Errorf("restore: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, t := range tables {
		// t is a schema identifier validated by restorableTables and cannot be
		// a bound parameter; the quoted, guarded name is not user input.
		if _, err := tx.ExecContext(ctx, `DELETE FROM main."`+t+`"`); err != nil { //nolint:gosec // G202: t is a validated identifier from sqlite_master, not user input
			return ServerState{}, fmt.Errorf("restore: clear %s: %w", t, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO main."`+t+`" SELECT * FROM restore_src."`+t+`"`); err != nil { //nolint:gosec // G202: t is a validated identifier from sqlite_master, not user input
			return ServerState{}, fmt.Errorf("restore: copy %s: %w", t, err)
		}
	}
	// Overwrite the epoch the checkpoint carried with a fresh one, in the same
	// transaction as the data copy: this is the rotation, and it cannot be
	// separated from the restore.
	if _, err := tx.ExecContext(ctx,
		`UPDATE main.server_state SET sync_epoch = ? WHERE id = 1`, epoch); err != nil {
		return ServerState{}, fmt.Errorf("restore: rotate epoch: %w", err)
	}
	// Read the post-restore state inside the transaction: the returned value
	// then cannot diverge from what commits, and there is no fallible
	// post-commit read that could report an already-committed restore as
	// failed.
	if err := tx.QueryRowContext(ctx,
		`SELECT sync_epoch, revision FROM main.server_state WHERE id = 1`).
		Scan(&state.SyncEpoch, &state.Revision); err != nil {
		return ServerState{}, fmt.Errorf("restore: read server_state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ServerState{}, fmt.Errorf("restore: commit: %w", err)
	}
	committed = true
	return state, nil
}

// checkSchemaMatch fails closed unless the attached checkpoint was produced at
// the live schema version. Restore copies rows, not DDL, so restoring an
// older checkpoint into a newer schema (or vice versa) would leave the two out
// of sync.
func checkSchemaMatch(ctx context.Context, conn *sql.Conn) error {
	var live, ckpt int
	if err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM main.schema_migrations`).Scan(&live); err != nil {
		return fmt.Errorf("restore: read live schema version: %w", err)
	}
	if err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM restore_src.schema_migrations`).Scan(&ckpt); err != nil {
		return fmt.Errorf("restore: read checkpoint schema version: %w", err)
	}
	if live != ckpt {
		return fmt.Errorf("restore: checkpoint schema version %d does not match live version %d", ckpt, live)
	}
	return nil
}

// restorableTables lists the data tables to copy: every table except SQLite's
// internal ones and schema_migrations, which tracks applied DDL (not reverted
// by a row copy) and is pinned to match by checkSchemaMatch.
func restorableTables(ctx context.Context, conn *sql.Conn) ([]string, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT name FROM main.sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_migrations'
		 ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("restore: list tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("restore: list tables: %w", err)
		}
		if !safeTableName.MatchString(name) {
			return nil, fmt.Errorf("restore: refusing to copy table with unexpected name %q", name)
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("restore: list tables: %w", err)
	}
	return tables, nil
}
