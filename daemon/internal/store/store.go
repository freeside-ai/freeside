package store

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"net/url"
	"slices"
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

	// AdmissionFloors is the minimum runner capability class current policy
	// requires of each operating mode (plan §5.7). Every write and read of an
	// execution admission re-checks the recorded spawn-time snapshot against
	// it, so a class admitted under a weaker floor stops reading as admissible
	// once policy raises it. A missing entry admits nothing: an unconfigured
	// floor is not an empty floor. Provisional and process-global for the same
	// reason ApprovedRecipes is.
	AdmissionFloors map[domain.OperatingMode]domain.CapabilitySnapshot

	// ApprovedCredentialModes is the set of credential containments policy has
	// approved for unattended running (§5.7). Empty approves nothing, so an
	// unattended admission fails closed; attended_dev is not held to it.
	ApprovedCredentialModes []domain.CredentialMode

	// BackupEncryptionWaiverRepositoryID is retained only for reconstructing
	// legacy waiver-posture notices in tests and migrations. Production
	// configuration rejects every non-nil value before Open, and the write
	// boundary rejects new waiver-bearing admissions.
	BackupEncryptionWaiverRepositoryID *int64

	// BackupHealthSource evaluates the four backup-health dimensions §5.7
	// requires of unattended running. Nil admits nothing unattended. The
	// source is queried on every admission write and reconstruction rather
	// than snapshotted at Open, so stale or unencrypted evidence closes the
	// gate for already-recorded work too.
	BackupHealthSource BackupHealthSource

	// VerificationFloorRegistryGeneration is the current tighten-only §6
	// floor and waiver-registry generation. Zero selects the compiled current
	// generation; an older persisted resolution never becomes current by
	// omission.
	VerificationFloorRegistryGeneration uint64

	// WaiverGrantApprovals is the daemon-owned authority registry for degraded
	// verification waivers. A waiver's self-described authority and digest are
	// insufficient: the store accepts it only when this current registry grants
	// that exact pair.
	WaiverGrantApprovals map[domain.WaiverGrantingAuthority]map[domain.Digest]bool

	// TrustedRequirementSets registers additional daemon-owned requirement
	// definitions by requirement-set digest. The compiled production set is
	// always registered under its resolved-generation digest; a resolution
	// naming any unregistered set, or disagreeing with its registered
	// definition's policy-bearing fields, fails closed at write and read.
	TrustedRequirementSets map[domain.Digest][]domain.RequirementDefinition
}

// Store is the daemon's handle on its SQLite database. Open configures the
// §5.2 pragmas and applies pending migrations; see the package documentation
// for the write-path rules.
type Store struct {
	db *sql.DB
	// readyItemCreated is a lossy, process-local wake-up for consumers whose
	// next background pass must not wait for their idle cadence after a new
	// ready-for-final-review item commits. Durable reconciliation remains the
	// source of truth, so one pending wake is sufficient.
	readyItemCreated chan struct{}
	// approvedRecipes is the boundary policy set (see Options.ApprovedRecipes),
	// snapshotted at Open and threaded into every transaction. Read-only after
	// Open, so it is safe to share across concurrent transactions.
	approvedRecipes map[domain.Digest]bool
	// admissionPolicy is the execution-admission half of the same boundary
	// policy (see Options.AdmissionFloors), snapshotted the same way.
	admissionPolicy domain.AdmissionPolicy
	// backupHealthSource is live operational evidence, not policy or part of an
	// admission's identity. It is queried at each admission trust boundary.
	backupHealthSource                  BackupHealthSource
	verificationFloorRegistryGeneration uint64
	waiverGrantApprovals                map[domain.WaiverGrantingAuthority]map[domain.Digest]bool
	requirementSets                     map[domain.Digest]map[domain.RequirementKey]domain.RequirementDefinition
}

