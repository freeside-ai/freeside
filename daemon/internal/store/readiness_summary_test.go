package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestDegradedReadyItemReadinessSurvivesRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-degraded-ready", ProjectID: "project-1",
		Subject:  domain.Subject{Type: domain.SubjectProject, ID: "project-1"},
		Type:     domain.AttentionReadyForFinalReview,
		Priority: domain.PriorityNormal,
		Reason:   "published with an allowed advisory failure",
		RequestedDecision: []domain.Action{
			domain.ActionOpenPR, domain.ActionDismiss,
		},
		PRReference: &domain.PRReference{Repo: "owner/repo", Number: 123},
		Readiness: &domain.ReadinessSummary{
			Class: domain.ReadinessReadyDegraded, EvaluationSetDigest: "sha256:evaluation",
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	st := openTemplateStoreAt(t, path, Options{})
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st = openTemplateStoreAt(t, path, Options{})
	var got domain.AttentionItem
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.Readiness == nil || *got.Readiness != *item.Readiness {
		t.Fatalf("readiness after restart = %+v, want %+v", got.Readiness, item.Readiness)
	}

	var originalBody, originalSummary string
	if err := st.db.QueryRowContext(ctx,
		`SELECT body, readiness_summary FROM attention_items WHERE id = ?`, item.ID,
	).Scan(&originalBody, &originalSummary); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"stripped body summary", `UPDATE attention_items SET body = json_remove(body, '$.readiness') WHERE id = ?`},
		{"changed body class", `UPDATE attention_items SET body = json_set(body, '$.readiness.class', 'ready_clean') WHERE id = ?`},
		{"changed body digest", `UPDATE attention_items SET body = json_set(body, '$.readiness.evaluation_set_digest', 'sha256:forged') WHERE id = ?`},
		{"changed store summary", `UPDATE attention_items SET readiness_summary = json_set(readiness_summary, '$.class', 'ready_clean') WHERE id = ?`},
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
				`UPDATE attention_items SET body = ?, readiness_summary = ? WHERE id = ?`,
				originalBody, originalSummary, item.ID,
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}
