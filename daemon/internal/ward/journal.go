package ward

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrInvalidJournalRecord is the class sentinel for a journal record recovery
// refuses at the reconstruction boundary; it is a caller/record error, not a
// conformance failure, and it never authorizes a destructive action.
var ErrInvalidJournalRecord = errors.New("invalid handoff journal record")

// The handoff journal is what makes a handoff recoverable across a daemon
// restart (plan §5.7: "an unpersisted proof is not a proof"; the exec
// StageDriver contract requires reconcilability). It deliberately records
// almost no progress: after a restart the only trustworthy account of how far
// a handoff got is the runtime world itself, classified by the persisted
// ownership token through the same evidence rules teardown uses — persisted
// per-stage progress bits would be decoded trust bits recovery would be
// tempted to believe instead of re-observing. What the journal does persist
// is the identity recovery cannot reconstruct (the run, its unpredictable
// ownership token, the spec digest it must be resumed with, the held lease)
// and the two proofs that are unreconstructible after the fact:
//
//   - the observed base: a pre-writer fact; the agent may legitimately move
//     HEAD, so it can never be re-attested (and the leased credential
//     store's pre-writer digest, for the same reason);
//   - writer-complete: check 3's absence proof plus the egress proxy's
//     process-local health check — the evidence that egress stayed enforced
//     for the writer's whole life dies with the daemon, so an unmarked
//     record can never be adopted, only torn down.

// HandoffJournalOutcome is how a journalled handoff ended. The zero value ""
// is invalid by design; an open record carries no outcome (a nil pointer),
// never an empty one.
type HandoffJournalOutcome string

const (
	// HandoffCompleted: the handoff (or its recovery) released a verified
	// export.
	HandoffCompleted HandoffJournalOutcome = "completed"
	// HandoffLoss: every runtime object was proven absent and no export was
	// released; the caller may safely rerun the stage from its durable
	// admission.
	HandoffLoss HandoffJournalOutcome = "loss"
)

// AllHandoffJournalOutcomes lists every valid outcome.
var AllHandoffJournalOutcomes = []HandoffJournalOutcome{HandoffCompleted, HandoffLoss}

func (o HandoffJournalOutcome) valid() bool {
	switch o {
	case HandoffCompleted, HandoffLoss:
		return true
	default:
		return false
	}
}

// HandoffJournalLease is the persisted reference to the §5.4 mutation lease a
// handoff holds: exactly what recovery needs to release it (the store
// re-verifies holder and fence on release, so nothing here is a trusted
// authorization). The acquired window (AcquiredAt, ExpiresAt) is part of the
// reference: identity, holder, and even fence-plus-record-ordering can be
// carried by a damaged row pointing at a later same-holder window, so
// recovery binds the record to its window by exact equality against the
// live store row rather than trusting any decoded ordering claim. A row
// forging the later window's complete tuple is an adversarial journal, which
// is outside the damage class this defends (the journal is daemon-owned).
type HandoffJournalLease struct {
	AuthIdentityID domain.AuthIdentityID `json:"auth_identity_id"`
	Holder         domain.InvocationID   `json:"holder"`
	Fence          int64                 `json:"fence"`
	AcquiredAt     time.Time             `json:"acquired_at"`
	ExpiresAt      time.Time             `json:"expires_at"`
}

