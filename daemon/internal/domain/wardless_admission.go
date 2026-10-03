package domain

import (
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// The wardless admission class (plan §5.4, Admission): the one class every
// judgment role is admitted through. It keeps stage admission's five steps
// and drops the ward-only parts. Resolve, Selected, Credentialed, and
// Snapshot hold as they do for a stage, with the role's prompt resolved by
// digest. Proved drops runner conformance, because there is no ward to
// conform, and requires proof that the adapter build's call launch runs the
// harness with no tools.

// InterimCallLaunchAudit is the dated interim no-tools proof: a hand audit of
// one Claude harness build under one adapter build. Plan revision 65 makes
// the call launch a capability the adapter build's conformance record proves
// in the stage contract suite; until #1424 lands that, the single Claude call
// driver keeps running on this audit, for the build it covered and no other.
// A deterministic fake and a budget never stand in for it: they cover output
// handling, not what the harness can do. No second call driver joins on a
// hand audit, so admission accepts this proof for the Claude client only.
type InterimCallLaunchAudit struct {
	// AdapterDigest names the exact adapter fragment the audit covered.
	AdapterDigest Digest `json:"adapter_digest"`
	// HarnessBuild is the audited harness build, as the adapter fragment
	// spells it.
	HarnessBuild string `json:"harness_build"`
	// AuditedOn is the calendar date of the audit (YYYY-MM-DD).
	AuditedOn string `json:"audited_on"`
}

// Validate reports whether the audit record is well-formed. Whether it covers
// a given adapter is AdmitWardlessRole's question.
func (a InterimCallLaunchAudit) Validate() error {
	if !contentaddr.Valid(string(a.AdapterDigest)) {
		return fmt.Errorf("interim call launch audit adapter_digest %q: %w", a.AdapterDigest, ErrInvalidDigest)
	}
	if a.HarnessBuild == "" {
		return fmt.Errorf("interim call launch audit harness_build: %w", ErrEmptyField)
	}
	if _, err := time.Parse(time.DateOnly, a.AuditedOn); err != nil {
		return fmt.Errorf("interim call launch audit audited_on %q: %w", a.AuditedOn, ErrMissingTimestamp)
	}
	return nil
}

// WardlessAdmissionInput is everything one wardless admission is decided
// from. The caller (the per-site lookup, #1425) supplies records it has
// already rebuilt through their own gates, all resolved against the one
// control-plane revision LineupRevision names. Tree facts outside every
// digest (the route names the offer and the enrollment are authored under)
// and whether the identity is enabled stay the caller's resolution to
// establish, as they do for a stage.
type WardlessAdmissionInput struct {
	Role RoleName
	// Line is the role's own lineup line, primary or shadow. A shadow line
	// is admitted in its own call: its error is the shadow's alone and never
	// touches the primary line's result.
	Line           LineupSelection
	LineupRevision Digest
	Agent          AgentDefinition
	Route          RouteFragment
	Adapter        AdapterFragment
	Offer          OfferFragment
	// PromptDigest is the digest the line's prompt name resolves to in the
	// same revision.
	PromptDigest Digest
	Enrollment   ClientEnrollment
	// Generation is the enrollment's current generation. Whether it is
	// current, retired, or revoked is the store's fact, and so is whether the
	// integrity probe marked it: the caller reads that in the same
	// transaction through the store's RequireGenerationUnmarked. This class
	// checks the binding and the expiry window.
	Generation   EnrollmentGeneration
	Deadline     time.Time
	ExpiryMargin time.Duration
	// LaunchProof is the proof that the adapter build's call launch runs
	// with no tools; nil is no proof and fails closed. The caller reads it
	// from the deployment's record of the audit and never builds it from
	// Adapter: admission can check that a proof covers this adapter, not that
	// the audit happened, so a proof copied from the adapter under admission
	// proves nothing. #1424 replaces this input with the adapter's
	// conformance record.
	LaunchProof *InterimCallLaunchAudit
}

// WardlessAdmission is the admission result a call record snapshots in the
// place of ExecutionAdmission (§5.4): the role, the agent and prompt it was
// admitted with, the lineup revision that named them, the enrollment
// generation the call draws its credential from, and the proof it was
// admitted under. It carries no model or effort; a consumer derives them
// from the admitted agent (DeriveAgentLaunchSelection). Which of the lineup
// or an alternate-agent card selected the agent arrives with the card (#869).
type WardlessAdmission struct {
	Role                 RoleName               `json:"role"`
	AgentDigest          Digest                 `json:"agent_digest"`
	PromptDigest         Digest                 `json:"prompt_digest"`
	LineupRevision       Digest                 `json:"lineup_revision"`
	EnrollmentID         ClientEnrollmentID     `json:"enrollment_id"`
	EnrollmentGeneration int                    `json:"enrollment_generation"`
	InterimLaunchAudit   InterimCallLaunchAudit `json:"interim_launch_audit"`
}

// Validate reports whether the admission result is well-formed.
func (a WardlessAdmission) Validate() error {
	if !a.Role.valid() {
		return fmt.Errorf("wardless admission role %q: %w", a.Role, ErrInvalidRoleName)
	}
	if a.Role.LaunchShape() != LaunchShapeWardless {
		return fmt.Errorf("wardless admission role %q: %w", a.Role, ErrRoleNotWardless)
	}
	for field, digest := range map[string]Digest{
		"agent_digest": a.AgentDigest, "prompt_digest": a.PromptDigest,
		"lineup_revision": a.LineupRevision,
	} {
		if !contentaddr.Valid(string(digest)) {
			return fmt.Errorf("wardless admission %s %s %q: %w", a.Role, field, digest, ErrInvalidDigest)
		}
	}
	if a.EnrollmentID == "" {
		return fmt.Errorf("wardless admission %s enrollment_id: %w", a.Role, ErrEmptyID)
	}
	if a.EnrollmentGeneration < 1 {
		return fmt.Errorf("wardless admission %s enrollment_generation %d: %w",
			a.Role, a.EnrollmentGeneration, ErrNonPositive)
	}
	if err := a.InterimLaunchAudit.Validate(); err != nil {
		return fmt.Errorf("wardless admission %s: %w", a.Role, err)
	}
	return nil
}

// AdmitWardlessRole admits one wardless role from its agent, its prompt
// digest, and its launch proof, failing closed at the first step that does
// not hold. A missing or failed admission is what makes a site return its
// fail-safe (#1425 wires that). Nothing here compares lineage groups: review
// independence is a recorded fact, never an admission gate, and an empty
// lineage group is recorded as unknown (plan revision 65).
func AdmitWardlessRole(in WardlessAdmissionInput) (WardlessAdmission, error) {
	if !in.Role.valid() {
		return WardlessAdmission{}, fmt.Errorf("wardless admission role %q: %w", in.Role, ErrInvalidRoleName)
	}
	// A ward role's agent has tools or a workspace; admitting it here would
	// run it on the host unsandboxed.
	if in.Role.LaunchShape() != LaunchShapeWardless {
		return WardlessAdmission{}, fmt.Errorf("wardless admission role %q: %w", in.Role, ErrRoleNotWardless)
	}
	fail := func(sentinel error, format string, args ...any) (WardlessAdmission, error) {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %s: %w",
			in.Role, fmt.Sprintf(format, args...), sentinel)
	}

	// Resolve.
	if !contentaddr.Valid(string(in.LineupRevision)) {
		return fail(ErrInvalidDigest, "lineup_revision %q", in.LineupRevision)
	}
	if err := in.Agent.Validate(); err != nil {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %w", in.Role, err)
	}
	if err := in.Route.Validate(); err != nil {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %w", in.Role, err)
	}
	if err := in.Adapter.Validate(); err != nil {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %w", in.Role, err)
	}
	if err := in.Offer.Validate(); err != nil {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %w", in.Role, err)
	}
	if in.Agent.RouteDigest != in.Route.Digest {
		return fail(ErrAdmissionDerivationMismatch,
			"agent pins route %s, closure supplies %s", in.Agent.RouteDigest, in.Route.Digest)
	}
	if in.Agent.AdapterDigest != in.Adapter.Digest {
		return fail(ErrAdmissionDerivationMismatch,
			"agent pins adapter %s, closure supplies %s", in.Agent.AdapterDigest, in.Adapter.Digest)
	}
	if in.Agent.OfferDigest != in.Offer.Digest {
		return fail(ErrAdmissionDerivationMismatch,
			"agent pins offer %s, closure supplies %s", in.Agent.OfferDigest, in.Offer.Digest)
	}
	// The join's effort legs, re-run as the stage recheck re-runs them: the
	// agent digest covers the effort but not whether the offer allows it.
	if !slices.Contains(in.Offer.AllowedEfforts, in.Agent.Effort) {
		return fail(ErrAgentJoinInvalid, "effort %q is not allowed by the agent's offer", in.Agent.Effort)
	}
	if !slices.Contains(in.Adapter.SendableEfforts, in.Agent.Effort) {
		return fail(ErrAgentJoinInvalid, "effort %q is not sendable by the agent's adapter", in.Agent.Effort)
	}
	if !contentaddr.Valid(string(in.PromptDigest)) {
		return fail(ErrInvalidDigest, "prompt digest %q", in.PromptDigest)
	}
	if err := ValidateOfferCoversDeadline(in.Offer, in.Deadline); err != nil {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %w", in.Role, err)
	}

	// Selected: the line names exactly this agent and this prompt.
	if in.Line.AgentDigest != in.Agent.Digest {
		return fail(ErrLineupSelectionMismatch,
			"line names agent %s, revision resolves %s", in.Line.AgentDigest, in.Agent.Digest)
	}
	if in.Line.PromptDigest != in.PromptDigest {
		return fail(ErrLineupSelectionMismatch,
			"line names prompt %s, revision resolves %s", in.Line.PromptDigest, in.PromptDigest)
	}

	// Proved: the launch proof covers this adapter build. Runner conformance
	// has no part here.
	if in.LaunchProof == nil {
		return fail(ErrCallLaunchUnproved, "no launch proof for adapter %s", in.Adapter.Digest)
	}
	proof := *in.LaunchProof
	if err := proof.Validate(); err != nil {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %w", in.Role, err)
	}
	if in.Adapter.ClientKind != HarnessClientClaudeCode {
		return fail(ErrCallLaunchUnproved,
			"a hand audit admits only the claude_code call driver, adapter drives %q", in.Adapter.ClientKind)
	}
	if proof.AdapterDigest != in.Adapter.Digest {
		return fail(ErrCallLaunchUnproved,
			"audit covers adapter %s, agent runs %s", proof.AdapterDigest, in.Adapter.Digest)
	}
	if proof.HarnessBuild != in.Adapter.HarnessBuild {
		return fail(ErrCallLaunchUnproved,
			"audit covers harness build %q, adapter pins %q", proof.HarnessBuild, in.Adapter.HarnessBuild)
	}

	// Credentialed.
	if err := in.Enrollment.Validate(); err != nil {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %w", in.Role, err)
	}
	if err := in.Generation.Validate(); err != nil {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %w", in.Role, err)
	}
	if in.Agent.EnrollmentID != in.Enrollment.ID {
		return fail(ErrAdmissionDerivationMismatch,
			"agent names enrollment %s, closure supplies %s", in.Agent.EnrollmentID, in.Enrollment.ID)
	}
	if in.Enrollment.HarnessClient != in.Adapter.ClientKind {
		return fail(ErrAdmissionDerivationMismatch,
			"enrollment binds client %q, adapter drives %q", in.Enrollment.HarnessClient, in.Adapter.ClientKind)
	}
	if in.Generation.Ordinal < 1 {
		return fail(ErrNonPositive, "enrollment generation ordinal %d is not a persisted generation", in.Generation.Ordinal)
	}
	if err := ValidateGenerationExpiryMargin(in.Enrollment, in.Generation, in.Deadline, in.ExpiryMargin); err != nil {
		return WardlessAdmission{}, fmt.Errorf("wardless admission %s: %w", in.Role, err)
	}

	// Snapshot.
	admission := WardlessAdmission{
		Role: in.Role, AgentDigest: in.Agent.Digest, PromptDigest: in.PromptDigest,
		LineupRevision: in.LineupRevision,
		EnrollmentID:   in.Enrollment.ID, EnrollmentGeneration: in.Generation.Ordinal,
		InterimLaunchAudit: proof,
	}
	if err := admission.Validate(); err != nil {
		return WardlessAdmission{}, err
	}
	return admission, nil
}
