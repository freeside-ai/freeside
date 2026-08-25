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
	history := validReviewYieldHistory()
	got, err := domain.NewReviewYieldHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	history.Rounds[0].NewFindings = 0
	if got.Rounds[0].NewFindings != 2 {
		t.Fatal("constructed history aliases caller-owned rounds")
	}
}
