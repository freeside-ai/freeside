package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestReviewRecordReadRevalidatesCanonicalBody(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/review.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run := domain.Run{
		ID: "run-review-tamper", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-tamper-1", RunID: run.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: when,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_records SET body = json_set(body, '$.configuration_digest', ?)
		 WHERE invocation_id = ?`, "sha256:"+strings.Repeat("f", 64), record.InvocationID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetReviewRecord(ctx, record.InvocationID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("partially rewritten review record read = %v", err)
	}
}

func TestReviewFailureReadRevalidatesCanonicalBody(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/review-failure.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run := domain.Run{
		ID: "run-review-failure", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	failure := domain.ReviewFailure{
		InvocationID: "review-failure-tamper-1", RunID: run.ID, Round: 1,
		BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureConfiguration,
		Reason: "configuration refused", ObservedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewFailure(ctx, failure)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_failures SET body = json_set(body, '$.reason', 'rewritten')
		 WHERE invocation_id = ?`, failure.InvocationID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetReviewFailure(ctx, failure.InvocationID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("partially rewritten review failure read = %v", err)
	}
}
