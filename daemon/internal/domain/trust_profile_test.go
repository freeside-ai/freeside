package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validTrustProfileInput() domain.AutomationTrustProfileInput {
	return domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/demo",
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraAutomationControlPatterns: []string{"deploy/**"},
		},
	}
}

// TestTrustProfileDigest: the profile digest is an authenticated content
// address (plan §5.5 binds runs and publication to it), verified rather than
// trusted at every boundary that re-runs Validate, so a forged digest and
// content altered under a bound digest both fail closed.
func TestTrustProfileDigest(t *testing.T) {
	base, err := domain.NewAutomationTrustProfile(validTrustProfileInput())
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	if base.ProfileDigest == "" {
		t.Fatal("constructor left an empty profile digest")
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("constructed profile rejected: %v", err)
	}

	// A forged digest is rejected on a path that bypasses the constructor
	// (what decode hits on read and a struct literal hits on write).
	forged := base
	forged.ProfileDigest = "sha256:forged"
	if err := forged.Validate(); !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("forged digest error = %v, want ErrProfileDigestMismatch", err)
	}

	// Content altered under the bound digest is drift and fails closed: every
	// posture field participates in the address.
	drifted := base
	drifted.AllowSelfHostedCI = true
	if err := drifted.Validate(); !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("drifted content error = %v, want ErrProfileDigestMismatch", err)
	}

	// Pattern order and duplication do not change the address: the
	// constructor canonicalizes, so equal content converges on one digest.
	in := validTrustProfileInput()
	in.ProtectedPaths.ExtraAutomationControlPatterns = []string{"deploy/**", "ci/*.sh"}
	sorted, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile sorted: %v", err)
	}
	in.ProtectedPaths.ExtraAutomationControlPatterns = []string{"ci/*.sh", "deploy/**", "ci/*.sh"}
	reordered, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile reordered: %v", err)
	}
	if sorted.ProfileDigest != reordered.ProfileDigest {
		t.Fatalf("digest depends on pattern order: %q vs %q", sorted.ProfileDigest, reordered.ProfileDigest)
	}
	// Nil and empty widening are one representation: canonicalize collapses
	// an empty list to nil, so "no widening" has a single digest.
	in.ProtectedPaths = domain.ProtectedPathConfig{}
	nilConfig, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile nil config: %v", err)
	}
	in.ProtectedPaths = domain.ProtectedPathConfig{ExtraGitMetadataPatterns: []string{}}
	emptyConfig, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile empty config: %v", err)
	}
	if nilConfig.ProfileDigest != emptyConfig.ProfileDigest {
		t.Fatalf("nil and empty widening diverge: %q vs %q", nilConfig.ProfileDigest, emptyConfig.ProfileDigest)
	}
}

