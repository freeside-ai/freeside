package store

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func degradedReadyDetailItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	recipe := domain.Digest("sha256:recipe")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-degraded-ready-detail", ProjectID: "project-1",
		Subject:  domain.Subject{Type: domain.SubjectProject, ID: "project-1"},
		Type:     domain.AttentionReadyForFinalReview,
		Priority: domain.PriorityNormal,
		Reason:   "published with a waived policy failure",
		RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionDismiss,
		},
		PRHeadSHA:   "head",
		PRReference: &domain.PRReference{Repo: "owner/repo", Number: 123},
		Readiness: &domain.ReadinessSummary{
			Class: domain.ReadinessReadyDegraded, EvaluationSetDigest: "sha256:evaluation",
		},
		ReadinessDetail: &domain.ReadinessDetail{
			EvaluationSetDigest: "sha256:evaluation", CandidateHead: "head",
			Base: domain.ReadinessBoundBase{BaseRef: "main", BaseSHA: "base"},
			Requirements: []domain.ReadinessRequirement{
				{
					RequirementKey: "clean-verification", CheckClass: domain.CheckClassCleanVerification,
					Kind: domain.RequirementRequired, State: domain.ReadinessRequirementPassed,
					ProofRecipeDigest: &recipe,
				},
				{
					RequirementKey: "repo-change-policy", CheckClass: domain.CheckClassRepoChangePolicy,
					Kind: domain.RequirementRequired, State: domain.ReadinessRequirementFailed,
					Waiver: &domain.ReadinessWaiver{
						ID: "waiver-1", Dimension: "repo_change_policy",
						Authority: domain.WaiverAuthorityHumanApproval,
						GrantedAt: time.Date(2026, 8, 24, 9, 27, 0, 0, time.UTC),
					},
				},
			},
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return item
}

func TestDegradedReadyItemReadinessDetailSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	item := degradedReadyDetailItem(t)

	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var got domain.AttentionItem
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.ReadinessDetail == nil || !reflect.DeepEqual(*got.ReadinessDetail, *item.ReadinessDetail) {
		t.Fatalf("readiness detail after restart = %+v, want %+v", got.ReadinessDetail, item.ReadinessDetail)
	}
	if waiver := got.ReadinessDetail.Requirements[1].Waiver; waiver == nil ||
		waiver.Authority != domain.WaiverAuthorityHumanApproval {
		t.Fatalf("waiver after restart = %+v", waiver)
	}

	var originalBody, originalDetail string
	if err := st.db.QueryRowContext(ctx,
		`SELECT body, readiness_detail FROM attention_items WHERE id = ?`, item.ID,
	).Scan(&originalBody, &originalDetail); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"stripped body detail", `UPDATE attention_items SET body = json_remove(body, '$.readiness_detail') WHERE id = ?`},
		{"changed body waiver authority", `UPDATE attention_items SET body = json_set(body, '$.readiness_detail.requirements[1].waiver.authority', 'daemon_trusted_configuration') WHERE id = ?`},
		{"changed body head", `UPDATE attention_items SET body = json_set(body, '$.readiness_detail.candidate_head', 'head', '$.pr_head_sha', 'head', '$.readiness_detail.base.base_sha', 'forged') WHERE id = ?`},
		{"changed store waiver authority", `UPDATE attention_items SET readiness_detail = json_set(readiness_detail, '$.requirements[1].waiver.authority', 'daemon_trusted_configuration') WHERE id = ?`},
		{"stripped store detail", `UPDATE attention_items SET readiness_detail = NULL WHERE id = ?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.sql, item.ID); err != nil {
				t.Fatal(err)
			}
			for _, read := range []struct {
				name string
				fn   func(*ReadTx) error
			}{
				{"get", func(tx *ReadTx) error {
					_, err := tx.GetAttentionItem(ctx, item.ID)
					return err
				}},
				{"list", func(tx *ReadTx) error {
					_, err := tx.ListAttentionItems(ctx)
					return err
				}},
			} {
				t.Run(read.name, func(t *testing.T) {
					if err := st.Read(ctx, read.fn); !errors.Is(err, errRowInconsistent) {
						t.Fatalf("read error = %v, want errRowInconsistent", err)
					}
				})
			}
			if _, err := st.db.ExecContext(ctx,
				`UPDATE attention_items SET body = ?, readiness_detail = ? WHERE id = ?`,
				originalBody, originalDetail, item.ID,
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestLegacyReadyItemWithoutReadinessDetailDecodes: a ready row persisted
// before the detail existed has a null column and no body field. It decodes
// with a nil detail rather than failing the trust-boundary comparison.
func TestLegacyReadyItemWithoutReadinessDetailDecodes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	item := degradedReadyDetailItem(t)
	item.ReadinessDetail = nil
	st := openTestStore(t)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE attention_items SET body = json_remove(body, '$.readiness_detail') WHERE id = ?`, item.ID,
	); err != nil {
		t.Fatal(err)
	}
	var got domain.AttentionItem
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.ReadinessDetail != nil || got.Readiness == nil {
		t.Fatalf("legacy item = readiness %+v detail %+v", got.Readiness, got.ReadinessDetail)
	}
}
