package domain

import (
	"cmp"
	"fmt"
	"slices"
	"time"
)

// AdmissionKind names the admission record an invocation's facts are read
// from: an execution admission for a stage attempt the daemon ran, or a
// review admission for a review the review source ran.
type AdmissionKind string

const (
	AdmissionKindExecution AdmissionKind = "execution"
	AdmissionKindReview    AdmissionKind = "review"
)

// AllAdmissionKinds lists every valid AdmissionKind.
var AllAdmissionKinds = []AdmissionKind{AdmissionKindExecution, AdmissionKindReview}

func (k AdmissionKind) valid() bool {
	switch k {
	case AdmissionKindExecution, AdmissionKindReview:
		return true
	default:
		return false
	}
}

// IndependenceEntry is a judging role's lineage relation to one writing role
// (plan §7), the shape the inference ledger records for a judgment call.
type IndependenceEntry struct {
	WritingRole RoleName        `json:"writing_role"`
	Lineage     LineageRelation `json:"lineage"`
}

// InvocationAdmissionFacts is what ran one invocation, as its admission
// recorded it (plan §5.4, §7): the wire record the run timeline and the
// execution_failure card carry so a client can show the harness, model,
// effort, cost owner, lineage, credential mode, egress, and isolation of each
// attempt. Every value is read from the admission record. A field the record
// does not carry is null, never filled from the current tree, identity,
// enrollment, image, or client configuration: a legacy execution admission
// has no agent binding, a binding admitted before #979 has no harness,
// lineage group, or cost owner, and a review admission never records the
// credential mode, egress profile, operating mode, or isolation class.
//
// Role is the ward role the invocation ran as when its recording names one,
// and null otherwise. Independence is set exactly on a review admission's
// facts, one entry per writing role.
type InvocationAdmissionFacts struct {
	InvocationID    InvocationID          `json:"invocation_id"`
	Kind            AdmissionKind         `json:"kind"`
	Role            *RoleName             `json:"role"`
	AdmittedAt      time.Time             `json:"admitted_at"`
	AgentDigest     *Digest               `json:"agent_digest"`
	SelectionSource *AgentSelectionSource `json:"selection_source"`
	Harness         *HarnessClientKind    `json:"harness"`
	Model           *string               `json:"model"`
	RequestedEffort *EffortLevel          `json:"requested_effort"`
	NativeEffort    *string               `json:"native_effort"`
	CostOwner       *string               `json:"cost_owner"`
	LineageGroup    *string               `json:"lineage_group"`
	CredentialMode  *CredentialMode       `json:"credential_mode"`
	EgressProfile   *EgressProfile        `json:"egress_profile"`
	OperatingMode   *OperatingMode        `json:"operating_mode"`
	// IsolationClass is the admitting backend's name, the class freesided
	// doctor reports.
	IsolationClass *string             `json:"isolation_class"`
	Independence   []IndependenceEntry `json:"independence"`
}

// AdmittedFactsOfExecution reads an execution admission's facts. role is the
// ward role the attempt ran as, or empty when its recording names none.
func AdmittedFactsOfExecution(a ExecutionAdmission, role RoleName) InvocationAdmissionFacts {
	facts := InvocationAdmissionFacts{
		InvocationID: a.InvocationID, Kind: AdmissionKindExecution,
		Role: recorded(role), AdmittedAt: a.AdmittedAt,
		CredentialMode: recorded(a.CredentialMode), EgressProfile: recorded(a.EgressProfile),
		OperatingMode: recorded(a.OperatingMode), IsolationClass: recorded(a.Backend),
	}
	if a.AgentBinding != nil {
		facts.withBinding(*a.AgentBinding)
	}
	return facts
}

// AdmittedFactsOfReview reads a review admission's facts. writerLineage maps
// each writing role to the lineage group its admission recorded; a role with
// no entry or an empty one compares as unknown. The review's own lineage group
// is the snapshot's, which every review admission has recorded.
func AdmittedFactsOfReview(a ReviewAdmission, writerLineage map[RoleName]string) InvocationAdmissionFacts {
	facts := InvocationAdmissionFacts{
		InvocationID: a.InvocationID, Kind: AdmissionKindReview,
		Role: recorded(a.Role), AdmittedAt: a.AdmittedAt,
	}
	facts.withBinding(a.AgentBinding)
	facts.LineageGroup = recorded(a.LineageGroup)
	facts.Independence = make([]IndependenceEntry, 0, len(WritingRoles))
	for _, writer := range WritingRoles {
		facts.Independence = append(facts.Independence, IndependenceEntry{
			WritingRole: writer, Lineage: CompareLineage(a.LineageGroup, writerLineage[writer]),
		})
	}
	return facts
}

