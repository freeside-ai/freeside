package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestReviewRecoveryTransitionValidate(t *testing.T) {
	at := time.Date(2026, 8, 7, 1, 2, 3, 0, time.UTC)
	commandID := "cmd-recover-1"
	emptyCommand := ""
	valid := domain.ReviewRecoveryTransition{
		RunID: "run-1", InvocationID: "review-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head", FailureDigest: "sha256:failure",
		CommandID: &commandID, Reason: "operator authorized recovery", OccurredAt: at,
	}
	cases := []struct {
		name    string
		mutate  func(*domain.ReviewRecoveryTransition)
		wantErr error
	}{
		{"valid", func(*domain.ReviewRecoveryTransition) {}, nil},
		{"missing run", func(tr *domain.ReviewRecoveryTransition) { tr.RunID = "" }, domain.ErrEmptyID},
		{"missing invocation", func(tr *domain.ReviewRecoveryTransition) { tr.InvocationID = "" }, domain.ErrEmptyID},
		{"zero round", func(tr *domain.ReviewRecoveryTransition) { tr.Round = 0 }, domain.ErrNonPositive},
		{"missing base", func(tr *domain.ReviewRecoveryTransition) { tr.BaseSHA = "" }, domain.ErrEmptyField},
		{"missing head", func(tr *domain.ReviewRecoveryTransition) { tr.HeadSHA = "" }, domain.ErrEmptyField},
		{"missing digest", func(tr *domain.ReviewRecoveryTransition) { tr.FailureDigest = "" }, domain.ErrEmptyField},
		{"missing command", func(tr *domain.ReviewRecoveryTransition) { tr.CommandID = nil }, domain.ErrTransitionUnbacked},
		{"empty command", func(tr *domain.ReviewRecoveryTransition) { tr.CommandID = &emptyCommand }, domain.ErrTransitionUnbacked},
		{"missing reason", func(tr *domain.ReviewRecoveryTransition) { tr.Reason = "" }, domain.ErrEmptyField},
		{"zero time", func(tr *domain.ReviewRecoveryTransition) { tr.OccurredAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC time", func(tr *domain.ReviewRecoveryTransition) {
			tr.OccurredAt = at.In(time.FixedZone("CST", -6*60*60))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := valid
			tc.mutate(&got)
			err := got.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
	if valid.AuthorizingAction() != domain.ActionRecoverReview {
		t.Fatalf("AuthorizingAction() = %q, want %q", valid.AuthorizingAction(), domain.ActionRecoverReview)
	}
}

func TestReviewRecoveryBindingMatchesExactFailure(t *testing.T) {
	failure := domain.ReviewFailure{
		RunID: "run-1", InvocationID: "review-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head",
	}
	binding := domain.ReviewRecoveryBinding{
		RunID: failure.RunID, InvocationID: failure.InvocationID, Round: failure.Round,
		BaseSHA: failure.BaseSHA, HeadSHA: failure.HeadSHA, FailureDigest: "sha256:failure",
	}
	if !binding.Matches(failure, "sha256:failure") {
		t.Fatal("matching binding did not match")
	}
	for name, mutate := range map[string]func(*domain.ReviewRecoveryBinding){
		"run":        func(b *domain.ReviewRecoveryBinding) { b.RunID = "run-2" },
		"invocation": func(b *domain.ReviewRecoveryBinding) { b.InvocationID = "review-2" },
		"round":      func(b *domain.ReviewRecoveryBinding) { b.Round++ },
		"base":       func(b *domain.ReviewRecoveryBinding) { b.BaseSHA = "other-base" },
		"head":       func(b *domain.ReviewRecoveryBinding) { b.HeadSHA = "other-head" },
		"digest":     func(b *domain.ReviewRecoveryBinding) { b.FailureDigest = "sha256:other" },
	} {
		t.Run(name, func(t *testing.T) {
			got := binding
			mutate(&got)
			if got.Matches(failure, "sha256:failure") {
				t.Fatal("mismatched binding matched")
			}
		})
	}
}

func TestAttentionItemReviewRecoveryBindingRules(t *testing.T) {
	runID := domain.RunID("run-1")
	binding := domain.ReviewRecoveryBinding{
		RunID: runID, InvocationID: "review-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head", FailureDigest: "sha256:failure",
	}
	recovery := validItemInput(domain.AttentionReviewContradiction)
	item, err := domain.NewAttentionItem(recovery, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	recovery.ReviewRecoveryBinding.Round = 2
	if item.ReviewRecoveryBinding.Round != 1 {
		t.Fatal("constructed item aliases caller recovery binding")
	}

	missing := validItemInput(domain.AttentionReviewContradiction)
	missing.ReviewRecoveryBinding = nil
	if _, err := domain.NewAttentionItem(missing, nil); !errors.Is(err, domain.ErrReviewRecoveryBindingMissing) {
		t.Fatalf("missing binding = %v, want %v", err, domain.ErrReviewRecoveryBindingMissing)
	}

	outside := validItemInput(domain.AttentionSpecApproval)
	outside.ReviewRecoveryBinding = &binding
	if _, err := domain.NewAttentionItem(outside, nil); !errors.Is(err, domain.ErrReviewRecoveryBindingOutsideItem) {
		t.Fatalf("binding outside recovery item = %v, want %v", err, domain.ErrReviewRecoveryBindingOutsideItem)
	}

	for name, mutate := range map[string]func(*domain.AttentionItemInput){
		"subject id": func(in *domain.AttentionItemInput) { in.Subject.ID = "run-2" },
		"subject run": func(in *domain.AttentionItemInput) {
			other := domain.RunID("run-2")
			in.Subject.RunID = &other
		},
		"head": func(in *domain.AttentionItemInput) { in.PRHeadSHA = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			in := validItemInput(domain.AttentionReviewContradiction)
			mutate(&in)
			if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, domain.ErrReviewRecoveryBindingMismatch) {
				t.Fatalf("NewAttentionItem = %v, want %v", err, domain.ErrReviewRecoveryBindingMismatch)
			}
		})
	}
}
