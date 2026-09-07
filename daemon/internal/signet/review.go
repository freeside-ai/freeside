package signet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type ReviewSource struct {
	Kind   domain.ReviewSourceKind `json:"kind"`
	Status *string                 `json:"status"`
}

type RunReviewFacts struct {
	Rounds []RunReviewRound `json:"rounds"`
}

type ReviewRoundDispositions struct {
	Fixed    int `json:"fixed"`
	Declined int `json:"declined"`
	Deferred int `json:"deferred"`
	Open     int `json:"open"`
}

type ReviewRoundFailure struct {
	Class  domain.ReviewFailureClass `json:"class"`
	Reason string                    `json:"reason"`
}

type ReviewRoundEvidence struct {
	CompletionEvidence *domain.Digest                    `json:"completion_evidence"`
	Availability       domain.ReviewEvidenceAvailability `json:"availability"`
}

// RunReviewRound contains daemon-held facts. Retained reviewer output is
// separately fetched and cannot supply any of these facts.
type RunReviewRound struct {
	Round              int                        `json:"round"`
	InvocationID       domain.InvocationID        `json:"invocation_id"`
	State              domain.ReviewProgressState `json:"state"`
	Source             ReviewSource               `json:"source"`
	BaseSHA            string                     `json:"base_sha"`
	HeadSHA            string                     `json:"head_sha"`
	RequestedAt        *time.Time                 `json:"requested_at"`
	CompletedAt        *time.Time                 `json:"completed_at"`
	Provider           *string                    `json:"provider"`
	ModelConfiguration *string                    `json:"model_configuration"`
	Outcome            *domain.ReviewOutcome      `json:"outcome"`
	FindingsCount      *int                       `json:"findings_count"`
	Dispositions       *ReviewRoundDispositions   `json:"dispositions"`
	Failure            *ReviewRoundFailure        `json:"failure"`
	RetryPending       bool                       `json:"retry_pending"`
	Evidence           ReviewRoundEvidence        `json:"evidence"`
}

func reviewSource() ReviewSource { return ReviewSource{Kind: domain.ReviewSourceFreesideInvoked} }