func (f *InvocationAdmissionFacts) withBinding(b AdmissionAgentBinding) {
	f.AgentDigest = recorded(b.AgentDigest)
	f.SelectionSource = recorded(b.SelectionSource)
	f.Harness = recorded(b.HarnessClient)
	f.Model = recorded(b.RouteModelID)
	f.RequestedEffort = recorded(b.RequestedEffort)
	f.NativeEffort = recorded(b.NativeEffort)
	f.CostOwner = recorded(b.CostOwner)
	f.LineageGroup = recorded(b.LineageGroup)
}

// recorded is a pointer to a recorded value, or nil for the empty value a
// record carries when it holds none.
func recorded[T comparable](value T) *T {
	var zero T
	if value == zero {
		return nil
	}
	return &value
}

// cloneAdmissionFacts detaches facts from the caller's pointers, so a record
// that holds them never shares a value the caller later edits.
func cloneAdmissionFacts(facts *InvocationAdmissionFacts) *InvocationAdmissionFacts {
	if facts == nil {
		return nil
	}
	cloned := *facts
	cloned.Role = clonePtr(facts.Role)
	cloned.AgentDigest = clonePtr(facts.AgentDigest)
	cloned.SelectionSource = clonePtr(facts.SelectionSource)
	cloned.Harness = clonePtr(facts.Harness)
	cloned.Model = clonePtr(facts.Model)
	cloned.RequestedEffort = clonePtr(facts.RequestedEffort)
	cloned.NativeEffort = clonePtr(facts.NativeEffort)
	cloned.CostOwner = clonePtr(facts.CostOwner)
	cloned.LineageGroup = clonePtr(facts.LineageGroup)
	cloned.CredentialMode = clonePtr(facts.CredentialMode)
	cloned.EgressProfile = clonePtr(facts.EgressProfile)
	cloned.OperatingMode = clonePtr(facts.OperatingMode)
	cloned.IsolationClass = clonePtr(facts.IsolationClass)
	cloned.Independence = slices.Clone(facts.Independence)
	return &cloned
}

// CompareAdmissionFacts orders facts by admission instant, then invocation id.
func CompareAdmissionFacts(a, b InvocationAdmissionFacts) int {
	return cmp.Or(a.AdmittedAt.Compare(b.AdmittedAt), cmp.Compare(a.InvocationID, b.InvocationID))
}

// Validate reports whether the facts are ones an admission record could
// carry: identified, of a known kind, with every present enum valid, and
// independence exactly on a review's facts.
func (f InvocationAdmissionFacts) Validate() error {
	fail := func(field string, value any) error {
		return fmt.Errorf("invocation %q admission facts %s %v: %w",
			f.InvocationID, field, value, ErrAdmissionFactsInconsistent)
	}
	switch {
	case f.InvocationID == "":
		return fmt.Errorf("invocation admission facts invocation_id: %w", ErrEmptyID)
	case !f.Kind.valid():
		return fail("kind", f.Kind)
	case f.AdmittedAt.IsZero() || f.AdmittedAt.Location() != time.UTC:
		return fail("admitted_at", f.AdmittedAt)
	case f.Role != nil && !f.Role.valid():
		return fail("role", *f.Role)
	case f.SelectionSource != nil && !f.SelectionSource.valid():
		return fail("selection_source", *f.SelectionSource)
	case f.Harness != nil && !f.Harness.valid():
		return fail("harness", *f.Harness)
	case f.RequestedEffort != nil && !f.RequestedEffort.valid():
		return fail("requested_effort", *f.RequestedEffort)
	case f.CredentialMode != nil && !f.CredentialMode.valid():
		return fail("credential_mode", *f.CredentialMode)
	case f.EgressProfile != nil && !f.EgressProfile.valid():
		return fail("egress_profile", *f.EgressProfile)
	case f.OperatingMode != nil && !f.OperatingMode.valid():
		return fail("operating_mode", *f.OperatingMode)
	case (f.Kind == AdmissionKindReview) != (f.Independence != nil):
		return fail("independence", f.Independence)
	}
	for _, entry := range f.Independence {
		if !entry.WritingRole.valid() || entry.Lineage.Validate() != nil {
			return fail("independence entry", entry)
		}
	}
	return nil
}
