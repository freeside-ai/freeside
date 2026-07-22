package domain_test

import (
	"encoding/json"
	"errors"
	"reflect"
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
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraAutomationControlPatterns: []string{"deploy/**"},
			ExtraPromptsAndPolicyPatterns:  []string{"prompts/**"},
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

	// A commit-plan policy flip under the bound digest is the same drift
	// class: the §5.6 gating keys participate in the address, so flipping the
	// mode requires owner re-approval of a new digest.
	planFlipped := base
	planFlipped.CommitPlan = domain.CommitPlanPlanPreferred
	if err := planFlipped.Validate(); !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("commit-plan flip error = %v, want ErrProfileDigestMismatch", err)
	}

	// Pattern order and duplication do not change the address: the
	// constructor canonicalizes, so equal content converges on one digest.
	in := validTrustProfileInput()
	in.ProtectedPaths.ExtraAutomationControlPatterns = []string{"deploy/**", "ci/*.sh"}
	in.ProtectedPaths.ExtraEgressAndTrustPatterns = []string{"egress.yaml", "trust/**"}
	sorted, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile sorted: %v", err)
	}
	in.ProtectedPaths.ExtraAutomationControlPatterns = []string{"ci/*.sh", "deploy/**", "ci/*.sh"}
	in.ProtectedPaths.ExtraEgressAndTrustPatterns = []string{"trust/**", "egress.yaml", "trust/**"}
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
		{"invalid commit plan", func(in *domain.AutomationTrustProfileInput) { in.CommitPlan = "plan_required" }, domain.ErrInvalidCommitPlanMode},
		{"empty commit plan", func(in *domain.AutomationTrustProfileInput) { in.CommitPlan = "" }, domain.ErrInvalidCommitPlanMode},
		{"unregistered message ruleset", func(in *domain.AutomationTrustProfileInput) { in.MessageRuleset = "github/2" }, domain.ErrUnknownMessageRuleset},
		{"empty message ruleset", func(in *domain.AutomationTrustProfileInput) { in.MessageRuleset = "" }, domain.ErrUnknownMessageRuleset},
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
		{"empty prompts pattern", func(in *domain.AutomationTrustProfileInput) {
			in.ProtectedPaths.ExtraPromptsAndPolicyPatterns = []string{""}
		}, domain.ErrEmptyField},
		{"absolute egress pattern", func(in *domain.AutomationTrustProfileInput) {
			in.ProtectedPaths.ExtraEgressAndTrustPatterns = []string{"/egress.yaml"}
		}, domain.ErrPatternsNotCanonical},
		{"parent segment materiality pattern", func(in *domain.AutomationTrustProfileInput) {
			in.ProtectedPaths.ExtraMaterialityRulesPatterns = []string{"docs/../plan.md"}
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

// TestTrustProfileRoundTrip: a serialized widened profile decodes to the
// same value and passes Validate's digest recompute — the path a store read
// takes (decode re-runs Validate), covered here for every pattern list.
func TestTrustProfileRoundTrip(t *testing.T) {
	in := validTrustProfileInput()
	in.ProtectedPaths = domain.ProtectedPathConfig{
		ExtraAutomationControlPatterns:   []string{"deploy/**"},
		ExtraReviewerInstructionPatterns: []string{"REVIEWING.md"},
		ExtraGitMetadataPatterns:         []string{"vendor/**"},
		ExtraVerificationControlPatterns: []string{"Makefile"},
		ExtraPromptsAndPolicyPatterns:    []string{"prompts/**"},
		ExtraEgressAndTrustPatterns:      []string{"egress.yaml"},
		ExtraMaterialityRulesPatterns:    []string{"docs/plan.md"},
	}
	original, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded domain.AutomationTrustProfile
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("decoded profile rejected: %v", err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("round trip diverged:\n got %#v\nwant %#v", decoded, original)
	}
}

// fullyPopulatedTrustProfileInput is the digest-stability fixture: every
// field non-zero where the posture allows it. It is also what the stale-v2
// re-approval test perturbs, so both pins describe one content.
func fullyPopulatedTrustProfileInput() domain.AutomationTrustProfileInput {
	return domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/demo",
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		AllowOIDC:                  true,
		AllowEnvironmentSecrets:    false,
		AllowSecretBearingPRJobs:   false,
		AllowSelfHostedCI:          true,
		AllowPullRequestTarget:     false,
		AllowReusableWorkflows:     true,
		AllowPackagePublishing:     true,
		AllowArtifactConsumers:     true,
		CommitPlan:                 domain.CommitPlanPlanPreferred,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewAuto, ConfigDigest: "sha256:review-config",
		},
		ProtectedPaths: domain.ProtectedPathConfig{
			ExtraAutomationControlPatterns:   []string{"deploy/**"},
			ExtraReviewerInstructionPatterns: []string{"REVIEWING.md"},
			ExtraGitMetadataPatterns:         []string{"vendor/**"},
			ExtraVerificationControlPatterns: []string{"Makefile"},
			ExtraPromptsAndPolicyPatterns:    []string{"prompts/**"},
			ExtraEgressAndTrustPatterns:      []string{"egress.yaml"},
			ExtraMaterialityRulesPatterns:    []string{"docs/plan.md"},
		},
	}
}

