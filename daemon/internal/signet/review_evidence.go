package signet

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ReviewEvidence is private reviewer output, not verifier evidence. The
// authenticated device route preserves provenance and never publishes it.
type ReviewEvidence struct {
	Source             ReviewSource                      `json:"source"`
	RunID              domain.RunID                      `json:"run_id"`
	Round              int                               `json:"round"`
	InvocationID       domain.InvocationID               `json:"invocation_id"`
	ContentKind        domain.ReviewContentKind          `json:"content_kind"`
	HeadBinding        domain.HeadBinding                `json:"head_binding"`
	SourceHeadSHA      string                            `json:"source_head_sha"`
	SensitivityClass   domain.SensitivityClass           `json:"sensitivity_class"`
	PublishEligible    bool                              `json:"publish_eligible"`
	Availability       domain.ReviewEvidenceAvailability `json:"availability"`
	CollectionEvidence *domain.Digest                    `json:"collection_evidence"`
	Events             []byte                            `json:"events"`
	Result             []byte                            `json:"result"`
	ExitStatus         *int                              `json:"exit_status"`
}

func retainedReviewEvidence(ctx context.Context, tx *store.ReadTx, runID domain.RunID, round RunReviewRound) (ReviewEvidence, error) {
	evidence := ReviewEvidence{
		Source: reviewSource(), RunID: runID, Round: round.Round,
		InvocationID: round.InvocationID, ContentKind: domain.ReviewContentReviewerOutput, HeadBinding: domain.HeadBound,
		SourceHeadSHA: round.HeadSHA, SensitivityClass: domain.SensitivitySensitive, Availability: domain.ReviewEvidenceUnavailable,
	}
	if round.State != domain.ReviewCompleted && round.State != domain.ReviewFailed {
		return evidence, nil
	}
	row, err := tx.GetCodexReviewOutcome(ctx, string(round.InvocationID))
	if errors.Is(err, store.ErrNotFound) {
		return evidence, nil
	}
	if err != nil {
		return evidence, err
	}
	request, err := tx.GetCodexReviewRequest(ctx, string(round.InvocationID))
	if err != nil {
		return evidence, err
	}
	binding := domain.ReviewRequestRecord{InvocationID: round.InvocationID, RunID: runID, Round: round.Round, BaseSHA: round.BaseSHA, HeadSHA: round.HeadSHA}
	outcome, err := ward.DecodeRetainedCodexReview(binding, request.Body, request.BodyDigest, row.State, row.Body, row.BodyDigest)
	if err != nil {
		return evidence, err
	}
	if round.State == domain.ReviewCompleted {
		if outcome.Result == nil || round.Evidence.CompletionEvidence == nil || outcome.Result.CompletionEvidence != *round.Evidence.CompletionEvidence {
			return evidence, domain.ErrInvalidReviewCompletionEvidence
		}
	}
	if row.State != "ready" || outcome.Collection == nil {
		return evidence, nil
	}
	evidence.Availability = domain.ReviewEvidenceAvailable
	evidence.CollectionEvidence = &outcome.CollectionEvidence
	evidence.Events, evidence.Result = outcome.Collection.Events, outcome.Collection.Result
	evidence.ExitStatus = &outcome.Collection.ExitStatus
	return evidence, nil
}

func (s *Service) GetReviewEvidence(ctx context.Context, runID domain.RunID, roundNumber int) (ReviewEvidence, error) {
	var evidence ReviewEvidence
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetRun(ctx, runID); err != nil {
			return err
		}
		facts, err := runReviewFacts(ctx, tx, runID, nil)
		if err != nil {
			return err
		}
		if facts != nil {
			for _, round := range facts.Rounds {
				if round.Round != roundNumber {
					continue
				}
				evidence, err = retainedReviewEvidence(ctx, tx, runID, round)
				return err
			}
		}
		return store.ErrNotFound
	})
	return evidence, err
}

func (h httpHandler) getReviewEvidence(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	round, err := strconv.Atoi(r.PathValue("round"))
	if err != nil || round < 1 {
		writeReadError(w, store.ErrNotFound)
		return
	}
	evidence, err := h.service.GetReviewEvidence(r.Context(), domain.RunID(r.PathValue("run_id")), round)
	if err != nil {
		writeReadError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, evidence)
}
