package exec

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// StageDriver runs stages as bounded batch jobs (plan §5.3). Every operation
// is keyed by the daemon-generated invocation id passed to Start, so the
// invocation is reconcilable across daemon restarts and provider crashes.
// Operations on an id never started return ErrUnknownInvocation.
type StageDriver interface {
	// Start commits the invocation intent and begins execution. A second
	// Start with the same id returns ErrDuplicateStart: intent is committed
	// once per id (§5.3), and a restart-after-crash reconciles via
	// Inspect/Collect, never by starting again.
	Start(ctx context.Context, id domain.InvocationID, spec StartSpec) error
	// Inspect reports the invocation's current lifecycle status.
	Inspect(ctx context.Context, id domain.InvocationID) (Status, error)
	// Stream returns a reader over the invocation's transcript so far. The
	// transcript is durably recorded (§5.3 session durability), so the
	// stream is replayable: each call reads from the beginning, and reading
	// concurrently with execution never loses committed output. The caller
	// closes it.
	Stream(ctx context.Context, id domain.InvocationID) (io.ReadCloser, error)
	// Cancel stops a non-terminal invocation and commits a canceled result,
	// so cancellation stays reconcilable like any other outcome. Canceling
	// an invocation that already committed a result is a no-op: the
	// committed result stands (at most one result per id).
	Cancel(ctx context.Context, id domain.InvocationID) error
	// Collect returns the committed terminal result. It is idempotent:
	// repeated calls re-deliver the identical result, and accepting it at
	// most once is the caller's job, not the driver's. Before a result is
	// committed it returns ErrResultNotReady; if the session was lost before
	// any result was committed it returns ErrNoResult.
	Collect(ctx context.Context, id domain.InvocationID) (StageResult, error)
}

// StartSpec is what a driver needs to run one stage attempt: everything is
// digest- or reference-addressed, so recovery is guaranteed from stage inputs,
// workspace state, and artifacts (§5.3) without consulting live policy that
// may since have moved. This is the widening the type's Phase 1 shape reserved
// for real drivers, and it is the same set of facts the durable
// domain.ExecutionAdmission records; StartSpecFromAdmission is the one
// conversion, so a replayed start carries byte-identical bindings.
type StartSpec struct {
	RunID   domain.RunID   `json:"run_id"`
	StageID domain.StageID `json:"stage_id"`
	// AttemptID names the attempt row this start belongs to, so a recovering
	// daemon rejoins a spec to its attempt without scanning the run body.
	AttemptID domain.AttemptID `json:"attempt_id"`
	// InputDigest is the logical invocation binding. StageInputs separately
	// content-addresses every materialized role that realizes that binding.
	InputDigest domain.Digest `json:"input_digest"`
	// SpecDigest and PolicyDigest are the trusted configuration this stage
	// runs under (§5.8). A driver holds no store handle, so they arrive here
	// rather than being looked up.
	SpecDigest   domain.Digest `json:"spec_digest"`
	PolicyDigest domain.Digest `json:"policy_digest"`
	// Base is the exact trusted base the workspace was seeded from; the
	// candidate head the gauntlet re-authors is bound to it (§5.6, §5.15).
	Base domain.BaseRevision `json:"base"`
	// Workspace is an opaque workspace reference; the ward lane defines its
	// shape (§5.7). Drivers pass it through, never interpret it.
	Workspace string `json:"workspace"`
	// ImageRef is the digest-pinned agent image the stage runs in.
	ImageRef domain.ImageRef `json:"image_ref"`
	// CredentialMode and EgressProfile are the containment and network
	// exposure the stage runs under (§5.4). A driver enforces them; it does
	// not choose them.
	CredentialMode domain.CredentialMode `json:"credential_mode"`
	EgressProfile  domain.EgressProfile  `json:"egress_profile"`
	// AuthIdentityID names the provider identity whose mutation lease and
	// parallelism limit govern this stage; empty for a stage that reaches no
	// provider.
	AuthIdentityID domain.AuthIdentityID `json:"auth_identity_id"`
	// AdmissionID is the content address of the admission record this start
	// was authorized by, so driver-side logs join to the audit row.
	AdmissionID domain.Digest `json:"admission_id"`
	// StageInputs freezes every content role the real driver may consume.
	// Historical walking-skeleton admissions leave it nil and cannot pass the
	// production materializer.
	StageInputs *domain.StageInputSnapshot `json:"stage_inputs,omitempty"`
}

