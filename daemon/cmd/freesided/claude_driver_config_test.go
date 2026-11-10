package main

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestClaudeStoreOptionsCarryTheSelectedModePolicy is the regression for a
// daemon whose own admissions the store would refuse: the persistence gate
// re-checks every recorded admission against the operator's policy, and an
// unset floor means "no policy configured", which fails closed.
func TestClaudeStoreOptionsCarryTheSelectedModePolicy(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode  domain.OperatingMode
		floor []domain.RunnerCapability
	}{
		{domain.ModeAttendedDev, []domain.RunnerCapability{
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
		}},
		{domain.ModeUnattended, []domain.RunnerCapability{
			domain.CapDetachableWorkspace,
			domain.CapPostExitExport,
			domain.CapReadOnlyRemount,
			domain.CapNetworklessExport,
			domain.CapEnforcedProviderEgress,
		}},
	}
	for _, test := range tests {
		test := test
		t.Run(string(test.mode), func(t *testing.T) {
			t.Parallel()
			cfg := config{
				DBPath: "/var/freeside/freeside.db",
				Claude: &ClaudeDriverConfig{OperatingMode: test.mode},
			}
			opts, err := cfg.storeOptions()
			if err != nil {
				t.Fatalf("storeOptions: %v", err)
			}
			floor, ok := opts.AdmissionFloors[test.mode]
			if !ok {
				t.Fatalf("claude mode configured no %s admission floor", test.mode)
			}
			if len(floor) != len(test.floor) {
				t.Fatalf("configured floor = %v, want %v", floor, test.floor)
			}
			for _, capability := range test.floor {
				if !floor.Has(capability) {
					t.Errorf("configured floor lacks %q", capability)
				}
			}
			if !slices.Equal(AdmissionFloor(test.mode), test.floor) {
				t.Errorf("engine floor = %v, want %v", AdmissionFloor(test.mode), test.floor)
			}
			if !slices.Contains(opts.ApprovedCredentialModes, domain.CredentialSubscriptionContained) {
				t.Errorf("approved credential modes = %v, want subscription_contained",
					opts.ApprovedCredentialModes)
			}
		})
	}
	if slices.Contains(AttendedAdmissionFloor, exec.CapNetworklessExport) ||
		slices.Contains(AttendedAdmissionFloor, exec.CapEnforcedProviderEgress) {
		t.Fatal("attended_dev floor includes unattended-only conformance capabilities")
	}

	// Fake mode keeps the walking skeleton's exact store policy.
	fakeOpts, err := (config{DBPath: "/var/freeside/freeside.db"}).storeOptions()
	if err != nil {
		t.Fatalf("fake storeOptions: %v", err)
	}
	if fakeOpts.AdmissionFloors != nil || fakeOpts.ApprovedCredentialModes != nil {
		t.Errorf("fake mode store options changed: %#v", fakeOpts)
	}
}

