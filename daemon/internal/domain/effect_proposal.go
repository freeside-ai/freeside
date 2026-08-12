package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// EffectProposalRecipeDigest identifies the daemon's built-in deterministic
// proposal-construction recipe. It lets the existing evidence metadata carry
// a proposal digest to clients while the dedicated EffectProposal remains the
// authority-bearing body re-gated by the closed registry.
var EffectProposalRecipeDigest = Digest(contentaddr.Sum([]byte("freeside/effect-proposal/v1")))

const (
	EffectProposalEncodingVersion = 1
	MaxEffectProposalBytes        = 128 << 10
	MaxRunProposalCostUnits       = 1_000_000
	MaxRunProposalComponentCount  = 32
	MaxRunProposalPathCount       = 4096
	MaxProposalOccurrenceIDBytes  = 512
)

// RunProposalParameters is the fixed parameter type for run_proposal. It
// contains bounded presentation facts and one opaque, daemon-enumerated
// subject handle. Event bodies, target identities, and authority do not fit.
type RunProposalParameters struct {
	SubjectHandle     OpaqueSubjectHandle `json:"subject_handle"`
	Intent            RunProposalIntent   `json:"intent"`
	ExpectedCostUnits int                 `json:"expected_cost_units"`
	Scope             RunProposalScope    `json:"scope"`
}

// RunProposalScope carries only bounded review facts. The declared-path count
// is re-bound to the durable work-unit declaration at every authority
// boundary. Paths, target identities, event bodies, and authority have no
// field.
type RunProposalScope struct {
	ComponentCount      int  `json:"component_count"`
	DeclaredPathCount   int  `json:"declared_path_count"`
	TouchesControlPlane bool `json:"touches_control_plane"`
}

func (s RunProposalScope) Validate() error {
	if s.ComponentCount < 1 || s.ComponentCount > MaxRunProposalComponentCount ||
		s.DeclaredPathCount < 1 || s.DeclaredPathCount > MaxRunProposalPathCount {
		return ErrProposalParameterTooLarge
	}
	return nil
}

// GateRunProposalScope binds the one scope fact represented exactly by the
// durable declaration. Component grouping and control-plane classification
// need repository policy that the declaration does not contain, so they stay
// bounded review estimates rather than being guessed from path strings.
func GateRunProposalScope(scope RunProposalScope, declaration WorkUnitDeclaration) error {
	if scope.DeclaredPathCount != len(declaration.DeclaredPaths) {
		return ErrEffectProposalInconsistent
	}
	return nil
}

func (p RunProposalParameters) Validate() error {
	if p.SubjectHandle == "" {
		return fmt.Errorf("run proposal subject_handle: %w", ErrEmptyID)
	}
	if !p.Intent.valid() {
		return fmt.Errorf("run proposal intent %q: %w", p.Intent, ErrEffectProposalInconsistent)
	}
	if p.ExpectedCostUnits < 1 || p.ExpectedCostUnits > MaxRunProposalCostUnits {
		return fmt.Errorf("run proposal expected_cost_units %d: %w", p.ExpectedCostUnits, ErrProposalParameterTooLarge)
	}
	if err := p.Scope.Validate(); err != nil {
		return fmt.Errorf("run proposal scope: %w", err)
	}
	return nil
}

// EffectProposal is the digest-addressed union behind the closed registry.
// Exactly one registered parameter pointer is present. ResolvedPolicyDigest
// is injected by the trusted constructor rather than accepted as a parameter.
type EffectProposal struct {
	EncodingVersion      int                    `json:"encoding_version"`
	Kind                 EffectKind             `json:"kind"`
	ResolvedPolicyRunID  RunID                  `json:"resolved_policy_run_id"`
	ResolvedPolicyDigest Digest                 `json:"resolved_policy_digest"`
	RunProposal          *RunProposalParameters `json:"run_proposal"`
	Digest               Digest                 `json:"digest"`
}

// ProposalInstance is one admitted occurrence. Its admission key, not the
// proposal's semantic digest, determines whether a retry converges.
type ProposalInstance struct {
	ID              ProposalInstanceID   `json:"id"`
	Admission       ProposalAdmissionKey `json:"admission"`
	ProposalBatchID ProposalBatchID      `json:"proposal_batch_id"`
	Proposal        EffectProposal       `json:"proposal"`
	CreatedAt       time.Time            `json:"created_at"`
}

func (i ProposalInstance) Validate() error {
	if i.ID == "" || i.ProposalBatchID == "" {
		return fmt.Errorf("proposal instance identity or batch: %w", ErrEmptyID)
	}
	if err := i.Admission.Validate(); err != nil {
		return fmt.Errorf("proposal instance %s admission: %w", i.ID, err)
	}
	if err := i.Proposal.Validate(); err != nil {
		return fmt.Errorf("proposal instance %s: %w", i.ID, err)
	}
	if i.CreatedAt.IsZero() {
		return fmt.Errorf("proposal instance %s created_at: %w", i.ID, ErrMissingTimestamp)
	}
	if i.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("proposal instance %s created_at: %w", i.ID, ErrTimestampNotUTC)
	}
	return nil
}

