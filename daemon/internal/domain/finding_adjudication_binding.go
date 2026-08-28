package domain

import (
	"fmt"
	"slices"
	"unicode/utf8"
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
// alternatives the operator may choose instead. It also carries the finding
// context §9 requires the operator to see before committing a durable route:
// the daemon-authenticated coordinates (FindingMessage, FindingLocation) copied
// from the immutable stored Finding, and the Evidence copied from the matching
// artifact entry — daemon-derived for the engine producer's fast path, model-
// derived otherwise (see Evidence). The store re-gates every copied field
// against its immutable source at persistence and on actionable reconstruction
// (#892).
type FindingAdjudicationProposal struct {
	FindingID FindingID `json:"finding_id"`
	// FindingMessage is the whitespace-normalized finding message and
	// FindingLocation the finding's source coordinates, both copied from the
	// immutable stored Finding (message via NormalizeFindingMessage, location
	// as-is). They are daemon-authenticated facts with no artifact-entry
	// counterpart, rendered in the daemon-fact register. FindingLocation is nil
	// for a review-level finding that carries no path.
	FindingMessage   string                 `json:"finding_message"`
	FindingLocation  *FindingLocation       `json:"finding_location"`
	Producer         AdjudicationProducer   `json:"producer"`
	GoalRelationship GoalRelationship       `json:"goal_relationship"`
	Compatibility    *WorkUnitCompatibility `json:"compatibility"`
	Route            AdjudicationRoute      `json:"route"`
	Rationale        string                 `json:"rationale"`
	// Evidence is the artifact-digest-bound evidence copied from the matching
	// FindingAdjudicationEntry. Its provenance follows Producer: the daemon fact
	// the engine fast path derives (the finding's own containment location) for
	// AdjudicationProducerEngine, model-authored free text otherwise. It renders
	// in that same producer's register, never presented as the other's.
	Evidence            []string                `json:"evidence"`
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
		// entry — including its digest-bound OfferedAlternatives and Evidence — and
		// delegate to the entry's own validation so the offered-set rules and the
		// evidence UTF-8 guard live in one place (#893, #892). The store re-gates
		// the copied fields against the artifact and the stored Finding.
		entry := FindingAdjudicationEntry{
			FindingID: proposal.FindingID, Producer: proposal.Producer,
			GoalRelationship: proposal.GoalRelationship, Compatibility: proposal.Compatibility,
			Route: proposal.Route, Rationale: proposal.Rationale, Evidence: proposal.Evidence,
			CitedRules: proposal.CitedRules, Assumptions: proposal.Assumptions,
			OpenQuestions: proposal.OpenQuestions, Confidence: proposal.Confidence,
			OfferedAlternatives: proposal.OfferedAlternatives,
		}
		if err := entry.Validate(); err != nil {
			return fmt.Errorf("finding adjudication proposal %q: %w", proposal.FindingID, err)
		}
		// FindingMessage and FindingLocation are daemon-authenticated coordinates
		// with no artifact-entry counterpart, so they are structurally validated
		// here; the store re-gates their authenticity against the stored Finding.
		// Emptiness is deliberately not rejected: an empty finding message is a
		// handled degenerate case elsewhere (an unfingerprintable finding is
		// carried, not refused), so the projection matches whatever the stored
		// Finding holds and the store re-gate is the sole authority on its content.
		if !utf8.ValidString(proposal.FindingMessage) {
			return fmt.Errorf("finding adjudication proposal %q message: %w",
				proposal.FindingID, ErrFindingAdjudicationInconsistent)
		}
		if proposal.FindingLocation != nil {
			if err := proposal.FindingLocation.Validate(); err != nil {
				return fmt.Errorf("finding adjudication proposal %q: %w", proposal.FindingID, err)
			}
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
		cloned.Proposals[idx].FindingLocation = clonePtr(proposal.FindingLocation)
		cloned.Proposals[idx].Evidence = slices.Clone(proposal.Evidence)
		cloned.Proposals[idx].Compatibility = clonePtr(proposal.Compatibility)
		cloned.Proposals[idx].Confidence = clonePtr(proposal.Confidence)
		cloned.Proposals[idx].CitedRules = slices.Clone(proposal.CitedRules)
		cloned.Proposals[idx].Assumptions = slices.Clone(proposal.Assumptions)
		cloned.Proposals[idx].OpenQuestions = slices.Clone(proposal.OpenQuestions)
		cloned.Proposals[idx].OfferedAlternatives = slices.Clone(proposal.OfferedAlternatives)
	}
	return &cloned
}
