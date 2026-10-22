package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func judgmentFixture(t *testing.T) (judgmentConfig, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil { //nolint:gosec // G302: owner traversal is required for the private credential directory.
		t.Fatal(err)
	}
	bin := filepath.Join(root, "cli")
	b := []byte("#!/bin/sh\nexit 1\n")
	h := sha256.Sum256(b)
	if err := os.WriteFile(bin, b, 0o700); err != nil { //nolint:gosec // G306: executable fixture in an owner-only test directory.
		t.Fatal(err)
	}
	return judgmentConfig{CLI: claudeinference.Config{Binary: bin, SHA256: hex.EncodeToString(h[:])}}, root
}

// pinAnotherJudgmentCLI points cfg at a second executable under root, pinned
// by its own digest: a configuration that composes and differs from the
// fixture's.
func pinAnotherJudgmentCLI(t *testing.T, cfg *judgmentConfig, root string) {
	t.Helper()
	bin := filepath.Join(root, "other-cli")
	b := []byte("#!/bin/sh\nexit 2\n")
	h := sha256.Sum256(b)
	if err := os.WriteFile(bin, b, 0o700); err != nil { //nolint:gosec // G306: executable fixture in an owner-only test directory.
		t.Fatal(err)
	}
	cfg.CLI = claudeinference.Config{Binary: bin, SHA256: hex.EncodeToString(h[:])}
}