func TestClaudeDriverConfigRequiresExplicitBindings(t *testing.T) {
	t.Parallel()
	valid := ClaudeDriverConfig{ //nolint:gosec // G101: fixture paths and digests, no credential material
		AgentImage:                     domain.ImageRef("ghcr.io/x/agent@sha256:" + strings.Repeat("a", 64)),
		ExporterImage:                  "ghcr.io/x/exporter@sha256:" + strings.Repeat("b", 64),
		SeedRoot:                       "/var/freeside/seeds",
		StateDir:                       "/var/freeside/state",
		ProviderEndpoints:              []string{"api.anthropic.com:443"},
		PromptPackageFile:              "/Users/operator/prompt-package.md",
		SpecificationPromptPackageFile: "/Users/operator/specifier.md",
		RemediationPromptPackageFile:   "/Users/operator/remediator.md",
		VendorInstructions:             "/Users/operator/CLAUDE.md",
		Repo:                           "freeside-ai/candidate", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("d", 40),
		AgentTreeCheckout: "/var/freeside/policy-checkout", AgentTreeCommit: strings.Repeat("e", 40),
		AllowedPaths: []string{"daemon/**", "docs/**"},
		StateRoot:    "/var/freeside/app", CredentialsDir: "/var/freeside/creds",
		ReviewImage:     "ghcr.io/x/codex@sha256:" + strings.Repeat("c", 64),
		ReviewInputRoot: "/var/freeside/review-inputs", ReviewAuthMode: ward.CodexAuthSubscription,
		ReviewAuthSnapshot:    "/var/freeside/review-inputs/auth.json",
		ReviewInstructions:    "/var/freeside/review-inputs/AGENTS.md",
		ReviewWorkspaceSizeMB: 8192,
		OperatingMode:         domain.ModeUnattended,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// Every binding that lands in a durable admission record must be
	// operator-supplied: a default would record an unaudited value.
	tests := map[string]func(*ClaudeDriverConfig){
		"agent image":                  func(c *ClaudeDriverConfig) { c.AgentImage = "" },
		"exporter image":               func(c *ClaudeDriverConfig) { c.ExporterImage = "" },
		"seed root":                    func(c *ClaudeDriverConfig) { c.SeedRoot = "" },
		"state dir":                    func(c *ClaudeDriverConfig) { c.StateDir = "" },
		"endpoints":                    func(c *ClaudeDriverConfig) { c.ProviderEndpoints = nil },
		"prompt package":               func(c *ClaudeDriverConfig) { c.PromptPackageFile = "" },
		"specification prompt package": func(c *ClaudeDriverConfig) { c.SpecificationPromptPackageFile = "" },
		"remediation prompt package":   func(c *ClaudeDriverConfig) { c.RemediationPromptPackageFile = "" },
		"instructions":                 func(c *ClaudeDriverConfig) { c.VendorInstructions = "" },
		"repository id":                func(c *ClaudeDriverConfig) { c.RepositoryID = 0 },
		"base sha":                     func(c *ClaudeDriverConfig) { c.BaseSHA = "" },
		"repository shape": func(c *ClaudeDriverConfig) {
			c.Repo = ".unsafe/repo"
		},
		"base ref shape": func(c *ClaudeDriverConfig) {
			c.BaseRef = "refs/heads/main"
		},
		"base sha shape": func(c *ClaudeDriverConfig) {
			c.BaseSHA = strings.Repeat("D", 40)
		},
		// The lineup names every identity, so the tree it is read from is a
		// required binding.
		"agent tree":        func(c *ClaudeDriverConfig) { c.AgentTreeCheckout = "" },
		"agent tree commit": func(c *ClaudeDriverConfig) { c.AgentTreeCommit = "" },
		"agent tree shape":  func(c *ClaudeDriverConfig) { c.AgentTreeCheckout = "relative/checkout" },
		"review image":      func(c *ClaudeDriverConfig) { c.ReviewImage = "" },
		"review image pin": func(c *ClaudeDriverConfig) {
			c.ReviewImage = "ghcr.io/x/codex:latest"
		},
		"review input root":       func(c *ClaudeDriverConfig) { c.ReviewInputRoot = "" },
		"review input root shape": func(c *ClaudeDriverConfig) { c.ReviewInputRoot = "relative" },
		"review auth mode":        func(c *ClaudeDriverConfig) { c.ReviewAuthMode = "" },
		"review auth snapshot":    func(c *ClaudeDriverConfig) { c.ReviewAuthSnapshot = "" },
		"review instructions":     func(c *ClaudeDriverConfig) { c.ReviewInstructions = "" },
		"review workspace size":   func(c *ClaudeDriverConfig) { c.ReviewWorkspaceSizeMB = 0 },
		"credentials dir":         func(c *ClaudeDriverConfig) { c.CredentialsDir = "" },
		"allowed paths":           func(c *ClaudeDriverConfig) { c.AllowedPaths = nil },
		// Match-everything equivalents do not name the paths unattended work
		// may rewrite, even when their spelling is not the literal "**".
		"match-everything allowlist": func(c *ClaudeDriverConfig) { c.AllowedPaths = []string{"**/*"} },
		"malformed allowlist":        func(c *ClaudeDriverConfig) { c.AllowedPaths = []string{"daemon/[abc"} },
		// The rebuild's egress flags mean nothing without the base to build
		// from, and a malformed one would refuse every rebuild at run time.
		"build proxy without base build ref": func(c *ClaudeDriverConfig) {
			c.Rebuild.BuildProxy = "http://192.168.64.1:3128"
		},
		"build dns without base build ref": func(c *ClaudeDriverConfig) { c.Rebuild.DNS = []string{"192.168.64.1"} },
		"base build ref shape": func(c *ClaudeDriverConfig) {
			c.Rebuild.BaseBuildRef = "ghcr.io/x/agent@sha256:" + strings.Repeat("a", 64)
		},
		"build proxy scheme": func(c *ClaudeDriverConfig) {
			c.Rebuild = projectimage.BuildInputs{
				BaseBuildRef: "local/agent:test", BuildProxy: "https://192.168.64.1:3128",
			}
		},
		"build dns shape": func(c *ClaudeDriverConfig) {
			c.Rebuild = projectimage.BuildInputs{BaseBuildRef: "local/agent:test", DNS: []string{"--flag"}}
		},
	}
	rebuilding := valid
	rebuilding.Rebuild = projectimage.BuildInputs{
		BaseBuildRef: "local/agent:test", BuildProxy: "http://192.168.64.1:3128", DNS: []string{"192.168.64.1"},
	}
	if err := rebuilding.Validate(); err != nil {
		t.Fatalf("valid rebuild inputs rejected: %v", err)
	}
	for name, edit := range tests {
		t.Run(name, func(t *testing.T) {
			cfg := valid
			edit(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("config missing %s was accepted", name)
			}
		})
	}
}

func TestClaudeDriverConfigAttendedModeDoesNotRequireProductionReview(t *testing.T) {
	t.Parallel()
	cfg := ClaudeDriverConfig{ //nolint:gosec // fixture paths and digests, no credential material
		AgentImage:                     domain.ImageRef("ghcr.io/x/agent@sha256:" + strings.Repeat("a", 64)),
		ExporterImage:                  "ghcr.io/x/exporter@sha256:" + strings.Repeat("b", 64),
		SeedRoot:                       "/var/freeside/seeds",
		StateDir:                       "/var/freeside/state",
		ProviderEndpoints:              []string{"api.anthropic.com:443"},
		PromptPackageFile:              "/Users/operator/prompt-package.md",
		SpecificationPromptPackageFile: "/Users/operator/specifier.md",
		RemediationPromptPackageFile:   "/Users/operator/remediator.md",
		VendorInstructions:             "/Users/operator/CLAUDE.md",
		Repo:                           "freeside-ai/candidate",
		RepositoryID:                   42,
		BaseRef:                        "main",
		BaseSHA:                        strings.Repeat("d", 40),
		AgentTreeCheckout:              "/var/freeside/policy-checkout",
		AgentTreeCommit:                strings.Repeat("e", 40),
		AllowedPaths:                   []string{"daemon/**", "docs/**"},
		StateRoot:                      "/var/freeside/app",
		CredentialsDir:                 "/var/freeside/creds",
		OperatingMode:                  domain.ModeAttendedDev,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("attended config without production review rejected: %v", err)
	}
}

func TestClaudeShadowReviewConfigurationIsExplicitAndDigestBound(t *testing.T) {
	t.Parallel()
	cfg := ClaudeDriverConfig{ //nolint:gosec // fixture paths and digests, no credential material
		AgentImage:    domain.ImageRef("ghcr.io/x/agent@sha256:" + strings.Repeat("a", 64)),
		ExporterImage: "ghcr.io/x/exporter@sha256:" + strings.Repeat("b", 64),
		SeedRoot:      "/var/freeside/seeds", StateDir: "/var/freeside/state",
		ProviderEndpoints:              []string{"api.anthropic.com:443"},
		PromptPackageFile:              "/Users/operator/prompt.md",
		SpecificationPromptPackageFile: "/Users/operator/specification.md",
		RemediationPromptPackageFile:   "/Users/operator/remediation.md",
		VendorInstructions:             "/Users/operator/CLAUDE.md",
		Repo:                           "freeside-ai/candidate", RepositoryID: 42,
		BaseRef: "main", BaseSHA: strings.Repeat("d", 40),
		ShadowReviewAuthIdentityID: "auth-claude-owner", AllowedPaths: []string{"daemon/**"},
		AgentTreeCheckout: "/var/freeside/policy-checkout", AgentTreeCommit: strings.Repeat("e", 40),
		StateRoot: "/var/freeside/app", CredentialsDir: "/var/freeside/creds",
		OperatingMode:   domain.ModeUnattended,
		ReviewImage:     "ghcr.io/x/codex@sha256:" + strings.Repeat("c", 64),
		ReviewInputRoot: "/var/freeside/review-inputs", ReviewAuthMode: ward.CodexAuthSubscription,
		ReviewAuthIdentityID: "auth-codex-owner", ReviewAuthSnapshot: "/var/freeside/review-inputs/auth.json",
		ReviewInstructions: "/var/freeside/review-inputs/AGENTS.md",
		ReviewModel:        "gpt-codex", ReviewReasoningEffort: "high",
		ReviewCostOwner: "subscription:owner", ReviewWorkspaceSizeMB: 8192,
		ShadowReviewImage:        "ghcr.io/x/claude@sha256:" + strings.Repeat("e", 64),
		ShadowReviewAuthSnapshot: "/var/freeside/review-inputs/claude-token",
		ShadowReviewModel:        "claude-opus", ShadowReviewReasoningEffort: "high",
		ShadowReviewCostOwner: "subscription:shadow", ShadowReviewWorkspaceSizeMB: 4096,
		ShadowReviewRate: 0.2,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid shadow config rejected: %v", err)
	}
	digests, err := ShadowReviewConfigurationDigests(cfg)
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "shadow-review-composition-digest", []byte(digests.Approval+"\n"))
	for name, mutate := range map[string]func(*ClaudeDriverConfig){
		"image": func(c *ClaudeDriverConfig) {
			c.ShadowReviewImage = "ghcr.io/x/claude@sha256:" + strings.Repeat("f", 64)
		},
		"exporter": func(c *ClaudeDriverConfig) {
			c.ExporterImage = "ghcr.io/x/exporter@sha256:" + strings.Repeat("7", 64)
		},
		"model":      func(c *ClaudeDriverConfig) { c.ShadowReviewModel = "claude-sonnet" },
		"reasoning":  func(c *ClaudeDriverConfig) { c.ShadowReviewReasoningEffort = "medium" },
		"identity":   func(c *ClaudeDriverConfig) { c.ShadowReviewAuthIdentityID = "auth-other-owner" },
		"cost owner": func(c *ClaudeDriverConfig) { c.ShadowReviewCostOwner = "subscription:other" },
		"workspace":  func(c *ClaudeDriverConfig) { c.ShadowReviewWorkspaceSizeMB++ },
		"rate":       func(c *ClaudeDriverConfig) { c.ShadowReviewRate = 0.3 },
	} {
		t.Run("digest binds "+name, func(t *testing.T) {
			changed := cfg
			mutate(&changed)
			changedDigests, err := ShadowReviewConfigurationDigests(changed)
			if err != nil {
				t.Fatal(err)
			}
			if changedDigests.Approval == digests.Approval {
				t.Fatalf("%s change did not change approval digest", name)
			}
			if name == "rate" && changedDigests.Runtime != digests.Runtime {
				t.Fatal("selection rate changed ward invocation-runtime digest")
			}
		})
	}
	for name, mutate := range map[string]func(*ClaudeDriverConfig){
		"image":     func(c *ClaudeDriverConfig) { c.ShadowReviewImage = "" },
		"snapshot":  func(c *ClaudeDriverConfig) { c.ShadowReviewAuthSnapshot = "" },
		"workspace": func(c *ClaudeDriverConfig) { c.ShadowReviewWorkspaceSizeMB = 0 },
		"rate":      func(c *ClaudeDriverConfig) { c.ShadowReviewRate = 1.1 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := cfg
			mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatalf("shadow configuration missing %s was accepted", name)
			}
		})
	}
}

