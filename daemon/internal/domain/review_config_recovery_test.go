package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validConfigRecoveryTransition() domain.ReviewConfigurationRecoveryTransition {
	commandID := "cmd-adopt-1"
	return domain.ReviewConfigurationRecoveryTransition{
		RunID: "run-1", InvocationID: "review-1", Round: 2,
		BaseSHA: "base", HeadSHA: "head", FailureDigest: "sha256:failure",
		Repo: "acme/widgets", RepositoryID: 7,
		SupersededProfileDigest:  "sha256:profile-old",
		SupersedingProfileDigest: "sha256:profile-new",
		CommandID:                &commandID,
		Reason:                   "operator adopted the superseding review configuration",
		OccurredAt:               time.Date(2026, 8, 8, 1, 2, 3, 0, time.UTC),
	}
}

func TestReviewConfigurationRecoveryTransitionValidate(t *testing.T) {
	emptyCommand := ""
	valid := validConfigRecoveryTransition()
	cases := []struct {
		name    string
		mutate  func(*domain.ReviewConfigurationRecoveryTransition)
		wantErr error
	}{
		{"valid", func(*domain.ReviewConfigurationRecoveryTransition) {}, nil},
		{"missing run", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.RunID = "" }, domain.ErrEmptyID},
		{"missing invocation", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.InvocationID = "" }, domain.ErrEmptyID},
		{"zero round", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.Round = 0 }, domain.ErrNonPositive},
		{"missing base", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.BaseSHA = "" }, domain.ErrEmptyField},
		{"missing head", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.HeadSHA = "" }, domain.ErrEmptyField},
		{"missing digest", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.FailureDigest = "" }, domain.ErrEmptyField},
		{"missing repo", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.Repo = "" }, domain.ErrEmptyField},
		{"zero repository id", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.RepositoryID = 0 }, domain.ErrNonPositive},
		{"missing superseded profile", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.SupersededProfileDigest = "" }, domain.ErrEmptyField},
		{"missing superseding profile", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.SupersedingProfileDigest = "" }, domain.ErrEmptyField},
		{"missing command", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.CommandID = nil }, domain.ErrTransitionUnbacked},
		{"empty command", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.CommandID = &emptyCommand }, domain.ErrTransitionUnbacked},
		{"missing reason", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.Reason = "" }, domain.ErrEmptyField},
		{"zero time", func(tr *domain.ReviewConfigurationRecoveryTransition) { tr.OccurredAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"non-UTC time", func(tr *domain.ReviewConfigurationRecoveryTransition) {
			tr.OccurredAt = tr.OccurredAt.In(time.FixedZone("CST", -6*60*60))
		}, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := validConfigRecoveryTransition()
			tc.mutate(&got)
			err := got.Validate()
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
	if valid.AuthorizingAction() != domain.ActionAdoptReviewConfiguration {
		t.Fatalf("AuthorizingAction() = %q, want %q",
			valid.AuthorizingAction(), domain.ActionAdoptReviewConfiguration)
	}
}

func TestReviewConfigurationRecoveryBindingMatchesExactFailure(t *testing.T) {
	failure := domain.ReviewFailure{
		RunID: "run-1", InvocationID: "review-1", Round: 2,
		BaseSHA: "base", HeadSHA: "head",
	}
	binding := validConfigRecoveryTransition().Binding()
	if !binding.Matches(failure, "sha256:failure") {
		t.Fatal("matching binding did not match")
	}
	for name, mutate := range map[string]func(*domain.ReviewConfigurationRecoveryBinding){
		"run":        func(b *domain.ReviewConfigurationRecoveryBinding) { b.RunID = "run-2" },
		"invocation": func(b *domain.ReviewConfigurationRecoveryBinding) { b.InvocationID = "review-2" },
		"round":      func(b *domain.ReviewConfigurationRecoveryBinding) { b.Round++ },
		"base":       func(b *domain.ReviewConfigurationRecoveryBinding) { b.BaseSHA = "other-base" },
		"head":       func(b *domain.ReviewConfigurationRecoveryBinding) { b.HeadSHA = "other-head" },
		"digest":     func(b *domain.ReviewConfigurationRecoveryBinding) { b.FailureDigest = "sha256:other" },
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

func TestAttentionItemReviewConfigurationRecoveryBindingRules(t *testing.T) {
	in := validItemInput(domain.AttentionReviewConfiguration)
	item, err := domain.NewAttentionItem(in, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	in.ReviewConfigurationRecovery.Round = 9
	if item.ReviewConfigurationRecovery.Round == 9 {
		t.Fatal("constructed item aliases caller configuration recovery binding")
	}

	missing := validItemInput(domain.AttentionReviewConfiguration)
	missing.ReviewConfigurationRecovery = nil
	if _, err := domain.NewAttentionItem(missing, nil); !errors.Is(err, domain.ErrReviewConfigRecoveryBindingMissing) {
		t.Fatalf("missing binding = %v, want %v", err, domain.ErrReviewConfigRecoveryBindingMissing)
	}

	outside := validItemInput(domain.AttentionSpecApproval)
	outside.ReviewConfigurationRecovery = validItemInput(domain.AttentionReviewConfiguration).ReviewConfigurationRecovery
	if _, err := domain.NewAttentionItem(outside, nil); !errors.Is(err, domain.ErrReviewConfigRecoveryBindingOutsideItem) {
		t.Fatalf("binding outside configuration item = %v, want %v",
			err, domain.ErrReviewConfigRecoveryBindingOutsideItem)
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
			in := validItemInput(domain.AttentionReviewConfiguration)
			mutate(&in)
			if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, domain.ErrReviewConfigRecoveryBindingMismatch) {
				t.Fatalf("NewAttentionItem = %v, want %v", err, domain.ErrReviewConfigRecoveryBindingMismatch)
			}
		})
	}
}

func testTrustProfileWithReviewConfig(t *testing.T, configDigest domain.Digest, mutate func(*domain.AutomationTrustProfileInput)) domain.AutomationTrustProfile {
	t.Helper()
	in := domain.AutomationTrustProfileInput{
		Repo:                       "acme/widgets",
		RepositoryID:               7,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: configDigest,
		},
	}
	if mutate != nil {
		mutate(&in)
	}
	profile, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatal(err)
	}
	return profile
}

