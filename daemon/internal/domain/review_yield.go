package domain

import (
	"fmt"
	"slices"
)

// ReviewRoundDiffMetrics is one review round's diff shape as two named pairs
// (plan §7 Review Drift). Cumulative compares the round's bound base with its
// candidate head. Round compares the previous round's candidate head with this
// round's; the first round has no predecessor, so there it compares the bound
// base with the head and equals Cumulative. The metrics are engine facts,
// never model output.
type ReviewRoundDiffMetrics struct {
	Cumulative DiffStats `json:"cumulative"`
	Round      DiffStats `json:"round"`
}

// Validate reports whether both pairs are well formed and describe one
// candidate head. Two pairs that compare the same commits are the first-round
// shape and must agree. Whether the named commits are the round's real base
// and heads is the store's re-check against the review records.
func (m ReviewRoundDiffMetrics) Validate() error {
	if err := m.Cumulative.Validate(); err != nil {
		return fmt.Errorf("review round diff metrics cumulative: %w: %w",
			err, ErrReviewYieldHistoryInconsistent)
	}
	if err := m.Round.Validate(); err != nil {
		return fmt.Errorf("review round diff metrics round: %w: %w",
			err, ErrReviewYieldHistoryInconsistent)
	}
	if m.Cumulative.HeadSHA != m.Round.HeadSHA {
		return fmt.Errorf("review round diff metrics heads %q and %q: %w",
			m.Cumulative.HeadSHA, m.Round.HeadSHA, ErrReviewYieldHistoryInconsistent)
	}
	if m.Cumulative.BaseSHA == m.Round.BaseSHA && m.Cumulative != m.Round {
		return fmt.Errorf("review round diff metrics disagree over %q..%q: %w",
			m.Round.BaseSHA, m.Round.HeadSHA, ErrReviewYieldHistoryInconsistent)
	}
	return nil
}

// ReviewYieldRound summarizes one persisted routed-review pass. Finding
// identity is evaluated against prior rounds in the same reviewer-configuration
// segment, while dispositions remain attributed to the round that produced the
// finding.
//
// DiffMetrics is absent when nothing was recorded for the round; that is how a
// gap shows. It is omitted rather than rendered as an explicit null because the
// store compares each stored history with its re-encoding byte for byte, so a
// null would stop every item stored before the field existed from loading.
type ReviewYieldRound struct {
	Round             int                     `json:"round"`
	FindingsIngested  int                     `json:"findings_ingested"`
	NewFindings       int                     `json:"new_findings"`
	RecurringFindings int                     `json:"recurring_findings"`
	Fixed             int                     `json:"fixed"`
	Declined          int                     `json:"declined"`
	Deferred          int                     `json:"deferred"`
	Outcome           ReviewOutcome           `json:"outcome"`
	DiffMetrics       *ReviewRoundDiffMetrics `json:"diff_metrics,omitempty"`
}

// ReviewYieldHistory is the immutable review-yield digest carried by
// ready_for_final_review and review_diminishing_returns items. TerminalOutcome
// deliberately duplicates the final round so consumers can read the terminal
// result without inferring it.
type ReviewYieldHistory struct {
	Rounds          []ReviewYieldRound `json:"rounds"`
	TerminalOutcome ReviewOutcome      `json:"terminal_outcome"`
}

// cloneReviewYieldRounds detaches the rounds and each round's metrics from the
// caller's backing storage.
func cloneReviewYieldRounds(rounds []ReviewYieldRound) []ReviewYieldRound {
	cloned := slices.Clone(rounds)
	for index := range cloned {
		if metrics := cloned[index].DiffMetrics; metrics != nil {
			detached := *metrics
			cloned[index].DiffMetrics = &detached
		}
	}
	return cloned
}

// NewReviewYieldHistory constructs a detached, validated yield history.
func NewReviewYieldHistory(history ReviewYieldHistory) (ReviewYieldHistory, error) {
	history.Rounds = cloneReviewYieldRounds(history.Rounds)
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
		if round.DiffMetrics != nil {
			if err := round.DiffMetrics.Validate(); err != nil {
				return fmt.Errorf("review yield round %d: %w", round.Round, err)
			}
		}
		previousRound = round.Round
	}
	if !h.TerminalOutcome.valid() || h.TerminalOutcome != h.Rounds[len(h.Rounds)-1].Outcome {
		return fmt.Errorf("review yield terminal outcome %q: %w",
			h.TerminalOutcome, ErrReviewYieldHistoryInconsistent)
	}
	return nil
}