func TestClaudeShadowReviewConfigurationUsesSeparateExactApproval(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	cfg := ClaudeDriverConfig{
		OperatingMode: domain.ModeUnattended, Repo: "example/repo", RepositoryID: 44,
		ExporterImage:   "ghcr.io/x/exporter@sha256:" + strings.Repeat("b", 64),
		ReviewInputRoot: "/var/freeside/review-inputs", ShadowReviewAuthIdentityID: "auth-claude-owner",
		ShadowReviewImage: "ghcr.io/x/claude@sha256:" + strings.Repeat("e", 64),
		ShadowReviewModel: "claude-opus", ShadowReviewReasoningEffort: "high",
		ShadowReviewCostOwner: "subscription:shadow", ShadowReviewWorkspaceSizeMB: 4096,
		ShadowReviewRate: 0.2,
	}
	digests, err := ShadowReviewConfigurationDigests(cfg)
	if err != nil {
		t.Fatal(err)
	}
	routedDigest := domain.Digest("sha256:" + strings.Repeat("9", 64))
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: cfg.Repo, RepositoryID: cfg.RepositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit, MessageRuleset: domain.MessageRulesetGitHub1,
		WorkflowAuditDigest: domain.Digest("sha256:" + strings.Repeat("8", 64)),
		Review:              domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: routedDigest},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 19, 0, 0, 0, time.UTC)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, profile, now)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := requireApprovedShadowReviewConfiguration(ctx, st, cfg); !errors.Is(
		err, domain.ErrShadowReviewConfigUnapproved,
	) {
		t.Fatalf("absent shadow approval error = %v", err)
	}
	disabled := cfg
	disabled.ShadowReviewImage = ""
	if got, err := requireApprovedShadowReviewConfiguration(ctx, st, disabled); err != nil ||
		got != (shadowReviewDigests{}) {
		t.Fatalf("disabled shadow approval = %#v, %v", got, err)
	}
	approval, err := domain.NewShadowReviewConfigurationApproval(
		domain.ShadowReviewConfigurationApprovalInput{
			Repo: cfg.Repo, RepositoryID: cfg.RepositoryID,
			Source: domain.ShadowReviewClaudeLocal, ConfigurationDigest: digests.Approval,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveShadowReviewConfigurationApproval(ctx, approval, now); err != nil {
			return err
		}
		return tx.ActivateShadowReviewConfigurationApproval(
			ctx, approval.Repo, approval.Source, approval.ApprovalDigest, now,
		)
	}); err != nil {
		t.Fatal(err)
	}
	approved, err := requireApprovedShadowReviewConfiguration(ctx, st, cfg)
	if err != nil {
		t.Fatalf("separately approved shadow configuration rejected: %v", err)
	}
	if approved != digests {
		t.Fatalf("approved digests = %#v, want %#v", approved, digests)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireReviewConfigurationApproved(ctx, digests.Approval)
	}); !errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
		t.Fatalf("shadow approval passed routed gate: %v", err)
	}
	changed := cfg
	changed.ShadowReviewRate = 0.3
	if _, err := requireApprovedShadowReviewConfiguration(ctx, st, changed); !errors.Is(
		err, domain.ErrShadowReviewConfigUnapproved,
	) {
		t.Fatalf("stale rate approval error = %v", err)
	}
	wrongIdentity := cfg
	wrongIdentity.RepositoryID++
	if _, err := requireApprovedShadowReviewConfiguration(ctx, st, wrongIdentity); !errors.Is(
		err, domain.ErrRepositoryIdentityMismatch,
	) {
		t.Fatalf("wrong repository identity error = %v", err)
	}
	wrongRepository := cfg
	wrongRepository.Repo = "example/other"
	if _, err := requireApprovedShadowReviewConfiguration(ctx, st, wrongRepository); !errors.Is(
		err, domain.ErrShadowReviewConfigUnapproved,
	) {
		t.Fatalf("wrong repository error = %v", err)
	}
}

