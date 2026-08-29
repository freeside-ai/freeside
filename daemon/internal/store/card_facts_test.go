package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestGoldenRoundTripCardFacts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	items := storeCardFactItems(t)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		finding := adjudicationFinding("finding-1", f.run.ID, "daemon/review.go", time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC))
		record := adjudicationReviewRecord(t, f.run.ID, 2, []domain.FindingID{finding.ID}, finding.CreatedAt)
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		for _, item := range items {
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed card facts: %v", err)
	}

	for name, item := range items {
		t.Run(name, func(t *testing.T) {
			var got domain.AttentionItem
			if err := s.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				got, err = tx.GetAttentionItem(ctx, item.ID)
				return err
			}); err != nil {
				t.Fatalf("get: %v", err)
			}
			want := projectedAttentionItem(t, item)
			gotJSON := marshalIndent(t, got)
			if string(gotJSON) != string(marshalIndent(t, want)) {
				t.Fatalf("round trip mismatch for %s", name)
			}
			golden.Assert(t, "attention_item_card_"+name, gotJSON)
		})
	}
}

func TestPutAttentionItemAuthenticatesReviewDisputeBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	finding := adjudicationFinding("finding-1", f.run.ID, "daemon/review.go", at)
	record := adjudicationReviewRecord(t, f.run.ID, 2, []domain.FindingID{finding.ID}, at)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, []domain.Finding{finding})
	}); err != nil {
		t.Fatalf("seed review: %v", err)
	}

	base := storeCardFactItems(t)["review_dispute"]
	cases := []struct {
		name   string
		mutate func(*domain.ReviewDisputeBinding)
	}{
		{"invented round", func(binding *domain.ReviewDisputeBinding) { binding.Round++ }},
		{"invented finding set", func(binding *domain.ReviewDisputeBinding) {
			binding.FindingIDs = []domain.FindingID{"finding-invented"}
		}},
		{"invented completion evidence", func(binding *domain.ReviewDisputeBinding) {
			binding.CompletionEvidence = adjudicationDigest("f")
		}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			candidate.ID = domain.ItemID(fmt.Sprintf("item-dispute-forged-%d", i))
			binding := *candidate.ReviewDispute
			binding.FindingIDs = slices.Clone(binding.FindingIDs)
			tc.mutate(&binding)
			candidate.ReviewDispute = &binding
			err := s.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAttentionItem(ctx, candidate)
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("put = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

func TestAttentionItemAuthenticatesShadowReviewDisputeBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-shadow-card-fact", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	routed := routedCandidate(t, run.ID, "routed-card-fact", 1, nil, at)
	finding := domain.Finding{
		ID: "shadow-finding-card-fact", RunID: run.ID,
		Source: string(domain.ShadowReviewClaudeLocal), Severity: domain.FindingSeverityP1,
		Location: &domain.FindingLocation{Path: "daemon/shadow.go", StartLine: 1, EndLine: 1},
		Message:  "shadow finding", RawText: "shadow finding", CreatedAt: at,
	}
	shadow := shadowRecord(t, run.ID, "shadow-card-fact", 1,
		[]domain.FindingID{finding.ID}, at)
	shadow.CompletionEvidence = adjudicationDigest("f")
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, routed, nil); err != nil {
			return err
		}
		return tx.PutShadowReviewRecord(ctx, shadow, []domain.Finding{finding})
	}); err != nil {
		t.Fatalf("seed shadow review: %v", err)
	}

	runID := run.ID
	input := domain.AttentionItemInput{
		ID: "item-shadow-card-fact", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionReviewDispute, Priority: domain.PriorityHigh,
		Reason: "a shadow finding is disputed", RequestedDecision: []domain.Action{domain.ActionDiscuss},
		ReviewDispute: &domain.ReviewDisputeBinding{
			RunID: run.ID, Round: shadow.ShadowedRound,
			FindingIDs: []domain.FindingID{finding.ID}, CompletionEvidence: shadow.CompletionEvidence,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &at, Status: domain.StatusOpen,
	}
	item, err := domain.NewAttentionItem(input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("put shadow dispute item: %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatalf("get shadow dispute item: %v", err)
	}

	forged := item
	forged.ID = "item-shadow-card-fact-forged"
	binding := *forged.ReviewDispute
	binding.CompletionEvidence = routed.CompletionEvidence
	forged.ReviewDispute = &binding
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, forged)
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("forged cross-lane binding put = %v, want ErrParentKeyMismatch", err)
	}
}

