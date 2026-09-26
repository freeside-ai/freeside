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

// ReviewRoundSubject names the invocation whose recorded export produced the
// round's head. It is set only when exactly one export in the run produced
// that head, so a nil subject means "not proven", never "implementation".
type ReviewRoundSubject struct {
	Kind            domain.ReviewSubjectKind `json:"kind"`
	InvocationID    domain.InvocationID      `json:"invocation_id"`
	RemediatesRound *int                     `json:"remediates_round"`
}

// ReviewRoundRemediation is the remediation this round's findings
// adjudication dispatched, bound to the round's review invocation and head.
type ReviewRoundRemediation struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	FindingIDs   []domain.FindingID  `json:"finding_ids"`
	DecidedAt    *time.Time          `json:"decided_at"`
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
	Subject            *ReviewRoundSubject        `json:"subject"`
	Remediation        *ReviewRoundRemediation    `json:"remediation"`
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
	producers, err := reviewedHeadProducers(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	facts := &RunReviewFacts{Rounds: make([]RunReviewRound, 0, len(rounds))}
	for _, r := range rounds {
		if candidates := producers[r.HeadSHA]; len(candidates) == 1 {
			if r.Subject, err = reviewRoundSubject(ctx, tx, runID, candidates[0]); err != nil {
				return nil, err
			}
		}
		if r.Remediation, err = reviewRoundRemediation(ctx, tx, runID, *r); err != nil {
			return nil, err
		}
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

// reviewedHeadProducers indexes every recorded export of the run's attempts by
// the head it produced. The engine reviews exactly an export's head, so a head
// with one producer proves what a round reviewed; a head exported twice (a
// no-op remediation) or not at all proves nothing.
func reviewedHeadProducers(ctx context.Context, tx *store.ReadTx, runID domain.RunID) (map[string][]domain.Attempt, error) {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	producers := make(map[string][]domain.Attempt)
	for _, stage := range run.Stages {
		for _, attempt := range stage.Attempts {
			export, err := tx.GetExecutionExportRecord(ctx, attempt.InvocationID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return nil, err
			}
			producers[export.HeadSHA] = append(producers[export.HeadSHA], attempt)
		}
	}
	return producers, nil
}

// reviewRoundSubject classifies a proven producer by its authenticated
// dispatch intent kind. A producer with no retained intent (legacy) or an
// intent kind that never produces a reviewed candidate yields nil.
func reviewRoundSubject(ctx context.Context, tx *store.ReadTx, runID domain.RunID, producer domain.Attempt) (*ReviewRoundSubject, error) {
	entry, err := tx.GetOutbox(ctx, string(producer.InvocationID))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := domain.AuthenticateInvocationDispatchIntent(domain.InvocationDispatchIntent{
		Kind: entry.Kind, IdempotencyKey: entry.IdempotencyKey, Payload: entry.Payload,
	}, producer.InvocationID, runID, producer.StageID); err != nil {
		return nil, err
	}
	subject := &ReviewRoundSubject{InvocationID: producer.InvocationID}
	switch domain.InvocationIntentKind(entry.Kind) {
	case domain.ProductionInvocationRequestedKind:
		subject.Kind = domain.ReviewSubjectImplementation
		return subject, nil
	case domain.RemediationInvocationRequestedKind:
		intent, err := domain.DecodeRemediationInvocationIntent(entry.Payload)
		if err != nil {
			return nil, err
		}
		if _, err := authenticateRemediationLineage(ctx, tx, runID, intent); err != nil {
			return nil, err
		}
		subject.Kind, subject.RemediatesRound = domain.ReviewSubjectRemediation, &intent.Round
		return subject, nil
	case domain.OperatorFeedbackInvocationRequestedKind:
		subject.Kind = domain.ReviewSubjectOperatorFeedback
		return subject, nil
	case domain.AgentInvocationRequestedKind, domain.SpecificationInvocationRequestedKind,
		domain.SpecificationDiscussionRequestedKind:
		return nil, nil
	}
	return nil, nil
}

// reviewRoundRemediation reads the remediation dispatched from this round's
// findings. An intent bound to a different review invocation, base or head
// did not supersede this round, the same rule the engine applies, so it
// yields nil rather than an error.
func reviewRoundRemediation(ctx context.Context, tx *store.ReadTx, runID domain.RunID, r RunReviewRound) (*ReviewRoundRemediation, error) {
	id := domain.RemediationInvocationID(runID, r.Round)
	entry, err := tx.GetOutbox(ctx, string(id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if domain.InvocationIntentKind(entry.Kind) != domain.RemediationInvocationRequestedKind {
		return nil, fmt.Errorf("remediation intent %q has kind %q: %w", id, entry.Kind, domain.ErrParentKeyMismatch)
	}
	if err := domain.AuthenticateInvocationDispatchIntent(domain.InvocationDispatchIntent{
		Kind: entry.Kind, IdempotencyKey: entry.IdempotencyKey, Payload: entry.Payload,
	}, id, runID, domain.RemediationStageID(runID, r.Round)); err != nil {
		return nil, err
	}
	intent, err := domain.DecodeRemediationInvocationIntent(entry.Payload)
	if err != nil {
		return nil, err
	}
	if intent.ReviewInvocationID != r.InvocationID || intent.BaseSHA != r.BaseSHA || intent.HeadSHA != r.HeadSHA {
		return nil, nil
	}
	adjudication, err := authenticateRemediationLineage(ctx, tx, runID, intent)
	if err != nil {
		return nil, err
	}
	decidedAt, err := findingAdjudicationDecidedAt(ctx, tx, adjudication)
	if err != nil {
		return nil, err
	}
	return &ReviewRoundRemediation{InvocationID: id, FindingIDs: slices.Clone(intent.FindingIDs), DecidedAt: decidedAt}, nil
}

// authenticateRemediationLineage re-gates a decoded remediation intent against
// the review record and adjudication it names, as the successor-producer gate
// does. Dispatch authentication binds only the intent's own identity, so its
// round lineage and finding IDs are decoded claims until proven here.
func authenticateRemediationLineage(ctx context.Context, tx *store.ReadTx, runID domain.RunID, intent domain.RemediationInvocationIntent) (domain.FindingAdjudication, error) {
	mismatch := fmt.Errorf("remediation intent %q lineage: %w", intent.InvocationID, domain.ErrParentKeyMismatch)
	review, err := tx.GetReviewRecord(ctx, intent.ReviewInvocationID)
	if errors.Is(err, store.ErrNotFound) {
		return domain.FindingAdjudication{}, mismatch
	}
	if err != nil {
		return domain.FindingAdjudication{}, err
	}
	if review.RunID != runID || review.Round != intent.Round || review.BaseSHA != intent.BaseSHA ||
		review.HeadSHA != intent.HeadSHA || review.Outcome != domain.ReviewFindings || len(intent.FindingIDs) == 0 {
		return domain.FindingAdjudication{}, mismatch
	}
	for _, finding := range intent.FindingIDs {
		if !slices.Contains(review.FindingIDs, finding) {
			return domain.FindingAdjudication{}, mismatch
		}
	}
	adjudication, err := tx.GetFindingAdjudication(ctx, intent.AdjudicationDigest)
	if errors.Is(err, store.ErrNotFound) {
		return domain.FindingAdjudication{}, mismatch
	}
	if err != nil {
		return domain.FindingAdjudication{}, err
	}
	if adjudication.Digest != intent.AdjudicationDigest || adjudication.RunID != runID || adjudication.Round != intent.Round {
		return domain.FindingAdjudication{}, mismatch
	}
	return adjudication, nil
}

// findingAdjudicationDecidedAt returns when the adjudication's item was
// decided. A missing item, or an item still open, yields nil; an item bound
// to another adjudication fails closed.
func findingAdjudicationDecidedAt(ctx context.Context, tx *store.ReadTx, adjudication domain.FindingAdjudication) (*time.Time, error) {
	runID, round, digest := adjudication.RunID, adjudication.Round, adjudication.Digest
	item, decision, err := tx.FindingAdjudicationDecision(ctx,
		domain.ProductionFindingAdjudicationItemID(runID, round, adjudication.Revision))
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	binding := item.FindingAdjudication
	if binding.RunID != runID || binding.Round != round || binding.AdjudicationDigest != digest {
		return nil, domain.ErrParentKeyMismatch
	}
	if decision == nil {
		return nil, nil
	}
	return item.DecidedAt, nil
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