// TestTrustProfileValidation rejects each malformed field with its sentinel.
func TestTrustProfileValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.AutomationTrustProfileInput)
		want   error
	}{
		{"empty repo", func(in *domain.AutomationTrustProfileInput) { in.Repo = "" }, domain.ErrEmptyField},
		{"invalid pr_execution", func(in *domain.AutomationTrustProfileInput) { in.PRExecution = "trusted" }, domain.ErrInvalidPRExecutionMode},
		{"empty pr_execution", func(in *domain.AutomationTrustProfileInput) { in.PRExecution = "" }, domain.ErrInvalidPRExecutionMode},
		{"invalid automation changes", func(in *domain.AutomationTrustProfileInput) { in.CandidateAutomationChanges = "allow" }, domain.ErrInvalidAutomationChanges},
		{"invalid token permissions", func(in *domain.AutomationTrustProfileInput) { in.PRGitHubTokenPermissions = "admin" }, domain.ErrInvalidTokenPermissions},
		{"empty workflow audit digest", func(in *domain.AutomationTrustProfileInput) { in.WorkflowAuditDigest = "" }, domain.ErrEmptyField},
		{"invalid review mode", func(in *domain.AutomationTrustProfileInput) { in.Review.Mode = "manual" }, domain.ErrInvalidReviewMode},
		{"empty review config digest", func(in *domain.AutomationTrustProfileInput) { in.Review.ConfigDigest = "" }, domain.ErrEmptyField},
		{"empty pattern", func(in *domain.AutomationTrustProfileInput) {
			in.ProtectedPaths.ExtraReviewerInstructionPatterns = []string{""}
		}, domain.ErrEmptyField},
		{"absolute pattern", func(in *domain.AutomationTrustProfileInput) {
			in.ProtectedPaths.ExtraReviewerInstructionPatterns = []string{"/etc/passwd"}
		}, domain.ErrPatternsNotCanonical},
		{"doubled slash", func(in *domain.AutomationTrustProfileInput) {
			in.ProtectedPaths.ExtraAutomationControlPatterns = []string{"deploy//**"}
		}, domain.ErrPatternsNotCanonical},
		{"parent segment", func(in *domain.AutomationTrustProfileInput) {
			in.ProtectedPaths.ExtraAutomationControlPatterns = []string{"tools/../ci/**"}
		}, domain.ErrPatternsNotCanonical},
		{"dot segment", func(in *domain.AutomationTrustProfileInput) {
			in.ProtectedPaths.ExtraVerificationControlPatterns = []string{"./Makefile"}
		}, domain.ErrPatternsNotCanonical},
		{"trailing slash", func(in *domain.AutomationTrustProfileInput) {
			in.ProtectedPaths.ExtraGitMetadataPatterns = []string{"vendor/"}
		}, domain.ErrPatternsNotCanonical},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := validTrustProfileInput()
			tt.mutate(&in)
			if _, err := domain.NewAutomationTrustProfile(in); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}

	// A malformed glob is rejected with path.Match's syntax error.
	in := validTrustProfileInput()
	in.ProtectedPaths.ExtraGitMetadataPatterns = []string{"[unclosed"}
	if _, err := domain.NewAutomationTrustProfile(in); err == nil {
		t.Fatal("malformed glob accepted")
	}

	// A literal that bypasses the constructor with unsorted or duplicated
	// patterns is rejected: canonical order is what makes the stored body
	// carry exactly the content the digest addresses.
	base, err := domain.NewAutomationTrustProfile(validTrustProfileInput())
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	unsorted := base
	unsorted.ProtectedPaths.ExtraAutomationControlPatterns = []string{"deploy/**", "ci/*.sh"}
	if err := unsorted.Validate(); !errors.Is(err, domain.ErrPatternsNotCanonical) {
		t.Fatalf("unsorted patterns error = %v, want ErrPatternsNotCanonical", err)
	}
	dup := base
	dup.ProtectedPaths.ExtraAutomationControlPatterns = []string{"deploy/**", "deploy/**"}
	if err := dup.Validate(); !errors.Is(err, domain.ErrPatternsNotCanonical) {
		t.Fatalf("duplicate patterns error = %v, want ErrPatternsNotCanonical", err)
	}
	// A non-nil empty list is the nil content in a different encoding; one
	// representation per content is what write-once replay convergence
	// depends on, so the literal path rejects it (the constructor
	// canonicalizes it away).
	emptyList := base
	emptyList.ProtectedPaths.ExtraGitMetadataPatterns = []string{}
	if err := emptyList.Validate(); !errors.Is(err, domain.ErrPatternsNotCanonical) {
		t.Fatalf("empty-list patterns error = %v, want ErrPatternsNotCanonical", err)
	}
}

// TestWorkflowAuditValidation: the audit snapshot is an observation record;
// every attested fact must be present and well-formed.
func TestWorkflowAuditValidation(t *testing.T) {
	valid := domain.WorkflowAudit{
		Repo:                "freeside-ai/demo",
		AuditedCommitSHA:    "cafebabe",
		AuditedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		WorkflowAuditDigest: "sha256:workflow-audit",
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid audit rejected: %v", err)
	}
	// read_write is representable: an audit records a drifted, more
	// permissive reality rather than failing to express it.
	drifted := valid
	drifted.EffectiveTokenPerms = domain.TokenPermissionsReadWrite
	if err := drifted.Validate(); err != nil {
		t.Fatalf("drifted-permissions audit rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.WorkflowAudit)
		want   error
	}{
		{"empty repo", func(a *domain.WorkflowAudit) { a.Repo = "" }, domain.ErrEmptyField},
		{"empty commit sha", func(a *domain.WorkflowAudit) { a.AuditedCommitSHA = "" }, domain.ErrEmptyField},
		{"zero audited_at", func(a *domain.WorkflowAudit) { a.AuditedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"empty digest", func(a *domain.WorkflowAudit) { a.WorkflowAuditDigest = "" }, domain.ErrEmptyField},
		{"invalid permissions", func(a *domain.WorkflowAudit) { a.EffectiveTokenPerms = "" }, domain.ErrInvalidTokenPermissions},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := valid
			tt.mutate(&a)
			if err := a.Validate(); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}
}
