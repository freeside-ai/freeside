package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// admissionEncodingVersion tags the canonical serialization
// ExecutionAdmission.ComputeID digests. A change to the encoding is a change
// to every admission's identity, so it is versioned explicitly rather than
// left implicit in the struct's field order.
const (
	admissionEncodingVersion              = "freeside.execution.admission/v1"
	admissionInputEncodingVersion         = "freeside.execution.admission/v2"
	admissionBackendConfigEncodingVersion = "freeside.execution.admission/v3"
	admissionAgentEncodingVersion         = "freeside.execution.admission/v4"
)

// digestPinnedImage binds an image reference to one full lowercase sha256
// digest, not a tag or a merely digest-shaped prefix.
var digestPinnedImage = regexp.MustCompile(`^[^\s@]+@sha256:[0-9a-f]{64}$`)

// ImageRef is a digest-pinned OCI image reference. The pin is the contract: a
// tag is a moving target, so a record naming one would attest to an image
// nobody can identify afterwards.
type ImageRef string

// Validate reports whether the reference is digest-pinned.
func (r ImageRef) Validate() error {
	if r == "" {
		return fmt.Errorf("image ref: %w", ErrEmptyField)
	}
	if !digestPinnedImage.MatchString(string(r)) {
		return fmt.Errorf("image ref %q: %w", r, ErrImageNotDigestPinned)
	}
	return nil
}

// BaseRevision is the exact trusted base a stage ran against: the repository,
// the ref the base was resolved from, and the commit that resolution landed
// on. The vocabulary matches the publication intent's (plan §5.9 exact-base
// binding), so the base a run was admitted under and the base publication
// authors onto are stated in the same terms and can be compared.
type BaseRevision struct {
	Repo string `json:"repo"`
	// RepositoryID is the forge's canonical numeric identity for Repo. A name
	// can be transferred or reused, so a policy decision keyed on the name
	// alone (the §5.7 waiver, most sharply) can silently follow the name onto
	// a different repository; #261 established the same canonical binding for
	// mint audits. The store re-checks it against the repository's trusted
	// profile, so the pair cannot be self-asserted.
	RepositoryID int64  `json:"repository_id"`
	BaseRef      string `json:"base_ref"`
	BaseSHA      string `json:"base_sha"`
}

// Validate reports whether the base revision is well-formed. The resolved
// commit is required: a ref alone names whatever the ref points at now, which
// is not a base a later audit can reconstruct.
func (b BaseRevision) Validate() error {
	if b.Repo == "" {
		return fmt.Errorf("base revision repo: %w", ErrEmptyField)
	}
	if b.RepositoryID <= 0 {
		return fmt.Errorf("base revision repository_id %d: %w", b.RepositoryID, ErrNonPositive)
	}
	if b.BaseRef == "" {
		return fmt.Errorf("base revision base_ref: %w", ErrEmptyField)
	}
	if b.BaseSHA == "" {
		return fmt.Errorf("base revision base_sha: %w", ErrEmptyField)
	}
	return nil
}

// BackupEncryptionWaiver records that an admission ran under the plan §5.7
// Phase 1A.2 exception, naming the exact trusted numeric repository ID the
// operator waived the encryption dimension for. §5.7 requires every admission
// under the waiver to record it; the store re-gates the recorded value against
// the operator's current configuration, so a record cannot claim a waiver the
// running daemon does not hold.
type BackupEncryptionWaiver struct {
	RepositoryID int64  `json:"repository_id"`
	Reason       string `json:"reason"`
}

// Validate reports whether the waiver record is well-formed.
func (w BackupEncryptionWaiver) Validate() error {
	if w.RepositoryID <= 0 {
		return fmt.Errorf("backup encryption waiver repository_id %d: %w", w.RepositoryID, ErrNonPositive)
	}
	if w.Reason == "" {
		return fmt.Errorf("backup encryption waiver reason: %w", ErrEmptyField)
	}
	return nil
}

// ExecutionAdmission is the durable, write-once record of what admitted one
// stage attempt: the capability class the backend declared at spawn (§5.3
// capabilities are fixed at spawn), the credential containment and egress
// exposure the stage runs under (§5.4), the digest-pinned agent image, the
// digests the run is bound to, and the exact base and workspace it started
// from. It is the record #39 deferred the admitted snapshot to and the record
// a restart reconstructs a stage's bindings from, rather than re-deriving them
// from policy that may since have moved.
//
// It is written once, at the moment of admission, and never refined: an
// attempt whose admission exists but whose ExecutionExport does not is
// in flight, which is exactly the distinction restart recovery needs.
//
// ID is exported so the record serializes, but it is a content address
// computed in NewExecutionAdmission and never taken from caller input;
// Validate recomputes it, so a decoded row with a forged field fails closed at
// every boundary that re-runs it.
type ExecutionAdmission struct {
	ID           Digest       `json:"id"`
	InvocationID InvocationID `json:"invocation_id"`
	RunID        RunID        `json:"run_id"`
	StageID      StageID      `json:"stage_id"`
	AttemptID    AttemptID    `json:"attempt_id"`
	// Backend and Capabilities are the exec.Admission snapshot: the admitting
	// backend's name and the capability set it declared at that decision, not
	// its live declaration now.
	Backend      string             `json:"backend"`
	Capabilities CapabilitySnapshot `json:"capabilities"`
	// BackendConfigurationDigest binds an unattended admission to the exact
	// normalized backend configuration whose latest durable conformance proof
	// authorizes it. An attended admission may omit it because §5.7 permits
	// that mode to run on an unproven backend class.
	BackendConfigurationDigest Digest         `json:"backend_configuration_digest,omitempty"`
	OperatingMode              OperatingMode  `json:"operating_mode"`
	CredentialMode             CredentialMode `json:"credential_mode"`
	EgressProfile              EgressProfile  `json:"egress_profile"`
	ImageRef                   ImageRef       `json:"image_ref"`
	SpecDigest                 Digest         `json:"spec_digest"`
	PolicyDigest               Digest         `json:"policy_digest"`
	InputDigest                Digest         `json:"input_digest"`
	// StageInputs is nil on historical pre-materialization admissions. A real
	// Phase 1A driver requires it: the snapshot binds every content role to
	// the digest the materializer must verify before process start.
	StageInputs *StageInputSnapshot `json:"stage_inputs,omitempty"`
	Base        BaseRevision        `json:"base"`
	// Workspace is an opaque workspace reference; the ward lane defines its
	// shape (§5.7). It is recorded, never interpreted here.
	Workspace string `json:"workspace"`
	// AuthIdentityID names the provider identity whose auth-store mutation
	// lease and parallelism limit govern this stage (§5.4). It is nil exactly
	// for a clean-verification stage, which reaches no provider at all.
	AuthIdentityID *AuthIdentityID `json:"auth_identity_id"`
	// TrustProfileDigest names the exact approved trust-profile revision this
	// admission was granted under, for the modes that require one. The
	// repository id alone cannot stand in for it: activating a revised profile
	// for the same repository keeps that id, so without the digest a run
	// admitted under a retired revision would keep passing after the operator
	// replaced it. Nil exactly when no profile is required.
	TrustProfileDigest *Digest `json:"trust_profile_digest"`
	// BackupEncryptionWaiver is nil in the ordinary case; a non-nil value is
	// re-gated against the operator's configured waiver at reconstruction.
	BackupEncryptionWaiver *BackupEncryptionWaiver `json:"backup_encryption_waiver"`
	// AgentBinding is the §5.4 admitted-agent snapshot (admission step 5):
	// present exactly on an admission whose selection went through an
	// admitted agent, and its presence selects the v4 encoding. Nil on every
	// admission written before the #867 cutover. The permanent legacy rule:
	// an admission carrying an AuthIdentityID but no agent binding keeps its
	// admitted identity and credential mode and is never resolved against a
	// current agent, however the tree has moved since.
	AgentBinding *AdmissionAgentBinding `json:"agent_binding,omitempty"`
	AdmittedAt   time.Time              `json:"admitted_at"`
}

