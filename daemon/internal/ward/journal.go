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

// ErrJournalRecordNotFound distinguishes a handoff that failed before opening
// its journal from a failure after durable handoff authority existed.
var ErrJournalRecordNotFound = errors.New("handoff journal record not found")

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
	// HandoffCanceled: daemon cancellation intent was durable before ward
	// stopped the writer and every runtime object was proven absent.
	HandoffCanceled HandoffJournalOutcome = "canceled"
	// HandoffFailed: the authenticated launcher marker carried a nonzero
	// status and every runtime object was proven absent.
	HandoffFailed HandoffJournalOutcome = "failed"
	// HandoffLoss: every runtime object was proven absent and no export was
	// released; the caller may safely rerun the stage from its durable
	// admission.
	HandoffLoss HandoffJournalOutcome = "loss"
)

// AllHandoffJournalOutcomes lists every valid outcome.
var AllHandoffJournalOutcomes = []HandoffJournalOutcome{
	HandoffCompleted, HandoffCanceled, HandoffFailed, HandoffLoss,
}

func (o HandoffJournalOutcome) valid() bool {
	switch o {
	case HandoffCompleted, HandoffCanceled, HandoffFailed, HandoffLoss:
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

// HandoffJournalState binds the three lifecycle-scoped Claude state volumes
// to the exact objects and empty/clean manifests proved before launch.
type HandoffJournalState struct {
	ConfigRootFingerprint     string `json:"config_root_fingerprint"`
	ContinuityFingerprint     string `json:"continuity_fingerprint"`
	SessionScratchFingerprint string `json:"session_scratch_fingerprint"`
	ConfigRootTarget          string `json:"config_root_target"`
	ContinuityTarget          string `json:"continuity_target"`
	SessionScratchTarget      string `json:"session_scratch_target"`
	ConfigRootReadOnly        bool   `json:"config_root_read_only"`
	ContinuityReadOnly        bool   `json:"continuity_read_only"`
	SessionScratchReadOnly    bool   `json:"session_scratch_read_only"`
	ConfigRootDigest          string `json:"config_root_digest"`
	ContinuityDigest          string `json:"continuity_digest"`
	SessionScratchDigest      string `json:"session_scratch_digest"`
}

func (s HandoffJournalState) validate() error {
	if s.ConfigRootFingerprint == "" || s.ContinuityFingerprint == "" ||
		s.SessionScratchFingerprint == "" {
		return errors.New("handoff journal state fingerprints are required")
	}
	if s.ConfigRootTarget != ClaudeConfigRootTarget ||
		s.ContinuityTarget != ClaudeContinuityTarget ||
		s.SessionScratchTarget != ClaudeSessionScratchTarget ||
		!s.ConfigRootReadOnly || s.ContinuityReadOnly || s.SessionScratchReadOnly {
		return errors.New("handoff journal state mount topology is invalid")
	}
	for _, digest := range []string{
		s.ConfigRootDigest, s.ContinuityDigest, s.SessionScratchDigest,
	} {
		if !sha256HexPattern.MatchString(digest) {
			return errors.New("handoff journal state manifest digest is invalid")
		}
	}
	return nil
}

// HandoffJournalInstructions binds the exact explicit instruction bundle to
// its composition algorithm and complete source-manifest digest.
type HandoffJournalInstructions struct {
	CompositionVersion       string `json:"composition_version"`
	HostDigest               string `json:"host_digest"`
	RepositoryManifestDigest string `json:"repository_manifest_digest"`
	BundleDigest             string `json:"bundle_digest"`
}

func (i HandoffJournalInstructions) validate() error {
	// The legacy v2 version stays acceptable so a run journalled before the
	// fencing upgrade recovers on the new binary. Recovery only validates and
	// dispositions such a binding against the runtime world; it never
	// recomposes the bundle, so the pre-fencing bytes are never re-emitted.
	if i.CompositionVersion != instructionCompositionVersionV2 &&
		i.CompositionVersion != instructionCompositionVersion {
		return errors.New("handoff journal instruction composition version is invalid")
	}
	if i.HostDigest != instructionSourceAbsent &&
		!sha256HexPattern.MatchString(i.HostDigest) {
		return errors.New("handoff journal host-instruction digest is invalid")
	}
	if !sha256HexPattern.MatchString(i.RepositoryManifestDigest) ||
		!sha256HexPattern.MatchString(i.BundleDigest) {
		return errors.New("handoff journal instruction bundle digest is invalid")
	}
	return nil
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
	// CancellationRequested is durable daemon intent written before ward
	// issues a stop. It outranks any signal-derived launcher status.
	CancellationRequested bool `json:"cancellation_requested"`
	// WriterFailureStatus is the nonzero status authenticated by the
	// journal-bound nonce marker. It is amended before cleanup can erase the
	// marker and outranks later marker absence during recovery.
	WriterFailureStatus *int `json:"writer_failure_status"`
	// State is the prepared clean launch-state topology. It is nil for
	// state-free synthetic writers and until preparation is durable.
	State *HandoffJournalState `json:"state"`
	// Instructions is the independently observed explicit bundle. It is nil
	// for state-free synthetic writers and until composition is durable.
	Instructions *HandoffJournalInstructions `json:"instructions"`
	// Lease is the held §5.4 mutation lease, nil for non-leased runs.
	Lease *HandoffJournalLease `json:"lease"`
	// ExportDir is the host directory holding the verified export, recorded
	// durably before the completed close: a crash between the two would
	// otherwise leave a closed-completed record whose delivery nobody can
	// locate — neither an export nor a rerun-safe signal. It authenticates a
	// caller-supplied released path by exact comparison, but is never itself
	// returned as a path to consume or delete.
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
	if r.WriterFailureStatus != nil {
		if *r.WriterFailureStatus < 1 || *r.WriterFailureStatus > 255 {
			return fmt.Errorf("%w: writer failure status %d is outside 1..255",
				ErrInvalidJournalRecord, *r.WriterFailureStatus)
		}
		if r.WriterComplete {
			return fmt.Errorf("%w: writer cannot be both complete and failed",
				ErrInvalidJournalRecord)
		}
	}
	if r.State != nil {
		if err := r.State.validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidJournalRecord, err)
		}
	}
	if r.Instructions != nil {
		if err := r.Instructions.validate(); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidJournalRecord, err)
		}
	}
	if r.Outcome != nil && *r.Outcome == HandoffCanceled && !r.CancellationRequested {
		return fmt.Errorf("%w: canceled record carries no cancellation intent",
			ErrInvalidJournalRecord)
	}
	if r.Outcome != nil && r.CancellationRequested && *r.Outcome != HandoffCanceled {
		return fmt.Errorf("%w: cancellation intent closed as %q",
			ErrInvalidJournalRecord, *r.Outcome)
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
	// Get reconstructs the run's current durable record; a run with no record
	// returns ErrJournalRecordNotFound. Recover reads the row through this
	// itself rather than accepting a caller-supplied copy: a stale copy's
	// WriterComplete or Outcome would be a decoded trust bit steering adoption.
	Get(ctx context.Context, runID string) (HandoffJournalRecord, error)
	// MarkSeedObserved durably records the attested pre-writer base.
	MarkSeedObserved(ctx context.Context, runID, observedBaseSHA string) error
	// MarkCredentialObserved durably records the leased credential store's
	// pre-writer digest.
	MarkCredentialObserved(ctx context.Context, runID, preDigest string) error
	// MarkStatePrepared durably records the freshly observed clean state
	// volumes before the writer container can be created.
	MarkStatePrepared(ctx context.Context, runID string, state HandoffJournalState) error
	// MarkInstructionsPrepared durably records the observed explicit bundle
	// before the writer container can be created.
	MarkInstructionsPrepared(
		ctx context.Context,
		runID string,
		instructions HandoffJournalInstructions,
	) error
	// MarkWriterComplete durably records that the writer was proven absent
	// with the egress proxy healthy throughout.
	MarkWriterComplete(ctx context.Context, runID string) error
	// MarkCancellationRequested durably records daemon cancellation before
	// teardown can stop the writer.
	MarkCancellationRequested(ctx context.Context, runID string) error
	// MarkWriterFailed durably records an authenticated nonzero launcher
	// status before cleanup removes its workspace evidence.
	MarkWriterFailed(ctx context.Context, runID string, status int) error
	// MarkExportMaterialized durably records where the verified export
	// landed, before the completed close makes the outcome terminal.
	MarkExportMaterialized(ctx context.Context, runID, exportDir string) error
	// Close durably ends the record with its outcome. Closing an already
	// closed record must fail.
	Close(ctx context.Context, runID string, outcome HandoffJournalOutcome) error
}

