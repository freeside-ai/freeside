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
// and carries a binding a review launch could have run: unattended, chosen
// by the lineup, and passing both a model and an effort.
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
	// Review sources run only in an unattended daemon.
	case a.AgentBinding.Attended:
		return fmt.Errorf("review admission %s ran attended: %w", a.InvocationID, ErrReviewAdmissionInvalid)
	// No task line or card selects a review agent: the source runs the one
	// agent the lineup names.
	case a.AgentBinding.SelectionSource != AgentSelectionSourceLineup:
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
//     not enforced for the review roles until an adapter proves it.
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