// EvidenceArtifact returns the existing client-visible metadata carrier whose
// digest binds a decision to this proposal body. The built-in recipe is a
// deterministic daemon authority, not caller configuration.
func (i ProposalInstance) EvidenceArtifact() (Artifact, error) {
	if err := i.Validate(); err != nil {
		return Artifact{}, err
	}
	recipe := EffectProposalRecipeDigest
	return NewArtifact(ArtifactInput{
		ID:     ArtifactID("effect-proposal/" + string(i.ID) + "/" + contentaddr.Hex(string(i.Proposal.Digest))),
		Type:   ArtifactKindEvidence,
		Digest: i.Proposal.Digest,
		Provenance: Provenance{
			ProducerClass: ProducerDaemon, ProducerInvocationID: InvocationID("effect-proposal/" + string(i.ID)),
			HeadBinding: HeadIndependent, VerificationRecipeDigest: &recipe,
			SensitivityClass: SensitivityNormal,
		},
	}, map[Digest]bool{EffectProposalRecipeDigest: true})
}

type canonicalEffectProposal struct {
	EncodingVersion      int                    `json:"encoding_version"`
	Kind                 EffectKind             `json:"kind"`
	ResolvedPolicyRunID  RunID                  `json:"resolved_policy_run_id"`
	ResolvedPolicyDigest Digest                 `json:"resolved_policy_digest"`
	RunProposal          *RunProposalParameters `json:"run_proposal"`
}

// NewEffectProposal dispatches construction through the kind's fixed Go type.
// The behaviour switch deliberately has no default so the exhaustive linter
// forces every new registry member to choose a constructor and gate.
func NewEffectProposal(
	kind EffectKind,
	parameters any,
	policy ResolvedPolicy,
) (EffectProposal, error) {
	if err := policy.Validate(); err != nil {
		return EffectProposal{}, fmt.Errorf("effect proposal resolved policy: %w", err)
	}
	var proposal EffectProposal
	switch kind {
	case EffectRunProposal:
		params, ok := parameters.(RunProposalParameters)
		if !ok {
			return EffectProposal{}, fmt.Errorf("effect kind %q requires RunProposalParameters: %w", kind, ErrEffectProposalInconsistent)
		}
		proposal = EffectProposal{
			EncodingVersion: EffectProposalEncodingVersion,
			Kind:            kind, ResolvedPolicyRunID: policy.RunID,
			ResolvedPolicyDigest: policy.Digest, RunProposal: &params,
		}
	}
	if proposal.Kind == "" {
		return EffectProposal{}, fmt.Errorf("effect kind %q: %w", kind, ErrInvalidEffectKind)
	}
	if err := gateEffectProposal(proposal, policy, false); err != nil {
		return EffectProposal{}, err
	}
	digest, err := proposal.ComputeDigest()
	if err != nil {
		return EffectProposal{}, err
	}
	proposal.Digest = digest
	return proposal, nil
}

// Validate is the structural and content-address backstop for reconstructed
// values. Current resolved policy and subject enumeration are re-gated by
// GateEffectProposal at each authority boundary.
func (p EffectProposal) Validate() error {
	if p.EncodingVersion != EffectProposalEncodingVersion {
		return fmt.Errorf("effect proposal encoding_version %d", p.EncodingVersion)
	}
	if !p.Kind.valid() {
		return fmt.Errorf("effect proposal kind %q: %w", p.Kind, ErrInvalidEffectKind)
	}
	if !contentaddr.Valid(string(p.ResolvedPolicyDigest)) {
		return fmt.Errorf("effect proposal resolved_policy_digest %q: %w", p.ResolvedPolicyDigest, ErrEffectProposalInconsistent)
	}
	if p.ResolvedPolicyRunID == "" {
		return fmt.Errorf("effect proposal resolved_policy_run_id: %w", ErrEmptyID)
	}
	switch p.Kind {
	case EffectRunProposal:
		if p.RunProposal == nil {
			return fmt.Errorf("effect proposal kind %q has no parameters: %w", p.Kind, ErrEffectProposalInconsistent)
		}
		if err := p.RunProposal.Validate(); err != nil {
			return err
		}
	}
	if !contentaddr.Valid(string(p.Digest)) {
		return fmt.Errorf("effect proposal digest %q: %w", p.Digest, ErrEffectProposalDigestMismatch)
	}
	computed, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	if p.Digest != computed {
		return fmt.Errorf("effect proposal digest %q, content resolves to %q: %w", p.Digest, computed, ErrEffectProposalDigestMismatch)
	}
	return nil
}

func (p EffectProposal) canonical() canonicalEffectProposal {
	return canonicalEffectProposal{
		EncodingVersion: p.EncodingVersion, Kind: p.Kind,
		ResolvedPolicyRunID:  p.ResolvedPolicyRunID,
		ResolvedPolicyDigest: p.ResolvedPolicyDigest, RunProposal: p.RunProposal,
	}
}

