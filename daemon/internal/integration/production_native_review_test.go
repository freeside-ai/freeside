package integration_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// appendNativeObservation writes a native review observation directly to the
// store, standing in for the cmd/freesided active-resource reconciler that
// records native review activity in production (issue #497). The engine
// readiness path never reads these rows.
func (p *productionPublicationHarness) appendNativeObservation(t *testing.T, o domain.NativeReviewObservation) {
	t.Helper()
	if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
		_, err := tx.AppendNativeReviewObservation(p.ctx, o)
		return err
	}); err != nil {
		t.Fatalf("append native observation: %v", err)
	}
}

func (p *productionPublicationHarness) nativeCleanPass(nativeID int64) domain.NativeReviewObservation {
	return domain.NativeReviewObservation{
		Repo: fakePublicationRepo, RepositoryID: p.profile.RepositoryID, PRNumber: 101,
		Provider: domain.NativeReviewCodexGitHub, Kind: domain.NativeReviewCleanPass,
		NativeID: nativeID, AuthorLogin: "chatgpt-codex-connector",
		BindingHeadSHA: p.replay.HeadSHA,
		SubmittedAt:    p.now.UTC(), ObservedAt: p.now.UTC(),
	}
}

// TestProductionNativeCleanPassNeverSatisfiesReadiness proves a native clean-pass
// signal in the store never yields readiness when the Freeside-invoked review
// found findings: readiness stays gated on the exact Freeside pass (plan §6,
// §7; issue #497), and the native observation coexists as extra evidence.
func TestProductionNativeCleanPassNeverSatisfiesReadiness(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	reviewID := engine.ProductionReviewInvocationID(p.runID, 1)
	p.reviewer.Script(reviewID, fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
			Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
			CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("review findings")),
			Findings: []domain.Finding{{
				ID: "review-finding-1", RunID: p.runID, Source: "codex_local", Severity: "P1",
				Location: "daemon/main.go:12", Message: "unsafe transition", RawText: "unsafe transition",
				CreatedAt: p.now,
			}},
		},
	})
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}

	// A native reviewer posts a clean-pass reaction on the PR.
	p.appendNativeObservation(t, p.nativeCleanPass(700300))

	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.ReadyItemsCreated != 0 {
		t.Fatalf("native clean pass created readiness: %#v", result)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetAttentionItem(p.ctx, domain.ItemID("production-ready-"+string(p.runID))); !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("ready item exists despite Freeside findings: %w", err)
		}
		obs, err := tx.ListNativeReviewObservations(p.ctx, p.profile.RepositoryID, 101)
		if err != nil {
			return err
		}
		if len(obs) != 1 || obs[0].Kind != domain.NativeReviewCleanPass {
			return fmt.Errorf("native observation not recorded alongside: %+v", obs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProductionReadyCoexistsWithNativeObservations proves a clean Freeside pass
// yields exactly one ready item with native observations present: the native
// evidence is additive, never a second or substitute readiness path, and a
// later pass never re-derives readiness from it.
func TestProductionReadyCoexistsWithNativeObservations(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if result.ReadyItemsCreated != 1 {
		t.Fatalf("clean review did not yield ready: %#v", result)
	}
	p.assertReady(t)

	p.appendNativeObservation(t, p.nativeCleanPass(700301))
	again, err := p.reconcileLanes()
	if err != nil {
		t.Fatal(err)
	}
	if again.ReadyItemsCreated != 0 {
		t.Fatalf("native observation re-created readiness: %#v", again)
	}
	p.assertReady(t)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		obs, err := tx.ListNativeReviewObservations(p.ctx, p.profile.RepositoryID, 101)
		if err != nil {
			return err
		}
		if len(obs) != 1 {
			return fmt.Errorf("native observation not recorded alongside ready item: %+v", obs)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
