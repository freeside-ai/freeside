package domain

import (
	"fmt"
	"time"
)

// ReviewRequestRecord preserves the first request instant and exact candidate
// binding across retries and restarts. It is a fact, not launch authority.
type ReviewRequestRecord struct {
	InvocationID InvocationID `json:"invocation_id"`
	RunID        RunID        `json:"run_id"`
	Round        int          `json:"round"`
	BaseSHA      string       `json:"base_sha"`
	HeadSHA      string       `json:"head_sha"`
	RequestedAt  time.Time    `json:"requested_at"`
}

func (r ReviewRequestRecord) Validate() error {
	switch {
	case r.InvocationID == "" || r.RunID == "":
		return fmt.Errorf("review request identity: %w", ErrEmptyID)
	case r.Round < 1:
		return fmt.Errorf("review request round: %w", ErrNonPositive)
	case r.BaseSHA == "" || r.HeadSHA == "":
		return fmt.Errorf("review request binding: %w", ErrEmptyField)
	case r.RequestedAt.IsZero():
		return fmt.Errorf("review request time: %w", ErrMissingTimestamp)
	case r.RequestedAt.Location() != time.UTC:
		return fmt.Errorf("review request time: %w", ErrTimestampNotUTC)
	}
	return nil
}

type ReviewProgressState string

const (
	ReviewPending   ReviewProgressState = "pending"
	ReviewRunning   ReviewProgressState = "running"
	ReviewCompleted ReviewProgressState = "completed"
	ReviewFailed    ReviewProgressState = "failed"
)

var AllReviewProgressStates = []ReviewProgressState{ReviewPending, ReviewRunning, ReviewCompleted, ReviewFailed}

func (s ReviewProgressState) valid() bool {
	switch s {
	case ReviewPending, ReviewRunning, ReviewCompleted, ReviewFailed:
		return true
	default:
		return false
	}
}

type ReviewSourceKind string

const ReviewSourceFreesideInvoked ReviewSourceKind = "freeside_invoked"

var AllReviewSourceKinds = []ReviewSourceKind{ReviewSourceFreesideInvoked}

func (s ReviewSourceKind) valid() bool {
	switch s {
	case ReviewSourceFreesideInvoked:
		return true
	default:
		return false
	}
}

type ReviewEvidenceAvailability string

const (
	ReviewEvidenceAvailable   ReviewEvidenceAvailability = "available"
	ReviewEvidenceUnavailable ReviewEvidenceAvailability = "unavailable"
	ReviewEvidenceUnknown     ReviewEvidenceAvailability = "unknown"
)

var AllReviewEvidenceAvailabilities = []ReviewEvidenceAvailability{ReviewEvidenceAvailable, ReviewEvidenceUnavailable, ReviewEvidenceUnknown}

func (a ReviewEvidenceAvailability) valid() bool {
	switch a {
	case ReviewEvidenceAvailable, ReviewEvidenceUnavailable, ReviewEvidenceUnknown:
		return true
	default:
		return false
	}
}

type ReviewContentKind string

const ReviewContentReviewerOutput ReviewContentKind = "reviewer_output"

var AllReviewContentKinds = []ReviewContentKind{ReviewContentReviewerOutput}

func (k ReviewContentKind) valid() bool {
	switch k {
	case ReviewContentReviewerOutput:
		return true
	default:
		return false
	}
}
