package fake_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// reopenReviewSource simulates a daemon restart for the review fake.
func reopenReviewSource(t *testing.T, dir string) *fake.ReviewSource {
	t.Helper()
	s, err := fake.NewReviewSourceAt(dir)
	if err != nil {
		t.Fatalf("reopen review source at %s: %v", dir, err)
	}
	return s
}

// TestReviewSourceRestartBeforeIntentDispatch mirrors the stage case: a
// scripted-but-unrequested review is still requestable after a restart.
func TestReviewSourceRestartBeforeIntentDispatch(t *testing.T) {
	dir := t.TempDir()
	s, err := fake.NewReviewSourceAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Script("inv-1", fake.ReviewScript{Outcome: fake.OutcomeComplete, Result: exec.ReviewResult{HeadSHA: "cafebabe"}})

	s = reopenReviewSource(t, dir)
	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatalf("request after restart must succeed (no phantom intent): %v", err)
	}
	result, _ := pollUntilResult(t, s, "inv-1")
	if result.InvocationID != "inv-1" {
		t.Errorf("result invocation_id = %q, want inv-1", result.InvocationID)
	}
}

// TestReviewSourceRestartAfterIntentBeforeResult: the intent committed but no
// result did; after restart the review reads gone with no result, and a
// re-request is a duplicate.
func TestReviewSourceRestartAfterIntentBeforeResult(t *testing.T) {
	dir := t.TempDir()
	s, err := fake.NewReviewSourceAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Script("inv-1", fake.ReviewScript{
		Outcome:      fake.OutcomeComplete,
		PendingPolls: 2,
		Result:       exec.ReviewResult{HeadSHA: "cafebabe"},
	})
	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	// A poll observes delivery lag (not-ready), then the process dies.
	if _, err := s.Poll(t.Context(), "inv-1"); !errors.Is(err, exec.ErrResultNotReady) {
		t.Fatalf("pre-restart poll = %v, want ErrResultNotReady", err)
	}

	s = reopenReviewSource(t, dir)
	if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusGone {
		t.Errorf("inspect after restart = %v, %v; want gone", status, err)
	}
	if _, err := s.Poll(t.Context(), "inv-1"); !errors.Is(err, exec.ErrNoResult) {
		t.Errorf("poll after restart = %v, want ErrNoResult", err)
	}
	if err := s.Verify(t.Context(), "inv-1", "cafebabe"); !errors.Is(err, exec.ErrNoResult) {
		t.Errorf("verify after restart = %v, want ErrNoResult", err)
	}
	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); !errors.Is(err, exec.ErrDuplicateStart) {
		t.Errorf("re-request of a committed intent = %v, want ErrDuplicateStart", err)
	}
}

// TestReviewSourceRestartAfterResultBeforeAcceptance: a committed review
// result survives the restart, re-delivers identically, and is accepted at
// most once across it.
func TestReviewSourceRestartAfterResultBeforeAcceptance(t *testing.T) {
	dir := t.TempDir()
	s, err := fake.NewReviewSourceAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Script("inv-1", fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			HeadSHA: "cafebabe",
			Findings: []domain.Finding{{
				ID: "finding-1", RunID: "run-1", Source: "codex",
				Location: "daemon/internal/exec/review.go:20", Message: "x", CreatedAt: fixedTime,
			}},
		},
	})
	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	acc := newAcceptor()
	before, _ := pollUntilResult(t, s, "inv-1")
	if !acc.accept(t, "inv-1", before) {
		t.Fatal("first delivery must be accepted")
	}

	s = reopenReviewSource(t, dir)
	if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusGone {
		t.Errorf("inspect after restart = %v, %v; want gone (session lost, result durable)", status, err)
	}
	after, err := s.Poll(t.Context(), "inv-1")
	if err != nil {
		t.Fatalf("committed review result must survive restart: %v", err)
	}
	if acc.accept(t, "inv-1", after) {
		t.Error("post-restart delivery accepted as a new result; the workflow would advance twice")
	}
	if err := s.Verify(t.Context(), "inv-1", "cafebabe"); err != nil {
		t.Errorf("verify the recovered result against its head = %v, want nil", err)
	}
}

// TestReviewSourceRestartCrashAfterResultRecoverable proves a result committed
// via the crash-after-result outcome (committed at the gone transition)
// survives the restart, while a failed review's absence of a result also
// survives (still ErrNoResult).
func TestReviewSourceRestartCrashAfterResultRecoverable(t *testing.T) {
	dir := t.TempDir()
	s, err := fake.NewReviewSourceAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	s.Script("inv-after", fake.ReviewScript{
		Outcome: fake.OutcomeCrashAfterResult,
		Result:  exec.ReviewResult{HeadSHA: "cafebabe"},
	})
	s.Script("inv-fail", fake.ReviewScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.ReviewResult{HeadSHA: "cafebabe"},
	})
	for _, id := range []domain.InvocationID{"inv-after", "inv-fail"} {
		if err := s.RequestReview(t.Context(), id, exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
			t.Fatal(err)
		}
		// Drive Inspect to the terminal transition (commits the crash-after
		// result before its session loss).
		if status, err := s.Inspect(t.Context(), id); err != nil {
			t.Fatalf("inspect %s: %v", id, err)
		} else if id == "inv-after" && status != exec.StatusGone {
			t.Errorf("inspect %s = %v; want gone", id, status)
		}
	}

	s = reopenReviewSource(t, dir)
	if r, err := s.Poll(t.Context(), "inv-after"); err != nil {
		t.Errorf("crash-after result must survive restart: %v", err)
	} else if r.HeadSHA != "cafebabe" {
		t.Errorf("recovered result head = %q, want cafebabe", r.HeadSHA)
	}
	if _, err := s.Poll(t.Context(), "inv-fail"); !errors.Is(err, exec.ErrNoResult) {
		t.Errorf("failed review after restart = %v, want ErrNoResult", err)
	}
}
