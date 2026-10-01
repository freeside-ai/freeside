package engine

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestPreviousRemediationSummaryClaimsUsesLatestProducer(t *testing.T) {
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	runID := domain.RunID("run-summary")
	producerID := remediationInvocationID(runID, 2)
	summary := summaryClaimFixture(producerID, "One recurring finding remains open.")
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := tx.PutAgentInvocation(t.Context(), domain.AgentInvocation{
			ID: producerID, InputIDs: []domain.ArtifactID{"remediation-summary-input"},
		}); err != nil {
			return err
		}
		return tx.PutAgentClaims(t.Context(), producerID, []domain.AgentClaim{summary})
	}); err != nil {
		t.Fatal(err)
	}
	w := productionPublicationWorkflow{store: st}
	claims, err := w.previousRemediationSummaryClaims(t.Context(), runID, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].Text == nil ||
		claims[0].Provenance.ProducerInvocationID != producerID {
		t.Fatalf("summary claims = %+v", claims)
	}
	absent, err := w.previousRemediationSummaryClaims(t.Context(), runID, 2)
	if err != nil || len(absent) != 0 {
		t.Fatalf("absent prior summary = %+v, %v", absent, err)
	}
}

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

// growthFixture is convergenceFixture with one reviewer configuration, medium
// findings, and a cumulative net that grows by 100 lines each round. The
// low-value streak is set out of reach so only the growth rule can stop.
func growthFixture(t *testing.T, configurations []domain.Digest, growthStreak int) store.ReviewConvergenceState {
	t.Helper()
	state := convergenceFixture(t, configurations)
	state.Policy.LowValueStreakBeforeAttention = 9
	state.Policy.DriftGrowthStreakBeforeAttention = growthStreak
	for index, record := range state.Records {
		finding := state.Findings[record.FindingIDs[0]]
		finding.Severity = domain.FindingSeverityP2
		state.Findings[finding.ID] = finding
		head := fmt.Sprintf("head-%d", record.Round)
		cumulative := domain.DiffStats{
			FilesChanged: 1, Additions: 100 * record.Round, Deletions: 10, BaseSHA: "base", HeadSHA: head,
		}
		state.History.Rounds[index].DiffMetrics = &domain.ReviewRoundDiffMetrics{
			Cumulative: cumulative, Round: cumulative,
		}
	}
	return state
}

