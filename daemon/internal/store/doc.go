// Package store is the daemon's durable storage layer: SQLite opened with the
// plan §5.2 pragmas (WAL, synchronous=FULL, foreign_keys=ON, configured
// busy_timeout), an embedded transactional migrations mechanism
// (daemon/migrations), the persisted form of the domain types, inbox/outbox
// tables with idempotency keys (§5.9), and the ServerState sync counters
// (§5.14). It is spine shared-contract territory; schema changes land only
// through kind:contract work units. Storage only: no sync endpoints and no
// engine live here.
//
// # Conventions
//
// These patterns are set here for every later lane to copy (recorded for
// spine review in the Wave 0 store devlog entry):
//
//   - Pragmas ride the DSN, never a db.Exec: with a database/sql pool every
//     pragma except journal_mode is per-connection, so only the driver's
//     _pragma= DSN parameters guarantee each new connection is configured.
//   - Domain entities persist as aggregate-root rows: identity and join keys
//     as real columns (so foreign keys actually enforce), entity_version and
//     as_of_revision for §5.14 sync, and the domain type's canonical JSON as
//     the body. Children stay embedded in their root's body (a Run carries
//     its Stages; a Conversation its Messages), matching Phase 1
//     whole-snapshot sync. Extracting a field into a column later is an
//     ordinary migration (json_extract backfill), not an API change.
//   - Every write happens inside Write or WriteInternal, whose callbacks
//     receive a WriteTx. Write is for client-visible transactions and
//     increments ServerState.revision exactly once at commit; WriteInternal
//     is for bookkeeping invisible to clients (inbox intake, dispatch
//     status). Choosing Write is what makes a change visible to sync; there
//     is no third path. Read callbacks receive a ReadTx, which exposes no
//     write methods, so mutating outside a write path (and dodging the
//     revision bump) does not compile.
//   - The pool is a single connection: SQLite has one writer regardless, and
//     serializing in Go avoids SQLITE_BUSY under self-contention;
//     busy_timeout still guards cross-process access. Widening later (a
//     read pool) is internal to this package, with one recorded caveat: the
//     DSN's _txlock=immediate makes every transaction, Read included, take
//     the write lock at BEGIN, so a read pool must use a separate reader
//     configuration (deferred _txlock) rather than reuse this DSN.
//   - Reads that must be consistent run inside Read; Puts validate before
//     writing and Gets validate after reading, so a corrupt row fails loudly
//     at the boundary instead of leaking an invalid value into the daemon.
package store
