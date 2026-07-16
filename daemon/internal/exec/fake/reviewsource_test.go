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
		Outcome: fake.OutcomeComplete,
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
		Outcome: fake.OutcomeComplete,
		Result:  exec.ReviewResult{HeadSHA: "cafebabe"},
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
		Outcome: fake.OutcomeComplete,
		Result:  exec.ReviewResult{HeadSHA: "cafebabe"},
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

// TestReviewSourceStaleHeadFailsVerify is scenario 4d, and #36's freshness
// half (request A / result A / current B): a review that ran against a
// superseded head fails freshness verification against the current one while
// still passing the request-binding gate (result head == requested head).
func TestReviewSourceStaleHeadFailsVerify(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result:  exec.ReviewResult{HeadSHA: "0ld0ld"},
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

// TestReviewSourceResultHeadMismatchFailsVerify is #36's binding half (request
// A / result B / verify B): a review invocation permanently binds the head it
// requested, so a result that ran against a different head fails verification
// as ErrResultHeadMismatch even when that head equals the caller's expected
// head. The mis-headed result still commits and re-delivers (a real reviewer
// that reviewed the wrong head returns something); the binding gate, not Poll,
// is what refuses it.
func TestReviewSourceResultHeadMismatchFailsVerify(t *testing.T) {
	s := fake.NewReviewSource()
	// Script a result whose head differs from the head the request commits.
	s.Script("inv-1", fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result:  exec.ReviewResult{HeadSHA: "headB"},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "headA"}); err != nil {
		t.Fatal(err)
	}
	// Poll delivers the committed (mis-headed) result unchanged: binding is
	// Verify's job, not Poll's.
	result, _ := pollUntilResult(t, s, "inv-1")
	if result.HeadSHA != "headB" {
		t.Errorf("polled result head = %q, want the committed %q", result.HeadSHA, "headB")
	}

	// Verify against the result's own head: the freshness comparison would
	// pass (headB == headB), but binding fails first because the request
	// committed headA.
	if err := s.Verify(t.Context(), "inv-1", "headB"); !errors.Is(err, fake.ErrResultHeadMismatch) {
		t.Errorf("verify against the result head = %v, want ErrResultHeadMismatch", err)
	}
	// Verify against the requested head fails too: the result never ran
	// against headA, so no expected head can bind it.
	if err := s.Verify(t.Context(), "inv-1", "headA"); !errors.Is(err, fake.ErrResultHeadMismatch) {
		t.Errorf("verify against the requested head = %v, want ErrResultHeadMismatch", err)
	}
}

// TestReviewSourceDelayedReview is scenario 4e: delivery lag is scripted
// poll-steps, independent of the execution lag Inspect observes.
func TestReviewSourceDelayedReview(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Outcome:         fake.OutcomeComplete,
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

// TestReviewSourceFailedOutcome: a failed review reports StatusFailed and
// commits no result, so Poll and Verify both answer ErrNoResult.
func TestReviewSourceFailedOutcome(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Outcome: fake.OutcomeFail,
		Result:  exec.ReviewResult{HeadSHA: "cafebabe"},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusFailed {
		t.Errorf("inspect = %v, %v; want failed", status, err)
	}
	if _, err := s.Poll(t.Context(), "inv-1"); !errors.Is(err, exec.ErrNoResult) {
		t.Errorf("poll of a failed review = %v, want ErrNoResult", err)
	}
	if err := s.Verify(t.Context(), "inv-1", "cafebabe"); !errors.Is(err, exec.ErrNoResult) {
		t.Errorf("verify of a failed review = %v, want ErrNoResult", err)
	}
}

// TestReviewSourceFailedWaitsForExecution: while a failed review is still
// running (inspect lag unspent), Poll and Verify report not-ready, never the
// terminal no-result; only once Inspect drives it to the failure do they
// answer ErrNoResult. Poll must not leak the eventual outcome ahead of Inspect.
func TestReviewSourceFailedWaitsForExecution(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		PendingInspects: 2,
		Outcome:         fake.OutcomeFail,
		Result:          exec.ReviewResult{HeadSHA: "cafebabe"},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Poll(t.Context(), "inv-1"); !errors.Is(err, exec.ErrResultNotReady) {
		t.Errorf("poll while still running = %v, want ErrResultNotReady", err)
	}
	if err := s.Verify(t.Context(), "inv-1", "cafebabe"); !errors.Is(err, exec.ErrResultNotReady) {
		t.Errorf("verify while still running = %v, want ErrResultNotReady", err)
	}

	for range 2 {
		if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusRunning {
			t.Fatalf("inspect during lag = %v, %v; want running", status, err)
		}
	}
	if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusFailed {
		t.Fatalf("inspect after lag = %v, %v; want failed", status, err)
	}
	if _, err := s.Poll(t.Context(), "inv-1"); !errors.Is(err, exec.ErrNoResult) {
		t.Errorf("poll after failure = %v, want ErrNoResult", err)
	}
	if err := s.Verify(t.Context(), "inv-1", "cafebabe"); !errors.Is(err, exec.ErrNoResult) {
		t.Errorf("verify after failure = %v, want ErrNoResult", err)
	}
}

// TestReviewSourceCrashBeforeResult: the session is lost before any result;
// Inspect reports gone and there is nothing to poll.
func TestReviewSourceCrashBeforeResult(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Outcome: fake.OutcomeCrashBeforeResult,
		Result:  exec.ReviewResult{HeadSHA: "cafebabe"},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusGone {
		t.Errorf("inspect = %v, %v; want gone", status, err)
	}
	if _, err := s.Poll(t.Context(), "inv-1"); !errors.Is(err, exec.ErrNoResult) {
		t.Errorf("poll after crash-before-result = %v, want ErrNoResult", err)
	}
}

// TestReviewSourceCrashAfterResultRecoverable: the result committed before the
// session was lost, so Inspect reports gone but the result stays pollable by
// id (§5.3 reconciliation), and redelivery is a stable replay.
func TestReviewSourceCrashAfterResultRecoverable(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Outcome: fake.OutcomeCrashAfterResult,
		Result:  exec.ReviewResult{HeadSHA: "cafebabe"},
	})

	if err := s.RequestReview(t.Context(), "inv-1", exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatal(err)
	}
	if status, err := s.Inspect(t.Context(), "inv-1"); err != nil || status != exec.StatusGone {
		t.Errorf("inspect = %v, %v; want gone", status, err)
	}
	first, err := s.Poll(t.Context(), "inv-1")
	if err != nil {
		t.Fatalf("result must be recoverable by id: %v", err)
	}
	if first.InvocationID != "inv-1" || first.HeadSHA != "cafebabe" {
		t.Errorf("recovered result = %+v; want inv-1, cafebabe", first)
	}
	acc := newAcceptor()
	acc.accept(t, "inv-1", first)
	second, err := s.Poll(t.Context(), "inv-1")
	if err != nil {
		t.Fatal(err)
	}
	if acc.accept(t, "inv-1", second) {
		t.Error("second poll accepted as a new result; want identical replay")
	}
	if err := s.Verify(t.Context(), "inv-1", "cafebabe"); err != nil {
		t.Errorf("verify the recovered result = %v, want nil", err)
	}
}

// TestReviewSourceGuards covers the identity guards: one committed intent
// per id, loud unscripted requests, unknown ids.
func TestReviewSourceGuards(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{Outcome: fake.OutcomeComplete, Result: exec.ReviewResult{HeadSHA: "cafebabe"}})

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