// AdmissionAgentBinding is what §5.4 admission step 5 snapshots beyond the
// existing fields: the exact agent, launch, lineup revision, enrollment
// generation, store bytes, and egress the attempt was admitted with. The
// existing fields (identity, credential mode, endpoints, instruction
// delivery) become derivations of these; the ones the pure closure expresses
// are rechecked at reconstruction (ValidateAdmissionAgentDerivations), while
// the image's build-to-image binding is the #867 resolver's, since no
// closure fragment carries an image reference. This value is the authority
// the derivations flow from.
type AdmissionAgentBinding struct {
	AgentDigest     Digest `json:"agent_digest"`
	LaunchDigest    Digest `json:"launch_digest"`
	TreatmentDigest Digest `json:"treatment_digest"`
	PricingRevision string `json:"pricing_revision"`
	// LineupRevision is the control-plane revision whose lineup (or recorded
	// alternate-agent card) named the agent digest for the role.
	LineupRevision       Digest             `json:"lineup_revision"`
	EnrollmentID         ClientEnrollmentID `json:"enrollment_id"`
	EnrollmentGeneration int                `json:"enrollment_generation"`
	StoreManifestDigest  Digest             `json:"store_manifest_digest"`
	// EffectiveEgress is the canonical (sorted, deduplicated) allowlist of
	// authorities the writer's proxy admitted for this attempt.
	EffectiveEgress []string `json:"effective_egress"`
	// Attended records whether the attempt ran attended (§5.4: a new
	// agent × launch pair runs attended until the operator marks it). It must
	// agree with the admission's operating mode; carrying both keeps the
	// snapshot self-contained while the cross-check refuses drift.
	Attended bool `json:"attended"`
}

// Validate reports whether the binding is well-formed.
func (b AdmissionAgentBinding) Validate() error {
	for field, digest := range map[string]Digest{
		"agent_digest": b.AgentDigest, "launch_digest": b.LaunchDigest,
		"treatment_digest": b.TreatmentDigest,
		"lineup_revision":  b.LineupRevision, "store_manifest_digest": b.StoreManifestDigest,
	} {
		if !contentaddr.Valid(string(digest)) {
			return fmt.Errorf("admission agent binding %s %q: %w", field, digest, ErrInvalidDigest)
		}
	}
	if b.PricingRevision == "" {
		return fmt.Errorf("admission agent binding pricing_revision: %w", ErrEmptyField)
	}
	if b.EnrollmentID == "" {
		return fmt.Errorf("admission agent binding enrollment_id: %w", ErrEmptyID)
	}
	if b.EnrollmentGeneration < 1 {
		return fmt.Errorf("admission agent binding enrollment_generation %d: %w",
			b.EnrollmentGeneration, ErrNonPositive)
	}
	if len(b.EffectiveEgress) == 0 {
		return fmt.Errorf("admission agent binding effective_egress: %w", ErrEmptyField)
	}
	for i, authority := range b.EffectiveEgress {
		if authority == "" {
			return fmt.Errorf("admission agent binding effective_egress: %w", ErrEmptyField)
		}
		if i > 0 && b.EffectiveEgress[i-1] >= authority {
			return fmt.Errorf("admission agent binding effective_egress at %q: %w",
				authority, ErrKeysNotCanonical)
		}
	}
	return nil
}

// canonicalAgentBinding detaches the binding from the caller's pointer and
// canonicalizes the egress allowlist (sorted, deduplicated), so one admitted
// configuration has exactly one byte form under the content address.
func canonicalAgentBinding(in *AdmissionAgentBinding) *AdmissionAgentBinding {
	if in == nil {
		return nil
	}
	cloned := *in
	if len(in.EffectiveEgress) > 0 {
		egress := slices.Clone(in.EffectiveEgress)
		slices.Sort(egress)
		cloned.EffectiveEgress = slices.Compact(egress)
	}
	return &cloned
}

// ExecutionAdmissionInput carries the caller-supplied fields of an
// ExecutionAdmission. It has no ID field: the identity is a content address,
// so no input path can set it.
type ExecutionAdmissionInput struct {
	InvocationID               InvocationID
	RunID                      RunID
	StageID                    StageID
	AttemptID                  AttemptID
	Backend                    string
	Capabilities               CapabilitySnapshot
	BackendConfigurationDigest Digest
	OperatingMode              OperatingMode
	CredentialMode             CredentialMode
	EgressProfile              EgressProfile
	ImageRef                   ImageRef
	SpecDigest                 Digest
	PolicyDigest               Digest
	InputDigest                Digest
	StageInputs                *StageInputSnapshot
	Base                       BaseRevision
	Workspace                  string
	AuthIdentityID             *AuthIdentityID
	TrustProfileDigest         *Digest
	BackupEncryptionWaiver     *BackupEncryptionWaiver
	AgentBinding               *AdmissionAgentBinding
	AdmittedAt                 time.Time
}

