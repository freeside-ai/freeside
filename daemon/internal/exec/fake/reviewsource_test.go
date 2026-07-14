package fake_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// fixedTime keeps finding fixtures deterministic; the fakes themselves never
// touch a clock.
var fixedTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// pollUntilResult drives Poll through scripted delivery lag and returns the
// result plus how many polls returned not-ready (bounded, a runaway guard).
func pollUntilResult(t *testing.T, s *fake.ReviewSource, id domain.InvocationID) (exec.ReviewResult, int) {
	t.Helper()
	notReady := 0
	for range 32 {
		result, err := s.Poll(t.Context(), id)
		switch {
		case err == nil:
			return result, notReady
		case errors.Is(err, exec.ErrResultNotReady):
			notReady++
		default:
			t.Fatalf("poll %s: %v", id, err)
		}
	}
	t.Fatalf("poll %s: no result after 32 polls (broken script?)", id)
	return exec.ReviewResult{}, 0
}

// TestReviewSourceFindingsPass is scenario 4a: a review that returns
// findings, fresh against the expected head.
func TestReviewSourceFindingsPass(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Result: exec.ReviewResult{
			HeadSHA: "cafebabe",
			Findings: []domain.Finding{{
				ID:        "finding-1",
				RunID:     "run-1",
				Source:    "codex",
				Location:  "daemon/internal/exec/driver.go:12",
				Message:   "possible off-by-one",
				CreatedAt: fixedTime,
			}},
		},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusCompleted {
		t.Errorf("inspect = %v, %v; want completed", status, err)
	}
	result, notReady := pollUntilResult(t, s, "inv-1")
	if notReady != 0 {
		t.Errorf("not-ready polls = %d, want 0", notReady)
	}
	if result.InvocationID != "inv-1" {
		t.Errorf("result invocation_id = %q, want stamped %q", result.InvocationID, "inv-1")
	}
	if len(result.Findings) != 1 {
		t.Fatalf("findings = %d, want 1", len(result.Findings))
	}
	if err := result.Validate(); err != nil {
		t.Errorf("committed result must validate: %v", err)
	}
	if err := s.Verify(t.Context(), "inv-1", "cafebabe"); err != nil {
		t.Errorf("verify against the reviewed head = %v, want nil", err)
	}
}

// TestReviewSourceCleanPass is scenario 4b: a clean pass is an empty
// findings list on an otherwise ordinary result.
func TestReviewSourceCleanPass(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Result: exec.ReviewResult{HeadSHA: "cafebabe"},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	result, _ := pollUntilResult(t, s, "inv-1")
	if len(result.Findings) != 0 {
		t.Errorf("clean pass findings = %d, want 0", len(result.Findings))
	}
	if err := result.Validate(); err != nil {
		t.Errorf("clean-pass result must validate: %v", err)
	}
	if err := s.Verify(t.Context(), "inv-1", "cafebabe"); err != nil {
		t.Errorf("verify = %v, want nil", err)
	}
}

// TestReviewSourceDuplicatePollAcceptsOnce is scenario 4c: repeated polls
// re-deliver the identical committed result; acceptance keyed by invocation
// id admits exactly one.
func TestReviewSourceDuplicatePollAcceptsOnce(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Result: exec.ReviewResult{HeadSHA: "cafebabe"},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	acc := newAcceptor()
	accepted := 0
	for range 3 {
		result, err := s.Poll(t.Context(), "inv-1")
		if err != nil {
			t.Fatal(err)
		}
		if acc.accept(t, "inv-1", result) {
			accepted++
		}
	}
	if accepted != 1 {
		t.Errorf("accepted %d results across 3 polls, want exactly 1", accepted)
	}
}

// TestReviewSourceStaleHeadFailsVerify is scenario 4d: a review that ran
// against a superseded head fails freshness verification against the
// current one.
func TestReviewSourceStaleHeadFailsVerify(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Result: exec.ReviewResult{HeadSHA: "0ld0ld"},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "0ld0ld"}); err != nil {
		t.Fatal(err)
	}
	// Freshness of an undelivered review is unknowable, not assumed.
	if err := s.Verify(t.Context(), "inv-1", "0ld0ld"); !errors.Is(err, exec.ErrResultNotReady) {
		t.Errorf("verify before result = %v, want ErrResultNotReady", err)
	}
	pollUntilResult(t, s, "inv-1")

	if err := s.Verify(t.Context(), "inv-1", "n3wn3w"); !errors.Is(err, exec.ErrStaleHead) {
		t.Errorf("verify against a newer head = %v, want ErrStaleHead", err)
	}
	if err := s.Verify(t.Context(), "inv-1", "0ld0ld"); err != nil {
		t.Errorf("verify against the reviewed head = %v, want nil", err)
	}
}

// TestReviewSourceDelayedReview is scenario 4e: delivery lag is scripted
// poll-steps, independent of the execution lag Inspect observes.
func TestReviewSourceDelayedReview(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		PendingInspects: 1,
		PendingPolls:    2,
		Result:          exec.ReviewResult{HeadSHA: "cafebabe"},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusRunning {
		t.Errorf("first inspect = %v, %v; want running", status, err)
	}
	if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusCompleted {
		t.Errorf("second inspect = %v, %v; want completed", status, err)
	}
	result, notReady := pollUntilResult(t, s, "inv-1")
	if notReady != 2 {
		t.Errorf("not-ready polls = %d, want the scripted 2", notReady)
	}
	if result.HeadSHA != "cafebabe" {
		t.Errorf("result head = %q", result.HeadSHA)
	}
}

// TestReviewSourceGuards covers the identity guards: one committed intent
// per id, loud unscripted requests, unknown ids.
func TestReviewSourceGuards(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{Result: exec.ReviewResult{HeadSHA: "cafebabe"}})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{}); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{}); !errors.Is(err, exec.ErrDuplicateStart) {
		t.Errorf("second request = %v, want ErrDuplicateStart", err)
	}
	if err := s.RequestReview(t.Context(), "inv-unscripted", exec.ReviewRequest{}); !errors.Is(err, fake.ErrUnscripted) {
		t.Errorf("unscripted request = %v, want ErrUnscripted", err)
	}
	if _, err := s.Inspect(t.Context(), "inv-unknown"); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Errorf("unknown inspect = %v, want ErrUnknownInvocation", err)
	}
	if _, err := s.Poll(t.Context(), "inv-unknown"); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Errorf("unknown poll = %v, want ErrUnknownInvocation", err)
	}
	if err := s.Verify(t.Context(), "inv-unknown", "head"); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Errorf("unknown verify = %v, want ErrUnknownInvocation", err)
	}
}
