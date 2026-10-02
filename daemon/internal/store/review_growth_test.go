package store_test

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestGrowthWithoutBlockersDecisionReconstructsAndContinueOpensAWindow drives
// the growth stop through stored rows: three growing rounds of medium findings
// stop at a streak of two, the item created under that cause loads again after
// a restart, and continue_under_policy lets the next growing round pass before
// the fresh window fills.
func TestGrowthWithoutBlockersDecisionReconstructsAndContinueOpensAWindow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	runID := domain.RunID("run-growth-stop")
	path := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, path, store.Options{})
	preset := func(key, value, fill string) domain.PolicyKey {
		return domain.PolicyKey{Key: key, Value: value, Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: domain.Digest("sha256:" + strings.Repeat(fill, 64)),
		}}
	}
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{
		preset("paths", "daemon/**", "a"),
		preset("review.continue_while", store.ReviewContinueWhileNewMaterialFindings, "b"),
		preset("review.low_value_streak_before_attention", "9", "c"),
		preset("review.hard_round_limit", "25", "d"),
		preset("review.drift_growth_streak_before_attention", "2", "e"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, policy)
	}); err != nil {
		t.Fatal(err)
	}
	// Each round finds one new medium finding on its own path, so neither
	// fixed_recurrence nor the low-value streak can claim the stop.
	putRound := func(round int) (domain.ReviewRecord, domain.FindingAdjudication) {
		t.Helper()
		completedAt := at.Add(time.Duration(round) * time.Minute)
		finding := adjudicationFinding(
			domain.FindingID("finding-"+strconv.Itoa(round)), runID,
			"daemon/"+strconv.Itoa(round)+".go", completedAt)
		finding.Severity = domain.FindingSeverityP2
		record := adjudicationReviewRecord(t, runID, round, []domain.FindingID{finding.ID}, completedAt)
		artifact, err := domain.NewFindingAdjudication(
			runID, round, adjSpecDigest, record.InstructionDigest, policy.Digest,
			[]domain.FindingAdjudicationEntry{adjudicationEngineEntry(t, finding.ID)}, "",
			completedAt.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
				return err
			}
			if err := tx.PutReviewRoundDiffMetrics(ctx, runID, round, roundDiffMetrics(round)); err != nil {
				return err
			}
			return tx.PutFindingAdjudication(ctx, artifact)
		}); err != nil {
			t.Fatalf("seed round %d: %v", round, err)
		}
		return record, artifact
	}
	evaluate := func(s *store.Store, record domain.ReviewRecord) (domain.ReviewDiminishingCause, bool) {
		t.Helper()
		var (
			cause domain.ReviewDiminishingCause
			stop  bool
		)
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			state, err := tx.ReviewConvergenceStateAtDecision(ctx, record)
			if err != nil {
				return err
			}
			cause, stop, err = store.EvaluateReviewConvergence(state, record)
			return err
		}); err != nil {
			t.Fatalf("evaluate round %d: %v", record.Round, err)
		}
		return cause, stop
	}

	putRound(1)
	second, _ := putRound(2)
	if cause, stop := evaluate(st, second); stop {
		t.Fatalf("round 2 stopped with %q below the streak", cause)
	}
	record, artifact := putRound(3)
	if cause, stop := evaluate(st, record); !stop || cause != domain.ReviewDiminishingGrowthWithoutBlockers {
		t.Fatalf("round 3 evaluate = %q, %v", cause, stop)
	}

	var history domain.ReviewYieldHistory
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		history, readErr = tx.ReviewYieldHistoryAtDecision(ctx, runID, record.Round)
		return readErr
	}); err != nil {
		t.Fatal(err)
	}
	itemID := store.ReviewDiminishingItemID(runID, record.Round)
	reason, err := store.ReviewDiminishingReason(store.ReviewDiminishingBinding{
		ItemID: itemID, RunID: runID, Round: record.Round, HeadSHA: record.HeadSHA,
		FindingIDs:         append([]domain.FindingID(nil), record.FindingIDs...),
		AdjudicationDigest: artifact.Digest, FindingBatchDigest: artifact.FindingBatchDigest,
		PolicyDigest: policy.Digest, ContinueWhile: store.ReviewContinueWhileNewMaterialFindings,
		LowValueStreakBeforeAttention: 9, HardRoundLimit: 25,
		Cause: domain.ReviewDiminishingGrowthWithoutBlockers,
	})
	if err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal, Reason: reason,
		RequestedDecision: store.ReviewDiminishingRequestedActions(record.Round, 25),
		PRHeadSHA:         record.HeadSHA, YieldHistory: &history, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "command-continue", DeviceID: "device-1",
		ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests, Action: domain.ActionContinueUnderPolicy,
	})
	if err != nil {
		t.Fatal(err)
	}
	concluded, err := item.WithDecidedAt(at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	concluded.Status = domain.StatusResolved
	concluded.ItemVersion++
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, concluded)
	}); err != nil {
		t.Fatalf("seed growth decision: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = storetest.Open(t, path, store.Options{})
	defer func() { _ = st.Close() }()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		decision, err := tx.ReviewDiminishingDecision(ctx, itemID)
		if err != nil {
			return err
		}
		if decision.Binding.Cause != domain.ReviewDiminishingGrowthWithoutBlockers ||
			decision.Command == nil || decision.Command.Action != domain.ActionContinueUnderPolicy {
			t.Fatalf("reloaded decision = %+v, command %+v", decision.Binding, decision.Command)
		}
		return nil
	}); err != nil {
		t.Fatalf("growth item no longer loads: %v", err)
	}
	fourth, _ := putRound(4)
	if cause, stop := evaluate(st, fourth); stop {
		t.Fatalf("round 4 stopped with %q inside the fresh window", cause)
	}
	fifth, _ := putRound(5)
	if cause, stop := evaluate(st, fifth); !stop || cause != domain.ReviewDiminishingGrowthWithoutBlockers {
		t.Fatalf("round 5 evaluate = %q, %v", cause, stop)
	}
}
