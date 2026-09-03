package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// safeTableName is the SQLite identifier shape restorableTables enforces
// before a name is interpolated into a copy statement: table and column names
// cannot be bound as parameters, so the copy quotes the name and this guard
// proves it is a plain identifier (defense in depth; the names already come
// from the trusted schema in sqlite_master).
var safeTableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

const outboxPublicationInsertTrigger = "outbox_publication_intent_requires_current_insert"

const (
	usageObservationsDeleteTrigger      = "usage_observations_append_only_delete"
	decisionActionSurfacesDeleteTrigger = "decision_action_surfaces_append_only_delete"
	comprehensionEventsDeleteTrigger    = "comprehension_events_append_only_delete"
	comprehensionDefectsDeleteTrigger   = "comprehension_defects_append_only_delete"
)

const canonicalOutboxPublicationInsertTriggerSQL = `CREATE TRIGGER outbox_publication_intent_requires_current_insert
BEFORE INSERT ON outbox
WHEN NEW.kind = 'publish.publication' AND NEW.payload_version != 2
BEGIN
    SELECT RAISE(ABORT, 'new publication intents require current payload version');
END`

const canonicalUsageObservationsDeleteTriggerSQL = `CREATE TRIGGER usage_observations_append_only_delete
BEFORE DELETE ON usage_observations
BEGIN
    SELECT RAISE(ABORT, 'usage observations are append-only');
END`

const canonicalDecisionActionSurfacesDeleteTriggerSQL = `CREATE TRIGGER decision_action_surfaces_append_only_delete
BEFORE DELETE ON decision_action_surfaces
BEGIN
    SELECT RAISE(ABORT, 'decision action surfaces are append-only');
END`

const canonicalComprehensionEventsDeleteTriggerSQL = `CREATE TRIGGER comprehension_events_append_only_delete
BEFORE DELETE ON comprehension_events
BEGIN
    SELECT RAISE(ABORT, 'comprehension events are append-only');
END`

const canonicalComprehensionDefectsDeleteTriggerSQL = `CREATE TRIGGER comprehension_defects_append_only_delete
BEFORE DELETE ON comprehension_defects
BEGIN
    SELECT RAISE(ABORT, 'comprehension defects are append-only');
END`

// deleteGuard names an append-only BEFORE DELETE trigger a restore must lift
// around its wholesale delete+reinsert and canonically reinstate before commit.
type deleteGuard struct {
	name         string
	canonicalSQL string
}

// appendOnlyDeleteGuards is every append-only delete trigger a restore must
// suspend. A restore issues DELETE FROM against every table, so a table whose
// BEFORE DELETE trigger aborts the delete blocks the whole restore once it
// holds any row. Each new append-only table (migration 0065 added three) must
// register its guard here, or a restore fails the moment that table is
// non-empty.
var appendOnlyDeleteGuards = []deleteGuard{
	{usageObservationsDeleteTrigger, canonicalUsageObservationsDeleteTriggerSQL},
	{decisionActionSurfacesDeleteTrigger, canonicalDecisionActionSurfacesDeleteTriggerSQL},
	{comprehensionEventsDeleteTrigger, canonicalComprehensionEventsDeleteTriggerSQL},
	{comprehensionDefectsDeleteTrigger, canonicalComprehensionDefectsDeleteTriggerSQL},
}

// Checkpoint writes a consistent snapshot of the live database to path: a
// standalone SQLite file carrying the schema, every row, and the current
// sync_epoch/revision. This legacy plaintext primitive remains available for
// explicit local tooling; the production backup producer uses an in-memory
// online backup and encrypts its serialization. VACUUM INTO refuses to
// overwrite an existing file, so the caller supplies a fresh path.
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
	src := "file:" + (&url.URL{Path: path}).EscapedPath() + "?mode=ro"
	return s.restoreFromSource(ctx, src)
}