// AuthenticateReleasedExport binds a caller-supplied released directory to
// the exact directory the ward journal recorded before closing the handoff
// completed. The decoded journal path is only compared, never used as an I/O
// target, so a damaged row cannot steer a read or deletion.
func (b *Backend) AuthenticateReleasedExport(ctx context.Context, runID, exportDir string) error {
	if b == nil || !b.initialized {
		return fmt.Errorf("%w: backend is not initialized", ErrInvalidConfig)
	}
	if b.cfg.Journal == nil {
		return fmt.Errorf("%w: handoff journal is required to authenticate released export",
			ErrInvalidJournalRecord)
	}
	rec, err := b.cfg.Journal.Get(ctx, runID)
	if err != nil {
		return fmt.Errorf("authenticate released export for %q: %w", runID, err)
	}
	if err := rec.Validate(); err != nil {
		return fmt.Errorf("authenticate released export for %q: %w", runID, err)
	}
	if rec.RunID != runID || rec.Outcome == nil || *rec.Outcome != HandoffCompleted ||
		rec.ExportDir == "" || rec.ExportDir != exportDir {
		return fmt.Errorf(
			"%w: released export for %q does not match its closed completed journal record",
			ErrInvalidJournalRecord, runID)
	}
	return nil
}

// HandoffStarted reports whether the journal contains authentic durable
// authority for runID. It is read-only: the Claude driver uses it to
// distinguish a pre-journal refusal, which is safe to rerun, from any opened
// handoff, which only Recover may disposition.
func (b *Backend) HandoffStarted(ctx context.Context, runID string) (bool, error) {
	if b == nil || !b.initialized {
		return false, fmt.Errorf("%w: backend is not initialized", ErrInvalidConfig)
	}
	if b.cfg.Journal == nil {
		return false, fmt.Errorf("%w: handoff journal is required", ErrInvalidConfig)
	}
	rec, err := b.cfg.Journal.Get(ctx, runID)
	switch {
	case errors.Is(err, ErrJournalRecordNotFound):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("inspect handoff journal for %q: %w", runID, err)
	}
	if err := rec.Validate(); err != nil {
		return false, err
	}
	if rec.RunID != runID {
		return false, fmt.Errorf("%w: journal returned record for run %q, asked for %q",
			ErrInvalidJournalRecord, rec.RunID, runID)
	}
	return true, nil
}