func TestPutAttentionItemRejectsChangedCardFact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	item := storeCardFactItems(t)["execution_failure"]
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	changed := item
	changed.ItemVersion = 2
	fact := *changed.ExecutionFailure
	fact.InvocationID = "inv-other"
	changed.ExecutionFailure = &fact
	err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, changed) })
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("changed fact put = %v, want ErrImmutableConflict", err)
	}
}

func storeCardFactItems(t *testing.T) map[string]domain.AttentionItem {
	t.Helper()
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	runID := domain.RunID("run-1")
	subject := domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID}
	itemID := domain.ItemID("item-spec")
	rule := domain.TrustRuleTrustProfileDrift
	posture := domain.HealthPostureAdvisory

	inputs := map[string]domain.AttentionItemInput{
		"cost": {
			ID: "item-card-cost", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal,
			Reason: "review yield is diminishing", RequestedDecision: []domain.Action{domain.ActionFinishNow},
			BillableCostSoFar: &domain.CostSoFar{Currency: "USD", Amount: "17.50", Invocations: 4},
			ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: &at, Status: domain.StatusOpen,
		},
		"execution_failure": {
			ID: "item-card-execution", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionExecutionFailure, Priority: domain.PriorityUrgent,
			Reason: "implementation failed", RequestedDecision: []domain.Action{domain.ActionRetry},
			ExecutionFailure: &domain.ExecutionFailureFacts{
				Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
				InvocationID: "inv-1",
			},
			ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, CreatedAt: &at, Status: domain.StatusOpen,
		},
		"publish_block": {
			ID: "item-card-publish", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
			Reason: "publication is blocked", RequestedDecision: []domain.Action{domain.ActionInspectTrustFailure},
			PublishBlock: &domain.PublishBlockFacts{TrustRule: &rule},
			ItemVersion:  1, InterruptionClass: domain.InterruptionExceptional, CreatedAt: &at, Status: domain.StatusOpen,
		},
		"blocked_on": {
			ID: "item-card-blocked", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionBlocked, Priority: domain.PriorityNormal,
			Reason: "waiting for specification approval", RequestedDecision: []domain.Action{},
			BlockedOn:   &domain.BlockedWait{Kind: domain.BlockedWaitSpecApproval, Since: at, ItemID: &itemID},
			ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, CreatedAt: &at, Status: domain.StatusOpen,
		},
		"health_diagnostic": {
			ID: "item-card-health", ProjectID: "proj-1",
			Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
			Type:    domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
			Reason: "run projection is unavailable", RequestedDecision: []domain.Action{domain.ActionRunDoctor},
			HealthDiagnostic: &domain.HealthDiagnostic{
				Code: "run_projection.unavailable", Impairs: domain.ImpairedCapabilityRunVisibility,
			},
			Posture: &posture, ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
			CreatedAt: &at, Status: domain.StatusOpen,
		},
		"review_dispute": {
			ID: "item-card-dispute", ProjectID: "proj-1", Subject: subject,
			Type: domain.AttentionReviewDispute, Priority: domain.PriorityHigh,
			Reason: "a review finding is disputed", RequestedDecision: []domain.Action{domain.ActionDiscuss},
			ReviewDispute: &domain.ReviewDisputeBinding{
				RunID: runID, Round: 2, FindingIDs: []domain.FindingID{"finding-1"},
				CompletionEvidence: adjudicationDigest("e"),
			},
			ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, CreatedAt: &at, Status: domain.StatusOpen,
		},
	}
	items := make(map[string]domain.AttentionItem, len(inputs))
	for name, input := range inputs {
		item, err := domain.NewAttentionItem(input, nil)
		if err != nil {
			t.Fatalf("NewAttentionItem %s: %v", name, err)
		}
		items[name] = item
	}
	return items
}