// restoreFromSource performs the legacy plaintext Restore against a SQLite
// URI. Encrypted checkpoints use restoreFromDatabase instead.
func (s *Store) restoreFromSource(
	ctx context.Context, src string,
) (state ServerState, err error) {
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
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS restore_src`, src); err != nil {
		return ServerState{}, fmt.Errorf("restore: attach source: %w", err)
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
	publicationInsertTriggerSQL, err := suspendPublicationInsertTrigger(ctx, tx)
	if err != nil {
		return ServerState{}, err
	}
	suspendedDeleteGuards, err := suspendDeleteGuards(ctx, tx)
	if err != nil {
		return ServerState{}, err
	}

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
	if err := restorePublicationInsertTrigger(ctx, tx, publicationInsertTriggerSQL); err != nil {
		return ServerState{}, err
	}
	if err := restoreDeleteGuards(ctx, tx, suspendedDeleteGuards); err != nil {
		return ServerState{}, err
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

// restoreFromDatabase is the encrypted-checkpoint restore path. It copies from
// a deserialized in-memory source connection, so authenticated plaintext never
// needs a filesystem path. The target transaction and epoch rotation retain
// the same atomicity as restoreFromSource.
func (s *Store) restoreFromDatabase(
	ctx context.Context, source *sql.Conn,
) (state ServerState, err error) {
	epoch, err := randomEpoch()
	if err != nil {
		return ServerState{}, err
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ServerState{}, fmt.Errorf("restore: %w", err)
	}
	defer func() { _ = conn.Close() }()
	cleanupCtx := context.WithoutCancel(ctx)

	var liveSchema, checkpointSchema int
	if err := conn.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).
		Scan(&liveSchema); err != nil {
		return ServerState{}, fmt.Errorf("restore: read live schema version: %w", err)
	}
	if err := source.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).
		Scan(&checkpointSchema); err != nil {
		return ServerState{}, fmt.Errorf("restore: read checkpoint schema version: %w", err)
	}
	if liveSchema != checkpointSchema {
		return ServerState{}, fmt.Errorf(
			"restore: checkpoint schema version %d does not match live version %d",
			checkpointSchema, liveSchema)
	}
	tables, err := restorableTablesFromDatabase(ctx, source)
	if err != nil {
		return ServerState{}, err
	}

	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return ServerState{}, fmt.Errorf("restore: suspend foreign keys: %w", err)
	}
	defer func() {
		if _, foreignKeyErr := conn.ExecContext(
			cleanupCtx, `PRAGMA foreign_keys = ON`,
		); foreignKeyErr != nil && err == nil {
			err = fmt.Errorf("restore: re-enable foreign keys: %w", foreignKeyErr)
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
	publicationInsertTriggerSQL, err := suspendPublicationInsertTrigger(ctx, tx)
	if err != nil {
		return ServerState{}, err
	}
	suspendedDeleteGuards, err := suspendDeleteGuards(ctx, tx)
	if err != nil {
		return ServerState{}, err
	}
	for _, table := range tables {
		if _, err := tx.ExecContext(ctx, `DELETE FROM "`+table+`"`); err != nil { //nolint:gosec // validated schema identifier
			return ServerState{}, fmt.Errorf("restore: clear %s: %w", table, err)
		}
		if err := copyRestoredTable(ctx, source, tx, table); err != nil {
			return ServerState{}, err
		}
	}
	if err := restorePublicationInsertTrigger(ctx, tx, publicationInsertTriggerSQL); err != nil {
		return ServerState{}, err
	}
	if err := restoreDeleteGuards(ctx, tx, suspendedDeleteGuards); err != nil {
		return ServerState{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE server_state SET sync_epoch = ? WHERE id = 1`, epoch); err != nil {
		return ServerState{}, fmt.Errorf("restore: rotate epoch: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT sync_epoch, revision FROM server_state WHERE id = 1`).
		Scan(&state.SyncEpoch, &state.Revision); err != nil {
		return ServerState{}, fmt.Errorf("restore: read server_state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ServerState{}, fmt.Errorf("restore: commit: %w", err)
	}
	committed = true
	return state, nil
}

// suspendPublicationInsertTrigger opens the one narrow exception to the
// migration-provenance insert guard: Restore copies a complete, same-schema
// checkpoint inside an exclusive transaction. The trigger is recreated before
// commit; any intervening failure rolls the DROP back with the copied rows.
func suspendPublicationInsertTrigger(ctx context.Context, tx *sql.Tx) (string, error) {
	var definition string
	if err := tx.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`,
		outboxPublicationInsertTrigger,
	).Scan(&definition); err != nil {
		return "", fmt.Errorf("restore: read publication insert guard: %w", err)
	}
	if definition != canonicalOutboxPublicationInsertTriggerSQL {
		return "", fmt.Errorf("restore: publication insert guard definition is not canonical")
	}
	if _, err := tx.ExecContext(ctx, `DROP TRIGGER "`+outboxPublicationInsertTrigger+`"`); err != nil {
		return "", fmt.Errorf("restore: suspend publication insert guard: %w", err)
	}
	return definition, nil
}

func restorePublicationInsertTrigger(ctx context.Context, tx *sql.Tx, definition string) error {
	if _, err := tx.ExecContext(ctx, definition); err != nil {
		return fmt.Errorf("restore: reinstate publication insert guard: %w", err)
	}
	return nil
}

// suspendDeleteGuards opens the restore-only exception to every append-only
// delete guard: it verifies each trigger's definition is canonical, drops it,
// and returns the guards it suspended for restoreDeleteGuards to reinstate
// before commit. The complete same-schema copy is atomic, and a rollback
// restores both the dropped triggers and the pre-restore rows. A drifted
// definition fails closed rather than silently reinstating an unexpected
// trigger.
func suspendDeleteGuards(ctx context.Context, tx *sql.Tx) ([]deleteGuard, error) {
	suspended := make([]deleteGuard, 0, len(appendOnlyDeleteGuards))
	for _, guard := range appendOnlyDeleteGuards {
		var definition string
		if err := tx.QueryRowContext(ctx,
			`SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?`,
			guard.name,
		).Scan(&definition); err != nil {
			return nil, fmt.Errorf("restore: read %s guard: %w", guard.name, err)
		}
		if definition != guard.canonicalSQL {
			return nil, fmt.Errorf("restore: %s guard definition is not canonical", guard.name)
		}
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER "`+guard.name+`"`); err != nil {
			return nil, fmt.Errorf("restore: suspend %s guard: %w", guard.name, err)
		}
		suspended = append(suspended, guard)
	}
	return suspended, nil
}

