package domain_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validReviewYieldHistory() domain.ReviewYieldHistory {
	return domain.ReviewYieldHistory{
		Rounds: []domain.ReviewYieldRound{
			{
				Round: 1, FindingsIngested: 2, NewFindings: 2,
				Declined: 1, Outcome: domain.ReviewFindings,
			},
			{
				Round: 2, FindingsIngested: 2, NewFindings: 1, RecurringFindings: 1,
				Fixed: 1, Deferred: 1, Outcome: domain.ReviewFindings,
			},
			{Round: 3, Outcome: domain.ReviewClean},
		},
		TerminalOutcome: domain.ReviewClean,
	}
}

func TestReviewYieldHistoryValidation(t *testing.T) {
	if err := validReviewYieldHistory().Validate(); err != nil {
		t.Fatalf("valid history: %v", err)
	}
	for name, mutate := range map[string]func(*domain.ReviewYieldHistory){
		"empty": func(history *domain.ReviewYieldHistory) { history.Rounds = nil },
		"unordered": func(history *domain.ReviewYieldHistory) {
			history.Rounds[1].Round = history.Rounds[0].Round
		},
		"negative":    func(history *domain.ReviewYieldHistory) { history.Rounds[0].Fixed = -1 },
		"finding sum": func(history *domain.ReviewYieldHistory) { history.Rounds[1].NewFindings = 2 },
		"too many dispositions": func(history *domain.ReviewYieldHistory) {
			history.Rounds[0].Fixed = 2
		},
		"first round recurring": func(history *domain.ReviewYieldHistory) {
			history.Rounds[0].NewFindings = 1
			history.Rounds[0].RecurringFindings = 1
		},
		"clean with findings": func(history *domain.ReviewYieldHistory) {
			history.Rounds[0].Outcome = domain.ReviewClean
		},
		"findings without findings": func(history *domain.ReviewYieldHistory) {
			history.Rounds[2].Outcome = domain.ReviewFindings
			history.TerminalOutcome = domain.ReviewFindings
		},
		"terminal mismatch": func(history *domain.ReviewYieldHistory) {
			history.TerminalOutcome = domain.ReviewFindings
		},
	} {
		t.Run(name, func(t *testing.T) {
			history := validReviewYieldHistory()
			mutate(&history)
			if err := history.Validate(); !errors.Is(err, domain.ErrReviewYieldHistoryInconsistent) {
				t.Fatalf("Validate = %v, want ErrReviewYieldHistoryInconsistent", err)
			}
		})
	}
}

func TestNewReviewYieldHistoryDetachesRounds(t *testing.T) {
	history := validReviewYieldHistoryWithDiffMetrics()
	got, err := domain.NewReviewYieldHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	history.Rounds[0].NewFindings = 0
	history.Rounds[0].DiffMetrics.Cumulative.Additions = 0
	*history.Rounds[0].DriftVerdict = domain.DriftVerdictStuck
	if got.Rounds[0].NewFindings != 2 || got.Rounds[0].DiffMetrics.Cumulative.Additions != 120 ||
		*got.Rounds[0].DriftVerdict != domain.DriftVerdictOverHardened {
		t.Fatal("constructed history aliases caller-owned rounds")
	}
}

// validReviewYieldHistoryWithDiffMetrics records metrics for rounds 1 and 3
// and leaves round 2 without them, the shape a gap takes. Round 1 also carries
// a drift verdict.
func validReviewYieldHistoryWithDiffMetrics() domain.ReviewYieldHistory {
	history := validReviewYieldHistory()
	first := domain.DiffStats{
		FilesChanged: 4, Additions: 120, Deletions: 8, BaseSHA: "base", HeadSHA: "head-1",
	}
	history.Rounds[0].DiffMetrics = &domain.ReviewRoundDiffMetrics{Cumulative: first, Round: first}
	history.Rounds[0].DriftVerdict = new(domain.DriftVerdictOverHardened)
	history.Rounds[2].DiffMetrics = &domain.ReviewRoundDiffMetrics{
		Cumulative: domain.DiffStats{
			FilesChanged: 6, Additions: 210, Deletions: 14, BaseSHA: "base", HeadSHA: "head-3",
		},
		Round: domain.DiffStats{
			FilesChanged: 2, Additions: 35, Deletions: 3, BaseSHA: "head-2", HeadSHA: "head-3",
		},
	}
	return history
}

func TestReviewYieldHistoryDiffMetricsValidation(t *testing.T) {
	if err := validReviewYieldHistoryWithDiffMetrics().Validate(); err != nil {
		t.Fatalf("valid history with diff metrics: %v", err)
	}
	for name, mutate := range map[string]func(*domain.ReviewRoundDiffMetrics){
		"negative count":       func(metrics *domain.ReviewRoundDiffMetrics) { metrics.Round.Deletions = -1 },
		"missing commit":       func(metrics *domain.ReviewRoundDiffMetrics) { metrics.Cumulative.BaseSHA = "" },
		"different heads":      func(metrics *domain.ReviewRoundDiffMetrics) { metrics.Round.HeadSHA = "head-other" },
		"same pair, different": func(metrics *domain.ReviewRoundDiffMetrics) { metrics.Round.BaseSHA = "base" },
	} {
		t.Run(name, func(t *testing.T) {
			history := validReviewYieldHistoryWithDiffMetrics()
			mutate(history.Rounds[2].DiffMetrics)
			if err := history.Validate(); !errors.Is(err, domain.ErrReviewYieldHistoryInconsistent) {
				t.Fatalf("Validate = %v, want ErrReviewYieldHistoryInconsistent", err)
			}
		})
	}
}

func TestReviewYieldHistoryDriftVerdictValidation(t *testing.T) {
	for _, verdict := range domain.AllDriftVerdicts {
		history := validReviewYieldHistory()
		history.Rounds[1].DriftVerdict = &verdict
		if err := history.Validate(); err != nil {
			t.Fatalf("verdict %q on a non-last round: %v", verdict, err)
		}
	}
	for name, mutate := range map[string]func(*domain.ReviewYieldHistory){
		"unknown verdict": func(history *domain.ReviewYieldHistory) {
			history.Rounds[0].DriftVerdict = new(domain.DriftVerdict("drifting"))
		},
		"empty verdict": func(history *domain.ReviewYieldHistory) {
			history.Rounds[0].DriftVerdict = new(domain.DriftVerdict(""))
		},
		"last round": func(history *domain.ReviewYieldHistory) {
			history.Rounds[2].DriftVerdict = new(domain.DriftVerdictConverged)
		},
		"only round": func(history *domain.ReviewYieldHistory) {
			history.Rounds = history.Rounds[:1]
			history.TerminalOutcome = domain.ReviewFindings
			history.Rounds[0].DriftVerdict = new(domain.DriftVerdictStuck)
		},
	} {
		t.Run(name, func(t *testing.T) {
			history := validReviewYieldHistory()
			mutate(&history)
			if err := history.Validate(); !errors.Is(err, domain.ErrReviewYieldHistoryInconsistent) {
				t.Fatalf("Validate = %v, want ErrReviewYieldHistoryInconsistent", err)
			}
		})
	}
}
