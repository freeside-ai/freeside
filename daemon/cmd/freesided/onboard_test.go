package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestParseOnboardConfigRefusesLoopbackRegistry(t *testing.T) {
	t.Parallel()
	baseArgs := []string{
		"example/repo",
		"-db", "/tmp/freeside.db",
		"-state-dir", "/tmp/freeside-state",
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", "/tmp/verify.json",
	}
	for _, registry := range []string{"127.0.0.1:5523", "localhost:5000/team", "[::1]:5000"} {
		_, err := parseOnboardConfig(append(slices.Clone(baseArgs), "-registry", registry), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "-local-registry-port") {
			t.Fatalf("parseOnboardConfig(-registry %s) = %v, want a refusal naming -local-registry-port",
				registry, err)
		}
	}
	cfg, err := parseOnboardConfig(append(slices.Clone(baseArgs), "-registry", "registry.example.test/team"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := "registry.example.test/team"; cfg.Registry != want {
		t.Fatalf("Registry = %q, want %q", cfg.Registry, want)
	}
}

func TestParseOnboardConfigBuildEgress(t *testing.T) {
	t.Parallel()
	baseArgs := []string{
		"example/repo",
		"-db", "/tmp/freeside.db",
		"-state-dir", "/tmp/freeside-state",
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", "/tmp/verify.json",
	}
	t.Run("provided values preserve order", func(t *testing.T) {
		cfg, err := parseOnboardConfig(append(baseArgs,
			"-dns", "1.1.1.1",
			"-build-proxy", "http://192.168.64.1:53536",
			"-dns", "8.8.8.8",
		), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"1.1.1.1", "8.8.8.8"}; !slices.Equal(cfg.DNS, want) {
			t.Fatalf("DNS = %v, want %v", cfg.DNS, want)
		}
		if want := "http://192.168.64.1:53536"; cfg.BuildProxy != want {
			t.Fatalf("BuildProxy = %q, want %q", cfg.BuildProxy, want)
		}
	})
	t.Run("absent values stay zero", func(t *testing.T) {
		cfg, err := parseOnboardConfig(baseArgs, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DNS != nil {
			t.Fatalf("DNS = %v, want nil", cfg.DNS)
		}
		if cfg.BuildProxy != "" {
			t.Fatalf("BuildProxy = %q, want empty", cfg.BuildProxy)
		}
	})
	t.Run("both toolchain directories together", func(t *testing.T) {
		cfg, err := parseOnboardConfig(append(slices.Clone(baseArgs),
			"-go-module-dir", "daemon", "-swift-package-dir", "app"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.GoModuleDir != "daemon" || cfg.SwiftPackageDir != "app" {
			t.Fatalf("GoModuleDir, SwiftPackageDir = %q, %q, want daemon, app", cfg.GoModuleDir, cfg.SwiftPackageDir)
		}
	})
	t.Run("recovery is an explicit fresh request", func(t *testing.T) {
		cfg, err := parseOnboardConfig(append(slices.Clone(baseArgs), "-recover-installation", "33"), io.Discard)
		if err != nil || cfg.RecoverInstallationID != 33 || cfg.InstallationID != 0 {
			t.Fatalf("recovery config = %+v, %v", cfg, err)
		}
		for _, extra := range [][]string{
			{"-resume"}, {"-replace-intent"}, {"-approve", "prior-digest"}, {"-installation-id", "33"},
		} {
			args := append(slices.Clone(baseArgs), "-recover-installation", "33")
			if _, err := parseOnboardConfig(append(args, extra...), io.Discard); err == nil {
				t.Fatalf("accepted incompatible recovery flags: %v", extra)
			}
		}
	})
	t.Run("intent replacement is a fresh request", func(t *testing.T) {
		cfg, err := parseOnboardConfig(append(slices.Clone(baseArgs), "-replace-intent"), io.Discard)
		if err != nil || !cfg.ReplaceIntent {
			t.Fatalf("replacement config = %+v, %v", cfg, err)
		}
		for _, extra := range [][]string{{"-resume"}, {"-approve", "prior-digest"}} {
			args := append(slices.Clone(baseArgs), "-replace-intent")
			if _, err := parseOnboardConfig(append(args, extra...), io.Discard); err == nil {
				t.Fatalf("accepted incompatible replacement flags: %v", extra)
			}
		}
	})
}

func TestParseOnboardConfigInstallWait(t *testing.T) {
	t.Parallel()
	baseArgs := []string{
		"example/repo",
		"-db", "/tmp/freeside.db",
		"-state-dir", "/tmp/freeside-state",
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", "/tmp/verify.json",
	}
	t.Run("absent flag lasts one hour", func(t *testing.T) {
		cfg, err := parseOnboardConfig(baseArgs, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.InstallWait != time.Hour {
			t.Fatalf("InstallWait = %v, want %v", cfg.InstallWait, time.Hour)
		}
	})
	t.Run("explicit flag overrides the default", func(t *testing.T) {
		cfg, err := parseOnboardConfig(append(slices.Clone(baseArgs), "-install-wait", "15m"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if want := 15 * time.Minute; cfg.InstallWait != want {
			t.Fatalf("InstallWait = %v, want %v", cfg.InstallWait, want)
		}
	})
}

func TestParseOnboardConfigExternalReviewers(t *testing.T) {
	t.Parallel()
	baseArgs := []string{
		"example/repo",
		"-db", "/tmp/freeside.db",
		"-state-dir", "/tmp/freeside-state",
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", "/tmp/verify.json",
	}
	t.Run("absent flag admits nobody", func(t *testing.T) {
		cfg, err := parseOnboardConfig(baseArgs, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ExternalReviewers != nil {
			t.Fatalf("ExternalReviewers = %v, want nil", cfg.ExternalReviewers)
		}
	})
	t.Run("repeated flag collects entries", func(t *testing.T) {
		cfg, err := parseOnboardConfig(append(slices.Clone(baseArgs),
			"-external-reviewer", "github:900:maintainer=drive_round",
			"-external-reviewer", "github:41:codex[bot]=drive_round",
		), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		want := []domain.ExternalReviewer{
			{Forge: domain.ExternalReviewForgeGitHub, AccountID: 900, Login: "maintainer", Authority: domain.ExternalReviewDriveRound},
			{Forge: domain.ExternalReviewForgeGitHub, AccountID: 41, Login: "codex[bot]", Authority: domain.ExternalReviewDriveRound},
		}
		if !slices.Equal(cfg.ExternalReviewers, want) {
			t.Fatalf("ExternalReviewers = %v, want %v", cfg.ExternalReviewers, want)
		}
	})
	for _, tt := range []struct {
		name  string
		value string
	}{
		{"no authority", "github:41:codex[bot]"},
		{"no login", "github:41=drive_round"},
		{"empty login", "github:41:=drive_round"},
		{"login with a space", "github:41:codex bot=drive_round"},
		{"account ID not a number", "github:codex[bot]:41=drive_round"},
		{"zero account ID", "github:0:codex[bot]=drive_round"},
		{"unknown forge", "gitlab:41:codex[bot]=drive_round"},
		{"unknown authority", "github:41:codex[bot]=advise"},
		{"empty authority", "github:41:codex[bot]="},
		{"empty value", ""},
	} {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			_, err := parseOnboardConfig(append(slices.Clone(baseArgs), "-external-reviewer", tt.value), io.Discard)
			if err == nil {
				t.Fatalf("accepted -external-reviewer %q", tt.value)
			}
		})
	}
}

func TestParseOnboardConfigProtectedPaths(t *testing.T) {
	t.Parallel()
	baseArgs := []string{
		"example/repo",
		"-db", "/tmp/freeside.db",
		"-state-dir", "/tmp/freeside-state",
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", "/tmp/verify.json",
	}
	t.Run("absent flag widens nothing", func(t *testing.T) {
		cfg, err := parseOnboardConfig(baseArgs, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(onboardPolicy(cfg).ProtectedPaths, domain.ProtectedPathConfig{}) {
			t.Fatalf("ProtectedPaths = %+v, want every list nil", cfg.ProtectedPaths)
		}
	})
	t.Run("each class fills its field and reaches the policy", func(t *testing.T) {
		cfg, err := parseOnboardConfig(append(slices.Clone(baseArgs),
			"-protected-path", "automation_control=.githooks/**",
			"-protected-path", "reviewer_instruction=REVIEW.md",
			"-protected-path", "git_metadata=.gitmodules.d/**",
			"-protected-path", "verification_control=.freeside/**",
			"-protected-path", "verification_control=scripts/check.sh",
			"-protected-path", "prompts_and_policy=prompts/**",
			"-protected-path", "egress_and_trust=egress/**",
			"-protected-path", "materiality_rules=docs/plan.md",
			"-protected-path", "materiality_rules=docs/rule=x.md",
		), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		want := domain.ProtectedPathConfig{
			ExtraAutomationControlPatterns:   []string{".githooks/**"},
			ExtraReviewerInstructionPatterns: []string{"REVIEW.md"},
			ExtraGitMetadataPatterns:         []string{".gitmodules.d/**"},
			ExtraVerificationControlPatterns: []string{".freeside/**", "scripts/check.sh"},
			ExtraPromptsAndPolicyPatterns:    []string{"prompts/**"},
			ExtraEgressAndTrustPatterns:      []string{"egress/**"},
			// The value splits at the first "=", so a pattern may hold one.
			ExtraMaterialityRulesPatterns: []string{"docs/plan.md", "docs/rule=x.md"},
		}
		if got := onboardPolicy(cfg).ProtectedPaths; !reflect.DeepEqual(got, want) {
			t.Fatalf("policy ProtectedPaths = %+v, want %+v", got, want)
		}
	})
	for _, tt := range []struct {
		name  string
		value string
	}{
		{"unknown class", "verification_recipes=.freeside/**"},
		{"field-name class", "extra_verification_control_patterns=.freeside/**"},
		{"no equals sign", "verification_control"},
		{"empty pattern", "verification_control="},
		{"empty class", "=.freeside/**"},
		{"empty value", ""},
	} {
		t.Run("rejects "+tt.name, func(t *testing.T) {
			_, err := parseOnboardConfig(append(slices.Clone(baseArgs), "-protected-path", tt.value), io.Discard)
			if err == nil {
				t.Fatalf("accepted -protected-path %q", tt.value)
			}
		})
	}
}

// TestOnboardRejectsNonCanonicalProtectedPathsBeforeStoreWork pins that the
// command refuses a widening the profile would otherwise canonicalize or
// record as unmatchable, with the profile validator's error, before it opens
// the store: the -db parent does not exist, so any store work would fail
// with a different error.
func TestOnboardRejectsNonCanonicalProtectedPathsBeforeStoreWork(t *testing.T) {
	t.Parallel()
	recipePath := filepath.Join(t.TempDir(), "verify.json")
	if err := os.WriteFile(
		recipePath, []byte(`{"commands":[["go","test","./..."]],"capture":"none"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name     string
		patterns []string
	}{
		{"unsorted", []string{"scripts/check.sh", ".freeside/**"}},
		{"duplicate", []string{".freeside/**", ".freeside/**"}},
		{"absolute", []string{"/scripts/check.sh"}},
		{"dot segment", []string{"scripts/../check.sh"}},
		{"trailing slash", []string{"scripts/"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			args := []string{
				"example/repo",
				"-db", filepath.Join(t.TempDir(), "missing", "freeside.db"),
				"-state-dir", t.TempDir(),
				"-registration-id", "11",
				"-repository-id", "44",
				"-commit", "0123456789012345678901234567890123456789",
				"-base-ref", "main",
				"-base-image", "example.invalid/agent@sha256:test",
				"-base-build-ref", "local/agent:test",
				"-review-config-digest", "sha256:review",
				"-recipe", recipePath,
			}
			for _, pattern := range tt.patterns {
				args = append(args, "-protected-path", "verification_control="+pattern)
			}
			err := runOnboardCommand(t.Context(), args, io.Discard, io.Discard)
			if !errors.Is(err, domain.ErrPatternsNotCanonical) ||
				!strings.HasPrefix(err.Error(), "-protected-path: protected paths extra_verification_control_patterns") {
				t.Fatalf("runOnboardCommand error = %v, want the -protected-path validation error", err)
			}
		})
	}
}

func TestOnboardUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"-h"}, {"example/repo", "-h"}, {"-db", "/tmp/freeside.db", "-h"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var output bytes.Buffer
			err := runOnboardCommand(context.Background(), args, io.Discard, &output)
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("err = %v, want flag.ErrHelp", err)
			}
			for _, want := range []string{"Usage: freesided onboard <owner/name> [flags]", "-db", "-protected-path", "-candidate-automation-changes"} {
				if !strings.Contains(output.String(), want) {
					t.Fatalf("usage missing %q:\n%s", want, output.String())
				}
			}
		})
	}
}

func TestParseOnboardConfigRepositoryPosition(t *testing.T) {
	t.Parallel()
	flagArgs := []string{
		"-db", "/tmp/freeside.db",
		"-state-dir", "/tmp/freeside-state",
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", "/tmp/verify.json",
	}
	t.Run("flags first matches repository first", func(t *testing.T) {
		repositoryFirst, err := parseOnboardConfig(append([]string{"example/repo"}, flagArgs...), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		flagsFirst, err := parseOnboardConfig(append(slices.Clone(flagArgs), "example/repo"), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if flagsFirst.Repository != "example/repo" || !reflect.DeepEqual(flagsFirst, repositoryFirst) {
			t.Fatalf("flags-first config = %+v, want %+v", flagsFirst, repositoryFirst)
		}
	})
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"no arguments", nil},
		{"flags without a repository", flagArgs},
	} {
		t.Run("requires repository with "+tt.name, func(t *testing.T) {
			_, err := parseOnboardConfig(tt.args, io.Discard)
			if err == nil || !strings.Contains(err.Error(), "repository owner/name is required") {
				t.Fatalf("err = %v, want the repository-required error", err)
			}
		})
	}
	t.Run("rejects a repository between flags", func(t *testing.T) {
		args := append(slices.Clone(flagArgs[:2]), "example/repo")
		_, err := parseOnboardConfig(append(args, flagArgs[2:]...), io.Discard)
		if err == nil || !strings.Contains(err.Error(), "unexpected positional arguments") {
			t.Fatalf("err = %v, want unexpected positional arguments", err)
		}
	})
}

func TestOnboardStoreCanPassSubmitTopicKeyGate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	recipePath := filepath.Join(root, "verify.json")
	if err := os.WriteFile(
		recipePath, []byte(`{"commands":[["go","test","./..."]],"capture":"none"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	// From empty on purpose: the store onboard creates is what submit's
	// topic-key gate must accept.
	err := runOnboardCommand(t.Context(), []string{
		"example/repo",
		"-db", dbPath,
		"-state-dir", filepath.Join(root, "state"),
		"-credentials-dir", filepath.Join(root, "credentials"),
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", recipePath,
	}, io.Discard, io.Discard)
	const want = "-account, positive -account-id, and non-negative -installation-id are required until the repository is trusted"
	if err == nil || err.Error() != want {
		t.Fatalf("runOnboardCommand error = %v, want %q", err, want)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("stat onboarded store: %v", err)
	}
	info, err := os.Stat(dbPath + topicKeySuffix)
	if err != nil {
		t.Fatalf("stat onboarded topic key: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != sha256.Size {
		t.Fatalf("onboarded topic key mode/size = %v/%d, want regular 0600/%d", info.Mode(), info.Size(), sha256.Size)
	}
	key, err := loadOrCreateTopicKey(dbPath, true)
	if err != nil {
		t.Fatalf("submit topic-key gate: %v", err)
	}
	if len(key) != sha256.Size {
		t.Fatalf("topic key length = %d, want %d", len(key), sha256.Size)
	}
}

func TestOnboardRejectsInvalidPolicyFlagsBeforeStoreWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ flag, want string }{
		{"-commit-plan", "-commit-plan \"bogus\" is invalid; valid values: [single_commit plan_preferred]"},
		{"-candidate-automation-changes", "-candidate-automation-changes \"bogus\" is invalid; valid values: [block advisory]"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			t.Parallel()
			recipePath := filepath.Join(t.TempDir(), "verify.json")
			if err := os.WriteFile(
				recipePath, []byte(`{"commands":[["go","test","./..."]],"capture":"none"}`), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			// The database path's parent does not exist, so any store work
			// would fail differently: the flag is refused first.
			err := runOnboardCommand(t.Context(), []string{
				"example/repo",
				"-db", filepath.Join(t.TempDir(), "missing", "freeside.db"),
				"-state-dir", t.TempDir(),
				"-registration-id", "11",
				"-repository-id", "44",
				"-commit", "0123456789012345678901234567890123456789",
				"-base-ref", "main",
				"-base-image", "example.invalid/agent@sha256:test",
				"-base-build-ref", "local/agent:test",
				"-review-config-digest", "sha256:review",
				"-recipe", recipePath,
				tc.flag, "bogus",
			}, io.Discard, io.Discard)
			if err == nil || err.Error() != tc.want {
				t.Fatalf("runOnboardCommand error = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestParseOnboardConfigAutomationChanges: the stance defaults to block, the
// owner's value reaches the proposed policy, and so the approval digest.
func TestParseOnboardConfigAutomationChanges(t *testing.T) {
	t.Parallel()
	baseArgs := []string{
		"example/repo",
		"-db", "/tmp/freeside.db",
		"-state-dir", "/tmp/freeside-state",
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", "/tmp/verify.json",
	}
	for _, tc := range []struct {
		name string
		args []string
		want domain.AutomationChangePolicy
	}{
		{"default", nil, domain.AutomationChangesBlocked},
		{"block", []string{"-candidate-automation-changes", "block"}, domain.AutomationChangesBlocked},
		{"advisory", []string{"-candidate-automation-changes", "advisory"}, domain.AutomationChangesAdvisory},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := parseOnboardConfig(append(slices.Clone(baseArgs), tc.args...), io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if got := onboardPolicy(cfg).CandidateAutomationChanges; got != tc.want {
				t.Fatalf("CandidateAutomationChanges = %q, want %q", got, tc.want)
			}
		})
	}
}

type installationAuthorityFixtures map[int64]publish.InstallationAuthority

func (f installationAuthorityFixtures) InstallationAuthority(
	_ context.Context, registrationID int64,
) (publish.InstallationAuthority, error) {
	authority, ok := f[registrationID]
	if !ok {
		return publish.InstallationAuthority{}, errors.New("missing registration fixture")
	}
	return authority, nil
}

func TestTrustedInstallationChecksEveryLocalRegistration(t *testing.T) {
	t.Parallel()
	const repositoryID int64 = 44
	trusted := func(registrationID, installationID int64) publish.InstallationAuthority {
		return publish.InstallationAuthority{
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: registrationID, InstallationID: installationID,
				Account: "example", AccountID: 33,
				RepositoryIDs: []int64{repositoryID},
			}},
		}
	}
	for _, tc := range []struct {
		name        string
		authorities installationAuthorityFixtures
		selected    int64
		want        *publish.TrustedInstallation
		wantErr     error
	}{
		{
			name: "selected registration owns repository",
			authorities: installationAuthorityFixtures{
				11: trusted(11, 21),
				12: {TrustedInstallations: []publish.TrustedInstallation{}},
			},
			selected: 11,
			want: &publish.TrustedInstallation{
				RegistrationID: 11, InstallationID: 21,
				Account: "example", AccountID: 33,
				RepositoryIDs: []int64{repositoryID},
			},
		},
		{
			name: "different registration already owns repository",
			authorities: installationAuthorityFixtures{
				11: {TrustedInstallations: []publish.TrustedInstallation{}},
				12: trusted(12, 22),
			},
			selected: 11, wantErr: publish.ErrAmbiguousInstallation,
		},
		{
			name: "multiple registrations already own repository",
			authorities: installationAuthorityFixtures{
				11: trusted(11, 21),
				12: trusted(12, 22),
			},
			selected: 11, wantErr: publish.ErrAmbiguousInstallation,
		},
		{
			name: "repository is not yet trusted",
			authorities: installationAuthorityFixtures{
				11: {TrustedInstallations: []publish.TrustedInstallation{}},
				12: {TrustedInstallations: []publish.TrustedInstallation{}},
			},
			selected: 11,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := trustedInstallation(
				t.Context(), tc.authorities, []int64{11, 12},
				tc.selected, repositoryID,
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("trustedInstallation error = %v, want %v", err, tc.wantErr)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("trustedInstallation = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.RegistrationID != tc.want.RegistrationID ||
				got.InstallationID != tc.want.InstallationID {
				t.Fatalf("trustedInstallation = %+v, want %+v", got, tc.want)
			}
		})
	}
}
