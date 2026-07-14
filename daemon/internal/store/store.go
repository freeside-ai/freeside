package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"net/url"
	"time"

	// The pure-Go SQLite driver: keeps the daemon a single static binary
	// (plan §5.2) and CI dual-platform without cgo.
	_ "modernc.org/sqlite"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// DefaultBusyTimeout is applied when Options.BusyTimeout is zero.
const DefaultBusyTimeout = 5 * time.Second

// Options configures Open.
type Options struct {
	// BusyTimeout is the SQLite busy_timeout applied to every connection:
	// how long a locked database is retried before an operation fails.
	// Zero means DefaultBusyTimeout.
	BusyTimeout time.Duration

	// ApprovedRecipes is the set of verification-recipe digests trusted policy
	// has approved. Every write and read of an evidence-bearing artifact
	// re-derives publish_eligibility against it at the persistence boundary, so
	// a caller cannot bypass NewArtifact/NewAttentionItem to persist a forged
	// publish_eligible under an unapproved recipe (plan §5.15 rule 2, §3.1). Nil
	// means nothing is approved: the boundary fails closed. Provisional: it is
	// process-global here, to be replaced by a per-run/per-policy resolver when
	// policy resolution is wired (no such source exists yet).
	ApprovedRecipes map[domain.Digest]bool
}

// Store is the daemon's handle on its SQLite database. Open configures the
// §5.2 pragmas and applies pending migrations; see the package documentation
// for the write-path rules.
type Store struct {
	db *sql.DB
	// approvedRecipes is the boundary policy set (see Options.ApprovedRecipes),
	// snapshotted at Open and threaded into every transaction. Read-only after
	// Open, so it is safe to share across concurrent transactions.
	approvedRecipes map[domain.Digest]bool
}

// Open opens (creating if absent) the database at path, applies the §5.2
// pragmas to every connection via the DSN, and migrates the schema to head.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	db, err := openDB(path, opts)
	if err != nil {
		return nil, err
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := seedEpoch(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	// Snapshot the approved-recipe set so a caller mutating its map after Open
	// cannot change the boundary policy under a live store.
	return &Store{db: db, approvedRecipes: maps.Clone(opts.ApprovedRecipes)}, nil
}

// openDB opens the raw database handle without migrating. The pragmas ride
// the DSN because all of them except journal_mode are per-connection state:
// a PRAGMA issued through the pool would configure one connection and leave
// every later one at the defaults.
func openDB(path string, opts Options) (*sql.DB, error) {
	busyTimeout := opts.BusyTimeout
	switch {
	case busyTimeout == 0:
		busyTimeout = DefaultBusyTimeout
	case busyTimeout < 0:
		return nil, fmt.Errorf("open %s: negative BusyTimeout %v", path, busyTimeout)
	case busyTimeout < time.Millisecond:
		// busy_timeout has millisecond resolution; anything smaller would
		// truncate to 0 and silently disable waiting.
		return nil, fmt.Errorf("open %s: BusyTimeout %v is below the 1ms pragma resolution", path, busyTimeout)
	}
	q := url.Values{}
	// Writes take the write lock at BEGIN instead of on first write,
	// converting upgrade deadlocks into busy_timeout waits.
	q.Add("_txlock", "immediate")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeout.Milliseconds()))
	// The path rides a file: URI, whose parser cuts the query at the first
	// '?' and decodes percent escapes: escape '%', '?', and '#' (EscapedPath
	// keeps '/') so every legal path opens exactly that file.
	escaped := (&url.URL{Path: path}).EscapedPath()
	db, err := sql.Open("sqlite", "file:"+escaped+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection: SQLite has a single writer regardless, and
	// serializing in Go avoids SQLITE_BUSY under self-contention (see the
	// package documentation).
	db.SetMaxOpenConns(1)
	return db, nil
}

// Close closes the underlying database.
func (s *Store) Close() error {
	return s.db.Close()
}

// Pragmas reports the effective per-connection configuration, for the
// pragma acceptance fixture and a future doctor check.
type Pragmas struct {
	JournalMode string        // "wal"
	Synchronous int           // 2 is FULL
	ForeignKeys bool          // true when enforced
	BusyTimeout time.Duration // the configured retry window
}

// Pragmas reads the effective pragma values from a single connection.
func (s *Store) Pragmas(ctx context.Context) (Pragmas, error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return Pragmas{}, fmt.Errorf("pragmas: %w", err)
	}
	defer func() { _ = conn.Close() }()
	return connPragmas(ctx, conn)
}

func connPragmas(ctx context.Context, conn *sql.Conn) (Pragmas, error) {
	var (
		p              Pragmas
		foreignKeys    int
		busyTimeoutMS  int64
		singleValueRow = func(query string, dst any) error {
			return conn.QueryRowContext(ctx, query).Scan(dst)
		}
	)
	if err := singleValueRow(`PRAGMA journal_mode`, &p.JournalMode); err != nil {
		return Pragmas{}, fmt.Errorf("pragma journal_mode: %w", err)
	}
	if err := singleValueRow(`PRAGMA synchronous`, &p.Synchronous); err != nil {
		return Pragmas{}, fmt.Errorf("pragma synchronous: %w", err)
	}
	if err := singleValueRow(`PRAGMA foreign_keys`, &foreignKeys); err != nil {
		return Pragmas{}, fmt.Errorf("pragma foreign_keys: %w", err)
	}
	if err := singleValueRow(`PRAGMA busy_timeout`, &busyTimeoutMS); err != nil {
		return Pragmas{}, fmt.Errorf("pragma busy_timeout: %w", err)
	}
	p.ForeignKeys = foreignKeys == 1
	p.BusyTimeout = time.Duration(busyTimeoutMS) * time.Millisecond
	return p, nil
}
