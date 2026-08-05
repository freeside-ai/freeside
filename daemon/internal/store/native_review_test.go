package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func nativeReviewFixtures(ts time.Time) (findings, clean domain.NativeReviewObservation) {
	findings = domain.NativeReviewObservation{
		Repo: "owner/repo", RepositoryID: 84958515, PRNumber: 450,
		Provider: domain.NativeReviewCodexGitHub, Kind: domain.NativeReviewFindings,
		NativeID: 900100, AuthorLogin: "chatgpt-codex-connector",
		ReviewCommitSHA: "cafebabe", ReviewState: "COMMENTED", BindingHeadSHA: "cafebabe",
		SubmittedAt: ts, ObservedAt: ts.Add(time.Minute),
		Findings: []domain.Finding{{
			ID: "native-comment-800200", RunID: "run-1", Source: "codex_github",
			Severity: "P2", Location: "daemon/main.go:42",
			Message: "unchecked error", RawText: "P2: the error return is dropped",
			CreatedAt: ts,
		}},
	}
	clean = domain.NativeReviewObservation{
		Repo: "owner/repo", RepositoryID: 84958515, PRNumber: 450,
		Provider: domain.NativeReviewCodexGitHub, Kind: domain.NativeReviewCleanPass,
		NativeID: 700300, AuthorLogin: "chatgpt-codex-connector",
		BindingHeadSHA: "cafebabe",
		SubmittedAt:    ts, ObservedAt: ts.Add(time.Minute),
	}
	return findings, clean
}

// TestNativeReviewObservationAppendOnMaterialChange mirrors the pull-fact rule
// for native review observations: a re-poll of unchanged native state
// coalesces, a material change (new findings) appends, and distinct identities
// (a findings review vs a clean-pass reaction) keep independent timelines.
func TestNativeReviewObservationAppendOnMaterialChange(t *testing.T) {
	ctx := context.Background()
	s := openCaptureStore(t)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	findings, clean := nativeReviewFixtures(ts)

	append_ := func(o domain.NativeReviewObservation) bool {
		var inserted bool
		if err := writeInternal(t, s, func(tx *store.InternalTx) error {
			var err error
			inserted, err = tx.AppendNativeReviewObservation(ctx, o)
			return err
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
		return inserted
	}

	if !append_(findings) {
		t.Fatal("first observation must append")
	}
	repeat := findings
	repeat.ObservedAt = findings.ObservedAt.Add(15 * time.Minute)
	if append_(repeat) {
		t.Fatal("an instant-only repeat must not append")
	}
	// An edited native review (new finding text) is a material change and
	// appends a fresh observation rather than rewriting the prior one.
	edited := findings
	edited.ObservedAt = findings.ObservedAt.Add(30 * time.Minute)
	edited.Findings = []domain.Finding{{
		ID: "native-comment-800200", RunID: "run-1", Source: "codex_github",
		Severity: "P1", Location: "daemon/main.go:42",
		Message: "unchecked error, now escalated", RawText: "P1: the error return is dropped",
		CreatedAt: ts,
	}}
	if !append_(edited) {
		t.Fatal("an edited native review must append")
	}
	// A dismissal that leaves the inline comments unchanged is a state-only
	// material change: without recording review_state it would coalesce and the
	// native timeline would never show the review was dismissed.
	dismissed := edited
	dismissed.ObservedAt = edited.ObservedAt.Add(15 * time.Minute)
	dismissed.ReviewState = "DISMISSED"
	if !append_(dismissed) {
		t.Fatal("a state-only dismissal must append")
	}
	if !append_(clean) {
		t.Fatal("a distinct clean-pass identity must append")
	}
	cleanRepeat := clean
	cleanRepeat.ObservedAt = clean.ObservedAt.Add(15 * time.Minute)
	if append_(cleanRepeat) {
		t.Fatal("an instant-only clean-pass repeat must not append")
	}

	var (
		timeline     []domain.NativeReviewObservation
		latestReview domain.NativeReviewObservation
		latestClean  domain.NativeReviewObservation
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if timeline, err = tx.ListNativeReviewObservations(ctx, 84958515, 450); err != nil {
			return err
		}
		if latestReview, err = tx.LatestNativeReviewObservation(ctx, 84958515, 450,
			domain.NativeReviewCodexGitHub, domain.NativeReviewFindings, 900100); err != nil {
			return err
		}
		latestClean, err = tx.LatestNativeReviewObservation(ctx, 84958515, 450,
			domain.NativeReviewCodexGitHub, domain.NativeReviewCleanPass, 700300)
		return err
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(timeline) != 4 {
		t.Fatalf("timeline length = %d, want 4 (findings, edited findings, dismissed findings, clean)", len(timeline))
	}
	if len(latestReview.Findings) != 1 || latestReview.Findings[0].Severity != "P1" ||
		latestReview.ReviewState != "DISMISSED" {
		t.Fatalf("latest findings review = %+v, want the dismissed P1 finding", latestReview)
	}
	if latestClean.Kind != domain.NativeReviewCleanPass || len(latestClean.Findings) != 0 {
		t.Fatalf("latest clean pass = %+v, want a finding-free clean signal", latestClean)
	}

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.LatestNativeReviewObservation(ctx, 84958515, 450,
			domain.NativeReviewCodexGitHub, domain.NativeReviewFindings, 999)
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unobserved identity = %v, want %v", err, store.ErrNotFound)
	}
}

// TestNativeReviewObservationStaleHeadRecorded proves a review that bound to a
// head other than the item's live binding head is persisted with the
// divergence visible rather than dropped.
func TestNativeReviewObservationStaleHeadRecorded(t *testing.T) {
	ctx := context.Background()
	s := openCaptureStore(t)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	stale, _ := nativeReviewFixtures(ts)
	stale.ReviewCommitSHA = "0ldc0de" // the review named an earlier head
	stale.BindingHeadSHA = "cafebabe" // the item advanced to a new head

	if err := writeInternal(t, s, func(tx *store.InternalTx) error {
		_, err := tx.AppendNativeReviewObservation(ctx, stale)
		return err
	}); err != nil {
		t.Fatalf("append stale-head review: %v", err)
	}
	var got domain.NativeReviewObservation
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.LatestNativeReviewObservation(ctx, 84958515, 450,
			domain.NativeReviewCodexGitHub, domain.NativeReviewFindings, 900100)
		return err
	}); err != nil {
		t.Fatalf("read stale-head review: %v", err)
	}
	if got.ReviewCommitSHA == got.BindingHeadSHA {
		t.Fatalf("stale-head divergence was not preserved: %+v", got)
	}
}
