package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestAttentionYieldHistoryMigrationAppliesFromPriorHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0055_")
	if got := rawVersion(t, db); got != 54 {
		t.Fatalf("prior schema version = %d, want 54", got)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 62 {
		t.Fatalf("schema version = %d, want 62", got)
	}
	var count int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(yield_history) FROM attention_items`,
	).Scan(&count); err != nil {
		t.Fatalf("query yield_history column: %v", err)
	}
}

func TestReadyItemReviewYieldHistorySurvivesRestartAndRejectsDivergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	history := domain.ReviewYieldHistory{
		Rounds: []domain.ReviewYieldRound{
			{Round: 1, FindingsIngested: 2, NewFindings: 2, Declined: 1, Outcome: domain.ReviewFindings},
			{Round: 2, FindingsIngested: 1, RecurringFindings: 1, Fixed: 1, Outcome: domain.ReviewFindings},
			{Round: 3, Outcome: domain.ReviewClean},
		},
		TerminalOutcome: domain.ReviewClean,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-yield-ready", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectProject, ID: "project-1"},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "published after review convergence",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionDismiss},
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
		Readiness: &domain.ReadinessSummary{
			Class: domain.ReadinessReadyClean, EvaluationSetDigest: "sha256:evaluation",
		},
		YieldHistory: &history,
		ItemVersion:  1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

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
	if got.YieldHistory == nil || len(got.YieldHistory.Rounds) != 3 ||
		got.YieldHistory.Rounds[1].RecurringFindings != 1 {
		t.Fatalf("yield history after restart = %+v", got.YieldHistory)
	}
	if got.Readiness == nil || *got.Readiness != *item.Readiness {
		t.Fatalf("readiness after restart = %+v, want %+v", got.Readiness, item.Readiness)
	}

	var originalBody, originalHistory string
	if err := st.db.QueryRowContext(ctx,
		`SELECT body, yield_history FROM attention_items WHERE id = ?`, item.ID,
	).Scan(&originalBody, &originalHistory); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"stripped body history", `UPDATE attention_items SET body = json_remove(body, '$.yield_history') WHERE id = ?`},
		{"stripped store history", `UPDATE attention_items SET yield_history = NULL WHERE id = ?`},
		{"changed body history", `UPDATE attention_items SET body = json_set(body, '$.yield_history.rounds[0].declined', 0) WHERE id = ?`},
		{"changed store history", `UPDATE attention_items SET yield_history = json_set(yield_history, '$.rounds[0].declined', 0) WHERE id = ?`},
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
				`UPDATE attention_items SET body = ?, yield_history = ? WHERE id = ?`,
				originalBody, originalHistory, item.ID,
			); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiminishingItemReviewYieldHistorySurvivesRestartAndRejectsDivergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	history := domain.ReviewYieldHistory{
		Rounds: []domain.ReviewYieldRound{
			{Round: 1, FindingsIngested: 4, NewFindings: 4, Fixed: 2, Declined: 1, Deferred: 1, Outcome: domain.ReviewFindings},
			{Round: 2, FindingsIngested: 3, NewFindings: 1, RecurringFindings: 2, Fixed: 1, Declined: 1, Deferred: 1, Outcome: domain.ReviewFindings},
			{Round: 3, FindingsIngested: 3, RecurringFindings: 3, Declined: 2, Deferred: 1, Outcome: domain.ReviewFindings},
		},
		TerminalOutcome: domain.ReviewFindings,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-yield-diminishing", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectProject, ID: "project-1"},
		Type:    domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal,
		Reason:            "review rounds are surfacing only marginal findings",
		RequestedDecision: []domain.Action{domain.ActionFinishNow, domain.ActionApplyThenFinish},
		YieldHistory:      &history,
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

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
	if got.YieldHistory == nil || len(got.YieldHistory.Rounds) != 3 ||
		got.YieldHistory.Rounds[2].RecurringFindings != 3 ||
		got.YieldHistory.TerminalOutcome != domain.ReviewFindings {
		t.Fatalf("yield history after restart = %+v", got.YieldHistory)
	}
	if got.Readiness != nil || got.PRReference != nil {
		t.Fatalf("diminishing item gained ready-only fields: readiness=%+v pr_reference=%+v",
			got.Readiness, got.PRReference)
	}

	if _, err := st.db.ExecContext(ctx,
		`UPDATE attention_items SET body = json_set(body, '$.yield_history.rounds[0].declined', 0) WHERE id = ?`,
		item.ID,
	); err != nil {
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
}