func TestReviewConfigurationApprovalBindsExporterBeforeComposition(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	cfg := ClaudeDriverConfig{
		OperatingMode:   domain.ModeUnattended,
		Repo:            "example/repo",
		RepositoryID:    44,
		ExporterImage:   "ghcr.io/x/exporter@sha256:" + strings.Repeat("b", 64),
		ReviewImage:     "ghcr.io/x/codex@sha256:" + strings.Repeat("c", 64),
		ReviewInputRoot: "/var/freeside/review-inputs", ReviewAuthMode: ward.CodexAuthSubscription,
		ReviewProvider:       mustRoutedReviewProvider(domain.HarnessClientCodexCLI),
		ReviewAuthIdentityID: "auth-codex-owner", ReviewModel: "gpt-codex",
		ReviewReasoningEffort: "high", ReviewCostOwner: "subscription:owner",
		ReviewWorkspaceSizeMB: 8192,
	}
	if _, err := requireApprovedReviewConfiguration(ctx, st, cfg); !errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
		t.Fatalf("unapproved configuration error = %v, want %v", err, domain.ErrReviewConfigurationUnapproved)
	}
	digest, err := ClaudeReviewConfigurationDigest(cfg)
	if err != nil {
		t.Fatalf("derive review configuration: %v", err)
	}
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "example/repo", RepositoryID: 44,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: digest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 17, 19, 0, 0, 0, time.UTC)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, profile, now); err != nil {
			return err
		}
		return tx.ActivateTrustProfile(ctx, profile.Repo, profile.ProfileDigest, now)
	}); err != nil {
		t.Fatal(err)
	}
	approvedDigest, err := requireApprovedReviewConfiguration(ctx, st, cfg)
	if err != nil {
		t.Fatalf("approved exporter rejected: %v", err)
	}
	if approvedDigest != digest {
		t.Fatalf("approved digest = %q, want %q", approvedDigest, digest)
	}
	cfg.ExporterImage = "ghcr.io/x/unapproved@sha256:" + strings.Repeat("d", 64)
	if _, err := requireApprovedReviewConfiguration(ctx, st, cfg); !errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
		t.Fatalf("unapproved exporter error = %v, want %v", err, domain.ErrReviewConfigurationUnapproved)
	}
}

func TestClaudeDriverConfigRejectsUnscopedAllowlistGlobs(t *testing.T) {
	t.Parallel()
	for _, pattern := range []string{
		"**", "**/*", "**/**", "*/**", "?*", "[a-z]*", "/daemon/**", "../daemon/**",
	} {
		t.Run(pattern, func(t *testing.T) {
			if engine.ExplicitAllowedPaths([]string{pattern}) {
				t.Fatalf("engine.ExplicitAllowedPaths(%q) = true", pattern)
			}
		})
	}
	for _, pattern := range []string{"daemon/**", "docs/*.md", "README.md"} {
		t.Run("accept "+pattern, func(t *testing.T) {
			if !engine.ExplicitAllowedPaths([]string{pattern}) {
				t.Fatalf("engine.ExplicitAllowedPaths(%q) = false", pattern)
			}
		})
	}
}
