package domain

import "fmt"

// The closure outcome matrix (plan revision 68, #1487). ClosureOutcomeFor is the
// single pure function the engine and the publisher (#1419 Part D) both consume,
// so the source-issue-closure truth table lives in one tested place instead of
// two copies of drifting prose. It maps what the daemon knows about a proposal
// (its provenance, whether it resolves, its origin), the project's human-gate
// setting, and the approval or decision that binds the current merge, onto three
// independent facts: how the pull request references the source issue, whether
// the merge is held, and which attention item (if any) is open.
//
// The reduction from durable state to the ClosureApprovalState input belongs to
// the caller: a stale approval (one AuthorizesClose rejects for the current
// merge) or a decision on a superseded item is reduced to none before it reaches
// here. This function judges only the state it is handed.

// ClosureReference is how a merged pull request refers to the source issue: it
// closes the issue, merely references it, or carries only a descriptive link
// (the no-source case). The zero value is invalid.
type ClosureReference string

const (
	ClosureReferenceCloses          ClosureReference = "closes"
	ClosureReferenceRefs            ClosureReference = "refs"
	ClosureReferenceDescriptiveLink ClosureReference = "descriptive_link"
)

// AllClosureReferences is the single registration point for the reference kind.
var AllClosureReferences = []ClosureReference{
	ClosureReferenceCloses, ClosureReferenceRefs, ClosureReferenceDescriptiveLink,
}

func (r ClosureReference) valid() bool {
	switch r {
	case ClosureReferenceCloses, ClosureReferenceRefs, ClosureReferenceDescriptiveLink:
		return true
	default:
		return false
	}
}

// ClosureHold is whether the closure outcome holds the pull request: none when
// nothing about the source issue blocks the merge, draft while an open human
// gate has not been decided. The zero value is invalid.
type ClosureHold string

const (
	ClosureHoldNone  ClosureHold = "none"
	ClosureHoldDraft ClosureHold = "draft"
)

// AllClosureHolds is the single registration point for the hold kind.
var AllClosureHolds = []ClosureHold{ClosureHoldNone, ClosureHoldDraft}

func (h ClosureHold) valid() bool {
	switch h {
	case ClosureHoldNone, ClosureHoldDraft:
		return true
	default:
		return false
	}
}

// ClosureItem is which attention item the closure outcome opens: none, the
// non-holding fallback_notice a default-policy daemon_fallback shows until a
// person acts on it, or the planned_gate an open human gate holds until decided.
// The zero value is invalid.
type ClosureItem string

const (
	ClosureItemNone           ClosureItem = "none"
	ClosureItemFallbackNotice ClosureItem = "fallback_notice"
	ClosureItemPlannedGate    ClosureItem = "planned_gate"
)

// AllClosureItems is the single registration point for the item kind.
var AllClosureItems = []ClosureItem{ClosureItemNone, ClosureItemFallbackNotice, ClosureItemPlannedGate}

func (i ClosureItem) valid() bool {
	switch i {
	case ClosureItemNone, ClosureItemFallbackNotice, ClosureItemPlannedGate:
		return true
	default:
		return false
	}
}

// ClosureApprovalState is the approval or decision that binds the current merge,
// reduced by the caller to one of four states: none (undecided, or a prior
// approval no longer bound to the current merge), declined, a bound human
// approval, or a bound policy approval. It names who, if anyone, has approved for
// the merge in front of the daemon now. The zero value is invalid.
type ClosureApprovalState string

const (
	ClosureApprovalStateNone     ClosureApprovalState = "none"
	ClosureApprovalStateDeclined ClosureApprovalState = "declined"
	ClosureApprovalStateHuman    ClosureApprovalState = "human"
	ClosureApprovalStatePolicy   ClosureApprovalState = "policy"
)

// AllClosureApprovalStates is the single registration point for the state.
var AllClosureApprovalStates = []ClosureApprovalState{
	ClosureApprovalStateNone, ClosureApprovalStateDeclined,
	ClosureApprovalStateHuman, ClosureApprovalStatePolicy,
}

func (s ClosureApprovalState) valid() bool {
	switch s {
	case ClosureApprovalStateNone, ClosureApprovalStateDeclined,
		ClosureApprovalStateHuman, ClosureApprovalStatePolicy:
		return true
	default:
		return false
	}
}

