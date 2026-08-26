package domain

import (
	"fmt"
	"slices"
)

// ReviewYieldRound summarizes one persisted routed-review pass. Finding
// identity is evaluated against prior rounds in the same reviewer-configuration
// segment, while dispositions remain attributed to the round that produced the
// finding.
type ReviewYieldRound struct {
	Round             int           `json:"round"`
	FindingsIngested  int           `json:"findings_ingested"`
	NewFindings       int           `json:"new_findings"`
	RecurringFindings int           `json:"recurring_findings"`
	Fixed             int           `json:"fixed"`
	Declined          int           `json:"declined"`
	Deferred          int           `json:"deferred"`
	Outcome           ReviewOutcome `json:"outcome"`
}

// ReviewYieldHistory is the immutable review-yield digest carried by
// ready_for_final_review and review_diminishing_returns items. TerminalOutcome
// deliberately duplicates the final round so consumers can read the terminal
// result without inferring it.
type ReviewYieldHistory struct {
	Rounds          []ReviewYieldRound `json:"rounds"`
	TerminalOutcome ReviewOutcome      `json:"terminal_outcome"`
}

// NewReviewYieldHistory constructs a detached, validated yield history.
func NewReviewYieldHistory(history ReviewYieldHistory) (ReviewYieldHistory, error) {
	history.Rounds = slices.Clone(history.Rounds)
	if err := history.Validate(); err != nil {
		return ReviewYieldHistory{}, err
	}
	return history, nil
}

// Validate reports whether the digest describes an ordered, internally
// possible routed-review history.
func (h ReviewYieldHistory) Validate() error {
	if len(h.Rounds) == 0 {
		return fmt.Errorf("review yield rounds: %w", ErrReviewYieldHistoryInconsistent)
	}
	previousRound := 0
	for idx, round := range h.Rounds {
		if round.Round < 1 || round.Round <= previousRound {
			return fmt.Errorf("review yield round %d position %d: %w",
				round.Round, idx, ErrReviewYieldHistoryInconsistent)
		}
		if round.FindingsIngested < 0 || round.NewFindings < 0 ||
			round.RecurringFindings < 0 || round.Fixed < 0 ||
			round.Declined < 0 || round.Deferred < 0 {
			return fmt.Errorf("review yield round %d counts: %w",
				round.Round, ErrReviewYieldHistoryInconsistent)
		}
		if round.NewFindings+round.RecurringFindings != round.FindingsIngested ||
			round.Fixed+round.Declined+round.Deferred > round.FindingsIngested ||
			(round.Round == 1 && round.RecurringFindings != 0) {
			return fmt.Errorf("review yield round %d totals: %w",
				round.Round, ErrReviewYieldHistoryInconsistent)
		}
		if !round.Outcome.valid() ||
			(round.Outcome == ReviewClean) != (round.FindingsIngested == 0) {
			return fmt.Errorf("review yield round %d outcome %q: %w",
				round.Round, round.Outcome, ErrReviewYieldHistoryInconsistent)
		}
		previousRound = round.Round
	}
	if !h.TerminalOutcome.valid() || h.TerminalOutcome != h.Rounds[len(h.Rounds)-1].Outcome {
		return fmt.Errorf("review yield terminal outcome %q: %w",
			h.TerminalOutcome, ErrReviewYieldHistoryInconsistent)
	}
	return nil
}
