package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
)

// deputySettingsFixtureWithEveryOption is the fixture with every optional
// key set, so the pinned configuration covers each mapping.
func deputySettingsFixtureWithEveryOption(t *testing.T) map[string]any {
	t.Helper()
	settings := deputySettingsFixture(t)
	settings["judgment"] = map[string]any{
		"claude_bin": "/bin/claude", "claude_sha256": strings.Repeat("a", 64),
		"model": "claude-opus", "effort": "medium", "publication_author_prompt": "/prompts/author.md",
	}
	settings["manual_submission_config"] = "/config/manual.json"
	settings["build"] = map[string]any{"proxy": "", "base_build_ref": "base:local", "dns": []string{"1.1.1.1"}}
	settings["macos_verification"] = macosVerificationFixture()
	settings["repository"].(map[string]any)["hand_back_label"] = "needs-rework"
	settings["repository"].(map[string]any)["writer_class"] = "fresh_vm_read_only_volume_handoff"
	return settings
}

// TestDeputyClaudeDriverConfigForTheFixture pins the configuration the
// deputy composes from its settings file, so composing it through the
// shared run settings (#2082) changes nothing the deputy runs with beyond
// the project fields readiness reads (#3205).
func TestDeputyClaudeDriverConfigForTheFixture(t *testing.T) {
	t.Parallel()
	image := func(name string) string { return "registry.local/" + name + "@sha256:" + strings.Repeat("a", 64) }
	paths := newDeputyPaths("/rig/daemon")
	base := daemoncompose.ClaudeDriverConfig{ //nolint:gosec // G101 false positive: fixture directory paths, not credentials.
		AgentImage: domain.ImageRef(image("agent")), ExporterImage: image("exporter"),
		ContainerBin: "container", SeedRoot: "/rig/seed",
		WriterStopTimeout: 20 * time.Minute, WriterStallInterval: 0,
		StateDir: "/rig/daemon", RigTokenFile: "/rig/daemon/rig-acquisition.json",
		ProviderEndpoints:              []string{"api.anthropic.com:443"},
		PromptPackageFile:              "/prompts/impl.json",
		SpecificationPromptPackageFile: "/prompts/spec.json",
		RemediationPromptPackageFile:   "/prompts/rem.json",
		VendorInstructions:             "/prompts/CLAUDE.md",
		Repo:                           "acme/widgets", RepositoryID: 42, BaseRef: "main",
		AgentTreeCheckout: "/src/freeside", AgentTreeCommit: strings.Repeat("d", 40),
		AllowedPaths: []string{"src/**"},
		// The project and approved recipe name what the readiness
		// evaluator checks (#3205).
		ProjectID:      "project-widgets",
		ApprovedRecipe: domain.Digest("sha256:" + strings.Repeat("b", 64)),
		RunConformance: true,
		StateRoot:      "/prod/daemon", CredentialsDir: "/prod/credentials",
		OperatingMode: domain.ModeUnattended,
		ReviewImage:   image("review"), ReviewInputRoot: "/review", ReviewAuthMode: "subscription",
		ReviewAuthSnapshot: "/review/auth.json", ReviewInstructions: "/review/AGENTS.md",
		ReviewWorkspaceSizeMB: 8192,
	}
	everyOption := base
	everyOption.Judgments = daemoncompose.JudgmentConfig{
		CLI:                     claudeinference.Config{Binary: "/bin/claude", SHA256: strings.Repeat("a", 64)},
		PublicationAuthorPrompt: "/prompts/author.md", ExpectedDigest: "judgment-digest",
	}
	everyOption.Rebuild = projectimage.BuildInputs{BaseBuildRef: "base:local", DNS: []string{"1.1.1.1"}}
	everyOption.HandBackLabel = "needs-rework"
	everyOption.HostVerification = daemoncompose.MacverifyExecutorConfig{
		Root: "/Library/Freeside/macverify", Slots: []string{"fvslot2", "fvslot1"},
		XcodeBuild: "17F42", Swift: "swiftlang-6.1.2.1.2", PortBase: macverify.DefaultPortBase, ListenPort: 7340,
	}
	for name, tc := range map[string]struct {
		settings map[string]any
		digest   string
		want     daemoncompose.ClaudeDriverConfig
	}{
		"fixture":      {deputySettingsFixture(t), "", base},
		"every option": {deputySettingsFixtureWithEveryOption(t), "judgment-digest", everyOption},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path, _ := writeDeputySettings(t, tc.settings, 0o600)
			loaded, err := loadDeputySettings(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := deputyClaudeDriverConfig(loaded, paths, tc.digest); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("config\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}
