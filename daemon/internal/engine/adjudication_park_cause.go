package engine

import (
	"context"
	"errors"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

type adjudicationParkCause string

const (
	parkAdjudicatorUnconfigured adjudicationParkCause = "adjudicator_unconfigured"
	parkJudgmentFallback        adjudicationParkCause = "judgment_fallback"
	parkInputRefused            adjudicationParkCause = "input_refused"
	parkOutputRefused           adjudicationParkCause = "output_refused"
	parkBindingMismatch         adjudicationParkCause = "binding_mismatch"
	parkBatchMismatch           adjudicationParkCause = "batch_mismatch"
	parkEntryNotAccepted        adjudicationParkCause = "entry_not_accepted"
	parkInternalError           adjudicationParkCause = "internal_error"
)

var AllAdjudicationParkCauses = []adjudicationParkCause{
	parkAdjudicatorUnconfigured, parkJudgmentFallback, parkInputRefused, parkOutputRefused,
	parkBindingMismatch, parkBatchMismatch, parkEntryNotAccepted, parkInternalError,
}

func (c adjudicationParkCause) valid() bool {
	switch c {
	case parkAdjudicatorUnconfigured, parkJudgmentFallback, parkInputRefused, parkOutputRefused,
		parkBindingMismatch, parkBatchMismatch, parkEntryNotAccepted, parkInternalError:
		return true
	default:
		return false
	}
}

func adjudicationErrorCause(err error) (adjudicationParkCause, inference.FallbackReason) {
	var validation *adjudicationBatchRefusal
	if errors.As(err, &validation) {
		return validation.cause, ""
	}
	var refusal *inference.AdjudicationUnavailable
	if errors.As(err, &refusal) {
		switch refusal.Cause {
		case inference.AdjudicationInputRefused:
			return parkInputRefused, ""
		case inference.AdjudicationOutputRefused:
			return parkOutputRefused, ""
		case inference.AdjudicationJudgmentFallback:
			if slices.Contains(inference.AllFallbackReasons, refusal.Fallback) {
				return parkJudgmentFallback, refusal.Fallback
			}
		}
		return parkInternalError, ""
	}
	if errors.Is(err, domain.ErrParentKeyMismatch) {
		return parkBindingMismatch, ""
	}
	return parkInternalError, ""
}

type adjudicationBatchRefusal struct {
	cause adjudicationParkCause
	err   error
}

func (e *adjudicationBatchRefusal) Error() string { return "adjudication batch refused" }
func (e *adjudicationBatchRefusal) Unwrap() error { return e.err }

func adjudicationValidationCause(err error) adjudicationParkCause {
	if errors.Is(err, inference.ErrAdjudicationNotAvailable) {
		return parkEntryNotAccepted
	}
	return parkBatchMismatch
}

func (w *productionPublicationWorkflow) logAdjudicationRefusal(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
	cause adjudicationParkCause, fallback inference.FallbackReason,
) {
	if w.logger == nil {
		return
	}
	attrs := []any{
		"run", record.RunID, "round", record.Round,
		"external_first_round", task.externalReviewFirstRound(record), "cause", cause,
	}
	if cause == parkJudgmentFallback {
		attrs = append(attrs, "fallback", fallback)
	}
	w.logger.WarnContext(ctx, "finding adjudication unavailable", attrs...)
}