// TestTrustProfileDigestStability pins the v4 canonical form: a fixed,
// fully-populated profile resolves to this digest on every build. A mismatch
// means the canonical encoding changed without a version bump (or a bump
// without repinning), either of which would read unchanged profiles as drift
// across a daemon upgrade (plan §5.5).
func TestTrustProfileDigestStability(t *testing.T) {
	p, err := domain.NewAutomationTrustProfile(fullyPopulatedTrustProfileInput())
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	const want = domain.Digest("sha256:5dda565a91631a7058152f2aacf85a6ab7870ee12f70f5e5da6fbeea4bdb83f5")
	if p.ProfileDigest != want {
		t.Fatalf("v4 canonical digest = %q, want %q", p.ProfileDigest, want)
	}
}

// TestTrustProfileV3DigestRequiresReapproval pins the migration behavior for
// the v4 allow-axis expansion: approval under v3 did not cover the three new
// privileges, so it cannot be silently treated as approval under v4.
func TestTrustProfileV3DigestRequiresReapproval(t *testing.T) {
	p, err := domain.NewAutomationTrustProfile(fullyPopulatedTrustProfileInput())
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	const v3Digest = domain.Digest("sha256:9c5e7c171d229057d8f75fcf844c51901c181a0f740cf0d142461b0a66cc696d")
	stale := p
	stale.ProfileDigest = v3Digest
	if err := stale.Validate(); !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("v3-approved digest error = %v, want ErrProfileDigestMismatch", err)
	}
}

