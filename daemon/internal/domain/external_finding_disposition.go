package domain

import (
	"fmt"
	"time"
)

// ExternalFindingDisposition is the immutable outcome for one admitted
// external finding (plan §5.19, §7): a finding an admitted reviewer outside
// Freeside left on a published pull request, dispositioned in the first round
// of the external review cycle that answers it.
//
// It is a separate type from ReviewDispositionRecord because that record
// proves its finding came from the round's review record, and an external
// finding never joins one. Nothing that reads review dispositions (review
// completeness, convergence, the drift audit, the publication history) reads
// this record.
//
// A declined or deferred outcome names the adjudication that decided it. A
// fixed outcome names the review record of the remediated head and claims no
// more than that: a later round of the same run reviewed another head on the
// same base. It does not say the finding's fingerprint was proven absent
// there. The binding an outcome does not carry renders an explicit null.
type ExternalFindingDisposition struct {
	FindingID               FindingID         `json:"finding_id"`
	RunID                   RunID             `json:"run_id"`
	Round                   int               `json:"round"`
	Disposition             ReviewDisposition `json:"disposition"`
	Reason                  string            `json:"reason"`
	AdjudicationDigest      *Digest           `json:"adjudication_digest"`
	RemediationInvocationID *InvocationID     `json:"remediation_invocation_id"`
	CreatedAt               time.Time         `json:"created_at"`
}

// Adjudication returns the adjudication a declined or deferred outcome
// names, and "" for a fixed one.
func (d ExternalFindingDisposition) Adjudication() Digest {
	if d.AdjudicationDigest == nil {
		return ""
	}
	return *d.AdjudicationDigest
}

// Remediation returns the review record a fixed outcome names, and "" for a
// declined or deferred one.
func (d ExternalFindingDisposition) Remediation() InvocationID {
	if d.RemediationInvocationID == nil {
		return ""
	}
	return *d.RemediationInvocationID
}

// Validate applies the review disposition's rules: the two records differ in
// which findings they may name, which the store decides, and in how an
// absent binding renders. The positional literal compiles only while it
// names every field of that record, so a field added there forces a decision
// here. A binding that is set is never empty: null is the one way to say it
// is absent.
func (d ExternalFindingDisposition) Validate() error {
	if (d.AdjudicationDigest != nil && *d.AdjudicationDigest == "") ||
		(d.RemediationInvocationID != nil && *d.RemediationInvocationID == "") {
		return fmt.Errorf("external finding disposition binding set but empty: %w", ErrEmptyField)
	}
	record := ReviewDispositionRecord{
		d.FindingID, d.RunID, d.Round, d.Disposition, d.Reason,
		d.Adjudication(), d.Remediation(), d.CreatedAt,
	}
	if err := record.Validate(); err != nil {
		return fmt.Errorf("external finding disposition: %w", err)
	}
	return nil
}
