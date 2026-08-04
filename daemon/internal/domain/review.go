package domain

import (
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ReviewRecord is the immutable authority record for one completed review
// pass (plan §7). Readiness consumes this exact base/head binding; a native
// forge review is deliberately not representable as a satisfying record.
type ReviewRecord struct {
	InvocationID        InvocationID  `json:"invocation_id"`
	RunID               RunID         `json:"run_id"`
	Round               int           `json:"round"`
	Provider            string        `json:"provider"`
	ModelConfiguration  string        `json:"model_configuration"`
	ConfigurationDigest Digest        `json:"configuration_digest"`
	CostOwner           string        `json:"cost_owner"`
	BaseSHA             string        `json:"base_sha"`
	HeadSHA             string        `json:"head_sha"`
	CompletedAt         time.Time     `json:"completed_at"`
	CompletionEvidence  Digest        `json:"completion_evidence"`
	Outcome             ReviewOutcome `json:"outcome"`
	FindingIDs          []FindingID   `json:"finding_ids"`
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
