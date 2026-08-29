package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestAttentionItemReadAuthenticatesReviewDisputeBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/card-facts.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-card-facts", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	finding := domain.Finding{
		ID: "finding-1", RunID: run.ID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/review.go", StartLine: 1, EndLine: 1},
		Message:  "finding one", RawText: "finding one", CreatedAt: at,
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-card-facts", RunID: run.ID, Round: 2,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: at,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	runID := run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-card-facts", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionReviewDispute, Priority: domain.PriorityHigh,
		Reason: "a review finding is disputed", RequestedDecision: []domain.Action{domain.ActionDiscuss},
		ReviewDispute: &domain.ReviewDisputeBinding{
			RunID: run.ID, Round: record.Round, FindingIDs: []domain.FindingID{finding.ID},
			CompletionEvidence: record.CompletionEvidence,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	var originalBody string
	if err := st.db.QueryRowContext(ctx,
		`SELECT body FROM attention_items WHERE id = ?`, item.ID).Scan(&originalBody); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		path  string
		value any
	}{
		{"invented round", "$.review_dispute.round", 3},
		{"invented finding set", "$.review_dispute.finding_ids[0]", "finding-invented"},
		{"invented completion evidence", "$.review_dispute.completion_evidence", "sha256:" + strings.Repeat("f", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx,
				`UPDATE attention_items SET body = json_set(?, ?, ?) WHERE id = ?`,
				originalBody, tc.path, tc.value, item.ID); err != nil {
				t.Fatal(err)
			}
			reads := []struct {
				name string
				read func(*ReadTx) error
			}{
				{"snapshot", func(tx *ReadTx) error {
					_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
					return err
				}},
				{"history", func(tx *ReadTx) error {
					_, err := tx.GetAttentionItemRecord(ctx, item.ID)
					return err
				}},
			}
			for _, read := range reads {
				err := st.Read(ctx, read.read)
				if !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("%s read = %v, want ErrParentKeyMismatch", read.name, err)
				}
			}
		})
	}
}
