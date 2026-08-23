package domain

import (
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ReviewRecord is the immutable authority record for one completed review
// pass (plan §7). Readiness consumes its exact base, head, configuration, and
// instruction binding; a native forge review is deliberately not representable
// as a satisfying record.
type ReviewRecord struct {
	InvocationID        InvocationID  `json:"invocation_id"`
	RunID               RunID         `json:"run_id"`
	Round               int           `json:"round"`
	Provider            string        `json:"provider"`
	ModelConfiguration  string        `json:"model_configuration"`
	ConfigurationDigest Digest        `json:"configuration_digest"`
	InstructionDigest   Digest        `json:"instruction_digest"`
	CostOwner           string        `json:"cost_owner"`
	BaseSHA             string        `json:"base_sha"`
	HeadSHA             string        `json:"head_sha"`
	CompletedAt         time.Time     `json:"completed_at"`
	CompletionEvidence  Digest        `json:"completion_evidence"`
	Outcome             ReviewOutcome `json:"outcome"`
	FindingIDs          []FindingID   `json:"finding_ids"`
}

// ReviewDispositionRecord is the immutable per-round outcome for one raw
// review finding (plan §7). A later round writes another record; it never
// rewrites the finding or an earlier disposition.
type ReviewDispositionRecord struct {
	FindingID               FindingID         `json:"finding_id"`
	RunID                   RunID             `json:"run_id"`
	Round                   int               `json:"round"`
	Disposition             ReviewDisposition `json:"disposition"`
	Reason                  string            `json:"reason"`
	AdjudicationDigest      Digest            `json:"adjudication_digest,omitempty"`
	RemediationInvocationID InvocationID      `json:"remediation_invocation_id,omitempty"`
	CreatedAt               time.Time         `json:"created_at"`
}

// Validate reports whether the disposition is identified, round-bound,
// explained, and stamped with a stable UTC time. Store reconstruction binds
// it to the named finding and the review pass that produced that finding.
func (r ReviewDispositionRecord) Validate() error {
	switch {
	case r.FindingID == "" || r.RunID == "":
		return fmt.Errorf("review disposition identity: %w", ErrEmptyID)
	case r.Round < 1:
		return fmt.Errorf("review disposition round %d: %w", r.Round, ErrNonPositive)
	case !r.Disposition.valid():
		return fmt.Errorf("review disposition %q: %w", r.Disposition, ErrInvalidReviewDisposition)
	case r.Reason == "":
		return fmt.Errorf("review disposition reason: %w", ErrEmptyField)
	case r.Disposition != ReviewDispositionFixed && r.AdjudicationDigest == "":
		return fmt.Errorf("final review disposition adjudication binding: %w", ErrEmptyField)
	case r.Disposition != ReviewDispositionFixed && !contentaddr.Valid(string(r.AdjudicationDigest)):
		return fmt.Errorf("final review disposition adjudication binding %q: %w",
			r.AdjudicationDigest, ErrInvalidDispositionAdjudication)
	case r.Disposition == ReviewDispositionFixed && r.AdjudicationDigest != "":
		return fmt.Errorf("fixed review disposition adjudication binding: %w", ErrInvalidDispositionAdjudication)
	case r.Disposition == ReviewDispositionFixed && r.RemediationInvocationID == "":
		return fmt.Errorf("fixed review disposition remediation binding: %w", ErrEmptyField)
	case r.Disposition != ReviewDispositionFixed && r.RemediationInvocationID != "":
		return fmt.Errorf("non-fixed review disposition remediation binding: %w", ErrInvalidHeadBinding)
	case r.CreatedAt.IsZero():
		return fmt.Errorf("review disposition created_at: %w", ErrMissingTimestamp)
	case r.CreatedAt.Location() != time.UTC:
		return fmt.Errorf("review disposition created_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

// Validate reports whether a review record is identified, attributable,
// exact-base/head bound, completed, and internally consistent.
func (r ReviewRecord) Validate() error {
	switch {
	case r.InvocationID == "":
		return fmt.Errorf("review record invocation_id: %w", ErrEmptyID)
	case r.RunID == "":
		return fmt.Errorf("review record run_id: %w", ErrEmptyID)
	case r.Round < 1:
		return fmt.Errorf("review record round %d: %w", r.Round, ErrNonPositive)
	case r.Provider == "":
		return fmt.Errorf("review record provider: %w", ErrEmptyField)
	case r.ModelConfiguration == "":
		return fmt.Errorf("review record model_configuration: %w", ErrEmptyField)
	case !contentaddr.Valid(string(r.ConfigurationDigest)):
		return fmt.Errorf("review record configuration_digest %q: %w",
			r.ConfigurationDigest, ErrInvalidReviewCompletionEvidence)
	case !contentaddr.Valid(string(r.InstructionDigest)):
		return fmt.Errorf("review record instruction_digest %q: %w",
			r.InstructionDigest, ErrInvalidReviewCompletionEvidence)
	case r.CostOwner == "":
		return fmt.Errorf("review record cost_owner: %w", ErrEmptyField)
	case r.BaseSHA == "":
		return fmt.Errorf("review record base_sha: %w", ErrEmptyField)
	case r.HeadSHA == "":
		return fmt.Errorf("review record head_sha: %w", ErrEmptyField)
	case r.CompletedAt.IsZero():
		return fmt.Errorf("review record completion time: %w", ErrMissingTimestamp)
	case r.CompletedAt.Location() != time.UTC:
		return fmt.Errorf("review record completion time: %w", ErrTimestampNotUTC)
	case !contentaddr.Valid(string(r.CompletionEvidence)):
		return fmt.Errorf("review record completion_evidence %q: %w",
			r.CompletionEvidence, ErrInvalidReviewCompletionEvidence)
	case !r.Outcome.valid():
		return fmt.Errorf("review record outcome %q: %w", r.Outcome, ErrInvalidReviewOutcome)
	}
	if r.FindingIDs != nil && len(r.FindingIDs) == 0 {
		return fmt.Errorf("review record finding_ids: empty list must be nil: %w", ErrFindingsNotCanonical)
	}
	for i, id := range r.FindingIDs {
		if id == "" {
			return fmt.Errorf("review record finding_ids[%d]: %w", i, ErrEmptyID)
		}
		if i > 0 && id <= r.FindingIDs[i-1] {
			return fmt.Errorf("review record finding_ids: %w", ErrFindingsNotCanonical)
		}
	}
	if (r.Outcome == ReviewClean) != (len(r.FindingIDs) == 0) {
		return fmt.Errorf("review record outcome disagrees with findings: %w", ErrInvalidReviewOutcome)
	}
	return nil
}

// NewReviewRecord constructs a detached record with canonical finding IDs.
func NewReviewRecord(record ReviewRecord) (ReviewRecord, error) {
	record.CompletedAt = record.CompletedAt.UTC()
	record.FindingIDs = slices.Clone(record.FindingIDs)
	slices.Sort(record.FindingIDs)
	record.FindingIDs = slices.Compact(record.FindingIDs)
	if len(record.FindingIDs) == 0 {
		record.FindingIDs = nil
	}
	if err := record.Validate(); err != nil {
		return ReviewRecord{}, err
	}
	return record, nil
}

// ReviewFailure is the durable routing account for a pass that produced no
// ReviewRecord. A contradiction is still recorded before the engine returns
// it loudly, so restart and audit do not lose the failed invocation.
type ReviewFailure struct {
	InvocationID InvocationID       `json:"invocation_id"`
	RunID        RunID              `json:"run_id"`
	Round        int                `json:"round"`
	BaseSHA      string             `json:"base_sha"`
	HeadSHA      string             `json:"head_sha"`
	Class        ReviewFailureClass `json:"class"`
	Reason       string             `json:"reason"`
	ObservedAt   time.Time          `json:"observed_at"`
}

func (f ReviewFailure) Validate() error {
	switch {
	case f.InvocationID == "" || f.RunID == "":
		return fmt.Errorf("review failure identity: %w", ErrEmptyID)
	case f.Round < 1:
		return fmt.Errorf("review failure round %d: %w", f.Round, ErrNonPositive)
	case f.BaseSHA == "" || f.HeadSHA == "" || f.Reason == "":
		return fmt.Errorf("review failure binding: %w", ErrEmptyField)
	case !f.Class.valid():
		return fmt.Errorf("review failure class %q: %w", f.Class, ErrInvalidReviewFailureClass)
	case f.ObservedAt.IsZero():
		return fmt.Errorf("review failure observed_at: %w", ErrMissingTimestamp)
	case f.ObservedAt.Location() != time.UTC:
		return fmt.Errorf("review failure observed_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

// ReviewRetry is the durable, mutable current-state of a pending
// same-invocation transient retry (plan §7; issue #498). A transient
// request/inspect/poll/verification failure does not terminalize the
// invocation, so its retry deadline lives only in process memory; this row
// records it durably so a daemon restart during the backoff reconstructs the
// remaining delay instead of retrying immediately. At most one is live per
// run, keyed by RunID; a new round or invocation overwrites it, and any
// superseding outcome clears it.
//
// The row is a delay claim, never authority. The deadline is derived, never
// stored: ObservedAt + delay(Round). The engine re-derives it and re-binds to
// the current candidate at the gate, so a decoded row can postpone a retry but
// can never authorize skipping backoff, changing the invocation, or advancing
// the round.
type ReviewRetry struct {
	RunID        RunID        `json:"run_id"`
	InvocationID InvocationID `json:"invocation_id"`
	Round        int          `json:"round"`
	BaseSHA      string       `json:"base_sha"`
	HeadSHA      string       `json:"head_sha"`
	ObservedAt   time.Time    `json:"observed_at"`
	Reason       string       `json:"reason"`
}

func (r ReviewRetry) Validate() error {
	switch {
	case r.RunID == "" || r.InvocationID == "":
		return fmt.Errorf("review retry identity: %w", ErrEmptyID)
	case r.Round < 1:
		return fmt.Errorf("review retry round %d: %w", r.Round, ErrNonPositive)
	case r.BaseSHA == "" || r.HeadSHA == "" || r.Reason == "":
		return fmt.Errorf("review retry binding: %w", ErrEmptyField)
	case r.ObservedAt.IsZero():
		return fmt.Errorf("review retry observed_at: %w", ErrMissingTimestamp)
	case r.ObservedAt.Location() != time.UTC:
		return fmt.Errorf("review retry observed_at: %w", ErrTimestampNotUTC)
	}
	return nil
}