// ClosureOutcome is the resolved reference, hold, and item for one closure
// proposal against one merge, plus whether the proposal's provenance is
// recommended. Recommended is orthogonal to the other three: it never changes
// the reference, hold, or item, and only tells the publisher to mark the Source
// issue line (plan revision 68 item 1).
type ClosureOutcome struct {
	Reference   ClosureReference `json:"reference"`
	Hold        ClosureHold      `json:"hold"`
	Item        ClosureItem      `json:"item"`
	Recommended bool             `json:"recommended"`
}

// ClosureOutcomeInput is what ClosureOutcomeFor reduces to an outcome. A nil
// Proposal is the no-source row (a cross-repository or absent source): it yields
// a descriptive link with no hold and no item. HumanGate is the project's gate
// setting; Approval is the caller-reduced state for the current merge.
type ClosureOutcomeInput struct {
	Proposal  *SourceIssueClosureParameters
	HumanGate bool
	Approval  ClosureApprovalState
}

// ClosureOutcomeFor maps the input to its outcome. It is total over valid
// inputs: an invalid approval state or an invalid proposal is an error, and a
// nil proposal is the descriptive-link no-source outcome. Every other valid
// input resolves through the three independent reductions below.
func ClosureOutcomeFor(in ClosureOutcomeInput) (ClosureOutcome, error) {
	if !in.Approval.valid() {
		return ClosureOutcome{}, fmt.Errorf("closure outcome approval %q: %w", in.Approval, ErrClosureOutcomeInconsistent)
	}
	if in.Proposal == nil {
		// No closable source: the pull request carries only a descriptive link and
		// nothing holds or items. Provenance does not apply, so it is not marked.
		return ClosureOutcome{
			Reference: ClosureReferenceDescriptiveLink, Hold: ClosureHoldNone, Item: ClosureItemNone,
		}, nil
	}
	if err := in.Proposal.Validate(); err != nil {
		return ClosureOutcome{}, err
	}
	proposal := *in.Proposal
	item := closureItem(in.Approval, in.HumanGate, proposal.Origin)
	return ClosureOutcome{
		Reference:   closureReference(in.Approval, proposal.Resolves),
		Hold:        closureHold(item),
		Item:        item,
		Recommended: proposal.Provenance == ClosureProvenanceRecommended,
	}, nil
}

// closureReference resolves how the merge references the source issue. A merged
// pull request closes the issue only when a binding approval (human or policy,
// whoever recorded it) covers a proposal that resolves; every other decided or
// undecided state, and every non-resolving proposal, only references it. The
// switch dispatches on the state and omits default; the trailing return guards
// the invalid zero value the caller already rejected.
func closureReference(approval ClosureApprovalState, resolves bool) ClosureReference {
	switch approval {
	case ClosureApprovalStateNone, ClosureApprovalStateDeclined:
		return ClosureReferenceRefs
	case ClosureApprovalStateHuman, ClosureApprovalStatePolicy:
		if resolves {
			return ClosureReferenceCloses
		}
		return ClosureReferenceRefs
	}
	return ClosureReferenceRefs
}

// closureItem resolves which attention item is open. Once anyone has acted (an
// approval or a decline binds the merge) no item is open. While the merge is
// unacted, an open human gate holds it as a planned gate, whatever the origin;
// without the gate, only a default-policy daemon_fallback shows the non-holding
// fallback notice, and a propose_site proposal shows nothing. The origin switch
// dispatches and omits default; the trailing return guards the invalid zero
// value the proposal's Validate already rejected.
func closureItem(approval ClosureApprovalState, gate bool, origin ClosureFlagOrigin) ClosureItem {
	if approval != ClosureApprovalStateNone {
		return ClosureItemNone
	}
	if gate {
		return ClosureItemPlannedGate
	}
	switch origin {
	case ClosureFlagOriginProposeSite:
		return ClosureItemNone
	case ClosureFlagOriginDaemonFallback:
		return ClosureItemFallbackNotice
	}
	return ClosureItemNone
}

// closureHold resolves whether the merge is held. Only an open, undecided human
// gate (the planned_gate item) holds the pull request as a draft; the fallback
// notice never holds, and no item never holds. The switch dispatches on the item
// and omits default; the trailing return guards the invalid zero value.
func closureHold(item ClosureItem) ClosureHold {
	switch item {
	case ClosureItemPlannedGate:
		return ClosureHoldDraft
	case ClosureItemNone, ClosureItemFallbackNotice:
		return ClosureHoldNone
	}
	return ClosureHoldNone
}
