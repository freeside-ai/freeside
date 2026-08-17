package domain

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ProductionAttemptKind distinguishes a campaign's content-addressed initial
// submission from an operator-requested retry of an already approved
// specification.
type ProductionAttemptKind string

const (
	ProductionAttemptInitial ProductionAttemptKind = "initial"
	ProductionAttemptRetry   ProductionAttemptKind = "retry"
)

// AllProductionAttemptKinds is the single registration point for production
// attempt kinds.
var AllProductionAttemptKinds = []ProductionAttemptKind{
	ProductionAttemptInitial,
	ProductionAttemptRetry,
}

func (k ProductionAttemptKind) valid() bool {
	switch k {
	case ProductionAttemptInitial, ProductionAttemptRetry:
		return true
	default:
		return false
	}
}

// ProductionAttempt is the durable, campaign-scoped identity and lineage of
// one production acceptance attempt. ApprovedSpecDigest is empty only while
// the initial attempt is still in elaboration; approval fills it exactly once.
type ProductionAttempt struct {
	CampaignID        CampaignID            `json:"campaign_id"`
	AttemptNumber     int                   `json:"attempt_number"`
	Kind              ProductionAttemptKind `json:"kind"`
	Reason            string                `json:"reason"`
	ParentRunID       RunID                 `json:"parent_run_id,omitempty"`
	SourceDigest      Digest                `json:"source_digest"`
	PublicationDigest Digest                `json:"publication_digest,omitempty"`
	// Publication is the exact daemon-authored metadata bytes admitted with an
	// initial attempt. It lets a delayed start reuse the approved publication
	// rather than silently recomputing it from later initiator configuration.
	Publication         json.RawMessage `json:"publication,omitempty"`
	ApprovedSpecDigest  Digest          `json:"approved_spec_digest,omitempty"`
	ElaborationRunID    RunID           `json:"elaboration_run_id"`
	ImplementationRunID RunID           `json:"implementation_run_id"`
}

// Validate reports whether the attempt's identity and lineage are coherent.
func (a ProductionAttempt) Validate() error {
	if a.CampaignID == "" || a.ElaborationRunID == "" || a.ImplementationRunID == "" {
		return fmt.Errorf("campaign, elaboration, and implementation ids are required: %w", ErrEmptyID)
	}
	if a.ElaborationRunID == a.ImplementationRunID {
		return fmt.Errorf("elaboration and implementation ids must differ: %w", ErrProductionAttemptInconsistent)
	}
	if a.SourceDigest == "" {
		return fmt.Errorf("source_digest: %w", ErrEmptyField)
	}
	if !a.Kind.valid() {
		return fmt.Errorf("kind %q: %w", a.Kind, ErrInvalidProductionAttemptKind)
	}
	if err := validateProductionLineage(a.CampaignID, a.AttemptNumber, a.Reason, a.ParentRunID); err != nil {
		return err
	}
	switch a.Kind {
	case ProductionAttemptInitial:
		if a.AttemptNumber != 1 {
			return fmt.Errorf("initial attempt number %d: %w", a.AttemptNumber, ErrProductionAttemptInconsistent)
		}
	case ProductionAttemptRetry:
		if a.AttemptNumber < 2 || a.ApprovedSpecDigest == "" {
			return fmt.Errorf("retry attempt lacks retry ordinal or approved spec: %w", ErrProductionAttemptInconsistent)
		}
	}
	return nil
}

func validateProductionLineage(campaignID CampaignID, attemptNumber int, reason string, parentRunID RunID) error {
	if campaignID == "" {
		if attemptNumber != 0 || reason != "" || parentRunID != "" {
			return fmt.Errorf("legacy run carries partial attempt lineage: %w", ErrProductionAttemptInconsistent)
		}
		return nil
	}
	if attemptNumber < 1 {
		return fmt.Errorf("attempt number %d: %w", attemptNumber, ErrNonPositive)
	}
	if attemptNumber == 1 {
		if reason != "" || parentRunID != "" {
			return fmt.Errorf("initial attempt carries retry lineage: %w", ErrProductionAttemptInconsistent)
		}
		return nil
	}
	if parentRunID == "" || strings.TrimSpace(reason) == "" || reason != strings.TrimSpace(reason) {
		return fmt.Errorf("retry requires a parent and trimmed reason: %w", ErrProductionAttemptInconsistent)
	}
	return nil
}
