package domain_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validTrustProfileInput() domain.AutomationTrustProfileInput {
	return domain.AutomationTrustProfileInput{
		Repo:                       "freeside-ai/demo",
		RepositoryID:               123456789,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
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

	// The immutable repository identity participates in the address, so a
	// caller cannot retain approval while redirecting a profile to another
	// repository that later occupies the same owner/name.
	rebound := base
	rebound.RepositoryID++
	if err := rebound.Validate(); !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("repository-ID flip error = %v, want ErrProfileDigestMismatch", err)
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
		{"zero repository id", func(in *domain.AutomationTrustProfileInput) { in.RepositoryID = 0 }, domain.ErrNonPositive},
		{"negative repository id", func(in *domain.AutomationTrustProfileInput) { in.RepositoryID = -1 }, domain.ErrNonPositive},
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
		RepositoryID:               123456789,
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
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review-config",
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

// TestTrustProfileDigestStability pins the v6 canonical form: a fixed,
// fully-populated profile resolves to this digest on every build. A mismatch
// means the canonical encoding changed without a version bump (or a bump
// without repinning), either of which would read unchanged profiles as drift
// across a daemon upgrade (plan §5.5).
func TestTrustProfileDigestStability(t *testing.T) {
	p, err := domain.NewAutomationTrustProfile(fullyPopulatedTrustProfileInput())
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	const want = domain.Digest("sha256:b94adf427ed789a207d3e42b2e10022575defc54f3317fa739a5e7704eb2dd47")
	if p.ProfileDigest != want {
		t.Fatalf("v6 canonical digest = %q, want %q", p.ProfileDigest, want)
	}
}

// TestTrustProfileV4DigestRequiresReapproval pins the v5 repository-ID
// binding: approval under v4 named only owner/repo, so it cannot authorize a
// canonical repository ID without owner re-approval.
func TestTrustProfileV4DigestRequiresReapproval(t *testing.T) {
	p, err := domain.NewAutomationTrustProfile(fullyPopulatedTrustProfileInput())
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	const v4Digest = domain.Digest("sha256:5dda565a91631a7058152f2aacf85a6ab7870ee12f70f5e5da6fbeea4bdb83f5")
	stale := p
	stale.ProfileDigest = v4Digest
	if err := stale.Validate(); !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("v4-approved digest error = %v, want ErrProfileDigestMismatch", err)
	}
}

// TestTrustProfileV3DigestRequiresReapproval pins the earlier v4 allow-axis
// expansion: approval under v3 did not cover the three new privileges.
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

// TestTrustProfileDigestStableWithoutExternalReviewers: the allowlist joined
// the profile without an encoding bump, which is sound only while a profile
// without one encodes to the bytes it always had. The fully populated
// fixture has no allowlist, so its digest is still the v6 pin above; nil and
// empty input are the same content; and the serialized body carries no
// external_reviewers member, so a stored row is byte-identical too.
func TestTrustProfileDigestStableWithoutExternalReviewers(t *testing.T) {
	in := fullyPopulatedTrustProfileInput()
	in.ExternalReviewers = []domain.ExternalReviewer{}
	p, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	if p.ExternalReviewers != nil {
		t.Fatalf("empty allowlist = %#v, want nil", p.ExternalReviewers)
	}
	const want = domain.Digest("sha256:b94adf427ed789a207d3e42b2e10022575defc54f3317fa739a5e7704eb2dd47")
	if p.ProfileDigest != want {
		t.Fatalf("digest without an allowlist = %q, want the v6 pin %q", p.ProfileDigest, want)
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(body), "external_reviewers") {
		t.Fatalf("profile without an allowlist serializes the member: %s", body)
	}
}

func externalReviewer(accountID int64, login string) domain.ExternalReviewer {
	return domain.ExternalReviewer{
		Forge: domain.ExternalReviewForgeGitHub, AccountID: accountID, Login: login,
		Authority: domain.ExternalReviewDriveRound,
	}
}

// TestTrustProfileExternalReviewers: the allowlist is part of the content
// address, canonical by construction, and admits only an exact identity.
func TestTrustProfileExternalReviewers(t *testing.T) {
	base, err := domain.NewAutomationTrustProfile(validTrustProfileInput())
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	in := validTrustProfileInput()
	in.ExternalReviewers = []domain.ExternalReviewer{
		externalReviewer(900, "maintainer"),
		externalReviewer(41, "codex[bot]"),
		externalReviewer(900, "maintainer"),
	}
	listed, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile with allowlist: %v", err)
	}
	want := []domain.ExternalReviewer{externalReviewer(41, "codex[bot]"), externalReviewer(900, "maintainer")}
	if !reflect.DeepEqual(listed.ExternalReviewers, want) {
		t.Fatalf("allowlist = %#v, want sorted and deduplicated %#v", listed.ExternalReviewers, want)
	}
	if listed.ProfileDigest == base.ProfileDigest {
		t.Fatal("adding external reviewers left the profile digest unchanged")
	}
	// The constructor detaches the list from the caller's backing array.
	in.ExternalReviewers[1].Login = "someone-else"
	if listed.ExternalReviewers[0].Login != "codex[bot]" {
		t.Fatal("profile allowlist aliases the caller's slice")
	}

	// Entries added under a bound digest are drift, like any posture field:
	// nobody is admitted without the owner approving a new digest.
	smuggled := base
	smuggled.ExternalReviewers = want
	if err := smuggled.Validate(); !errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("allowlist under a bound digest error = %v, want ErrProfileDigestMismatch", err)
	}

	// A serialized profile with entries decodes to the same value and passes
	// the digest recompute, which is the store's read path.
	body, err := json.Marshal(listed)
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
	if !reflect.DeepEqual(decoded, listed) {
		t.Fatalf("round trip diverged:\n got %#v\nwant %#v", decoded, listed)
	}

	admitted := []struct {
		name      string
		forge     domain.ExternalReviewForge
		accountID int64
		login     string
		want      bool
	}{
		{"exact identity", domain.ExternalReviewForgeGitHub, 41, "codex[bot]", true},
		{"renamed login", domain.ExternalReviewForgeGitHub, 41, "codex-renamed[bot]", false},
		{"reused login on another account", domain.ExternalReviewForgeGitHub, 42, "codex[bot]", false},
		{"login in another case", domain.ExternalReviewForgeGitHub, 41, "Codex[bot]", false},
		{"another forge", "gitlab", 41, "codex[bot]", false},
		{"unlisted reviewer", domain.ExternalReviewForgeGitHub, 7, "stranger", false},
	}
	for _, tt := range admitted {
		t.Run(tt.name, func(t *testing.T) {
			authority, ok := listed.ExternalReviewAuthorityFor(tt.forge, tt.accountID, tt.login)
			if ok != tt.want {
				t.Fatalf("admitted = %v, want %v", ok, tt.want)
			}
			if ok && authority != domain.ExternalReviewDriveRound {
				t.Fatalf("authority = %q, want drive_round", authority)
			}
			if !ok && authority != "" {
				t.Fatalf("unadmitted reviewer got authority %q", authority)
			}
		})
	}
	if _, ok := base.ExternalReviewAuthorityFor(domain.ExternalReviewForgeGitHub, 41, "codex[bot]"); ok {
		t.Fatal("a profile without an allowlist admitted a reviewer")
	}
}

