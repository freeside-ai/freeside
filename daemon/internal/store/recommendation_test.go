package store_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type registeredRecommendationRule struct {
	projection domain.RecommendationProjection
	input      domain.Digest
}

func (r registeredRecommendationRule) EvaluateRecommendation(
	domain.AttentionItem, domain.DecisionSurface,
) (domain.RecommendationProjection, domain.Digest, bool, error) {
	return r.projection, r.input, true, nil
}

func storedAgentRecommendationRecord(
	t *testing.T, item domain.AttentionItem, surface domain.DecisionSurface,
	invocationID domain.InvocationID,
) domain.RecommendationSourceRecord {
	t.Helper()
	record, err := domain.NewRecommendationSourceRecord(domain.RecommendationSourceRecord{
		ItemID: item.ID, Source: domain.RecommendationAgentJudgment,
		Provenance: domain.RecommendationProvenance{AgentJudgment: &domain.AgentJudgmentRecommendationProvenance{
			JudgmentSite:   domain.JudgmentSiteFindingAdjudicator,
			InvocationID:   invocationID,
			ArtifactDigest: item.FindingAdjudication.AdjudicationDigest,
		}},
		Action:                domain.ActionAcceptRecommendedRoute,
		Reason:                domain.FindingAdjudicatorRecommendationReason,
		DecisionSurfaceDigest: surface.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestRecommendationSourceDerivesAtCreationAndSuppressesOnCollision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-recommendation")
	at := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	finding := adjudicationFinding("finding-recommendation", runID, "daemon/a.go", at)
	st := seedReviewRound(t, runID, 1, []domain.Finding{finding}, at)
	artifact := modelAdjudication(t, runID, 1, finding.ID, at)
	item := adjudicationItem(t, "item-recommendation", bindingFromAdjudication(artifact, finding))
	surface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatal(err)
	}
	record := storedAgentRecommendationRecord(
		t, item, surface, domain.InvocationID("review-"+string(runID)+"-1"))

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, artifact); err != nil {
			return err
		}
		// The deferred foreign key permits the source to precede its new item,
		// so PutAttentionItem sees the complete eligible set at creation.
		if err := tx.PutRecommendationSource(ctx, record); err != nil {
			return err
		}
		caller := item
		caller.Recommendation = &domain.Recommendation{Action: domain.ActionStop, Reason: "caller substitution"}
		caller.DecisionSurface = domain.DecisionSurfaceRef{Epoch: 99, Digest: adjudicationDigest("f")}
		return tx.PutAttentionItem(ctx, caller)
	}); err != nil {
		t.Fatal(err)
	}

	var stored domain.AttentionItem
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := &domain.Recommendation{
		Action: record.Action, Reason: record.Reason, Source: record.Source,
		Provenance: record.Provenance,
	}
	if !reflect.DeepEqual(stored.Recommendation, want) {
		t.Fatalf("stored recommendation = %#v, want %#v", stored.Recommendation, want)
	}
	if stored.DecisionSurface != (domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}) {
		t.Fatalf("stored decision surface = %#v", stored.DecisionSurface)
	}

	second := storedAgentRecommendationRecord(
		t, item, surface, domain.InvocationID("review-"+string(runID)+"-1"))
	second.Reason += " Duplicate record."
	second, err = domain.NewRecommendationSourceRecord(second)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRecommendationSource(ctx, second)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if stored.Recommendation != nil {
		t.Fatalf("two eligible records rendered %#v, want absent", stored.Recommendation)
	}
	if len(stored.RequestedDecision) != len(item.RequestedDecision) {
		t.Fatalf("collision changed requested_decision: %v", stored.RequestedDecision)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		records, err := tx.ListRecommendationSources(ctx, item.ID)
		if err != nil {
			return err
		}
		if len(records) != 2 {
			t.Fatalf("source count = %d, want 2", len(records))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRecommendationSourceStalesOnStructuralTransition(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-recommendation-stale")
	at := time.Date(2026, 8, 29, 11, 0, 0, 0, time.UTC)
	finding := adjudicationFinding("finding-recommendation-stale", runID, "daemon/a.go", at)
	st := seedReviewRound(t, runID, 1, []domain.Finding{finding}, at)
	artifact := modelAdjudication(t, runID, 1, finding.ID, at)
	item := adjudicationItem(t, "item-recommendation-stale", bindingFromAdjudication(artifact, finding))
	surface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatal(err)
	}
	record := storedAgentRecommendationRecord(
		t, item, surface, domain.InvocationID("review-"+string(runID)+"-1"))
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutFindingAdjudication(ctx, artifact); err != nil {
			return err
		}
		if err := tx.PutRecommendationSource(ctx, record); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}

	updated := item
	updated.ItemVersion++
	updated.RequestedDecision = append(updated.RequestedDecision, domain.ActionDismiss)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, updated)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if got.Recommendation != nil || got.DecisionSurface.Epoch != 2 {
			t.Fatalf("transition derived %#v at surface %#v", got.Recommendation, got.DecisionSurface)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAttentionItemDerivedFieldReplayDoesNotAdvanceVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	runID := domain.RunID("run-derived-replay")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-derived-replay", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionBlocked, Priority: domain.PriorityNormal,
		Reason: "waiting", RequestedDecision: []domain.Action{}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatal(err)
	}
	var before store.Snapshot
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, snap, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
		before = snap
		return err
	}); err != nil {
		t.Fatal(err)
	}
	caller := item
	caller.DecisionSurface = domain.DecisionSurfaceRef{Epoch: 7, Digest: adjudicationDigest("e")}
	caller.Recommendation = &domain.Recommendation{Action: domain.ActionStop, Reason: "caller value"}
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, caller) }); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, snap, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
		if err != nil {
			return err
		}
		if snap.EntityVersion != before.EntityVersion {
			t.Fatalf("entity_version advanced from %d to %d", before.EntityVersion, snap.EntityVersion)
		}
		if got.Recommendation != nil || got.DecisionSurface.Epoch != 1 {
			t.Fatalf("replay stored caller-derived fields: %#v / %#v", got.Recommendation, got.DecisionSurface)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDaemonRecommendationAuthorityCanDisappearAndReappear(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	ruleDigest := adjudicationDigest("c")
	inputDigest := adjudicationDigest("d")
	rule := registeredRecommendationRule{
		projection: domain.RecommendationProjection{Action: domain.ActionDiscuss, Reason: "Discuss the policy exception."},
		input:      inputDigest,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-rule-recommendation", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionBlocked, Priority: domain.PriorityNormal,
		Reason:            "policy needs operator judgment",
		RequestedDecision: []domain.Action{domain.ActionDiscuss, domain.ActionStop},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	surface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatal(err)
	}
	record, err := domain.NewRecommendationSourceRecord(domain.RecommendationSourceRecord{
		ItemID: item.ID, Source: domain.RecommendationDaemonPolicy,
		Provenance: domain.RecommendationProvenance{DaemonPolicy: &domain.DaemonPolicyRecommendationProvenance{
			RuleDigest: ruleDigest, InputDigest: inputDigest,
		}},
		Action: rule.projection.Action, Reason: rule.projection.Reason,
		DecisionSurfaceDigest: surface.Digest,
	})
	if err != nil {
		t.Fatal(err)
	}

	open := func(rules map[domain.Digest]domain.DaemonPolicyRule) *store.Store {
		t.Helper()
		st, err := store.Open(ctx, path, store.Options{RecommendationRules: rules})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	read := func(st *store.Store) *domain.Recommendation {
		t.Helper()
		var got domain.AttentionItem
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, err = tx.GetAttentionItem(ctx, item.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got.Recommendation
	}

	st := open(map[domain.Digest]domain.DaemonPolicyRule{ruleDigest: rule})
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRecommendationSource(ctx, record); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(st); got == nil || got.Action != domain.ActionDiscuss {
		t.Fatalf("registered rule recommendation = %#v", got)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = open(nil)
	if got := read(st); got != nil {
		t.Fatalf("deregistered rule recommendation = %#v, want absent", got)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = open(map[domain.Digest]domain.DaemonPolicyRule{ruleDigest: rule})
	t.Cleanup(func() { _ = st.Close() })
	if got := read(st); got == nil || got.Action != domain.ActionDiscuss {
		t.Fatalf("restored rule recommendation = %#v", got)
	}
}