func TestEvaluateReviewConvergenceGrowthWithoutBlockers(t *testing.T) {
	t.Parallel()
	first := domain.Digest("sha256:" + strings.Repeat("a", 64))
	second := domain.Digest("sha256:" + strings.Repeat("b", 64))
	same := func(rounds int) []domain.Digest {
		configurations := make([]domain.Digest, rounds)
		for index := range configurations {
			configurations[index] = first
		}
		return configurations
	}
	setSeverity := func(state store.ReviewConvergenceState, index int, severity domain.FindingSeverity) {
		finding := state.Findings[state.Records[index].FindingIDs[0]]
		finding.Severity = severity
		state.Findings[finding.ID] = finding
	}
	setNet := func(state store.ReviewConvergenceState, index, additions int) {
		state.History.Rounds[index].DiffMetrics.Cumulative.Additions = additions
	}
	continueAt := func(state *store.ReviewConvergenceState, round int) {
		state.Decisions = []store.ReviewDiminishingDecision{{
			Binding: store.ReviewDiminishingBinding{Round: round},
			Command: &domain.Command{Action: domain.ActionContinueUnderPolicy},
		}}
	}
	cases := []struct {
		name           string
		configurations []domain.Digest
		growthStreak   int
		mutate         func(*store.ReviewConvergenceState)
		want           store.ReviewDiminishingCause
	}{
		{name: "streak below the policy value", configurations: same(3), growthStreak: 3},
		{
			name: "streak at the policy value", configurations: same(3), growthStreak: 2,
			want: store.ReviewDiminishingGrowthWithoutBlockers,
		},
		{
			name: "first recorded round never counts", configurations: same(1), growthStreak: 1,
		},
		{
			name: "critical finding resets", configurations: same(3), growthStreak: 2,
			mutate: func(s *store.ReviewConvergenceState) { setSeverity(*s, 1, domain.FindingSeverityP0) },
		},
		{
			name: "high finding resets", configurations: same(3), growthStreak: 2,
			mutate: func(s *store.ReviewConvergenceState) { setSeverity(*s, 2, domain.FindingSeverityP1) },
		},
		{
			name: "unset severity resets", configurations: same(3), growthStreak: 2,
			mutate: func(s *store.ReviewConvergenceState) { setSeverity(*s, 2, "") },
		},
		{
			name: "low findings still count", configurations: same(3), growthStreak: 2,
			mutate: func(s *store.ReviewConvergenceState) { setSeverity(*s, 2, domain.FindingSeverityP3) },
			want:   store.ReviewDiminishingGrowthWithoutBlockers,
		},
		{
			name: "equal net resets", configurations: same(3), growthStreak: 1,
			mutate: func(s *store.ReviewConvergenceState) { setNet(*s, 2, 200) },
		},
		{
			name: "lower net resets", configurations: same(3), growthStreak: 1,
			mutate: func(s *store.ReviewConvergenceState) { setNet(*s, 2, 150) },
		},
		{
			name: "removed lines offset added lines", configurations: same(3), growthStreak: 1,
			mutate: func(s *store.ReviewConvergenceState) {
				s.History.Rounds[2].DiffMetrics.Cumulative.Deletions = 110
			},
		},
		{
			name: "clean round inside the run resets", configurations: same(4), growthStreak: 2,
			mutate: func(s *store.ReviewConvergenceState) {
				s.Records[2].Outcome = domain.ReviewClean
				s.Records[2].FindingIDs = nil
				s.History.Rounds[2].FindingsIngested = 0
				s.History.Rounds[2].RecurringFindings = 0
				s.History.Rounds[2].Outcome = domain.ReviewClean
			},
		},
		{
			name: "continue opens a fresh window", configurations: same(4), growthStreak: 2,
			mutate: func(s *store.ReviewConvergenceState) { continueAt(s, 3) },
		},
		{
			name: "fresh window fills again", configurations: same(5), growthStreak: 2,
			mutate: func(s *store.ReviewConvergenceState) { continueAt(s, 3) },
			want:   store.ReviewDiminishingGrowthWithoutBlockers,
		},
		{
			name:           "reviewer configuration change opens a fresh window",
			configurations: []domain.Digest{first, first, second}, growthStreak: 2,
		},
		{
			// The round being counted must sit inside the window; the round
			// it is compared with may sit before the segment start.
			name:           "segment's first round compares with the round before it",
			configurations: []domain.Digest{first, second, second}, growthStreak: 2,
			want: store.ReviewDiminishingGrowthWithoutBlockers,
		},
		{
			name: "missing metrics on the current round", configurations: same(3), growthStreak: 1,
			mutate: func(s *store.ReviewConvergenceState) { s.History.Rounds[2].DiffMetrics = nil },
		},
		{
			name: "missing metrics on the previous round", configurations: same(3), growthStreak: 1,
			mutate: func(s *store.ReviewConvergenceState) { s.History.Rounds[1].DiffMetrics = nil },
		},
		{
			name: "changed base", configurations: same(3), growthStreak: 1,
			mutate: func(s *store.ReviewConvergenceState) {
				s.History.Rounds[2].DiffMetrics.Cumulative.BaseSHA = "other-base"
			},
		},
		{
			name: "landed cause wins on the same round", configurations: same(3), growthStreak: 2,
			mutate: func(s *store.ReviewConvergenceState) { s.Policy.LowValueStreakBeforeAttention = 2 },
			want:   store.ReviewDiminishingLowValue,
		},
		{name: "policy key unset", configurations: same(3), growthStreak: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			state := growthFixture(t, tc.configurations, tc.growthStreak)
			if tc.mutate != nil {
				tc.mutate(&state)
			}
			current := state.Records[len(state.Records)-1]
			cause, stop, err := store.EvaluateReviewConvergence(state, current)
			if err != nil || cause != tc.want || stop != (tc.want != "") {
				t.Fatalf("evaluate = %q, %v, %v; want %q", cause, stop, err, tc.want)
			}
		})
	}
}
