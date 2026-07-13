package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
)

// ServerState is the §5.14 sync anchor: every client-visible write
// transaction increments Revision; a restore issues a new SyncEpoch, forcing
// clients to discard their caches. Revision is monotonic across epochs
// (never reset), so a cursor can never be ambiguous between epochs.
type ServerState struct {
	SyncEpoch string
	Revision  int64
}

// ReadTx is the read-only transaction handle passed to Read callbacks: it
// carries the Get methods and no write capability, so a Read cannot mutate
// state (and in particular cannot dodge the revision bump) even by mistake.
// Only valid until the callback returns.
type ReadTx struct {
	tx *sql.Tx
}

// WriteTx is the transaction handle passed to Write and WriteInternal
// callbacks: everything a ReadTx can do, plus the Put and queue methods.
// Only valid until the callback returns.
type WriteTx struct {
	ReadTx
	// asOfRevision is stamped into every row a Put touches: the revision
	// the enclosing Write commits as (current+1), or the current revision
	// inside WriteInternal.
	asOfRevision int64
}

// Write runs fn in a client-visible write transaction and increments
// ServerState.revision exactly once at commit, regardless of how many rows
// fn touches. Everything a client may observe through sync goes through
// Write; there is no other path to a revision bump.
func (s *Store) Write(ctx context.Context, fn func(*WriteTx) error) error {
	return s.transact(ctx, true, fn)
}

// WriteInternal runs fn in a write transaction without a revision bump, for
// bookkeeping invisible to clients (inbox intake, dispatch status). Using it
// for client-visible state would silently break sync invalidation; when in
// doubt, use Write.
func (s *Store) WriteInternal(ctx context.Context, fn func(*WriteTx) error) error {
	return s.transact(ctx, false, fn)
}

// Read runs fn in a read-only transaction: a consistent snapshot for
// multi-entity reads. Read-only is enforced by the ReadTx type, which
// exposes no write methods. (The transaction still takes the immediate
// write lock via the DSN's _txlock; splitting reader and writer connection
// configurations is deliberately deferred until a read pool exists.)
func (s *Store) Read(ctx context.Context, fn func(*ReadTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(&ReadTx{tx: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *Store) transact(ctx context.Context, clientVisible bool, fn func(*WriteTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current int64
	if err := tx.QueryRowContext(ctx,
		`SELECT revision FROM server_state WHERE id = 1`).Scan(&current); err != nil {
		return fmt.Errorf("read server_state: %w", err)
	}
	asOf := current
	if clientVisible {
		asOf = current + 1
	}
	if err := fn(&WriteTx{ReadTx: ReadTx{tx: tx}, asOfRevision: asOf}); err != nil {
		return err
	}
	if clientVisible {
		if _, err := tx.ExecContext(ctx,
			`UPDATE server_state SET revision = revision + 1 WHERE id = 1`); err != nil {
			return fmt.Errorf("increment revision: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ServerState reads the current sync epoch and revision inside the
// transaction.
func (tx *ReadTx) ServerState(ctx context.Context) (ServerState, error) {
	var state ServerState
	err := tx.tx.QueryRowContext(ctx,
		`SELECT sync_epoch, revision FROM server_state WHERE id = 1`).
		Scan(&state.SyncEpoch, &state.Revision)
	if err != nil {
		return ServerState{}, fmt.Errorf("read server_state: %w", err)
	}
	return state, nil
}

// ServerState reads the current sync epoch and revision.
func (s *Store) ServerState(ctx context.Context) (ServerState, error) {
	var state ServerState
	err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		state, err = tx.ServerState(ctx)
		return err
	})
	return state, err
}

// NewEpoch issues a fresh sync epoch (§5.14 restore path): clients discard
// their caches and bootstrap. The revision is deliberately not bumped; the
// epoch change itself invalidates every cursor.
func (s *Store) NewEpoch(ctx context.Context) (ServerState, error) {
	epoch, err := randomEpoch()
	if err != nil {
		return ServerState{}, err
	}
	if _, err := s.db.ExecContext(ctx,
		`UPDATE server_state SET sync_epoch = ? WHERE id = 1`, epoch); err != nil {
		return ServerState{}, fmt.Errorf("set sync_epoch: %w", err)
	}
	return s.ServerState(ctx)
}

// seedEpoch assigns the first epoch to a database whose migration seed left
// sync_epoch empty. Idempotent: an established epoch is never overwritten.
func seedEpoch(ctx context.Context, db *sql.DB) error {
	epoch, err := randomEpoch()
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE server_state SET sync_epoch = ? WHERE id = 1 AND sync_epoch = ''`, epoch); err != nil {
		return fmt.Errorf("seed sync_epoch: %w", err)
	}
	return nil
}

func randomEpoch() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate epoch: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
