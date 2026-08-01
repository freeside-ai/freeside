package claude

import (
	"context"
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ErrSeedRetryable marks an operational exact-base failure that leaves
// phaseSeeding recoverable. A Seeder returns unmarked errors for definitive
// refusals that retrying cannot change.
var ErrSeedRetryable = errors.New("claude driver seed is retryable")

// ErrSeedRefused marks a definitive exact-base or credential refusal.
// It outranks concurrent context cancellation so a permanent verdict cannot
// be converted into indefinitely retryable work during shutdown.
var ErrSeedRefused = errors.New("claude driver seed was refused")

// Gate is the ward workspace-handoff gate (production: *ward.Backend). The
// driver never talks to a container runtime directly: every containment
// property §5.4 and §5.7 require is the gate's, and a driver-side shortcut
// would be a second, unaudited path to the same effects.
type Gate interface {
	Handoff(ctx context.Context, hs ward.HandoffSpec) (*ward.HandoffResult, error)
	HandoffStarted(ctx context.Context, runID string) (bool, error)
	RequestCancellation(ctx context.Context, runID string) error
	Recover(ctx context.Context, runID string, hs ward.HandoffSpec) (*ward.RecoveryResult, error)
	AuthenticateReleasedExport(ctx context.Context, runID, exportDir string) error
}

// Seeder materializes daemon-owned checkouts at exactly base (production:
// publish.Transport, whose results carry the canonical repository-name and
// numeric-ID binding ward refuses to seed without). dir must not exist yet.
//
// The two methods differ in one load-bearing way. FetchBaseWorktree carries
// the working-tree files and is what a workspace seed requires: ward's
// observer proves the raw worktree against HEAD, so a repository-only
// directory is dirty (every tracked path missing) and the run is refused
// before the writer starts. FetchBase omits them for the import lane, which
// applies an export over the checkout and must not inherit files nobody put
// there.
type Seeder interface {
	FetchBase(ctx context.Context, repo, baseRef, baseSHA, dir string) error
	FetchBaseWorktree(ctx context.Context, repo, baseRef, baseSHA, dir string) error
}

// ExportRecorder persists the write-once ExecutionExport and reads it back.
// store.RecordExecutionExport lives on the store's internal transaction, so
// the composition supplies this adapter rather than the driver holding a
// store handle.
type ExportRecorder interface {
	// RecordExecutionExport commits the export and its directory-free replay
	// as one durable decision. Implementations must converge an identical
	// retry and reject a differing replay for an existing invocation.
	RecordExecutionExport(
		ctx context.Context,
		export domain.ExecutionExport,
		replay ExecutionReplay,
	) error
	// LookupExecutionExport distinguishes confirmed absence from a read or
	// reconstruction error. Recovery may declare a run lost only on absence.
	LookupExecutionExport(
		ctx context.Context, id domain.InvocationID,
	) (domain.ExecutionExport, bool, error)
	LookupExecutionExportRecord(
		ctx context.Context, id domain.InvocationID,
	) (domain.ExecutionExport, bool, error)
}

// OutcomeRecorder is the trusted durable authority for a non-export terminal
// outcome. A private intent file can replay one only when this port returns
// the same write-once record.
type OutcomeRecorder interface {
	RecordExecutionOutcome(context.Context, domain.ExecutionOutcome) error
	LookupExecutionOutcome(
		context.Context, domain.InvocationID,
	) (domain.ExecutionOutcome, bool, error)
	LookupExecutionOutcomeRecord(
		context.Context, domain.InvocationID,
	) (domain.ExecutionOutcome, bool, error)
}

// AdmissionAuthority authenticates a reconstructed record against its durable
// admission, requires current conformance before work can start or recover,
// and derives the exact import policy bound to that admission's trust profile.
// A private driver-state file is not an authority for any of those decisions.
type AdmissionAuthority interface {
	AuthenticateAdmission(context.Context, domain.InvocationID, exec.StartSpec) error
	AuthenticateStart(context.Context, domain.InvocationID, exec.StartSpec) error
	ImportOptions(context.Context, domain.InvocationID, exec.StartSpec, importer.Options) (importer.Options, error)
	// ImportOptionsRecord reconstructs the exact policy of an already
	// completed import from immutable admission and resolved-policy records.
	// Mutable daemon configuration may block new work but cannot retarget or
	// strand terminal replay before the publisher applies its current gates.
	ImportOptionsRecord(context.Context, domain.InvocationID, exec.StartSpec, importer.Options) (importer.Options, error)
}

// Artifacts persists the evidence bytes an export released, and records the
// agent claims that name them. The gate hands the export back in a directory
// the driver owns and deletes, so anything a terminal result names must be
// copied into durable storage first: a result naming artifacts no store can
// resolve is an audit trail with no evidence behind it.
type Artifacts interface {
	// PutBlob stores one evidence blob under its content address.
	PutBlob(ctx context.Context, digest domain.Digest, body []byte) error
	// RecordClaims persists the §5.15 agent claims for one invocation,
	// binding each stored blob to its label and provenance.
	RecordClaims(ctx context.Context, id domain.InvocationID, claims []domain.AgentClaim) error
}

// AuthStoreVolumes resolves the trusted identity-to-credential-volume
// binding (production: wardstore.Leaser, whose AuthStoreVolume reads the
// durable domain.AuthIdentity row). The driver asks rather than deriving a
// volume name: the binding is trusted state, and the gate re-checks the
// answer against the same row before creating anything.
type AuthStoreVolumes interface {
	AuthStoreVolume(ctx context.Context, id domain.AuthIdentityID) (string, error)
}
