package contract

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

const (
	ReviewCaseStart                = "request_and_duplicate_request"
	ReviewCaseUnknownInvocation    = "unknown_invocation"
	ReviewCasePollBeforeReady      = "poll_before_ready"
	ReviewCaseIdempotentRedelivery = "idempotent_redelivery"
	ReviewCaseCrashBeforeResult    = "crash_before_result_after_restart"
	ReviewCaseCrashAfterResult     = "crash_after_result_after_restart"
	ReviewCaseStatusVocabulary     = "status_vocabulary_after_restart"
	ReviewCaseFailedOutcome        = "failed_outcome_after_restart"
	ReviewCaseStaleHead            = "superseded_head"
	ReviewCaseRequestAuthority     = "request_authority"
)

var reviewCases = map[string]struct{}{
	ReviewCaseStart: {}, ReviewCaseUnknownInvocation: {},
	ReviewCasePollBeforeReady: {}, ReviewCaseIdempotentRedelivery: {},
	ReviewCaseCrashBeforeResult: {}, ReviewCaseCrashAfterResult: {},
	ReviewCaseStatusVocabulary: {}, ReviewCaseStaleHead: {},
	ReviewCaseFailedOutcome:    {},
	ReviewCaseRequestAuthority: {},
}

// RunReviewSourceContract runs the reusable ReviewSource contract against one
// implementation factory.
func RunReviewSourceContract(t *testing.T, factory ReviewSourceFactory) {
	t.Helper()
	if factory.New == nil {
		t.Fatal("review source contract: nil factory")
	}
	divergences := divergenceMap(t, factory.KnownDivergences, reviewCases)

	runCase(t, ReviewCaseStart, divergences, func(t *testing.T) error {
		h, id, request := newReviewScenario(t, factory, OutcomeComplete)
		source := h.Source()
		if err := source.RequestReview(t.Context(), id, request); err != nil {
			return fmt.Errorf("request review: %w", err)
		}
		if err := source.RequestReview(t.Context(), id, request); !errors.Is(err, exec.ErrDuplicateStart) {
			return wrongError("duplicate RequestReview", "ErrDuplicateStart", err)
		}
		h.AwaitReady(t, id)
		source = h.Restart(t)
		if err := source.RequestReview(t.Context(), id, request); !errors.Is(err, exec.ErrDuplicateStart) {
			return wrongError("RequestReview after restart", "ErrDuplicateStart", err)
		}
		return nil
	})

	runCase(t, ReviewCaseUnknownInvocation, divergences, func(t *testing.T) error {
		h, _, request := newReviewScenario(t, factory, OutcomeComplete)
		source := h.Source()
		id := domain.InvocationID("contract-review-unknown")
		if _, err := source.Inspect(t.Context(), id); !errors.Is(err, exec.ErrUnknownInvocation) {
			return wrongError("Inspect unknown", "ErrUnknownInvocation", err)
		}
		if _, err := source.Poll(t.Context(), id); !errors.Is(err, exec.ErrUnknownInvocation) {
			return wrongError("Poll unknown", "ErrUnknownInvocation", err)
		}
		if err := source.Verify(t.Context(), id, request.BaseSHA, request.HeadSHA); !errors.Is(err, exec.ErrUnknownInvocation) {
			return wrongError("Verify unknown", "ErrUnknownInvocation", err)
		}
		verifier, ok := source.(exec.ReviewRequestAuthorityVerifier)
		if !ok {
			return errors.New("review source does not implement ReviewRequestAuthorityVerifier")
		}
		unknownDigest := domain.Digest(contentaddr.Sum([]byte("unknown review authority")))
		if err := verifier.VerifyRequestAuthority(t.Context(), id, unknownDigest); !errors.Is(err, exec.ErrUnknownInvocation) {
			return wrongError("VerifyRequestAuthority unknown", "ErrUnknownInvocation", err)
		}
		return nil
	})

	runCase(t, ReviewCasePollBeforeReady, divergences, func(t *testing.T) error {
		h, id, request := newReviewScenario(t, factory, OutcomeComplete)
		if err := h.Source().RequestReview(t.Context(), id, request); err != nil {
			return fmt.Errorf("request review: %w", err)
		}
		h.AwaitReady(t, id)
		if _, err := h.Source().Poll(t.Context(), id); !errors.Is(err, exec.ErrResultNotReady) {
			return wrongError("Poll before ready", "ErrResultNotReady", err)
		}
		return nil
	})

	runCase(t, ReviewCaseIdempotentRedelivery, divergences, func(t *testing.T) error {
		h, id, request := newReviewScenario(t, factory, OutcomeComplete)
		source := h.Source()
		if err := source.RequestReview(t.Context(), id, request); err != nil {
			return fmt.Errorf("request review: %w", err)
		}
		h.AwaitReady(t, id)
		h.Finish(t, id)
		first, err := source.Poll(t.Context(), id)
		if err != nil {
			return fmt.Errorf("first Poll: %w", err)
		}
		second, err := source.Poll(t.Context(), id)
		if err != nil || !reflect.DeepEqual(first, second) {
			return changedValue("repeated Poll", first, second, err)
		}
		source = h.Restart(t)
		third, err := source.Poll(t.Context(), id)
		if err != nil || !reflect.DeepEqual(first, third) {
			return changedValue("Poll after restart", first, third, err)
		}
		return nil
	})

	runCase(t, ReviewCaseCrashBeforeResult, divergences, func(t *testing.T) error {
		h, id, request := newReviewScenario(t, factory, OutcomeCrashBeforeResult)
		if err := h.Source().RequestReview(t.Context(), id, request); err != nil {
			return fmt.Errorf("request review: %w", err)
		}
		h.Finish(t, id)
		source := h.Restart(t)
		status, err := source.Inspect(t.Context(), id)
		if err != nil || status != exec.StatusGone {
			return wrongValue("Inspect after crash-before-result", status, err, "StatusGone")
		}
		if _, err := source.Poll(t.Context(), id); !errors.Is(err, exec.ErrNoResult) {
			return wrongError("Poll after crash-before-result", "ErrNoResult", err)
		}
		return nil
	})

	runCase(t, ReviewCaseCrashAfterResult, divergences, func(t *testing.T) error {
		h, id, request := newReviewScenario(t, factory, OutcomeCrashAfterResult)
		if err := h.Source().RequestReview(t.Context(), id, request); err != nil {
			return fmt.Errorf("request review: %w", err)
		}
		h.AwaitReady(t, id)
		h.Finish(t, id)
		source := h.Restart(t)
		status, err := source.Inspect(t.Context(), id)
		if err != nil || status != exec.StatusGone {
			return wrongValue("Inspect after crash-after-result", status, err, "StatusGone")
		}
		first, err := source.Poll(t.Context(), id)
		if err != nil {
			return fmt.Errorf("poll crash-after result: %w", err)
		}
		second, err := source.Poll(t.Context(), id)
		if err != nil || !reflect.DeepEqual(first, second) {
			return changedValue("crash-after Poll", first, second, err)
		}
		return nil
	})

	runCase(t, ReviewCaseStatusVocabulary, divergences, func(t *testing.T) error {
		h, id, request := newReviewScenario(t, factory, OutcomeComplete)
		source := h.Source()
		if err := source.RequestReview(t.Context(), id, request); err != nil {
			return fmt.Errorf("request review: %w", err)
		}
		h.AwaitReady(t, id)
		status, err := source.Inspect(t.Context(), id)
		if err != nil || status != exec.StatusRunning {
			return wrongValue("first Inspect", status, err, "StatusRunning")
		}
		if !slices.Contains(exec.AllStatuses, status) {
			return fmt.Errorf("first Inspect returned undeclared status %q", status)
		}
		h.Finish(t, id)
		status, err = source.Inspect(t.Context(), id)
		if err != nil || status != exec.StatusCompleted {
			return wrongValue("terminal Inspect", status, err, "StatusCompleted")
		}
		source = h.Restart(t)
		status, err = source.Inspect(t.Context(), id)
		if err != nil {
			return fmt.Errorf("inspect after terminal restart: %w", err)
		}
		if status != exec.StatusCompleted && status != exec.StatusGone {
			return wrongValue("Inspect after terminal restart", status, nil, "StatusCompleted or StatusGone")
		}
		return nil
	})

	runCase(t, ReviewCaseFailedOutcome, divergences, func(t *testing.T) error {
		h, id, request := newReviewScenario(t, factory, OutcomeFail)
		source := h.Source()
		if err := source.RequestReview(t.Context(), id, request); err != nil {
			return fmt.Errorf("request review: %w", err)
		}
		h.AwaitReady(t, id)
		h.Finish(t, id)
		status, err := source.Inspect(t.Context(), id)
		if err != nil || status != exec.StatusFailed {
			return wrongValue("failed Inspect", status, err, "StatusFailed")
		}
		if _, err := source.Poll(t.Context(), id); !errors.Is(err, exec.ErrNoResult) {
			return wrongError("Poll failed review", "ErrNoResult", err)
		}
		source = h.Restart(t)
		status, err = source.Inspect(t.Context(), id)
		if err != nil || status != exec.StatusFailed {
			return wrongValue("failed Inspect after restart", status, err, "StatusFailed")
		}
		if _, err := source.Poll(t.Context(), id); !errors.Is(err, exec.ErrNoResult) {
			return wrongError("Poll failed review after restart", "ErrNoResult", err)
		}
		return nil
	})

	runCase(t, ReviewCaseStaleHead, divergences, func(t *testing.T) error {
		h, id, request := newReviewScenario(t, factory, OutcomeComplete)
		source := h.Source()
		if err := source.RequestReview(t.Context(), id, request); err != nil {
			return fmt.Errorf("request review: %w", err)
		}
		h.Finish(t, id)
		if err := source.Verify(t.Context(), id, request.BaseSHA, request.HeadSHA); err != nil {
			return fmt.Errorf("verify current head: %w", err)
		}
		if err := source.Verify(t.Context(), id, request.BaseSHA, strings.Repeat("f", 40)); !errors.Is(err, exec.ErrStaleHead) {
			return wrongError("Verify superseded head", "ErrStaleHead", err)
		}
		if err := source.Verify(t.Context(), id, strings.Repeat("b", 40), request.HeadSHA); !errors.Is(err, exec.ErrStaleHead) {
			return wrongError("Verify superseded base", "ErrStaleHead", err)
		}
		return nil
	})

	runCase(t, ReviewCaseRequestAuthority, divergences, func(t *testing.T) error {
		h, id, request := newReviewScenario(t, factory, OutcomeComplete)
		source := h.Source()
		verifier, ok := source.(exec.ReviewRequestAuthorityVerifier)
		if !ok {
			return errors.New("review source does not implement ReviewRequestAuthorityVerifier")
		}
		if err := source.RequestReview(t.Context(), id, request); err != nil {
			return fmt.Errorf("request review: %w", err)
		}
		digest, err := request.AuthorityDigest()
		if err != nil {
			return fmt.Errorf("request authority digest: %w", err)
		}
		if err := verifier.VerifyRequestAuthority(t.Context(), id, digest); err != nil {
			return fmt.Errorf("verify request authority matching digest: %w", err)
		}
		mismatchedDigest := domain.Digest(contentaddr.Sum([]byte("mismatched review authority")))
		if err := verifier.VerifyRequestAuthority(t.Context(), id, mismatchedDigest); !errors.Is(err, domain.ErrParentKeyMismatch) {
			return wrongError("verify request authority mismatched digest", "ErrParentKeyMismatch", err)
		}
		return h.AuthorityRejectionComplete(t, id)
	})
}

func newReviewScenario(
	t *testing.T, factory ReviewSourceFactory, outcome Outcome,
) (ReviewSourceHarness, domain.InvocationID, exec.ReviewRequest) {
	t.Helper()
	if !outcome.valid() {
		t.Fatalf("unknown review contract outcome %q", outcome)
	}
	h := factory.New(t)
	id := domain.InvocationID("contract-review-invocation")
	request := h.Prepare(t, id, Scenario{Outcome: outcome})
	return h, id, request
}