// ComputeDigest hashes the explicit-version canonical encoding. Struct field
// order is part of the contract and is pinned by a golden.
func (p EffectProposal) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(p.canonical())
	if err != nil {
		return "", fmt.Errorf("effect proposal canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Encode emits the validated canonical persisted form.
func (p EffectProposal) Encode() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("effect proposal encode: %w", err)
	}
	return body, nil
}

// DecodeEffectProposal rejects oversized, unknown-field, invalid-UTF8, and
// trailing-data payloads before revalidating the content address.
func DecodeEffectProposal(body []byte) (EffectProposal, error) {
	var proposal EffectProposal
	if err := strictjson.Decode(body, &proposal, strictjson.RejectInvalidUTF8, MaxEffectProposalBytes); err != nil {
		return EffectProposal{}, fmt.Errorf("effect proposal decode: %w", err)
	}
	if err := proposal.Validate(); err != nil {
		return EffectProposal{}, err
	}
	return proposal, nil
}

// GateEffectProposal re-runs the registered effect gate against current
// resolved policy. The store independently resolves the proposal's opaque
// subject handle before it supplies this policy.
func GateEffectProposal(
	proposal EffectProposal,
	policy ResolvedPolicy,
) error {
	return gateEffectProposal(proposal, policy, true)
}

func gateEffectProposal(
	proposal EffectProposal,
	policy ResolvedPolicy,
	validateDigest bool,
) error {
	if validateDigest {
		if err := proposal.Validate(); err != nil {
			return err
		}
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("effect proposal resolved policy: %w", err)
	}
	if proposal.ResolvedPolicyRunID != policy.RunID || proposal.ResolvedPolicyDigest != policy.Digest {
		return fmt.Errorf("effect proposal policy %q, current %q: %w", proposal.ResolvedPolicyDigest, policy.Digest, ErrProposalPolicyMismatch)
	}
	switch proposal.Kind {
	case EffectRunProposal:
		if proposal.RunProposal == nil {
			return ErrEffectProposalInconsistent
		}
		if err := proposal.RunProposal.Validate(); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("effect proposal kind %q: %w", proposal.Kind, ErrInvalidEffectKind)
}

// ProposalAdmissionKey is the typed union of the three allowed occurrence
// sources. Proposal content is absent, so equal semantics cannot collapse two
// deliberate occurrences and changed semantics cannot split one retry.
type ProposalAdmissionKey struct {
	Source              ProposalAdmissionSource `json:"source"`
	UpstreamEventID     string                  `json:"upstream_event_id,omitempty"`
	SubmissionCommandID string                  `json:"submission_command_id,omitempty"`
	InvocationID        InvocationID            `json:"invocation_id,omitempty"`
	ExportIdentity      Digest                  `json:"export_identity,omitempty"`
	EmissionOrdinal     int                     `json:"emission_ordinal,omitempty"`
}

func (k ProposalAdmissionKey) Validate() error {
	if !k.Source.valid() {
		return fmt.Errorf("proposal admission source %q: %w", k.Source, ErrInvalidProposalAdmissionSource)
	}
	switch k.Source {
	case ProposalSourceUpstreamEvent:
		if !validProposalOccurrenceID(k.UpstreamEventID) || k.SubmissionCommandID != "" || k.InvocationID != "" || k.ExportIdentity != "" || k.EmissionOrdinal != 0 {
			return ErrProposalAdmissionKeyInconsistent
		}
	case ProposalSourceClientCommand:
		if !validProposalOccurrenceID(k.SubmissionCommandID) || k.UpstreamEventID != "" || k.InvocationID != "" || k.ExportIdentity != "" || k.EmissionOrdinal != 0 {
			return ErrProposalAdmissionKeyInconsistent
		}
	case ProposalSourceRunEmission:
		exactlyOneIdentity := (k.InvocationID == "") != (k.ExportIdentity == "")
		if k.UpstreamEventID != "" || k.SubmissionCommandID != "" || !exactlyOneIdentity || k.EmissionOrdinal < 1 ||
			(k.InvocationID != "" && !validProposalOccurrenceID(string(k.InvocationID))) {
			return ErrProposalAdmissionKeyInconsistent
		}
		if k.ExportIdentity != "" && !contentaddr.Valid(string(k.ExportIdentity)) {
			return ErrProposalAdmissionKeyInconsistent
		}
	}
	return nil
}

func validProposalOccurrenceID(value string) bool {
	return value != "" && len(value) <= MaxProposalOccurrenceIDBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value
}

// String returns the stable store key as a digest of the typed canonical
// union, avoiding delimiter ambiguity in caller-supplied source identifiers.
func (k ProposalAdmissionKey) String() (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(k)
	if err != nil {
		return "", err
	}
	return "effect-proposal/" + contentaddr.Sum(body), nil
}