// StartSpecFromAdmission renders the durable admission record as the spec its
// stage starts under. Recovery rebuilds a spec this way rather than from
// current policy: re-deriving would silently re-target an in-flight
// invocation whose policy has moved, which is the run-level analogue of the
// retargeting a publication intent's bound authorization exists to prevent.
func StartSpecFromAdmission(a domain.ExecutionAdmission) StartSpec {
	spec := StartSpec{
		RunID:          a.RunID,
		StageID:        a.StageID,
		AttemptID:      a.AttemptID,
		InputDigest:    a.InputDigest,
		SpecDigest:     a.SpecDigest,
		PolicyDigest:   a.PolicyDigest,
		Base:           a.Base,
		Workspace:      a.Workspace,
		ImageRef:       a.ImageRef,
		CredentialMode: a.CredentialMode,
		EgressProfile:  a.EgressProfile,
		AdmissionID:    a.ID,
		StageInputs:    cloneStageInputSnapshot(a.StageInputs),
	}
	if a.AuthIdentityID != nil {
		spec.AuthIdentityID = *a.AuthIdentityID
	}
	return spec
}

func cloneStageInputSnapshot(in *domain.StageInputSnapshot) *domain.StageInputSnapshot {
	if in == nil {
		return nil
	}
	cloned := *in
	if in.VendorInstructions != nil {
		vendor := *in.VendorInstructions
		if in.VendorInstructions.Digest != nil {
			digest := *in.VendorInstructions.Digest
			vendor.Digest = &digest
		}
		cloned.VendorInstructions = &vendor
	}
	if in.ConversationDigest != nil {
		digest := *in.ConversationDigest
		cloned.ConversationDigest = &digest
	}
	cloned.PriorArtifactDigests = slices.Clone(in.PriorArtifactDigests)
	cloned.ImageInputDigests = slices.Clone(in.ImageInputDigests)
	return &cloned
}

// StartSpec deliberately has no Validate method, unlike the other serialized
// shapes here. A complete spec is only ever produced by
// StartSpecFromAdmission, from a record whose own Validate already checked
// every one of these facts; a second validator over the copy would restate
// the domain's vocabulary rules in a package that cannot see them, and the
// two would eventually disagree. The dispatch path that starts the fake
// driver with a zero spec is unaffected either way.

// StageResult is the committed terminal outcome of a stage invocation: the
// serialized contract the store persists and the engine accepts (at most
// once, §5.3).
type StageResult struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	// Status is the terminal outcome: completed, failed, or canceled.
	Status Status `json:"status"`
	// HeadSHA is the workspace head the stage left behind, when the stage
	// produced one; empty for stages that move no head.
	HeadSHA string `json:"head_sha"`
	// Artifacts lists the content addresses of the invocation's recorded
	// outputs (transcripts, logs, produced files), §5.15.
	Artifacts []domain.Digest `json:"artifacts"`
	// Summary is the driver's short human-readable outcome description.
	Summary string `json:"summary"`
}

// Validate reports whether the result is well-formed: a result must be
// reconcilable (non-empty invocation id) and terminal. It is the
// deserialization backstop for results reconstructed from the store.
func (r StageResult) Validate() error {
	if r.InvocationID == "" {
		return fmt.Errorf("stage result invocation_id: %w", domain.ErrEmptyID)
	}
	if !r.Status.valid() {
		return fmt.Errorf("stage result status %q: %w", r.Status, ErrInvalidStatus)
	}
	if !r.Status.Terminal() {
		return fmt.Errorf("stage result status %q: %w", r.Status, ErrNonTerminalResult)
	}
	for i, d := range r.Artifacts {
		if d == "" {
			return fmt.Errorf("stage result artifacts[%d]: %w", i, domain.ErrEmptyID)
		}
	}
	return nil
}
