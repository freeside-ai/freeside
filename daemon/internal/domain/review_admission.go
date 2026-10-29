package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// ReviewAdmission is the admission snapshot of one routed or shadow review
// invocation (plan §5.4 admission step 5, §7): which agent the daemon
// admitted to review, under which identity, and from which lineup. The daemon
// writes it once, before it asks the review source for the review, and every
// later pass over the invocation reuses it; a restart, a retry, or a lineup
// change never replaces it.
//
// It is a record of a past selection. Nothing resolves it against the
// current lineup, and a review record that carries one is attributed to this
// agent however the tree has moved since.
type ReviewAdmission struct {
	InvocationID InvocationID `json:"invocation_id"`
	RunID        RunID        `json:"run_id"`
	// Role is reviewer for a routed review and shadow_reviewer for a shadow
	// one.
	Role           RoleName       `json:"role"`
	AuthIdentityID AuthIdentityID `json:"auth_identity_id"`
	// LineageGroup is the lineage group of the agent's offer. It is the
	// provider label a review record shows. Empty means the offer's lineage
	// is unknown, which is a legal authored state (OfferFragment) and is
	// recorded as unknown, never refused.
	LineageGroup string `json:"lineage_group"`
	// AgentBinding is the binding an execution admission records, unchanged.
	// Its launch digest is recorded but its coverage is not enforced for the
	// review roles, and its egress is the allowlist the review source's proxy
	// admits.
	AgentBinding AdmissionAgentBinding `json:"agent_binding"`
	AdmittedAt   time.Time             `json:"admitted_at"`
}

// NewReviewAdmission constructs a detached snapshot with a canonical egress
// allowlist and a UTC admission time.
func NewReviewAdmission(admission ReviewAdmission) (ReviewAdmission, error) {
	admission.AdmittedAt = admission.AdmittedAt.UTC()
	admission.AgentBinding = *canonicalAgentBinding(&admission.AgentBinding)
	if err := admission.Validate(); err != nil {
		return ReviewAdmission{}, err
	}
	return admission, nil
}

// Validate reports whether the snapshot is identified, names a review role,
// and carries a binding a review launch could have run: chosen by the lineup
// or a task line, and passing both a model and an effort.
func (a ReviewAdmission) Validate() error {
	switch {
	case a.InvocationID == "":
		return fmt.Errorf("review admission invocation_id: %w", ErrEmptyID)
	case a.RunID == "":
		return fmt.Errorf("review admission run_id: %w", ErrEmptyID)
	case a.Role != RoleReviewer && a.Role != RoleShadowReviewer:
		return fmt.Errorf("review admission role %q: %w", a.Role, ErrInvalidRoleName)
	case a.AuthIdentityID == "":
		return fmt.Errorf("review admission auth_identity_id: %w", ErrEmptyID)
	case a.AdmittedAt.IsZero():
		return fmt.Errorf("review admission admitted_at: %w", ErrMissingTimestamp)
	case a.AdmittedAt.Location() != time.UTC:
		return fmt.Errorf("review admission admitted_at: %w", ErrTimestampNotUTC)
	}
	if err := a.AgentBinding.Validate(); err != nil {
		return fmt.Errorf("review admission %s: %w", a.InvocationID, err)
	}
	switch {
	// Every review launch passes a model and an effort, and the model
	// configuration a review record shows is built from both.
	case a.AgentBinding.RouteModelID == "" || a.AgentBinding.NativeEffort == "":
		return fmt.Errorf("review admission %s model and effort: %w", a.InvocationID, ErrEmptyField)
	// The lineup or the task's reviewer line selects a review agent. No card
	// does: nothing writes an alternate-agent card for a review (#869). An
	// absent source predates the field, which no review snapshot does.
	case a.AgentBinding.SelectionSource != AgentSelectionSourceLineup &&
		a.AgentBinding.SelectionSource != AgentSelectionSourceTaskLine:
		return fmt.Errorf("review admission %s selection_source %q: %w",
			a.InvocationID, a.AgentBinding.SelectionSource, ErrReviewAdmissionInvalid)
	}
	return nil
}

// Equal reports whether two snapshots are the same admission. The engine
// holds a snapshot a review source returns against the one it stored with
// this. It compares the encoded forms, so a field added to the snapshot or
// its binding is covered without a change here.
func (a ReviewAdmission) Equal(b ReviewAdmission) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}

// ModelConfiguration is the model configuration a review record shows: the
// model and the effort the review launch passed, as the harness names them.
func (a ReviewAdmission) ModelConfiguration() string {
	return a.AgentBinding.RouteModelID + "/" + a.AgentBinding.NativeEffort
}

// ReviewProviderUnknown is the provider label of a review whose agent's offer
// names no lineage group. Unknown lineage is recorded as unknown, never
// refused (plan §5.4), and a record's provider label is never empty.
const ReviewProviderUnknown = "unknown"