// HandoffJournalRecord is one handoff's durable record. Begin, or
// LeasedHandoffOpener.BeginLeased for a leased run, writes it before the first
// runtime object exists (intent-before-create: a run that cannot be
// journalled is refused, so no object can outlive the daemon unrecorded); the
// amendments below add the unreconstructible proofs as they are earned; Close
// ends it.
//
// A record read back after a restart is a trust boundary: recovery re-gates
// every field's shape (validate), re-derives the spec digest from the
// caller's re-supplied spec, and treats the runtime world — not this record —
// as the account of progress.
type HandoffJournalRecord struct {
	RunID string `json:"run_id"`
	// OwnershipToken is the run's unpredictable freeside.handoff-owner label
	// value: after a restart it is the only evidence that a runtime object
	// is this run's, so an unpersisted token would leave every object
	// unprovable.
	OwnershipToken string `json:"ownership_token"`
	// SpecDigest binds the record to the exact HandoffSpec the run started
	// with. Recovery takes the spec from the caller's durable admission and
	// refuses a digest mismatch, so a diverged or tampered record cannot
	// resume under a different spec.
	SpecDigest string `json:"spec_digest"`
	// ObservedBaseSHA is the seeded base the pre-writer observer attested;
	// empty until MarkSeedObserved, and permanently empty for a blank seed.
	ObservedBaseSHA string `json:"observed_base_sha"`
	// CredentialPreDigest is the leased credential store's pre-writer
	// digest; empty until MarkCredentialObserved and for non-leased runs.
	CredentialPreDigest string `json:"credential_pre_digest"`
	// WriterComplete records that the writer was proven absent AND the
	// egress proxy was healthy for its whole life. Only a record carrying it
	// may be adopted to completion; without it the egress proof died with
	// the daemon and recovery must tear down.
	WriterComplete bool `json:"writer_complete"`
	// Lease is the held §5.4 mutation lease, nil for non-leased runs.
	Lease *HandoffJournalLease `json:"lease"`
	// ExportDir is the host directory holding the verified export, recorded
	// durably before the completed close: a crash between the two would
	// otherwise leave a closed-completed record whose delivery nobody can
	// locate — neither an export nor a rerun-safe signal. It is diagnostic
	// state for the caller, never an input to recovery's decisions (a
	// decoded path must not steer anything, least of all a deletion).
	ExportDir string `json:"export_dir"`
	// Outcome is nil while the record is open; a closed record refuses
	// recovery (the handoff already ended, in the recorded way).
	Outcome  *HandoffJournalOutcome `json:"outcome"`
	OpenedAt time.Time              `json:"opened_at"`
}

// ownershipTokenPattern is the exact shape newOwnershipLabel mints: 16 random
// bytes, lowercase hex.
var ownershipTokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Validate re-gates a record's shape at the reconstruction boundary. It
// checks shapes only — whether the record's claims hold is decided against
// the re-supplied spec (digest) and the live runtime world (everything
// else), never by believing the record.
func (r HandoffJournalRecord) Validate() error {
	if !runIDPattern.MatchString(r.RunID) {
		return fmt.Errorf("%w: journal record run id does not match %s", ErrInvalidJournalRecord, runIDPattern)
	}
	if !ownershipTokenPattern.MatchString(r.OwnershipToken) {
		return fmt.Errorf("%w: journal record ownership token is not a 16-byte lowercase hex value", ErrInvalidJournalRecord)
	}
	if !sha256HexPattern.MatchString(r.SpecDigest) {
		return fmt.Errorf("%w: journal record spec digest is not a sha256 hex value", ErrInvalidJournalRecord)
	}
	if r.ObservedBaseSHA != "" && !commitSHAPattern.MatchString(r.ObservedBaseSHA) {
		return fmt.Errorf("%w: journal record observed base is not a full lowercase commit SHA", ErrInvalidJournalRecord)
	}
	if r.CredentialPreDigest != "" && !sha256HexPattern.MatchString(r.CredentialPreDigest) {
		return fmt.Errorf("%w: journal record credential pre-digest is not a sha256 hex value", ErrInvalidJournalRecord)
	}
	if r.Lease != nil {
		if r.Lease.AuthIdentityID == "" || r.Lease.Holder == "" {
			return fmt.Errorf("%w: journal record lease does not name an identity and holder", ErrInvalidJournalRecord)
		}
		if r.Lease.Fence < 1 {
			return fmt.Errorf("%w: journal record lease fence %d is not positive", ErrInvalidJournalRecord, r.Lease.Fence)
		}
		if r.Lease.AcquiredAt.IsZero() || r.Lease.ExpiresAt.IsZero() {
			return fmt.Errorf("%w: journal record lease window is missing", ErrInvalidJournalRecord)
		}
		if !r.Lease.ExpiresAt.After(r.Lease.AcquiredAt) {
			return fmt.Errorf("%w: journal record lease window expires at %s, not after its acquisition %s",
				ErrInvalidJournalRecord, r.Lease.ExpiresAt, r.Lease.AcquiredAt)
		}
	}
	if r.ExportDir != "" && !filepath.IsAbs(r.ExportDir) {
		return fmt.Errorf("%w: journal record export dir is not an absolute path", ErrInvalidJournalRecord)
	}
	if r.Outcome != nil && !r.Outcome.valid() {
		return fmt.Errorf("%w: journal record outcome %q is not one of %v", ErrInvalidJournalRecord, *r.Outcome, AllHandoffJournalOutcomes)
	}
	if r.OpenedAt.IsZero() {
		return fmt.Errorf("%w: journal record opened_at is missing", ErrInvalidJournalRecord)
	}
	return nil
}