// TestTrustProfileV2DigestRequiresReapproval is a migration-path proof for
// profile encoding bumps: a digest a human approved under v2 (the pinned v2
// stability digest, computed over this same content before commit_plan and
// message_ruleset existed) no longer validates, so every stored profile
// fails closed until an owner records a re-approved current profile. The
// conservative single_commit default therefore arrives only through that
// re-approval, never by silent injection into an already-approved digest.
func TestTrustProfileV2DigestRequiresReapproval(t *testing.T) {
	p, err := domain.NewAutomationTrustProfile(fullyPopulatedTrustProfileInput())
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	// The v2 pin from TestTrustProfileDigestStability before the bump.
	const v2Digest = domain.Digest("sha256:47ea9bd9d11adf9daf2f5861b87a54feb1d6a47829fef78bc32c7bef9e5d9ea3")
	stale := p
	stale.ProfileDigest = v2Digest
	if err := stale.Validate(); !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("v2-approved digest error = %v, want ErrProfileDigestMismatch", err)
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

// conformantWorkflowAudit returns an audit that matches validTrustProfileInput
// on every axis: the approved surface digest, read_only token permissions, and
// no privilege the profile does not allow.
func conformantWorkflowAudit() domain.WorkflowAudit {
	return domain.WorkflowAudit{
		Repo:                "freeside-ai/demo",
		AuditedCommitSHA:    "cafebabe",
		AuditedAt:           time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		WorkflowAuditDigest: "sha256:workflow-audit",
		EffectiveTokenPerms: domain.TokenPermissionsReadOnly,
	}
}

// TestEvaluateTrustDrift: the publication decision-point comparison (plan
// §5.5) passes a conformant observation and fails closed on each axis where
// the observed audit exceeds the approved profile, naming the drifted axis.
func TestEvaluateTrustDrift(t *testing.T) {
	profile, err := domain.NewAutomationTrustProfile(validTrustProfileInput())
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}

	// A conformant audit is not drift.
	if err := domain.EvaluateTrustDrift(profile, conformantWorkflowAudit()); err != nil {
		t.Fatalf("conformant audit reported drift: %v", err)
	}

	// A less-permissive-than-allowed observation is not drift: a profile that
	// approves read_write tolerates a read_only reality. WorkflowAuditDigest is
	// unchanged by the token-mode field, so the surface digest still matches.
	rwInput := validTrustProfileInput()
	rwInput.PRGitHubTokenPermissions = domain.TokenPermissionsReadWrite
	rwProfile, err := domain.NewAutomationTrustProfile(rwInput)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile read_write: %v", err)
	}
	if err := domain.EvaluateTrustDrift(rwProfile, conformantWorkflowAudit()); err != nil {
		t.Fatalf("read_only observation under read_write profile reported drift: %v", err)
	}

	// The file surface and repository settings folded into it (workflows,
	// branch protection, rulesets) have no attested bool, so they are guarded
	// by the WorkflowAuditDigest equality check. Every attested privilege is
	// compared explicitly, including reusable workflows, package publishing,
	// and artifact consumers.
	tests := []struct {
		name     string
		mutate   func(*domain.WorkflowAudit)
		wantAxis string
	}{
		{"workflow surface drift", func(a *domain.WorkflowAudit) { a.WorkflowAuditDigest = "sha256:workflow-file-changed" }, "workflow_audit_digest"},
		{"branch/ruleset drift", func(a *domain.WorkflowAudit) { a.WorkflowAuditDigest = "sha256:branch-protection-relaxed" }, "workflow_audit_digest"},
		{"token permission drift", func(a *domain.WorkflowAudit) { a.EffectiveTokenPerms = domain.TokenPermissionsReadWrite }, "token_permissions"},
		{"oidc drift", func(a *domain.WorkflowAudit) { a.OIDCAvailable = true }, "oidc"},
		{"environment secrets drift", func(a *domain.WorkflowAudit) { a.EnvironmentSecrets = true }, "environment_secrets"},
		{"secret-bearing PR jobs drift", func(a *domain.WorkflowAudit) { a.SecretBearingPRJobs = true }, "secret_bearing_pr_jobs"},
		{"self-hosted runner drift", func(a *domain.WorkflowAudit) { a.SelfHostedRunners = true }, "self_hosted_runners"},
		{"pull_request_target drift", func(a *domain.WorkflowAudit) { a.PullRequestTarget = true }, "pull_request_target"},
		{"reusable workflows drift", func(a *domain.WorkflowAudit) { a.ReusableWorkflows = true }, "reusable_workflows"},
		{"package publishing drift", func(a *domain.WorkflowAudit) { a.PackagePublishing = true }, "package_publishing"},
		{"artifact consumers drift", func(a *domain.WorkflowAudit) { a.ArtifactConsumers = true }, "artifact_consumers"},
		{"repo mismatch", func(a *domain.WorkflowAudit) { a.Repo = "attacker/demo" }, "repo"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			audit := conformantWorkflowAudit()
			tt.mutate(&audit)
			err := domain.EvaluateTrustDrift(profile, audit)
			if !errors.Is(err, domain.ErrTrustProfileDrift) {
				t.Fatalf("error = %v, want ErrTrustProfileDrift", err)
			}
			var de *domain.TrustDriftError
			if !errors.As(err, &de) {
				t.Fatalf("error = %v, want *TrustDriftError", err)
			}
			if de.Axis != tt.wantAxis {
				t.Fatalf("drift axis = %q, want %q", de.Axis, tt.wantAxis)
			}
		})
	}

	allowedInput := validTrustProfileInput()
	allowedInput.AllowReusableWorkflows = true
	allowedInput.AllowPackagePublishing = true
	allowedInput.AllowArtifactConsumers = true
	allowed, err := domain.NewAutomationTrustProfile(allowedInput)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile allowed privileges: %v", err)
	}
	allowedAudit := conformantWorkflowAudit()
	allowedAudit.ReusableWorkflows = true
	allowedAudit.PackagePublishing = true
	allowedAudit.ArtifactConsumers = true
	if err := domain.EvaluateTrustDrift(allowed, allowedAudit); err != nil {
		t.Fatalf("explicitly allowed privileges reported drift: %v", err)
	}
}
