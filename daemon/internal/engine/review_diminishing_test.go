package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestEvaluateReviewConvergencePolicy(t *testing.T) {
	t.Parallel()
	firstConfig := domain.Digest("sha256:" + strings.Repeat("a", 64))
	secondConfig := domain.Digest("sha256:" + strings.Repeat("b", 64))
	base := convergenceFixture(t, []domain.Digest{firstConfig, firstConfig, firstConfig})

	t.Run("low-value streak", func(t *testing.T) {
		cause, stop, err := store.EvaluateReviewConvergence(base, base.Records[2])
		if err != nil || !stop || cause != store.ReviewDiminishingLowValue {
			t.Fatalf("evaluate = %q, %v, %v", cause, stop, err)
		}
	})

	t.Run("configuration change resets streak", func(t *testing.T) {
		state := convergenceFixture(t, []domain.Digest{firstConfig, firstConfig, secondConfig})
		cause, stop, err := store.EvaluateReviewConvergence(state, state.Records[2])
		if err != nil || stop || cause != "" {
			t.Fatalf("evaluate = %q, %v, %v", cause, stop, err)
		}
	})

	t.Run("continue grants a fresh policy window", func(t *testing.T) {
		state := convergenceFixture(t, []domain.Digest{
			firstConfig, firstConfig, firstConfig, firstConfig, firstConfig,
		})
		state.Decisions = []store.ReviewDiminishingDecision{{
			Binding: store.ReviewDiminishingBinding{Round: 3},
			Command: &domain.Command{Action: domain.ActionContinueUnderPolicy},
		}}
		if cause, stop, err := store.EvaluateReviewConvergence(state, state.Records[3]); err != nil || stop {
			t.Fatalf("round 4 evaluate = %q, %v, %v", cause, stop, err)
		}
		cause, stop, err := store.EvaluateReviewConvergence(state, state.Records[4])
		if err != nil || !stop || cause != store.ReviewDiminishingLowValue {
			t.Fatalf("round 5 evaluate = %q, %v, %v", cause, stop, err)
		}
	})

	t.Run("new non-material findings do not reset streak", func(t *testing.T) {
		state := convergenceFixture(t, []domain.Digest{firstConfig, firstConfig, firstConfig})
		for index, record := range state.Records {
			finding := state.Findings[record.FindingIDs[0]]
			finding.Message = fmt.Sprintf("non-material finding %d", index+1)
			finding.RawText = finding.Message
			state.Findings[finding.ID] = finding
			state.History.Rounds[index].NewFindings = 1
			state.History.Rounds[index].RecurringFindings = 0
		}
		cause, stop, err := store.EvaluateReviewConvergence(state, state.Records[2])
		if err != nil || !stop || cause != store.ReviewDiminishingLowValue {
			t.Fatalf("evaluate = %q, %v, %v", cause, stop, err)
		}
	})

	t.Run("new material finding resets streak", func(t *testing.T) {
		state := convergenceFixture(t, []domain.Digest{firstConfig, firstConfig, firstConfig})
		current := state.Records[2]
		finding := state.Findings[current.FindingIDs[0]]
		finding.Message = "new material finding"
		finding.RawText = finding.Message
		state.Findings[finding.ID] = finding
		state.History.Rounds[2].NewFindings = 1
		state.History.Rounds[2].RecurringFindings = 0
		state.MaterialFindings[current.Round][finding.ID] = struct{}{}
		cause, stop, err := store.EvaluateReviewConvergence(state, current)
		if err != nil || stop || cause != "" {
			t.Fatalf("evaluate = %q, %v, %v", cause, stop, err)
		}
	})

	t.Run("fixed recurrence stops immediately", func(t *testing.T) {
		state := convergenceFixture(t, []domain.Digest{firstConfig, firstConfig})
		state.Dispositions = []domain.ReviewDispositionRecord{{
			FindingID: state.Records[0].FindingIDs[0], Round: 1,
			Disposition: domain.ReviewDispositionFixed,
		}}
		cause, stop, err := store.EvaluateReviewConvergence(state, state.Records[1])
		if err != nil || !stop || cause != store.ReviewDiminishingFixedRecurrence {
			t.Fatalf("evaluate = %q, %v, %v", cause, stop, err)
		}
	})

	t.Run("apply then finish makes the next findings review a gate", func(t *testing.T) {
		state := convergenceFixture(t, []domain.Digest{firstConfig, firstConfig})
		state.Decisions = []store.ReviewDiminishingDecision{{
			Binding: store.ReviewDiminishingBinding{Round: 1},
			Command: &domain.Command{Action: domain.ActionApplyThenFinish},
		}}
		cause, stop, err := store.EvaluateReviewConvergence(state, state.Records[1])
		if err != nil || !stop || cause != store.ReviewDiminishingFinalFindings {
			t.Fatalf("evaluate = %q, %v, %v", cause, stop, err)
		}
	})

	t.Run("past hard cap cannot create diminishing authority", func(t *testing.T) {
		state := convergenceFixture(t, []domain.Digest{firstConfig, firstConfig, firstConfig})
		state.Policy.HardRoundLimit = 2
		cause, stop, err := store.EvaluateReviewConvergence(state, state.Records[2])
		if err != nil || stop || cause != "" {
			t.Fatalf("evaluate = %q, %v, %v", cause, stop, err)
		}
	})
}

func convergenceFixture(t *testing.T, configurations []domain.Digest) store.ReviewConvergenceState {
	t.Helper()
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	records := make([]domain.ReviewRecord, len(configurations))
	history := domain.ReviewYieldHistory{Rounds: make([]domain.ReviewYieldRound, len(configurations))}
	findings := make(map[domain.FindingID]domain.Finding, len(configurations))
	material := make(map[int]map[domain.FindingID]struct{}, len(configurations))
	var previousConfiguration domain.Digest
	for index, configuration := range configurations {
		round := index + 1
		id := domain.FindingID("finding-" + strings.Repeat("x", round))
		findings[id] = domain.Finding{
			ID: id, RunID: "run-convergence", Source: "codex_local",
			Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: round, EndLine: round},
			Message:  "recurring finding", RawText: "recurring finding", CreatedAt: at,
		}
		records[index] = domain.ReviewRecord{
			RunID: "run-convergence", Round: round, ConfigurationDigest: configuration,
			Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{id},
		}
		history.Rounds[index] = domain.ReviewYieldRound{
			Round: round, FindingsIngested: 1, RecurringFindings: 1,
			Outcome: domain.ReviewFindings,
		}
		material[round] = map[domain.FindingID]struct{}{}
		startsSegment := index == 0 || configuration != previousConfiguration
		if startsSegment {
			history.Rounds[index].NewFindings = 1
			history.Rounds[index].RecurringFindings = 0
		}
		previousConfiguration = configuration
	}
	if len(history.Rounds) > 0 {
		history.TerminalOutcome = domain.ReviewFindings
	}
	return store.ReviewConvergenceState{
		History: history, Records: records, Findings: findings, MaterialFindings: material,
		Policy: store.ReviewConvergencePolicy{
			ContinueWhile:                 store.ReviewContinueWhileNewMaterialFindings,
			LowValueStreakBeforeAttention: 2,
			HardRoundLimit:                25,
		},
	}
}