// Open opens (creating if absent) the database at path, applies the §5.2
// pragmas to every connection via the DSN, and migrates the schema to head.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	verificationGeneration, err := resolveVerificationFloorRegistryGeneration(
		opts.VerificationFloorRegistryGeneration,
		domain.CurrentVerificationFloorRegistryGeneration,
	)
	if err != nil {
		return nil, err
	}
	requirementSets, err := resolveTrustedRequirementSets(verificationGeneration, opts.TrustedRequirementSets)
	if err != nil {
		return nil, err
	}
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
	// Snapshot the boundary policy so a caller mutating its maps or slices
	// after Open cannot change it under a live store.
	return &Store{
		db:                                  db,
		readyItemCreated:                    make(chan struct{}, 1),
		approvedRecipes:                     maps.Clone(opts.ApprovedRecipes),
		admissionPolicy:                     cloneAdmissionPolicy(opts),
		backupHealthSource:                  opts.BackupHealthSource,
		verificationFloorRegistryGeneration: verificationGeneration,
		waiverGrantApprovals:                cloneWaiverGrantApprovals(opts.WaiverGrantApprovals),
		requirementSets:                     requirementSets,
	}, nil
}

// resolveTrustedRequirementSets builds the daemon-owned requirement registry:
// the compiled production set under its resolved-generation digest, plus any
// configured sets. The production entry always wins its own digest, so
// configuration cannot redefine the compiled set.
func resolveTrustedRequirementSets(
	generation uint64,
	configured map[domain.Digest][]domain.RequirementDefinition,
) (map[domain.Digest]map[domain.RequirementKey]domain.RequirementDefinition, error) {
	out := make(map[domain.Digest]map[domain.RequirementKey]domain.RequirementDefinition, len(configured)+1)
	for setDigest, definitions := range configured {
		out[setDigest] = requirementDefinitionsByKey(definitions)
	}
	productionDigest, err := domain.ProductionRequirementSetDigest(generation)
	if err != nil {
		return nil, err
	}
	out[productionDigest] = requirementDefinitionsByKey(domain.ProductionRequirementDefinitions())
	return out, nil
}

func requirementDefinitionsByKey(definitions []domain.RequirementDefinition) map[domain.RequirementKey]domain.RequirementDefinition {
	byKey := make(map[domain.RequirementKey]domain.RequirementDefinition, len(definitions))
	for _, definition := range definitions {
		byKey[definition.Key] = definition
	}
	return byKey
}

func cloneWaiverGrantApprovals(in map[domain.WaiverGrantingAuthority]map[domain.Digest]bool) map[domain.WaiverGrantingAuthority]map[domain.Digest]bool {
	out := make(map[domain.WaiverGrantingAuthority]map[domain.Digest]bool, len(in))
	for authority, grants := range in {
		out[authority] = maps.Clone(grants)
	}
	return out
}

func resolveVerificationFloorRegistryGeneration(configured, compiled uint64) (uint64, error) {
	if configured == 0 {
		return compiled, nil
	}
	if configured < compiled {
		return 0, fmt.Errorf("verification floor registry generation: %w", domain.ErrVerificationFloorRegressed)
	}
	return configured, nil
}

// ReadyItemCreated reports that a new open ready-for-final-review item
// committed in this process. It is intentionally best-effort: consumers must
// still reconcile from durable state after restart or a missed wake.
func (s *Store) ReadyItemCreated() <-chan struct{} {
	return s.readyItemCreated
}

// VerificationFloorRegistryGeneration returns the current tighten-only
// verification registry generation selected when the store opened.
func (s *Store) VerificationFloorRegistryGeneration() uint64 {
	return s.verificationFloorRegistryGeneration
}

// cloneAdmissionPolicy detaches the admission policy from the caller's
// options: the floors map, each floor slice, and the historical waiver
// pointer used only when reconstructing pre-encryption audit records.
func cloneAdmissionPolicy(opts Options) domain.AdmissionPolicy {
	policy := domain.AdmissionPolicy{}
	if opts.AdmissionFloors != nil {
		policy.Floors = make(map[domain.OperatingMode]domain.CapabilitySnapshot, len(opts.AdmissionFloors))
		for mode, floor := range opts.AdmissionFloors {
			policy.Floors[mode] = floor.Clone()
		}
	}
	policy.ApprovedCredentialModes = slices.Clone(opts.ApprovedCredentialModes)
	if opts.BackupEncryptionWaiverRepositoryID != nil {
		waiver := *opts.BackupEncryptionWaiverRepositoryID
		policy.BackupEncryptionWaiverRepositoryID = &waiver
	}
	return policy
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