// NewExecutionAdmission builds a validated admission in canonical byte-form:
// the capability snapshot is canonicalized and detached from the caller's
// slice, optional pointers are detached, the timestamp is normalized to UTC,
// and the ID is computed last from the bound facts. Canonical form is what
// makes a retried admission of the same facts converge on the stored body
// instead of colliding under a false immutable conflict (the #33 lesson).
func NewExecutionAdmission(in ExecutionAdmissionInput) (ExecutionAdmission, error) {
	if in.BackendConfigurationDigest == UnboundBackendConfigurationDigest ||
		(in.OperatingMode == ModeUnattended && in.BackendConfigurationDigest == "") {
		return ExecutionAdmission{}, fmt.Errorf(
			"new execution admission %s under mode %q backend configuration: %w",
			in.InvocationID, in.OperatingMode, ErrConformanceConfigurationUnbound)
	}
	a := ExecutionAdmission{
		InvocationID:               in.InvocationID,
		RunID:                      in.RunID,
		StageID:                    in.StageID,
		AttemptID:                  in.AttemptID,
		Backend:                    in.Backend,
		Capabilities:               NewCapabilitySnapshot(in.Capabilities...),
		BackendConfigurationDigest: in.BackendConfigurationDigest,
		OperatingMode:              in.OperatingMode,
		CredentialMode:             in.CredentialMode,
		EgressProfile:              in.EgressProfile,
		ImageRef:                   in.ImageRef,
		SpecDigest:                 in.SpecDigest,
		PolicyDigest:               in.PolicyDigest,
		InputDigest:                in.InputDigest,
		StageInputs:                cloneStageInputSnapshot(in.StageInputs),
		Base:                       in.Base,
		Workspace:                  in.Workspace,
		AuthIdentityID:             clonePtr(in.AuthIdentityID),
		TrustProfileDigest:         clonePtr(in.TrustProfileDigest),
		BackupEncryptionWaiver:     clonePtr(in.BackupEncryptionWaiver),
		AgentBinding:               canonicalAgentBinding(in.AgentBinding),
		AdmittedAt:                 in.AdmittedAt.UTC(),
	}
	id, err := a.ComputeID()
	if err != nil {
		return ExecutionAdmission{}, err
	}
	a.ID = id
	if err := a.Validate(); err != nil {
		return ExecutionAdmission{}, err
	}
	return a, nil
}

// canonicalAdmission is the versioned serialization ComputeID digests: every
// bound fact and nothing derived (ID is this value). It is a distinct type so
// adding a field to the record without deciding whether it belongs in the
// identity is a compile-visible choice, not an accident.
type canonicalAdmission struct {
	Version                    string                  `json:"version"`
	InvocationID               InvocationID            `json:"invocation_id"`
	RunID                      RunID                   `json:"run_id"`
	StageID                    StageID                 `json:"stage_id"`
	AttemptID                  AttemptID               `json:"attempt_id"`
	Backend                    string                  `json:"backend"`
	Capabilities               CapabilitySnapshot      `json:"capabilities"`
	BackendConfigurationDigest Digest                  `json:"backend_configuration_digest,omitempty"`
	OperatingMode              OperatingMode           `json:"operating_mode"`
	CredentialMode             CredentialMode          `json:"credential_mode"`
	EgressProfile              EgressProfile           `json:"egress_profile"`
	ImageRef                   ImageRef                `json:"image_ref"`
	SpecDigest                 Digest                  `json:"spec_digest"`
	PolicyDigest               Digest                  `json:"policy_digest"`
	InputDigest                Digest                  `json:"input_digest"`
	StageInputs                *StageInputSnapshot     `json:"stage_inputs,omitempty"`
	Base                       BaseRevision            `json:"base"`
	Workspace                  string                  `json:"workspace"`
	AuthIdentityID             *AuthIdentityID         `json:"auth_identity_id"`
	TrustProfileDigest         *Digest                 `json:"trust_profile_digest"`
	BackupEncryptionWaiver     *BackupEncryptionWaiver `json:"backup_encryption_waiver"`
	AgentBinding               *AdmissionAgentBinding  `json:"agent_binding,omitempty"`
	AdmittedAt                 time.Time               `json:"admitted_at"`
}