// HandoffJournal is ward's seam to durable handoff records (the
// ConformanceRecorder posture: ward declares the interface it needs; the
// store-backed implementation is wired by the daemon). Every method must be
// durable before it returns — a journal write that only buffered is exactly
// the unpersisted proof §5.7 rejects — and every error fails the handoff
// closed.
type HandoffJournal interface {
	// Begin durably opens an unleased record, before any runtime object
	// exists. A leased run uses LeasedHandoffOpener.BeginLeased instead.
	// Opening a run id whose record is already open or closed must fail: one
	// record per run, ever, is what makes double recovery refusable.
	Begin(ctx context.Context, rec HandoffJournalRecord) error
	// Get reconstructs the run's current durable record; a run with no
	// record errors. Recover reads the row through this itself rather than
	// accepting a caller-supplied copy: a stale copy's WriterComplete or
	// Outcome would be a decoded trust bit steering adoption.
	Get(ctx context.Context, runID string) (HandoffJournalRecord, error)
	// MarkSeedObserved durably records the attested pre-writer base.
	MarkSeedObserved(ctx context.Context, runID, observedBaseSHA string) error
	// MarkCredentialObserved durably records the leased credential store's
	// pre-writer digest.
	MarkCredentialObserved(ctx context.Context, runID, preDigest string) error
	// MarkWriterComplete durably records that the writer was proven absent
	// with the egress proxy healthy throughout.
	MarkWriterComplete(ctx context.Context, runID string) error
	// MarkExportMaterialized durably records where the verified export
	// landed, before the completed close makes the outcome terminal.
	MarkExportMaterialized(ctx context.Context, runID, exportDir string) error
	// Close durably ends the record with its outcome. Closing an already
	// closed record must fail.
	Close(ctx context.Context, runID string, outcome HandoffJournalOutcome) error
}

// LeasedHandoffOpener is the additional production transaction seam for a
// leased journalled handoff. It atomically acquires the mutation lease and
// opens rec with that exact lease reference. Returning success means both are
// durable; returning an error means neither is. This closes the crash window a
// separate AuthStoreLeaser.Acquire followed by HandoffJournal.Begin creates.
//
// Handoff requires its configured Journal to implement this interface for a
// leased run. Journalless one-shot test/development operation keeps the
// standalone AuthStoreLeaser path.
type LeasedHandoffOpener interface {
	BeginLeased(
		ctx context.Context,
		rec HandoffJournalRecord,
		claim AuthStoreLeaseClaim,
		now, expiresAt time.Time,
	) (domain.AuthStoreMutationLease, error)
}

// specDigest canonically digests the frozen HandoffSpec so a journal record
// can bind to the exact request its run started with. JSON over the struct's
// fixed field order is deterministic for this vocabulary (no maps).
func specDigest(hs HandoffSpec) (string, error) {
	data, err := json.Marshal(hs)
	if err != nil {
		return "", fmt.Errorf("digest handoff spec: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// legacyAgentSpec and legacyHandoffSpec reproduce the exact exported-field
// shape and order used before vendor instructions and invocation policy joined
// AgentSpec. Open records have no explicit schema version, so recovery tests
// the current digest first and this historical projection second.
type legacyAgentSpec struct {
	Image            string
	Command          []string
	Env              []string
	EgressProfile    domain.EgressProfile
	CredentialMounts []CredentialMount
}

type legacyHandoffSpec struct {
	RunID           string
	WorkspaceSizeMB int64
	Seed            WorkspaceSeed
	Agent           legacyAgentSpec
	AuthStoreLease  *AuthStoreLeaseClaim
}

func legacySpecDigest(hs HandoffSpec) (string, error) {
	legacy := legacyHandoffSpec{
		RunID:           hs.RunID,
		WorkspaceSizeMB: hs.WorkspaceSizeMB,
		Seed:            hs.Seed,
		Agent: legacyAgentSpec{
			Image:            hs.Agent.Image,
			Command:          hs.Agent.Command,
			Env:              hs.Agent.Env,
			EgressProfile:    hs.Agent.EgressProfile,
			CredentialMounts: hs.Agent.CredentialMounts,
		},
		AuthStoreLease: hs.AuthStoreLease,
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		return "", fmt.Errorf("digest legacy handoff spec: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