func TestPublicationAuthorPromptBindingAndFailsafe(t *testing.T) {
	// With no prompt file the publication author is off: the role source says
	// so before it reads a line, both sites return their fail-safe with
	// nothing blocking, and the runtime carries no driver.
	unavailable, digest, err := composeJudgments(judgmentConfig{})
	if err != nil || digest != "" || unavailable.Driver != nil || unavailable.AuthorPrompt != nil {
		t.Fatalf("unconfigured runtime carries a driver or prompt; digest=%q, err=%v", digest, err)
	}
	budget := inference.Budget{
		Window: time.Hour,
		Site:   inference.Limits{Calls: 10, ComputeUnits: 100_000, Starvation: time.Hour},
	}
	budget.Project, budget.Global = budget.Site, budget.Site
	budget.MaxCallsPerRoot, budget.MaxStarvationPerRoot = 10, time.Hour
	store, err := advisory.Open(filepath.Join(t.TempDir(), "advisory.json"), 100, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	client, err := inference.New(inference.Config{
		StatePath: filepath.Join(t.TempDir(), "ledger.json"),
		Roles:     judgmentRoles{runtime: unavailable},
		Sites:     []inference.Site{inference.PublicationAuthorExplainSite(budget), inference.PublicationAuthorProposeSite(budget)},
		Advisory:  store,
	})
	if err != nil {
		t.Fatal(err)
	}
	input := inference.PublicationAuthorInput{
		Project: "p", RootLineage: "r", TargetRepository: "o/r", TargetVisibility: inference.RepositoryPublic,
		SourceIssueRef: "#1", SourceVisibility: inference.RepositoryPublic, Diff: "diff",
		PRTemplate:          inference.ControlFile{Content: "t", Digest: contentaddr.Sum([]byte("t")), TrustedBaseCommit: "base"},
		InstructionSnapshot: inference.ControlFile{Content: "s", Digest: contentaddr.Sum([]byte("s")), TrustedBaseCommit: "base"},
	}
	authored, err := client.AuthorPublication(t.Context(), input)
	if err != nil || !authored.Fallback {
		t.Fatalf("explain on unavailable binding = %+v, %v", authored, err)
	}
	proposed, err := client.ProposeSourceIssueClosure(t.Context(), input)
	if err != nil || !proposed.Fallback || proposed.Resolves {
		t.Fatalf("propose on unavailable binding = %+v, %v", proposed, err)
	}

	// A configured prompt changes the configuration digest, and a different
	// prompt file changes it again; the driver is bound.
	cfg, root := judgmentFixture(t)
	_, base, err := composeJudgments(cfg)
	if err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(root, "author.md")
	if err := os.WriteFile(promptPath, []byte("role prompt one"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.PublicationAuthorPrompt = promptPath
	bound, withPrompt, err := composeJudgments(cfg)
	if err != nil || bound.Driver == nil || withPrompt == base || string(bound.AuthorPrompt) != "role prompt one" {
		t.Fatalf("prompt digest = %q (base %q), err = %v", withPrompt, base, err)
	}
	if err := os.WriteFile(promptPath, []byte("role prompt two, different"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed, err := composeJudgments(cfg)
	if err != nil || changed == withPrompt {
		t.Fatalf("different prompt did not change the digest: %q vs %q, err %v", changed, withPrompt, err)
	}

	// An empty or oversized prompt file is a composition error.
	if err := os.WriteFile(promptPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := composeJudgments(cfg); err == nil {
		t.Fatal("empty prompt file accepted")
	}
	if err := os.WriteFile(promptPath, bytes.Repeat([]byte("x"), (32<<10)+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := composeJudgments(cfg); err == nil {
		t.Fatal("oversized prompt file accepted")
	}
}

// The receipt commits to the configuration and to no credential: nothing the
// judgment flags compose reads one.
func TestJudgmentBindingAndReceipt(t *testing.T) {
	cfg, root := judgmentFixture(t)
	b, digest, err := composeJudgments(cfg)
	if err != nil || b.Driver == nil || digest == "" {
		t.Fatalf("binding failed: %v", err)
	}
	_, again, err := composeJudgments(cfg)
	if err != nil || again != digest {
		t.Fatal("unstable binding")
	}
	pinAnotherJudgmentCLI(t, &cfg, root)
	_, changed, err := composeJudgments(cfg)
	if err != nil || changed == digest {
		t.Fatal("CLI pin absent from receipt")
	}
}

func TestJudgmentStartupRequiresPreflightBinding(t *testing.T) {
	for _, change := range []string{"unchanged", "changed CLI", "missing digest", "invalid digest"} {
		t.Run(change, func(t *testing.T) {
			cfg, root := judgmentFixture(t)
			_, digest, err := composeJudgments(cfg)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ExpectedDigest = digest
			switch change {
			case "changed CLI":
				pinAnotherJudgmentCLI(t, &cfg, root)
			case "missing digest":
				cfg.ExpectedDigest = ""
			case "invalid digest":
				cfg.ExpectedDigest = "invalid"
			}
			binding, err := composeRuntimeJudgments(cfg)
			if change == "unchanged" {
				if err != nil || binding.Driver == nil {
					t.Fatalf("unchanged preflight binding rejected: %v", err)
				}
			} else if err == nil || binding.Driver != nil {
				t.Fatal("startup accepted an unattested binding")
			}
		})
	}
	binding, err := composeRuntimeJudgments(judgmentConfig{})
	if err != nil || binding.Driver != nil {
		t.Fatal("unconfigured startup fallback changed")
	}
}

// The production composition starts with the judgment flags absent: the
// runtime has no driver, and the task-judgment ownership journal is opened
// around none, so a task stop still confirms what an earlier start with
// judgments on left in it while no call can be made through it. With the
// flags set the driver is wrapped in the journal, still held to the
// preflight digest (#1945).
func TestProductionJudgmentsComposeWithTheFlagsAbsent(t *testing.T) {
	stateDir := t.TempDir()
	ownership := filepath.Join(stateDir, "task-judgments")
	off, owned, err := composeProductionJudgments(stateDir, judgmentConfig{})
	if err != nil || off.Driver != nil || off.AuthorPrompt != nil || owned == nil {
		t.Fatalf("composition with the flags absent = %+v, %v, %v", off, owned, err)
	}
	if info, err := os.Stat(ownership); err != nil || !info.IsDir() {
		t.Fatalf("ownership directory with judgments off: %v", err)
	}
	if _, err := owned.Complete(t.Context(), inference.Request{SiteID: "task-name"}, ""); err == nil {
		t.Fatal("the journal made a call with judgments off")
	}
	if err := owned.ConfirmRun(t.Context(), "task", "run"); err != nil {
		t.Fatalf("a run with no judgment records did not confirm: %v", err)
	}

	cfg, _ := judgmentFixture(t)
	_, digest, err := composeJudgments(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ExpectedDigest = digest
	on, owned, err := composeProductionJudgments(stateDir, cfg)
	if err != nil || owned == nil {
		t.Fatalf("composition with the flags set = %+v, %v, %v", on, owned, err)
	}
	if journal, ok := on.Driver.(*ward.TaskJudgments); !ok || journal != owned {
		t.Fatalf("configured driver is %T, want the returned ownership journal", on.Driver)
	}
	cfg.ExpectedDigest = ""
	if _, _, err := composeProductionJudgments(stateDir, cfg); err == nil {
		t.Fatal("a configured runtime composed without its preflight digest")
	}
}

func TestJudgmentConfigRefusesAPartialPin(t *testing.T) {
	cfg, _ := judgmentFixture(t)
	cfg.CLI.SHA256 = ""
	if _, _, err := composeJudgments(cfg); err == nil {
		t.Fatal("partial configuration accepted")
	}
	b, digest, err := composeJudgments(judgmentConfig{})
	if err != nil || b.Driver != nil || digest != "" {
		t.Fatal("unconfigured fallback changed")
	}
}

// The judgment flags name the CLI pin and no model. -judgment-auth-snapshot
// is gone with the host token file it named (#1426), and -judgment-model
// with the fallback it fed (#1964): a command line that still passes either
// is refused, not silently ignored.
func TestJudgmentFlags(t *testing.T) {
	cfg, _ := judgmentFixture(t)
	args := []string{"-judgment-claude-bin", cfg.CLI.Binary, "-judgment-claude-sha256", cfg.CLI.SHA256}
	parse := func(args []string) (judgmentConfig, error) {
		var parsed judgmentConfig
		flags := flag.NewFlagSet("test", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		judgmentFlags(flags, &parsed)
		return parsed, flags.Parse(args)
	}
	parsed, err := parse(args)
	if err != nil {
		t.Fatal(err)
	}
	if parsed != cfg {
		t.Fatal("flag binding mismatch")
	}
	if _, _, err := composeJudgments(parsed); err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"-judgment-auth-snapshot", "-judgment-model"} {
		if _, err := parse(append(slices.Clone(args), removed, "value")); err == nil ||
			!strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatalf("removed flag %s: err = %v, want it unknown", removed, err)
		}
	}
}

// Preflight binds the judgment configuration and refuses one whose CLI pin
// does not hold. It reads no host token file and names no model: the removed
// flags are unknown to it too.
func TestJudgmentPreflightBindsAndRejectsConfiguration(t *testing.T) {
	base, environment := preflightFixture(t)
	cfg, _ := judgmentFixture(t)
	run := func(extra ...string) (compositionManifest, error) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		err := runPreflightCommandWithEnvironment(t.Context(), append(slices.Clone(base), extra...),
			&stdout, &stderr, environment, time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "test-build")
		var manifest compositionManifest
		if stdout.Len() != 0 {
			if decodeErr := json.Unmarshal(stdout.Bytes(), &manifest); decodeErr != nil {
				t.Fatal(decodeErr)
			}
		}
		return manifest, err
	}
	manifest, err := run("-judgment-claude-bin", cfg.CLI.Binary, "-judgment-claude-sha256", cfg.CLI.SHA256)
	if err != nil || manifest.JudgmentConfigurationDigest == "" ||
		checkStatus(manifest, "judgment_configuration") != compositionPassed {
		t.Fatalf("valid judgment preflight failed: %v", err)
	}
	manifest, err = run("-judgment-claude-bin", cfg.CLI.Binary, "-judgment-claude-sha256", strings.Repeat("0", 64))
	if err == nil || manifest.JudgmentConfigurationDigest != "" ||
		checkStatus(manifest, "judgment_configuration") != compositionFailed {
		t.Fatal("judgment preflight passed a CLI that does not match its pin")
	}
	for _, removed := range []string{"-judgment-auth-snapshot", "-judgment-model"} {
		manifest, err = run("-judgment-claude-bin", cfg.CLI.Binary, "-judgment-claude-sha256", cfg.CLI.SHA256,
			removed, "value")
		if err == nil || len(manifest.Checks) != 0 {
			t.Fatalf("preflight accepted the removed %s flag: %v", removed, err)
		}
	}
}
