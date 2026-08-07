package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestReviewRecordRoundTripsWithRawFindingsAndIsExclusiveWithFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	run := domain.Run{ID: "run-1", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	finding := domain.Finding{
		ID: "finding-1", RunID: run.ID, Source: "codex_local", Severity: "P2",
		Location: "daemon/main.go:12", Message: "unchecked error", RawText: "unchecked error",
		CreatedAt: when,
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-run-1-1", RunID: run.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)), CostOwner: "owner",
		BaseSHA: "base", HeadSHA: "head", CompletedAt: when,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, []domain.Finding{finding})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.LatestReviewRecord(ctx, run.ID)
		if err != nil {
			return err
		}
		if got.InvocationID != record.InvocationID || len(got.FindingIDs) != 1 {
			t.Fatalf("review record = %#v", got)
		}
		gotFinding, err := tx.GetFinding(ctx, finding.ID)
		if err != nil || gotFinding != finding {
			t.Fatalf("finding = %#v, %v", gotFinding, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	failure := domain.ReviewFailure{
		InvocationID: record.InvocationID, RunID: run.ID, Round: 1,
		BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureTransient,
		Reason: "lost session", ObservedAt: when,
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewFailure(ctx, failure)
	}); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("failure after result = %v", err)
	}
	// The migration triggers close the cross-table gap even when the caller
	// uses a different invocation id for the same run/round.
	failure.InvocationID = "review-run-1-alias"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewFailure(ctx, failure)
	}); err == nil {
		t.Fatal("same-round failure under a different invocation id succeeded")
	}
	failure.InvocationID = "review-run-1-2"
	failure.Round = 2
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewFailure(ctx, failure)
	}); err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-run-1-2-alias", RunID: run.ID, Round: 2,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)), CostOwner: "owner",
		BaseSHA: "base", HeadSHA: "head", CompletedAt: when,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(ctx, second, nil)
	}); err == nil {
		t.Fatal("same-round result under a different invocation id succeeded")
	}
}

func TestReviewRetryUpsertsAndClears(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	run := domain.Run{ID: "run-retry", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	first := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	retry := domain.ReviewRetry{
		RunID: run.ID, InvocationID: "review-run-retry-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head", ObservedAt: first, Reason: "transient poll failure",
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewRetry(ctx, retry)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetReviewRetry(ctx, run.ID)
		if err != nil {
			return err
		}
		if got != retry {
			t.Fatalf("review retry = %#v, want %#v", got, retry)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A repeated same-round transient advances the deadline in place: the same
	// key upserts rather than accumulating a second row.
	advanced := retry
	advanced.ObservedAt = first.Add(time.Second)
	advanced.Reason = "second transient poll failure"
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRetry(ctx, advanced)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetReviewRetry(ctx, run.ID)
		if err != nil {
			return err
		}
		if !got.ObservedAt.Equal(advanced.ObservedAt) || got.Reason != advanced.Reason {
			t.Fatalf("advanced review retry = %#v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// Delete is idempotent: the second delete on an absent row is not an error.
	for i := 0; i < 2; i++ {
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.DeleteReviewRetry(ctx, run.ID)
		}); err != nil {
			t.Fatalf("delete review retry #%d: %v", i, err)
		}
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetReviewRetry(ctx, run.ID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
}

func TestReviewRecordCanonicalizesFindingOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	run := domain.Run{ID: "run-order", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	findings := []domain.Finding{
		{ID: "finding-z", RunID: run.ID, Source: "codex_local", Severity: "P2", Location: "z.go:1", Message: "z", RawText: "z", CreatedAt: when},
		{ID: "finding-a", RunID: run.ID, Source: "codex_local", Severity: "P1", Location: "a.go:1", Message: "a", RawText: "a", CreatedAt: when},
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-run-order-1", RunID: run.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)), CostOwner: "owner",
		BaseSHA: "base", HeadSHA: "head", CompletedAt: when,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{findings[0].ID, findings[1].ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, findings)
	}); err != nil {
		t.Fatal(err)
	}
}

// TestPutReviewRecordRejectsNonUTCCompletedAt pins the write-boundary guard the
// #553 verification pass confirmed already exists: PutReviewRecord runs
// Validate before any write, so a ReviewRecord whose CompletedAt bypassed the
// constructor's UTC normalization (a +02:00 clock) is refused with
// ErrTimestampNotUTC and never persisted. This replaces the convergence
// regression test the pass refuted (the offset column and its non-convergence
// were unreachable behind this guard).
func TestPutReviewRecordRejectsNonUTCCompletedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	run := domain.Run{ID: "run-1", ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	finding := domain.Finding{
		ID: "finding-1", RunID: run.ID, Source: "codex_local", Severity: "P2",
		Location: "daemon/main.go:12", Message: "unchecked error", RawText: "unchecked error",
		CreatedAt: when,
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-run-1-1", RunID: run.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)), CostOwner: "owner",
		BaseSHA: "base", HeadSHA: "head", CompletedAt: when,
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Bypass the constructor's UTC normalization: stamp the same instant in a
	// +02:00 offset, as a caller building the struct directly could.
	record.CompletedAt = when.In(time.FixedZone("CEST", 2*60*60))

	err = st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRecord(ctx, record, []domain.Finding{finding})
	})
	if !errors.Is(err, domain.ErrTimestampNotUTC) {
		t.Fatalf("PutReviewRecord with a non-UTC completed_at = %v, want ErrTimestampNotUTC", err)
	}

	// The record was refused before any write: no row exists to read back.
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.LatestReviewRecord(ctx, run.ID)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LatestReviewRecord after a refused write = %v, want ErrNotFound", err)
	}
}