// ComputeID returns the content address of the admission: a sha256 over its
// versioned canonical serialization. It canonicalizes the capability snapshot
// defensively so it is a true content address for any input; a value that also
// passes Validate is already canonical.
func (a ExecutionAdmission) ComputeID() (Digest, error) {
	version := admissionEncodingVersion
	var stageInputs *StageInputSnapshot
	if a.StageInputs != nil {
		version = admissionInputEncodingVersion
		cloned := a.StageInputs.clone()
		stageInputs = &cloned
	}
	if a.BackendConfigurationDigest != "" {
		version = admissionBackendConfigEncodingVersion
	}
	if a.AgentBinding != nil {
		version = admissionAgentEncodingVersion
	}
	body, err := json.Marshal(canonicalAdmission{
		Version:                    version,
		InvocationID:               a.InvocationID,
		RunID:                      a.RunID,
		StageID:                    a.StageID,
		AttemptID:                  a.AttemptID,
		Backend:                    a.Backend,
		Capabilities:               NewCapabilitySnapshot(a.Capabilities...),
		BackendConfigurationDigest: a.BackendConfigurationDigest,
		OperatingMode:              a.OperatingMode,
		CredentialMode:             a.CredentialMode,
		EgressProfile:              a.EgressProfile,
		ImageRef:                   a.ImageRef,
		SpecDigest:                 a.SpecDigest,
		PolicyDigest:               a.PolicyDigest,
		InputDigest:                a.InputDigest,
		StageInputs:                stageInputs,
		Base:                       a.Base,
		Workspace:                  a.Workspace,
		AuthIdentityID:             a.AuthIdentityID,
		TrustProfileDigest:         a.TrustProfileDigest,
		BackupEncryptionWaiver:     a.BackupEncryptionWaiver,
		AgentBinding:               canonicalAgentBinding(a.AgentBinding),
		AdmittedAt:                 a.AdmittedAt,
	})
	if err != nil {
		return "", fmt.Errorf("execution admission id: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the admission is well-formed and its identity
// authentic. It is the reconstruction backstop the store's decode re-runs on
// every read: a row whose body was edited resolves to a different content
// address and fails here. It deliberately checks structure and identity only;
// whether the recorded class still satisfies current policy is AdmittedUnder's
// question, because that answer depends on state this value does not carry.
func (a ExecutionAdmission) Validate() error {
	if a.InvocationID == "" {
		return fmt.Errorf("execution admission invocation_id: %w", ErrEmptyID)
	}
	if a.RunID == "" {
		return fmt.Errorf("execution admission %s run_id: %w", a.InvocationID, ErrEmptyID)
	}
	if a.StageID == "" {
		return fmt.Errorf("execution admission %s stage_id: %w", a.InvocationID, ErrEmptyID)
	}
	if a.AttemptID == "" {
		return fmt.Errorf("execution admission %s attempt_id: %w", a.InvocationID, ErrEmptyID)
	}
	if a.Backend == "" {
		return fmt.Errorf("execution admission %s backend: %w", a.InvocationID, ErrEmptyField)
	}
	if err := a.Capabilities.Validate(); err != nil {
		return fmt.Errorf("execution admission %s: %w", a.InvocationID, err)
	}
	if a.BackendConfigurationDigest != "" &&
		!contentaddr.Valid(string(a.BackendConfigurationDigest)) {
		return fmt.Errorf("execution admission %s backend_configuration_digest %q: %w",
			a.InvocationID, a.BackendConfigurationDigest, ErrConformanceConfigurationUnbound)
	}
	if a.BackendConfigurationDigest == UnboundBackendConfigurationDigest {
		return fmt.Errorf("execution admission %s backend configuration: %w",
			a.InvocationID, ErrConformanceConfigurationUnbound)
	}
	if !a.OperatingMode.valid() {
		return fmt.Errorf("execution admission %s operating_mode %q: %w",
			a.InvocationID, a.OperatingMode, ErrInvalidOperatingMode)
	}
	if !a.CredentialMode.valid() {
		return fmt.Errorf("execution admission %s credential_mode %q: %w",
			a.InvocationID, a.CredentialMode, ErrInvalidCredentialMode)
	}
	if !a.EgressProfile.valid() {
		return fmt.Errorf("execution admission %s egress_profile %q: %w",
			a.InvocationID, a.EgressProfile, ErrInvalidEgressProfile)
	}
	if err := a.ImageRef.Validate(); err != nil {
		return fmt.Errorf("execution admission %s: %w", a.InvocationID, err)
	}
	if a.SpecDigest == "" {
		return fmt.Errorf("execution admission %s spec_digest: %w", a.InvocationID, ErrEmptyField)
	}
	if a.PolicyDigest == "" {
		return fmt.Errorf("execution admission %s policy_digest: %w", a.InvocationID, ErrEmptyField)
	}
	if a.InputDigest == "" {
		return fmt.Errorf("execution admission %s input_digest: %w", a.InvocationID, ErrEmptyField)
	}
	if a.StageInputs != nil {
		if err := a.StageInputs.Validate(); err != nil {
			return fmt.Errorf("execution admission %s: %w", a.InvocationID, err)
		}
		if a.StageInputs.InputDigest != a.InputDigest ||
			a.StageInputs.SpecificationDigest != a.SpecDigest ||
			a.StageInputs.PolicyDigest != a.PolicyDigest {
			return fmt.Errorf("execution admission %s stage input bindings disagree: %w",
				a.InvocationID, ErrParentKeyMismatch)
		}
	}
	if err := a.Base.Validate(); err != nil {
		return fmt.Errorf("execution admission %s: %w", a.InvocationID, err)
	}
	if a.Workspace == "" {
		return fmt.Errorf("execution admission %s workspace: %w", a.InvocationID, ErrEmptyField)
	}
	if err := a.validateAuthIdentity(); err != nil {
		return err
	}
	if err := a.validateTrustBinding(); err != nil {
		return err
	}
	if a.AgentBinding != nil {
		if err := a.AgentBinding.Validate(); err != nil {
			return fmt.Errorf("execution admission %s: %w", a.InvocationID, err)
		}
		// The binding's attended flag and the admission's operating mode are
		// two spellings of one fact; drift between them would let a record
		// claim an attended first run while dispatching unattended.
		if a.AgentBinding.Attended == (a.OperatingMode == ModeUnattended) {
			return fmt.Errorf("execution admission %s attended flag disagrees with mode %q: %w",
				a.InvocationID, a.OperatingMode, ErrAdmissionInconsistent)
		}
		// An agent-admitted attempt always runs under some provider identity;
		// a clean-verification stage never resolves an agent.
		if a.AuthIdentityID == nil {
			return fmt.Errorf("execution admission %s carries an agent binding with no identity: %w",
				a.InvocationID, ErrAuthIdentityInconsistent)
		}
		// Unlike the historical nil shapes the legacy reader preserves, a v4
		// admission is newly written and derives its instruction delivery
		// from the adapter (§5.4 admission step 5): an agent-bound record
		// with no explicit vendor-instruction snapshot has lost the
		// provenance of what the attempt received, so it is an incoherent
		// record, not history.
		if a.StageInputs == nil || a.StageInputs.VendorInstructions == nil ||
			a.StageInputs.VendorInstructions.Delivery == "" {
			return fmt.Errorf(
				"execution admission %s carries an agent binding without an explicit vendor-instruction snapshot: %w",
				a.InvocationID, ErrAdmissionInconsistent)
		}
	}
	if a.AdmittedAt.IsZero() {
		return fmt.Errorf("execution admission %s admitted_at: %w", a.InvocationID, ErrMissingTimestamp)
	}
	// admitted_at is part of the canonical encoding the id addresses: the
	// constructor normalizes to UTC and the decode path must hold the same
	// form, or one instant would yield two valid identities.
	if a.AdmittedAt.Location() != time.UTC {
		return fmt.Errorf("execution admission %s admitted_at: %w", a.InvocationID, ErrTimestampNotUTC)
	}
	if a.ID == "" {
		return fmt.Errorf("execution admission %s id: %w", a.InvocationID, ErrEmptyID)
	}
	computed, err := a.ComputeID()
	if err != nil {
		return err
	}
	if a.ID != computed {
		return fmt.Errorf("execution admission %s id, content resolves to %s: %w",
			a.ID, computed, ErrAdmissionInconsistent)
	}
	return nil
}

func cloneStageInputSnapshot(in *StageInputSnapshot) *StageInputSnapshot {
	if in == nil {
		return nil
	}
	cloned := in.clone()
	return &cloned
}

// validateAuthIdentity binds the provider identity to the egress profile: a
// stage that reaches the provider runs under some identity, and a
// clean-verification stage reaches no network at all (§5.4), so naming one
// there would record an identity that was never used. The switch omits
// default, so a new profile has to decide its stance here.
func (a ExecutionAdmission) validateAuthIdentity() error {
	switch a.EgressProfile {
	case EgressProviderOnly, EgressProviderWebRead:
		if a.AuthIdentityID == nil || *a.AuthIdentityID == "" {
			return fmt.Errorf("execution admission %s auth_identity_id under egress %q: %w",
				a.InvocationID, a.EgressProfile, ErrEmptyID)
		}
		return nil
	case EgressCleanVerification:
		if a.AuthIdentityID != nil {
			return fmt.Errorf("execution admission %s names an auth identity under egress %q: %w",
				a.InvocationID, a.EgressProfile, ErrAuthIdentityInconsistent)
		}
		return nil
	}
	return fmt.Errorf("execution admission %s egress_profile %q: %w",
		a.InvocationID, a.EgressProfile, ErrInvalidEgressProfile)
}

// RequiresTrustProfile reports whether this admission must be anchored to an
// approved trust-profile revision: §5.7 lists a trust profile among the
// conformance an unattended run requires, and a waiver's whole meaning is
// which repository it covers.
func (a ExecutionAdmission) RequiresTrustProfile() bool {
	return a.OperatingMode == ModeUnattended || a.BackupEncryptionWaiver != nil
}

// validateTrustBinding holds the waiver and the profile digest to the modes
// they belong to. A waiver outside unattended running is a claim on an
// exception §5.7 does not extend to that mode; a profile digest is required
// exactly where a profile is, and naming one elsewhere would invite a
// comparison against a revision the run never needed.
func (a ExecutionAdmission) validateTrustBinding() error {
	if a.BackupEncryptionWaiver != nil {
		if err := a.BackupEncryptionWaiver.Validate(); err != nil {
			return fmt.Errorf("execution admission %s: %w", a.InvocationID, err)
		}
		if a.OperatingMode != ModeUnattended {
			return fmt.Errorf("execution admission %s claims a backup encryption waiver under mode %q: %w",
				a.InvocationID, a.OperatingMode, ErrWaiverModeMismatch)
		}
	}
	switch required := a.RequiresTrustProfile(); {
	case required && (a.TrustProfileDigest == nil || *a.TrustProfileDigest == ""):
		return fmt.Errorf("execution admission %s under mode %q: trust_profile_digest: %w",
			a.InvocationID, a.OperatingMode, ErrEmptyField)
	case !required && a.TrustProfileDigest != nil:
		return fmt.Errorf("execution admission %s names a trust profile under mode %q: %w",
			a.InvocationID, a.OperatingMode, ErrTrustProfileInconsistent)
	}
	return nil
}

// AdmissionPolicy is the live authority a persisted admission is re-gated
// against at reconstruction: the capability floor current policy states per
// operating mode, and the backup-encryption waiver the operator has actually
// configured. It is passed in rather than read here, because domain holds no
// state; the store carries it on every transaction, as it does the approved
// recipe set.
type AdmissionPolicy struct {
	// Floors is the minimum capability class each operating mode requires. A
	// nil map admits nothing: an unconfigured floor is not an empty floor.
	Floors map[OperatingMode]CapabilitySnapshot
	// ApprovedCredentialModes is the set of credential containments policy has
	// approved for unattended running (§5.7 lists an approved credential mode
	// among its conformance). Enum validity is not authorization: a mode can
	// be spelled correctly and still be one nobody approved, such as the
	// Phase 2 api_key_isolated or the trusted-inputs-only local_trusted. An
	// empty set approves nothing, so an unattended admission fails closed.
	ApprovedCredentialModes []CredentialMode
	// BackupEncryptionWaiverRepositoryID is retained only while reconstructing
	// legacy waiver-posture notices. Encrypted-checkpoint builds reject new
	// admissions that carry the waiver.
	BackupEncryptionWaiverRepositoryID *int64
	// BackupHealth is the latest evaluation supplied by the configured health
	// source. Nil is not an implicit pass: unattended admission fails closed
	// when no source is configured or no signal is available.
	BackupHealth *BackupHealth
}

// AdmittedUnder re-runs the trusted admission gate over a record against
// current policy: the persisted snapshot is what the backend declared at
// spawn, and this is the only thing that says the record still describes an
// admissible run. It is the store's enforcement of the half
// ExecutionAdmission.Validate cannot check (it holds no policy), and it fails
// closed in every direction: an unconfigured floor, a class below the floor,
// or a waiver the operator does not hold.
//
// It deliberately does not re-read the backend's live declaration. §5.3 fixes
// capabilities at spawn and #39 froze the snapshot precisely so a later
// backend change cannot retroactively rewrite the admitted class; re-gating
// against a live declaration would turn an audit record into a liveness check.
func AdmittedUnder(a ExecutionAdmission, policy AdmissionPolicy) error {
	if err := a.Validate(); err != nil {
		return err
	}
	floor, ok := policy.Floors[a.OperatingMode]
	if !ok {
		return fmt.Errorf("execution admission %s operating_mode %q: %w",
			a.InvocationID, a.OperatingMode, ErrUnknownAdmissionFloor)
	}
	// This compares the recorded snapshot against the floor, not against what
	// the named backend was ever proven to declare. The conformance half lives
	// at the write boundary instead (the store's RequireBackendConformant,
	// #320): a new unattended admission must sit within the backend's current
	// durable BackendConformance record, while this re-gate keeps a snapshot
	// honest about *policy drift* without turning recorded history unreadable
	// when conformance later lapses.
	if missing := MissingCapabilities(a.Capabilities, RequiredCapabilities(a.OperatingMode, floor)); len(missing) > 0 {
		return fmt.Errorf("execution admission %s lacks %v: %w", a.InvocationID, missing, ErrCapabilityBelowFloor)
	}
	// §5.7 requires an approved credential mode of an unattended run.
	// attended_dev is deliberately not held to it: the plan admits the weaker
	// class there, and the dev loop is where an unapproved containment is
	// exercised on purpose.
	if a.OperatingMode == ModeUnattended && !slices.Contains(policy.ApprovedCredentialModes, a.CredentialMode) {
		return fmt.Errorf("execution admission %s runs unattended under credential mode %q: %w",
			a.InvocationID, a.CredentialMode, ErrCredentialModeNotApproved)
	}
	// Historical records may still carry §5.7's Phase 1A.2 waiver. It is inert
	// under the encrypted-checkpoint gate: RequireHealthy below includes
	// encryption, while the record's own repository binding remains an
	// internal-consistency check. The store write boundary rejects the field
	// on every new admission.
	if a.BackupEncryptionWaiver != nil {
		waived := a.BackupEncryptionWaiver.RepositoryID
		if a.Base.RepositoryID != waived {
			return fmt.Errorf("execution admission %s targets repository %d under a waiver for %d: %w",
				a.InvocationID, a.Base.RepositoryID, waived, ErrWaiverRepositoryMismatch)
		}
	}
	if a.OperatingMode == ModeUnattended {
		if policy.BackupHealth == nil {
			return fmt.Errorf("execution admission %s runs unattended: %w",
				a.InvocationID, ErrBackupHealthUnavailable)
		}
		if err := policy.BackupHealth.RequireHealthy(); err != nil {
			return fmt.Errorf("execution admission %s backup health: %w", a.InvocationID, err)
		}
	}
	return nil
}

// waiverConfiguredFor reports whether current policy configures the retired
// §5.7 backup-encryption waiver for exactly repositoryID. Only reconstruction
// of a legacy degraded-posture notice consumes it.
func waiverConfiguredFor(repositoryID int64, policy AdmissionPolicy) bool {
	configured := policy.BackupEncryptionWaiverRepositoryID
	return configured != nil && *configured == repositoryID
}

// RequiredCapabilities returns the floor a mode must clear: the configured
// floor plus the capabilities the plan itself requires of the mode.
// Unattended running demands the networkless-export proof and the enforced
// provider-egress proof (§5.7), so a misconfigured floor cannot admit an
// unattended record without them. The switch omits default so a new mode
// decides its own plan-mandated minimum.
func RequiredCapabilities(mode OperatingMode, floor CapabilitySnapshot) []RunnerCapability {
	switch mode {
	case ModeUnattended:
		return append(floor.Clone(), CapNetworklessExport, CapEnforcedProviderEgress)
	case ModeAttendedDev:
		return floor
	}
	return floor
}

// ExecutionExport is the durable, write-once record of what one admitted
// attempt handed back: the base the workspace was observed at, the candidate
// head, and the digests of the manifests the ward gate verified before
// releasing the export (§5.6). It is the link the publication chain joins on —
// a publication intent's source head is checked against this record's head —
// and its absence beside an admission is what marks an attempt still in
// flight.
type ExecutionExport struct {
	InvocationID InvocationID `json:"invocation_id"`
	AdmissionID  Digest       `json:"admission_id"`
	// ObservedBaseSHA is the base the export was actually taken over, read
	// from the workspace rather than from the request, so the store can hold
	// it against the base the attempt was admitted under.
	ObservedBaseSHA string `json:"observed_base_sha"`
	HeadSHA         string `json:"head_sha"`
	ManifestDigest  Digest `json:"manifest_digest"`
	// EvidenceManifestDigest is nil when the workspace declared no evidence.
	EvidenceManifestDigest *Digest   `json:"evidence_manifest_digest"`
	CommitPlanPresent      bool      `json:"commit_plan_present"`
	RecordedAt             time.Time `json:"recorded_at"`
}

// ExecutionExportInput carries the caller-supplied fields of an
// ExecutionExport; it exists for symmetry with the admission constructor and
// to detach the optional digest from the caller's pointer.
type ExecutionExportInput struct {
	InvocationID           InvocationID
	AdmissionID            Digest
	ObservedBaseSHA        string
	HeadSHA                string
	ManifestDigest         Digest
	EvidenceManifestDigest *Digest
	CommitPlanPresent      bool
	RecordedAt             time.Time
}

// NewExecutionExport builds a validated export record in canonical byte-form,
// so a replayed record of the same handoff converges on the stored body.
func NewExecutionExport(in ExecutionExportInput) (ExecutionExport, error) {
	x := ExecutionExport{
		InvocationID:           in.InvocationID,
		AdmissionID:            in.AdmissionID,
		ObservedBaseSHA:        in.ObservedBaseSHA,
		HeadSHA:                in.HeadSHA,
		ManifestDigest:         in.ManifestDigest,
		EvidenceManifestDigest: clonePtr(in.EvidenceManifestDigest),
		CommitPlanPresent:      in.CommitPlanPresent,
		RecordedAt:             in.RecordedAt.UTC(),
	}
	if err := x.Validate(); err != nil {
		return ExecutionExport{}, err
	}
	return x, nil
}

// Validate reports whether the export record is well-formed. It checks the
// record alone; that it belongs to its admission is ValidateExportBinding's
// question, since this value does not carry the admission.
func (x ExecutionExport) Validate() error {
	if x.InvocationID == "" {
		return fmt.Errorf("execution export invocation_id: %w", ErrEmptyID)
	}
	if x.AdmissionID == "" {
		return fmt.Errorf("execution export %s admission_id: %w", x.InvocationID, ErrEmptyID)
	}
	if x.ObservedBaseSHA == "" {
		return fmt.Errorf("execution export %s observed_base_sha: %w", x.InvocationID, ErrEmptyField)
	}
	if x.HeadSHA == "" {
		return fmt.Errorf("execution export %s head_sha: %w", x.InvocationID, ErrEmptyField)
	}
	if x.ManifestDigest == "" {
		return fmt.Errorf("execution export %s manifest_digest: %w", x.InvocationID, ErrEmptyField)
	}
	if x.EvidenceManifestDigest != nil && *x.EvidenceManifestDigest == "" {
		return fmt.Errorf("execution export %s evidence_manifest_digest: %w", x.InvocationID, ErrEmptyField)
	}
	if x.RecordedAt.IsZero() {
		return fmt.Errorf("execution export %s recorded_at: %w", x.InvocationID, ErrMissingTimestamp)
	}
	// One instant has one byte form, so a replayed record converges instead of
	// colliding under a false immutable conflict.
	if x.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("execution export %s recorded_at: %w", x.InvocationID, ErrTimestampNotUTC)
	}
	return nil
}

// ValidateExportBinding reports whether an export record belongs to the
// admission it names: the same invocation, that admission's exact identity,
// and the base the workspace was observed at equal to the base the attempt was
// admitted under. A handoff that seeded a different commit than the one
// admitted is not this attempt's export, and the mismatch is refused on the
// write and again on every read, since a foreign export would let the
// publication chain bind a head to an admission that never produced it.
func ValidateExportBinding(a ExecutionAdmission, x ExecutionExport) error {
	if x.InvocationID != a.InvocationID {
		return fmt.Errorf("execution export %s under admission %s: %w",
			x.InvocationID, a.InvocationID, ErrParentKeyMismatch)
	}
	if x.AdmissionID != a.ID {
		return fmt.Errorf("execution export %s admission_id %s, admission is %s: %w",
			x.InvocationID, x.AdmissionID, a.ID, ErrParentKeyMismatch)
	}
	if x.ObservedBaseSHA != a.Base.BaseSHA {
		return fmt.Errorf("execution export %s observed base %s, admitted base %s: %w",
			x.InvocationID, x.ObservedBaseSHA, a.Base.BaseSHA, ErrExportBaseMismatch)
	}
	// An export is what the attempt handed back, so it cannot predate the
	// admission that let the attempt start. A record saying otherwise is not a
	// clock skew to tolerate; it is an audit trail that reads backwards.
	if x.RecordedAt.Before(a.AdmittedAt) {
		return fmt.Errorf("execution export %s recorded %s, admitted %s: %w",
			x.InvocationID, x.RecordedAt, a.AdmittedAt, ErrTimestampOutOfOrder)
	}
	return nil
}

// CurrentImportStart is the trusted, write-once marker that a released export
// crossed into the current-policy import lane. Private driver state can name
// that lane for replay, but only this admission-bound record can require it.
type CurrentImportStart struct {
	InvocationID InvocationID `json:"invocation_id"`
	AdmissionID  Digest       `json:"admission_id"`
}

// Validate reports whether the marker is well-formed. Its admission binding
// is checked separately because the marker does not carry that record.
func (x CurrentImportStart) Validate() error {
	if x.InvocationID == "" {
		return fmt.Errorf("current import start invocation_id: %w", ErrEmptyID)
	}
	if x.AdmissionID == "" {
		return fmt.Errorf("current import start %s admission_id: %w", x.InvocationID, ErrEmptyID)
	}
	return nil
}

// ValidateCurrentImportStartBinding requires the marker to name the exact
// immutable admission that authorized its invocation.
func ValidateCurrentImportStartBinding(a ExecutionAdmission, x CurrentImportStart) error {
	if x.InvocationID != a.InvocationID || x.AdmissionID != a.ID {
		return fmt.Errorf("current import start %s disagrees with admission %s: %w",
			x.InvocationID, a.InvocationID, ErrParentKeyMismatch)
	}
	return nil
}

// ExecutionOutcomeStatus is a non-export terminal class. Completed work is
// authenticated by ExecutionExport instead; keeping it out of this vocabulary
// makes the two authorities disjoint.
type ExecutionOutcomeStatus string

const (
	ExecutionOutcomeFailed   ExecutionOutcomeStatus = "failed"
	ExecutionOutcomeCanceled ExecutionOutcomeStatus = "canceled"
	ExecutionOutcomeLost     ExecutionOutcomeStatus = "lost"
)

// AllExecutionOutcomeStatuses is the single registration point for durable
// non-export outcomes.
var AllExecutionOutcomeStatuses = []ExecutionOutcomeStatus{
	ExecutionOutcomeFailed,
	ExecutionOutcomeCanceled,
	ExecutionOutcomeLost,
}

func (s ExecutionOutcomeStatus) valid() bool {
	switch s {
	case ExecutionOutcomeFailed, ExecutionOutcomeCanceled, ExecutionOutcomeLost:
		return true
	default:
		return false
	}
}

// ExecutionOutcome is the trusted, write-once authority for a failed,
// canceled, or proven-lost attempt. Private driver state may replay it but
// cannot mint one.
type ExecutionOutcome struct {
	InvocationID InvocationID           `json:"invocation_id"`
	AdmissionID  Digest                 `json:"admission_id"`
	Status       ExecutionOutcomeStatus `json:"status"`
	Summary      string                 `json:"summary,omitempty"`
	RecordedAt   time.Time              `json:"recorded_at"`
}

func (x ExecutionOutcome) Validate() error {
	if x.InvocationID == "" {
		return fmt.Errorf("execution outcome invocation_id: %w", ErrEmptyID)
	}
	if x.AdmissionID == "" {
		return fmt.Errorf("execution outcome %s admission_id: %w", x.InvocationID, ErrEmptyID)
	}
	if !x.Status.valid() {
		return fmt.Errorf("execution outcome %s status %q: %w",
			x.InvocationID, x.Status, ErrInvalidExecOutcome)
	}
	if x.Status == ExecutionOutcomeLost && x.Summary != "" {
		return fmt.Errorf("execution outcome %s lost with summary: %w",
			x.InvocationID, ErrOutcomeInconsistent)
	}
	if x.RecordedAt.IsZero() {
		return fmt.Errorf("execution outcome %s recorded_at: %w", x.InvocationID, ErrMissingTimestamp)
	}
	if x.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("execution outcome %s recorded_at: %w", x.InvocationID, ErrTimestampNotUTC)
	}
	return nil
}

// ValidateOutcomeBinding requires the same invocation and admission, and
// forward-moving time, as the trusted admission that authorized the attempt.
func ValidateOutcomeBinding(a ExecutionAdmission, x ExecutionOutcome) error {
	if x.InvocationID != a.InvocationID || x.AdmissionID != a.ID {
		return fmt.Errorf("execution outcome %s disagrees with admission %s: %w",
			x.InvocationID, a.InvocationID, ErrParentKeyMismatch)
	}
	if x.RecordedAt.Before(a.AdmittedAt) {
		return fmt.Errorf("execution outcome %s recorded %s, admitted %s: %w",
			x.InvocationID, x.RecordedAt, a.AdmittedAt, ErrTimestampOutOfOrder)
	}
	return nil
}

// ValidateAdmissionAgentDerivations rechecks a v4 admission's derived fields
// against the closure its agent binding names (§5.4 admission step 5:
// "Reconstruction reads by digest and rechecks the derivations"). The caller
// supplies the closure it resolved by digest — the agent, its adapter,
// route, and offer fragments, the stage's launch with the role the
// admission's stage resolves to, and the enrollment and generation records —
// and every derived field must agree: the identity and credential mode with
// the enrollment, the store manifest with the generation, the egress with
// the route, the instruction-delivery vendor with the adapter, the binding's
// launch digest with an authenticated launch for exactly that role, and the
// agent's offer digest with an authenticated offer, so attended eligibility,
// launch-capability provenance, and the admitted model can never be
// attributed to a fragment the admission did not run. The joins the closure
// itself expresses are re-run: the agent's effort must be one the offer
// allows and the adapter can send, the enrollment's client must be the one
// the adapter drives, and the launch must require nothing beyond the
// adapter's declared capability set (the proved-set gate needs the
// conformance record and stays with admission step 3). The enrollment and
// generation records carry no content address; their authenticity is the
// re-gating store read's (identity binding, column/body cross-checks), which
// is the only supported source for them. Tree facts outside every digest
// (the route names the offer and enrollment are authored under, the lineup
// revision's content) and admission-time policy gates (identity enabled,
// offer expiry against the attempt deadline) are the #867 resolver's to
// re-establish; a record of a past selection is not invalidated by current
// configuration. Any disagreement fails closed with a
// typed error; a legacy admission (no binding) never reaches this function,
// because the legacy rule forbids resolving it against current configuration
// at all.
func ValidateAdmissionAgentDerivations(
	a ExecutionAdmission, agent AgentDefinition, adapter AdapterFragment,
	route RouteFragment, offer OfferFragment, launch LaunchSpec, role StageName,
	enrollment ClientEnrollment, generation EnrollmentGeneration,
) error {
	if err := a.Validate(); err != nil {
		return err
	}
	fail := func(format string, args ...any) error {
		detail := fmt.Sprintf(format, args...)
		return fmt.Errorf("execution admission %s: %s: %w",
			a.InvocationID, detail, ErrAdmissionDerivationMismatch)
	}
	binding := a.AgentBinding
	if binding == nil {
		return fail("no agent binding to recheck")
	}
	if err := agent.Validate(); err != nil {
		return err
	}
	if err := adapter.Validate(); err != nil {
		return err
	}
	if err := route.Validate(); err != nil {
		return err
	}
	if err := offer.Validate(); err != nil {
		return err
	}
	if err := launch.Validate(); err != nil {
		return err
	}
	if !role.valid() {
		return fmt.Errorf("execution admission %s stage role %q: %w",
			a.InvocationID, role, ErrInvalidStageName)
	}
	if err := enrollment.Validate(); err != nil {
		return err
	}
	if err := ValidateEnrollmentGenerationBinding(enrollment, generation); err != nil {
		return err
	}
	if binding.AgentDigest != agent.Digest {
		return fail("binding names agent %s, closure resolves %s", binding.AgentDigest, agent.Digest)
	}
	if agent.AdapterDigest != adapter.Digest {
		return fail("agent pins adapter %s, closure supplies %s", agent.AdapterDigest, adapter.Digest)
	}
	if agent.RouteDigest != route.Digest {
		return fail("agent pins route %s, closure supplies %s", agent.RouteDigest, route.Digest)
	}
	if agent.OfferDigest != offer.Digest {
		return fail("agent pins offer %s, closure supplies %s", agent.OfferDigest, offer.Digest)
	}
	if binding.LaunchDigest != launch.Digest {
		return fail("binding names launch %s, closure resolves %s", binding.LaunchDigest, launch.Digest)
	}
	if launch.Stage != role {
		return fail("launch is for stage %q, the admission's stage resolves to %q", launch.Stage, role)
	}
	if !slices.Contains(offer.AllowedEfforts, agent.Effort) {
		return fail("effort %q is not allowed by the agent's offer", agent.Effort)
	}
	if !slices.Contains(adapter.SendableEfforts, agent.Effort) {
		return fail("effort %q is not sendable by the agent's adapter", agent.Effort)
	}
	if enrollment.HarnessClient != adapter.ClientKind {
		return fail("enrollment binds client %q, adapter drives %q",
			enrollment.HarnessClient, adapter.ClientKind)
	}
	if missing := MissingLaunchCapabilities(adapter.LaunchCapabilities, launch.RequiredCapabilities()); len(missing) > 0 {
		return fail("launch requires %v beyond the adapter's declared capabilities", missing)
	}
	if agent.EnrollmentID != binding.EnrollmentID || enrollment.ID != binding.EnrollmentID {
		return fail("enrollment %s disagrees with binding %s", enrollment.ID, binding.EnrollmentID)
	}
	if a.AuthIdentityID == nil || *a.AuthIdentityID != enrollment.AuthIdentityID {
		return fail("admitted identity disagrees with enrollment identity %s", enrollment.AuthIdentityID)
	}
	if a.CredentialMode != enrollment.CredentialMode {
		return fail("credential mode %q, enrollment carries %q", a.CredentialMode, enrollment.CredentialMode)
	}
	if generation.Ordinal != binding.EnrollmentGeneration {
		return fail("generation ordinal %d, binding names %d", generation.Ordinal, binding.EnrollmentGeneration)
	}
	if generation.StoreManifestDigest != binding.StoreManifestDigest {
		return fail("store manifest %s, binding names %s",
			generation.StoreManifestDigest, binding.StoreManifestDigest)
	}
	for _, authority := range binding.EffectiveEgress {
		if !slices.Contains(route.InferenceAuthorities, authority) {
			return fail("effective egress authority %q is outside the route's authorities", authority)
		}
	}
	// Validate already required the explicit snapshot on every agent-bound
	// admission, so absence cannot reach this comparison.
	if a.StageInputs.VendorInstructions.Vendor != adapter.Vendor {
		return fail("instruction delivery vendor %q, adapter derives %q",
			a.StageInputs.VendorInstructions.Vendor, adapter.Vendor)
	}
	return nil
}
