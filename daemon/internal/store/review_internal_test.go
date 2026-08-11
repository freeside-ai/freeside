package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestReviewRecordReadRevalidatesCanonicalBody(t *testing.T) {
	t.Parallel()
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
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
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

func TestListReviewRecordsRejectsRunKeyOmissionTamper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/review-list.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	first := domain.Run{ID: "run-review-list", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	second := domain.Run{ID: "run-review-list-foreign", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-list-1", RunID: first.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head",
		CompletedAt:        time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, first); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, second); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, nil)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_records SET run_id = ? WHERE invocation_id = ?`, second.ID, record.InvocationID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.ListReviewRecords(ctx, first.ID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("run-key omission tamper list = %v, want row inconsistency", err)
	}
}

func TestReviewRecordReadPreservesLegacyMissingInstructionAuthority(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/review.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run := domain.Run{
		ID: "run-review-legacy", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-legacy-1", RunID: run.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head",
		CompletedAt:        time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
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
	var raw []byte
	if err := st.db.QueryRowContext(ctx,
		`SELECT body FROM review_records WHERE invocation_id = ?`, record.InvocationID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "instruction_digest")
	raw, err = json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_records SET body_digest = ?, body = ? WHERE invocation_id = ?`,
		reviewBodyDigest(string(raw)), string(raw), record.InvocationID); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetReviewRecord(ctx, record.InvocationID)
		if err == nil && got.InstructionDigest != "" {
			t.Fatalf("legacy instruction digest = %q", got.InstructionDigest)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewFailureReadRevalidatesCanonicalBody(t *testing.T) {
	t.Parallel()
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

func TestReviewRetryReadRevalidatesCanonicalBody(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, t.TempDir()+"/review-retry.db", Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	run := domain.Run{
		ID: "run-review-retry-tamper", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	retry := domain.ReviewRetry{
		RunID: run.ID, InvocationID: "review-retry-tamper-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head",
		ObservedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC), Reason: "transient",
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewRetry(ctx, retry)
	}); err != nil {
		t.Fatal(err)
	}
	// Rewrite the body's round without recomputing body_digest: the read must
	// fail closed rather than trust the decoded delay claim. A restored, larger
	// round would otherwise extend the backoff a rewriter never authorized.
	if _, err := st.db.ExecContext(ctx,
		`UPDATE review_retries SET body = json_set(body, '$.round', 9)
		 WHERE run_id = ?`, run.ID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetReviewRetry(ctx, run.ID)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("partially rewritten review retry read = %v", err)
	}
}