func TestReviewConfigurationOnlySupersession(t *testing.T) {
	superseded := testTrustProfileWithReviewConfig(t, "sha256:config-old", nil)
	reviewOnly := testTrustProfileWithReviewConfig(t, "sha256:config-new", nil)
	identical := testTrustProfileWithReviewConfig(t, "sha256:config-old", nil)
	widened := testTrustProfileWithReviewConfig(t, "sha256:config-new",
		func(in *domain.AutomationTrustProfileInput) {
			in.PRGitHubTokenPermissions = domain.TokenPermissionsReadWrite
		})

	if ok, err := domain.ReviewConfigurationOnlySupersession(superseded, reviewOnly); err != nil || !ok {
		t.Fatalf("review-config-only supersession = %v, %v; want true", ok, err)
	}
	// A restored configuration re-adopts the pinned revision itself.
	if ok, err := domain.ReviewConfigurationOnlySupersession(superseded, identical); err != nil || !ok {
		t.Fatalf("identical-profile supersession = %v, %v; want true", ok, err)
	}
	if ok, err := domain.ReviewConfigurationOnlySupersession(superseded, widened); err != nil || ok {
		t.Fatalf("trust-widening supersession = %v, %v; want false", ok, err)
	}

	// A tampered body cannot pass under its stale digest: the comparison
	// re-validates both content addresses before deciding by overlay.
	tampered := reviewOnly
	tampered.PRGitHubTokenPermissions = domain.TokenPermissionsReadWrite
	if _, err := domain.ReviewConfigurationOnlySupersession(superseded, tampered); err == nil {
		t.Fatal("tampered superseding profile accepted")
	}
	tamperedOld := superseded
	tamperedOld.AllowSelfHostedCI = true
	if _, err := domain.ReviewConfigurationOnlySupersession(tamperedOld, reviewOnly); err == nil {
		t.Fatal("tampered superseded profile accepted")
	}
}