// TestTrustProfileExternalReviewersValidation rejects each malformed entry
// through the constructor and each non-canonical list on the literal path
// that decode and exported structs take.
func TestTrustProfileExternalReviewersValidation(t *testing.T) {
	entries := []struct {
		name   string
		mutate func(*domain.ExternalReviewer)
		want   error
	}{
		{"unknown forge", func(r *domain.ExternalReviewer) { r.Forge = "gitlab" }, domain.ErrInvalidExternalReviewForge},
		{"empty forge", func(r *domain.ExternalReviewer) { r.Forge = "" }, domain.ErrInvalidExternalReviewForge},
		{"unknown authority", func(r *domain.ExternalReviewer) { r.Authority = "advise" }, domain.ErrInvalidExternalReviewAuthority},
		{"empty authority", func(r *domain.ExternalReviewer) { r.Authority = "" }, domain.ErrInvalidExternalReviewAuthority},
		{"zero account id", func(r *domain.ExternalReviewer) { r.AccountID = 0 }, domain.ErrNonPositive},
		{"negative account id", func(r *domain.ExternalReviewer) { r.AccountID = -41 }, domain.ErrNonPositive},
		{"empty login", func(r *domain.ExternalReviewer) { r.Login = "" }, domain.ErrEmptyField},
		{"login with a space", func(r *domain.ExternalReviewer) { r.Login = "codex bot" }, domain.ErrExternalReviewerLoginInvalid},
		{"login with a newline", func(r *domain.ExternalReviewer) { r.Login = "codex\nbot" }, domain.ErrExternalReviewerLoginInvalid},
		{"login with a control character", func(r *domain.ExternalReviewer) { r.Login = "codex\x1b[0m" }, domain.ErrExternalReviewerLoginInvalid},
		{"login not UTF-8", func(r *domain.ExternalReviewer) { r.Login = "codex\xff" }, domain.ErrExternalReviewerLoginInvalid},
	}
	for _, tt := range entries {
		t.Run(tt.name, func(t *testing.T) {
			in := validTrustProfileInput()
			r := externalReviewer(41, "codex[bot]")
			tt.mutate(&r)
			in.ExternalReviewers = []domain.ExternalReviewer{r}
			if _, err := domain.NewAutomationTrustProfile(in); !errors.Is(err, tt.want) {
				t.Fatalf("error = %v, want %v", err, tt.want)
			}
		})
	}

	// One account with two logins is a contradiction the constructor refuses
	// to settle: it cannot know which spelling the owner approved.
	in := validTrustProfileInput()
	in.ExternalReviewers = []domain.ExternalReviewer{
		externalReviewer(41, "codex[bot]"), externalReviewer(41, "codex-renamed[bot]"),
	}
	if _, err := domain.NewAutomationTrustProfile(in); !errors.Is(err, domain.ErrExternalReviewersNotCanonical) {
		t.Fatalf("two logins for one account error = %v, want ErrExternalReviewersNotCanonical", err)
	}

	in = validTrustProfileInput()
	in.ExternalReviewers = []domain.ExternalReviewer{externalReviewer(41, "codex[bot]"), externalReviewer(900, "maintainer")}
	base, err := domain.NewAutomationTrustProfile(in)
	if err != nil {
		t.Fatalf("NewAutomationTrustProfile: %v", err)
	}
	literals := []struct {
		name string
		list []domain.ExternalReviewer
	}{
		{"unsorted", []domain.ExternalReviewer{externalReviewer(900, "maintainer"), externalReviewer(41, "codex[bot]")}},
		{"duplicate account", []domain.ExternalReviewer{externalReviewer(41, "codex[bot]"), externalReviewer(41, "codex[bot]")}},
		{"empty non-nil", []domain.ExternalReviewer{}},
	}
	for _, tt := range literals {
		t.Run(tt.name, func(t *testing.T) {
			p := base
			p.ExternalReviewers = tt.list
			if err := p.Validate(); !errors.Is(err, domain.ErrExternalReviewersNotCanonical) {
				t.Fatalf("error = %v, want ErrExternalReviewersNotCanonical", err)
			}
		})
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