func runReviewFacts(ctx context.Context, tx *store.ReadTx, runID domain.RunID, observations []domain.InvocationObservation) (*RunReviewFacts, error) {
	requests, err := tx.ListReviewRequests(ctx, runID)
	if err != nil {
		return nil, err
	}
	records, err := tx.ListReviewRecords(ctx, runID)
	if err != nil {
		return nil, err
	}
	failures, err := tx.ListReviewFailures(ctx, runID)
	if err != nil {
		return nil, err
	}
	dispositions, err := tx.ListFindingDispositions(ctx, runID)
	if err != nil {
		return nil, err
	}
	retry, retryErr := tx.GetReviewRetry(ctx, runID)
	if retryErr != nil && !errors.Is(retryErr, store.ErrNotFound) {
		return nil, retryErr
	}
	rounds := make(map[int]*RunReviewRound)
	bind := func(id domain.InvocationID, boundRun domain.RunID, round int, base, head string) (*RunReviewRound, error) {
		if boundRun != runID {
			return nil, domain.ErrParentKeyMismatch
		}
		if existing, ok := rounds[round]; ok {
			if existing.InvocationID != id || existing.BaseSHA != base || existing.HeadSHA != head {
				return nil, domain.ErrParentKeyMismatch
			}
			return existing, nil
		}
		r := &RunReviewRound{
			Round: round, InvocationID: id, State: domain.ReviewPending,
			Source: reviewSource(), BaseSHA: base, HeadSHA: head,
			Evidence: ReviewRoundEvidence{Availability: domain.ReviewEvidenceUnavailable},
		}
		rounds[round] = r
		return r, nil
	}
	for _, request := range requests {
		// Keyed reads also catch a terminal row moved to a foreign run or
		// round, which would otherwise be absent from this run's lists.
		if record, err := tx.GetReviewRecord(ctx, request.InvocationID); err == nil {
			if record.RunID != request.RunID || record.Round != request.Round || record.BaseSHA != request.BaseSHA || record.HeadSHA != request.HeadSHA {
				return nil, domain.ErrParentKeyMismatch
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		if failure, err := tx.GetReviewFailure(ctx, request.InvocationID); err == nil {
			if failure.RunID != request.RunID || failure.Round != request.Round || failure.BaseSHA != request.BaseSHA || failure.HeadSHA != request.HeadSHA {
				return nil, domain.ErrParentKeyMismatch
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
		r, err := bind(request.InvocationID, request.RunID, request.Round, request.BaseSHA, request.HeadSHA)
		if err != nil {
			return nil, err
		}
		r.RequestedAt = &request.RequestedAt
		for _, observation := range observations {
			if observation.InvocationID != request.InvocationID {
				continue
			}
			if observation.RunID != runID {
				return nil, domain.ErrParentKeyMismatch
			}
			if observation.Status == domain.ObservedStatusRunning || observation.Status == domain.ObservedStatusPending {
				r.State = domain.ReviewRunning
			}
		}
	}
	for _, record := range records {
		r, err := bind(record.InvocationID, record.RunID, record.Round, record.BaseSHA, record.HeadSHA)
		if err != nil {
			return nil, err
		}
		r.State, r.CompletedAt = domain.ReviewCompleted, &record.CompletedAt
		r.Provider, r.ModelConfiguration = &record.Provider, &record.ModelConfiguration
		r.Outcome, r.FindingsCount = &record.Outcome, new(len(record.FindingIDs))
		r.Evidence.CompletionEvidence = &record.CompletionEvidence
		r.Dispositions = &ReviewRoundDispositions{Open: len(record.FindingIDs)}
		for _, disposition := range dispositions {
			if disposition.Round != record.Round {
				continue
			}
			if disposition.RunID != runID || !slices.Contains(record.FindingIDs, disposition.FindingID) {
				return nil, domain.ErrParentKeyMismatch
			}
			switch disposition.Disposition {
			case domain.ReviewDispositionFixed:
				r.Dispositions.Fixed++
			case domain.ReviewDispositionDeclined:
				r.Dispositions.Declined++
			case domain.ReviewDispositionDeferred:
				r.Dispositions.Deferred++
			}
			r.Dispositions.Open--
		}
	}
	for _, failure := range failures {
		r, err := bind(failure.InvocationID, failure.RunID, failure.Round, failure.BaseSHA, failure.HeadSHA)
		if err != nil {
			return nil, err
		}
		if r.State == domain.ReviewCompleted {
			return nil, fmt.Errorf("review round has two terminal facts: %w", domain.ErrParentKeyMismatch)
		}
		r.State, r.CompletedAt = domain.ReviewFailed, &failure.ObservedAt
		r.Failure = &ReviewRoundFailure{Class: failure.Class, Reason: failure.Reason}
	}
	if retryErr == nil {
		r, err := bind(retry.InvocationID, retry.RunID, retry.Round, retry.BaseSHA, retry.HeadSHA)
		if err != nil {
			return nil, err
		}
		if r.State == domain.ReviewCompleted || r.State == domain.ReviewFailed {
			return nil, domain.ErrParentKeyMismatch
		}
		r.RetryPending = true
	}
	if len(rounds) == 0 {
		return nil, nil
	}
	facts := &RunReviewFacts{Rounds: make([]RunReviewRound, 0, len(rounds))}
	for _, r := range rounds {
		// Evidence integrity is checked again on download. A failed availability
		// read stays unknown, never evidence of a clean or missing review.
		evidence, err := retainedReviewEvidence(ctx, tx, runID, *r)
		if err != nil {
			r.Evidence.Availability = domain.ReviewEvidenceUnknown
		} else {
			r.Evidence.Availability = evidence.Availability
		}
		facts.Rounds = append(facts.Rounds, *r)
	}
	slices.SortFunc(facts.Rounds, func(a, b RunReviewRound) int { return a.Round - b.Round })
	return facts, nil
}

// authenticateReviewObservation extends the attempt-only observation gate
// with a durable request binding. Legacy terminal reviews have no observations.
func authenticateReviewObservation(ctx context.Context, tx *store.ReadTx, runID domain.RunID, id domain.InvocationID) error {
	r, err := tx.GetReviewRequest(ctx, id)
	if err != nil {
		return err
	}
	if r.RunID != runID {
		return domain.ErrParentKeyMismatch
	}
	return nil
}
