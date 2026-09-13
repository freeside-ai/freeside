package domain

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
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
// the initial attempt is still in specification; approval fills it exactly once.
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
	Publication              json.RawMessage `json:"publication,omitempty"`
	ApprovedSpecDigest       Digest          `json:"approved_spec_digest,omitempty"`
	SpecificationRunID       RunID           `json:"specification_run_id"`
	ImplementationRunID      RunID           `json:"implementation_run_id"`
	OperatorCommandID        *string         `json:"operator_command_id,omitempty"`
	RetryOfInvocationID      *InvocationID   `json:"retry_of_invocation_id,omitempty"`
	CapabilityManifestDigest *Digest         `json:"capability_manifest_digest,omitempty"`
	// RevisesRunID and RevisionCommandID link a fresh campaign's initial attempt
	// back to the blocked implementation run whose agent_question was answered
	// with revise_specification, and to the answer_and_retry command that did so
	// (plan §5.12; #1083 D2). They are set together or not at all, and only on an
	// initial attempt. ParentRunID keeps its retry-only meaning: a revision is a
	// new campaign, not a retry within the blocked one. The store gate binds the
	// revised run to the same task and requires it to be a terminal
	// implementation run.
	RevisesRunID      *RunID  `json:"revises_run_id"`
	RevisionCommandID *string `json:"revision_command_id"`
}

// Validate reports whether the attempt's identity and lineage are coherent.
func (a ProductionAttempt) Validate() error {
	if a.CampaignID == "" || a.SpecificationRunID == "" || a.ImplementationRunID == "" {
		return fmt.Errorf("campaign, specification, and implementation ids are required: %w", ErrEmptyID)
	}
	if a.SpecificationRunID == a.ImplementationRunID {
		return fmt.Errorf("specification and implementation ids must differ: %w", ErrProductionAttemptInconsistent)
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
		if a.OperatorCommandID != nil || a.RetryOfInvocationID != nil || a.CapabilityManifestDigest != nil {
			return fmt.Errorf("initial attempt carries operator retry bindings: %w", ErrProductionAttemptInconsistent)
		}
	case ProductionAttemptRetry:
		if a.AttemptNumber < 2 || a.ApprovedSpecDigest == "" {
			return fmt.Errorf("retry attempt lacks retry ordinal or approved spec: %w", ErrProductionAttemptInconsistent)
		}
		bindingCount := 0
		if a.OperatorCommandID != nil {
			bindingCount++
			if *a.OperatorCommandID == "" {
				return fmt.Errorf("retry operator command id: %w", ErrEmptyID)
			}
		}
		if a.RetryOfInvocationID != nil {
			bindingCount++
			if *a.RetryOfInvocationID == "" {
				return fmt.Errorf("retry source invocation id: %w", ErrEmptyID)
			}
		}
		if a.CapabilityManifestDigest != nil {
			bindingCount++
			if !contentaddr.Valid(string(*a.CapabilityManifestDigest)) {
				return fmt.Errorf("retry capability manifest digest %q: %w",
					*a.CapabilityManifestDigest, ErrInvalidDigest)
			}
		}
		if bindingCount != 0 && bindingCount != 3 {
			return fmt.Errorf("retry carries partial operator bindings: %w", ErrProductionAttemptInconsistent)
		}
	}
	if err := validateRevisionLink(a.Kind, a.ImplementationRunID, a.RevisesRunID, a.RevisionCommandID); err != nil {
		return err
	}
	return nil
}

// validateRevisionLink checks the optional revise_specification link (#1083 D2).
// RevisesRunID and RevisionCommandID are set together or not at all, appear only
// on an initial attempt (a revision is a new campaign, not a retry), and the
// revised run differs from this attempt's implementation run. The store gate
// binds the revised run to the same task and to a terminal implementation run;
// those are cross-row checks that live there, not in this pure predicate.
func validateRevisionLink(kind ProductionAttemptKind, implementationRunID RunID, revisesRunID *RunID, revisionCommandID *string) error {
	if revisesRunID == nil && revisionCommandID == nil {
		return nil
	}
	if revisesRunID == nil || revisionCommandID == nil {
		return fmt.Errorf("revision link requires both revises_run_id and revision_command_id: %w", ErrProductionAttemptInconsistent)
	}
	if kind != ProductionAttemptInitial {
		return fmt.Errorf("revision link on %s attempt: %w", kind, ErrProductionAttemptInconsistent)
	}
	if *revisesRunID == "" {
		return fmt.Errorf("revises_run_id: %w", ErrEmptyID)
	}
	if *revisionCommandID == "" {
		return fmt.Errorf("revision_command_id: %w", ErrEmptyID)
	}
	if *revisesRunID == implementationRunID {
		return fmt.Errorf("revises_run_id equals implementation run id: %w", ErrProductionAttemptInconsistent)
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