// RequestCancellation durably records daemon cancellation intent. Callers
// must do this before canceling the handoff context; Handoff repeats the
// amendment on unwind before teardown as a race-closing backstop.
func (b *Backend) RequestCancellation(ctx context.Context, runID string) error {
	if b == nil || !b.initialized {
		return fmt.Errorf("%w: backend is not initialized", ErrInvalidConfig)
	}
	if b.cfg.Journal == nil {
		return fmt.Errorf("%w: handoff journal is required", ErrInvalidConfig)
	}
	if err := b.cfg.Journal.MarkCancellationRequested(ctx, runID); err != nil {
		return fmt.Errorf("request cancellation for %q: %w", runID, err)
	}
	return nil
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
	CredentialMounts []legacyCredentialMount
}

type legacyCredentialMount struct {
	Volume   string
	Target   string
	Writable bool
}

type legacyHandoffSpec struct {
	RunID           string
	WorkspaceSizeMB int64
	Seed            WorkspaceSeed
	Agent           legacyAgentSpec
	AuthStoreLease  *AuthStoreLeaseClaim
}

func legacySpecDigest(hs HandoffSpec) (string, error) {
	credentialMounts := make([]legacyCredentialMount, len(hs.Agent.CredentialMounts))
	for i, mount := range hs.Agent.CredentialMounts {
		credentialMounts[i] = legacyCredentialMount{
			Volume: mount.Volume, Target: mount.Target, Writable: mount.Writable,
		}
	}
	legacy := legacyHandoffSpec{
		RunID:           hs.RunID,
		WorkspaceSizeMB: hs.WorkspaceSizeMB,
		Seed:            hs.Seed,
		Agent: legacyAgentSpec{
			Image:            hs.Agent.Image,
			Command:          hs.Agent.Command,
			Env:              hs.Agent.Env,
			EgressProfile:    hs.Agent.EgressProfile,
			CredentialMounts: credentialMounts,
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
