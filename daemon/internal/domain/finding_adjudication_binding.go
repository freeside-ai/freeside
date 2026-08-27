package domain

import (
	"fmt"
	"slices"
)

// FindingAdjudicationBinding is the sync-carried projection of one immutable
// adjudication artifact. The store re-gates the copied proposal axes against
// that artifact before the item may persist.
type FindingAdjudicationBinding struct {
	RunID              RunID                         `json:"run_id"`
	Round              int                           `json:"round"`
	AdjudicationDigest Digest                        `json:"adjudication_digest"`
	Proposals          []FindingAdjudicationProposal `json:"proposals"`
}

// FindingAdjudicationProposal presents one recommended route and the typed
// alternatives the operator may choose instead.
type FindingAdjudicationProposal struct {
	FindingID           FindingID               `json:"finding_id"`
	Producer            AdjudicationProducer    `json:"producer"`
	GoalRelationship    GoalRelationship        `json:"goal_relationship"`
	Compatibility       *WorkUnitCompatibility  `json:"compatibility"`
	Route               AdjudicationRoute       `json:"route"`
	Rationale           string                  `json:"rationale"`
	CitedRules          []string                `json:"cited_rules"`
	Assumptions         []string                `json:"assumptions"`
	OpenQuestions       []string                `json:"open_questions"`
	Confidence          *AdjudicationConfidence `json:"confidence"`
	OfferedAlternatives []OfferedAlternative    `json:"offered_alternatives"`
}

// OfferedAlternative is an executable route other than the recommendation,
// paired with the consequence the operator is choosing.
type OfferedAlternative struct {
	Route       AdjudicationRoute `json:"route"`
	Consequence string            `json:"consequence"`
}

// Validate requires a non-empty, uniquely keyed proposal set whose axes obey
// the artifact contract and whose alternatives are valid, distinct choices.
func (b FindingAdjudicationBinding) Validate() error {
	if b.RunID == "" {
		return fmt.Errorf("finding adjudication binding run_id: %w", ErrEmptyID)
	}
	if b.Round < 1 {
		return fmt.Errorf("finding adjudication binding round %d: %w", b.Round, ErrNonPositive)
	}
	if b.AdjudicationDigest == "" {
		return fmt.Errorf("finding adjudication binding digest: %w", ErrEmptyField)
	}
	if len(b.Proposals) == 0 {
		return fmt.Errorf("finding adjudication binding proposals: %w", ErrEmptyField)
	}
	seenFindings := make(map[FindingID]struct{}, len(b.Proposals))
	for _, proposal := range b.Proposals {
		if _, duplicate := seenFindings[proposal.FindingID]; duplicate {
			return fmt.Errorf("finding adjudication proposal %q: %w", proposal.FindingID, ErrDuplicate)
		}
		seenFindings[proposal.FindingID] = struct{}{}
		// The proposal is a projection of one artifact entry; reconstruct that
		// entry — including its digest-bound OfferedAlternatives — and delegate to
		// the entry's own validation so the offered-set rules live in one place
		// (#893). The store re-gates the copied fields against the artifact.
		entry := FindingAdjudicationEntry{
			FindingID: proposal.FindingID, Producer: proposal.Producer,
			GoalRelationship: proposal.GoalRelationship, Compatibility: proposal.Compatibility,
			Route: proposal.Route, Rationale: proposal.Rationale,
			CitedRules: proposal.CitedRules, Assumptions: proposal.Assumptions,
			OpenQuestions: proposal.OpenQuestions, Confidence: proposal.Confidence,
			OfferedAlternatives: proposal.OfferedAlternatives,
		}
		if err := entry.Validate(); err != nil {
			return fmt.Errorf("finding adjudication proposal %q: %w", proposal.FindingID, err)
		}
	}
	return nil
}

func (b FindingAdjudicationBinding) hasOfferedAlternative() bool {
	for _, proposal := range b.Proposals {
		if len(proposal.OfferedAlternatives) > 0 {
			return true
		}
	}
	return false
}

func cloneFindingAdjudicationBinding(binding *FindingAdjudicationBinding) *FindingAdjudicationBinding {
	if binding == nil {
		return nil
	}
	cloned := *binding
	cloned.Proposals = make([]FindingAdjudicationProposal, len(binding.Proposals))
	for idx, proposal := range binding.Proposals {
		cloned.Proposals[idx] = proposal
		cloned.Proposals[idx].Compatibility = clonePtr(proposal.Compatibility)
		cloned.Proposals[idx].Confidence = clonePtr(proposal.Confidence)
		cloned.Proposals[idx].CitedRules = slices.Clone(proposal.CitedRules)
		cloned.Proposals[idx].Assumptions = slices.Clone(proposal.Assumptions)
		cloned.Proposals[idx].OpenQuestions = slices.Clone(proposal.OpenQuestions)
		cloned.Proposals[idx].OfferedAlternatives = slices.Clone(proposal.OfferedAlternatives)
	}
	return &cloned
}