// ProviderLabel is the provider a review record shows: the lineage group of
// the offer the admitted agent ran.
func (a ReviewAdmission) ProviderLabel() string {
	if a.LineageGroup == "" {
		return ReviewProviderUnknown
	}
	return a.LineageGroup
}

// reviewAttribution is how a review record says who reviewed. A record
// written since review admission carries the invocation's snapshot and none
// of the three strings. A legacy record carries the three strings as its
// source reported them and no snapshot; nothing resolves those against a
// lineup. Carrying both would leave two answers to the same question, and
// carrying neither would leave none, so both are refused wherever a record
// is written or read.
type reviewAttribution struct {
	admission                           *ReviewAdmission
	provider, modelConfiguration, payer string
	invocationID                        InvocationID
	runID                               RunID
	role                                RoleName
}

func (a reviewAttribution) validate(kind string) error {
	if a.admission == nil {
		switch {
		case a.provider == "":
			return fmt.Errorf("%s provider: %w", kind, ErrEmptyField)
		case a.modelConfiguration == "":
			return fmt.Errorf("%s model_configuration: %w", kind, ErrEmptyField)
		case a.payer == "":
			return fmt.Errorf("%s cost_owner: %w", kind, ErrEmptyField)
		}
		return nil
	}
	if a.provider != "" || a.modelConfiguration != "" || a.payer != "" {
		return fmt.Errorf("%s carries an admission and legacy attribution: %w", kind, ErrReviewAttribution)
	}
	if err := a.admission.Validate(); err != nil {
		return fmt.Errorf("%s: %w", kind, err)
	}
	switch {
	case a.admission.InvocationID != a.invocationID:
		return fmt.Errorf("%s admission names invocation %q: %w",
			kind, a.admission.InvocationID, ErrParentKeyMismatch)
	case a.admission.RunID != a.runID:
		return fmt.Errorf("%s admission names run %q: %w", kind, a.admission.RunID, ErrParentKeyMismatch)
	case a.admission.Role != a.role:
		return fmt.Errorf("%s admission is for role %q, want %q: %w",
			kind, a.admission.Role, a.role, ErrReviewAdmissionInvalid)
	}
	return nil
}

func (a reviewAttribution) providerLabel() string {
	if a.admission != nil {
		return a.admission.ProviderLabel()
	}
	return a.provider
}

func (a reviewAttribution) modelConfigurationLabel() string {
	if a.admission != nil {
		return a.admission.ModelConfiguration()
	}
	return a.modelConfiguration
}

// detachedReviewAdmission returns a canonical copy a record owns, so a
// caller's later edit to its snapshot never reaches the record.
func detachedReviewAdmission(admission *ReviewAdmission) *ReviewAdmission {
	if admission == nil {
		return nil
	}
	detached := *admission
	detached.AdmittedAt = detached.AdmittedAt.UTC()
	detached.AgentBinding = *canonicalAgentBinding(&detached.AgentBinding)
	return &detached
}

// ValidateReviewAdmissionDerivations re-runs the closure joins a review
// snapshot rests on, as ValidateAdmissionAgentDerivations does for an
// execution admission: the binding against the agent, its fragments, the
// launch for the role's stage, and the enrollment and generation records,
// plus the identity against the enrollment and the lineage group against the
// offer. The resolver runs it before it returns a snapshot, so a resolver
// bug cannot store one this check would refuse.
//
// Two joins of the execution check are left out, and each is a recorded gap,
// not a relaxation of that check:
//   - Launch capabilities. The review launch forbids auxiliary inference and
//     no review adapter declares that control, so coverage (§5.4 step 3) is
//     not enforced for the review roles until an adapter proves it (#1980).
//   - Egress against the route's authorities. An API-key review credential
//     reaches an authority its enrollment's route does not name until that
//     credential is admitted as an enrollment (#1677). The snapshot records
//     the allowlist the review proxy admits either way.
func ValidateReviewAdmissionDerivations(
	a ReviewAdmission, agent AgentDefinition, adapter AdapterFragment,
	route RouteFragment, offer OfferFragment, launch LaunchSpec,
	enrollment ClientEnrollment, generation EnrollmentGeneration,
) error {
	if err := a.Validate(); err != nil {
		return err
	}
	fail := func(format string, args ...any) error {
		return fmt.Errorf("review admission %s: %s: %w",
			a.InvocationID, fmt.Sprintf(format, args...), ErrAdmissionDerivationMismatch)
	}
	stage, ok := a.Role.Stage()
	if !ok {
		return fail("role %q runs in no ward stage", a.Role)
	}
	if err := validateBindingClosure(
		fail, a.AgentBinding, agent, adapter, route, offer, launch, stage, enrollment, generation,
	); err != nil {
		return err
	}
	if a.AuthIdentityID != enrollment.AuthIdentityID {
		return fail("admitted identity disagrees with enrollment identity %s", enrollment.AuthIdentityID)
	}
	if a.LineageGroup != offer.LineageGroup {
		return fail("lineage group %q, the offer carries %q", a.LineageGroup, offer.LineageGroup)
	}
	return nil
}