func restoreDeleteGuards(ctx context.Context, tx *sql.Tx, guards []deleteGuard) error {
	for _, guard := range guards {
		if _, err := tx.ExecContext(ctx, guard.canonicalSQL); err != nil {
			return fmt.Errorf("restore: reinstate %s guard: %w", guard.name, err)
		}
	}
	return nil
}

func restorableTablesFromDatabase(ctx context.Context, source *sql.Conn) ([]string, error) {
	rows, err := source.QueryContext(ctx,
		`SELECT name FROM sqlite_master
		 WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name != 'schema_migrations'
		 ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("restore: list checkpoint tables: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("restore: list checkpoint tables: %w", err)
		}
		if !safeTableName.MatchString(name) {
			return nil, fmt.Errorf("restore: refusing to copy table with unexpected name %q", name)
		}
		tables = append(tables, name)
	}
	return tables, rows.Err()
}

func copyRestoredTable(
	ctx context.Context, source *sql.Conn, target *sql.Tx, table string,
) error {
	rows, err := source.QueryContext(ctx, `SELECT * FROM "`+table+`"`) //nolint:gosec // validated schema identifier
	if err != nil {
		return fmt.Errorf("restore: read %s: %w", table, err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		return fmt.Errorf("restore: read %s columns: %w", table, err)
	}
	if len(columns) == 0 {
		return fmt.Errorf("restore: table %s has no columns", table)
	}
	insert := `INSERT INTO "` + table + `" VALUES (` + //nolint:gosec // validated schema identifier
		strings.TrimSuffix(strings.Repeat("?,", len(columns)), ",") + `)`
	for rows.Next() {
		values := make([]any, len(columns))
		destinations := make([]any, len(columns))
		for i := range values {
			destinations[i] = &values[i]
		}
		if err := rows.Scan(destinations...); err != nil {
			return fmt.Errorf("restore: read %s row: %w", table, err)
		}
		if _, err := target.ExecContext(ctx, insert, values...); err != nil {
			return fmt.Errorf("restore: copy %s row: %w", table, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("restore: read %s rows: %w", table, err)
	}
	return nil
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
