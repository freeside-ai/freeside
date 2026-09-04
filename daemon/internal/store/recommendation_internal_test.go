package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func internalRecommendationDigest(fill string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(fill, 64))
}

type internalRecommendationRule struct {
	projection domain.RecommendationProjection
	input      domain.Digest
}

func (r internalRecommendationRule) EvaluateRecommendation(
	domain.AttentionItem, domain.DecisionSurface,
) (domain.RecommendationProjection, domain.Digest, bool, error) {
	return r.projection, r.input, true, nil
}

func TestRecommendationMismatchSuppressesOnlyDerivedProjection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ruleDigest := internalRecommendationDigest("a")
	inputDigest := internalRecommendationDigest("b")
	rule := internalRecommendationRule{
		projection: domain.RecommendationProjection{Action: domain.ActionDiscuss, Reason: "Discuss the policy exception."},
		input:      inputDigest,
	}
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{
		RecommendationRules: map[domain.Digest]domain.DaemonPolicyRule{ruleDigest: rule},
	})
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-recommendation-mismatch", ProjectID: "project-1",
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
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRecommendationSource(ctx, record); err != nil {
			return err
		}
		if err := tx.PutRecommendationSource(ctx, record); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.db.ExecContext(ctx, `UPDATE attention_items
SET body = json_set(body, '$.recommendation.reason', 'substituted but structurally valid')
WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if got.Recommendation != nil || len(got.RequestedDecision) != 2 {
			t.Fatalf("snapshot = %#v / %v, want readable item with absent recommendation", got.Recommendation, got.RequestedDecision)
		}
		recordTier, err := tx.GetAttentionItemRecord(ctx, item.ID)
		if err != nil {
			return err
		}
		if recordTier.Recommendation == nil || recordTier.Recommendation.Reason != "substituted but structurally valid" {
			t.Fatalf("record tier lost stored projection: %#v", recordTier.Recommendation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := st.db.ExecContext(ctx, `UPDATE attention_recommendation_sources
SET source = 'project_policy' WHERE digest = ?`, record.Digest); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListRecommendationSources(ctx, item.ID)
		return err
	}); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("tampered source row = %v, want row inconsistency", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		if got.Recommendation != nil {
			t.Fatalf("corrupt source rendered recommendation %#v", got.Recommendation)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
