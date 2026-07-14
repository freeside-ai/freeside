package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
	// approvedRecipes is the store's boundary policy set (see
	// Options.ApprovedRecipes), carried on every transaction so a Get can
	// re-derive an evidence artifact's publish_eligibility instead of trusting
	// the decoded row. Read-only.
	approvedRecipes map[domain.Digest]bool
}

// InternalTx is the transaction handle passed to WriteInternal callbacks:
// everything a ReadTx can do, plus the inbox/outbox queue methods. It
// deliberately exposes no Put method, so a transaction that does not bump the
// revision cannot mutate any state exposed through synchronization (§5.14):
// the Put methods live only on WriteTx, unreachable from here at compile
// time, not by convention. Only valid until the callback returns.
//
// Only non-synchronized bookkeeping (inbox, outbox, dispatch) belongs on this
// type; a write to any entity carrying as_of_revision must live on WriteTx,
// or the §5.14 guarantee is broken without a revision bump.
type InternalTx struct {
	ReadTx
}

// WriteTx is the transaction handle passed to Write callbacks: everything an
// InternalTx can do, plus the Put methods for synchronized entities. Only
// valid until the callback returns.
type WriteTx struct {
	InternalTx
	// asOfRevision is stamped into every row a Put touches: the revision the
	// enclosing Write commits as (current+1). Only Write produces a WriteTx,
	// so this is always a client-visible revision.
	asOfRevision int64
}

// Write runs fn in a client-visible write transaction and increments
// ServerState.revision exactly once at commit, regardless of how many rows
// fn touches. Everything a client may observe through sync goes through
// Write; there is no other path to a revision bump.
func (s *Store) Write(ctx context.Context, fn func(*WriteTx) error) error {
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
	wtx := &WriteTx{
		InternalTx:   InternalTx{ReadTx: ReadTx{tx: tx, approvedRecipes: s.approvedRecipes}},
		asOfRevision: current + 1,
	}
	if err := fn(wtx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE server_state SET revision = revision + 1 WHERE id = 1`); err != nil {
		return fmt.Errorf("increment revision: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// WriteInternal runs fn in a write transaction without a revision bump, for
// bookkeeping invisible to clients (inbox intake, dispatch status). Its
// callback receives an InternalTx, which exposes no Put method, so a
// non-bumping transaction cannot mutate synchronized state even by mistake.
func (s *Store) WriteInternal(ctx context.Context, fn func(*InternalTx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := fn(&InternalTx{ReadTx: ReadTx{tx: tx, approvedRecipes: s.approvedRecipes}}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
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
	if err := fn(&ReadTx{tx: tx, approvedRecipes: s.approvedRecipes}); err != nil {
		return err
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
