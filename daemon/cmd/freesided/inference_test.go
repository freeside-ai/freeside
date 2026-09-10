package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
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
	if err := os.WriteFile(filepath.Join(root, "token"), []byte("synthetic-subscription-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return judgmentConfig{CLI: claudeinference.Config{Binary: bin, SHA256: hex.EncodeToString(h[:]), Model: "test-model"}, AuthSnapshot: "token"}, root
}

func TestJudgmentBindingAndReceipt(t *testing.T) {
	cfg, root := judgmentFixture(t)
	b, digest, err := composeJudgments(cfg, root)
	if err != nil || b.Driver == nil || digest == "" || b.Model != "test-model" || b.Credential.Reveal() != "synthetic-subscription-token" {
		t.Fatalf("binding failed: %v", err)
	}
	_, again, err := composeJudgments(cfg, root)
	if err != nil || again != digest {
		t.Fatal("unstable binding")
	}
	cfg.CLI.Model = "changed-model"
	_, changed, err := composeJudgments(cfg, root)
	if err != nil || changed == digest {
		t.Fatal("model absent from receipt")
	}
	cfg.CLI.Model = "test-model"
	if err := os.WriteFile(filepath.Join(root, "token"), []byte("rotated-subscription-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed, err = composeJudgments(cfg, root)
	if err != nil || changed == digest {
		t.Fatal("credential absent from receipt")
	}
	encoded, _ := json.Marshal(struct {
		Config     judgmentConfig
		Receipt    string
		Credential any
	}{cfg, digest, b.Credential})
	if strings.Contains(string(encoded), "synthetic-subscription-token") {
		t.Fatal("credential leaked")
	}
}

func TestJudgmentStartupRequiresPreflightBinding(t *testing.T) {
	for _, change := range []string{"unchanged", "rotated token", "changed model", "missing digest", "invalid digest"} {
		t.Run(change, func(t *testing.T) {
			cfg, root := judgmentFixture(t)
			_, digest, err := composeJudgments(cfg, root)
			if err != nil {
				t.Fatal(err)
			}
			cfg.ExpectedDigest = digest
			switch change {
			case "rotated token":
				if err := os.WriteFile(filepath.Join(root, "token"), []byte("rotated-subscription-token"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "changed model":
				cfg.CLI.Model = "changed-model"
			case "missing digest":
				cfg.ExpectedDigest = ""
			case "invalid digest":
				cfg.ExpectedDigest = "invalid"
			}
			binding, err := composeRuntimeJudgments(cfg, root)
			if change == "unchanged" {
				if err != nil || binding.Driver == nil || binding.Credential.Reveal() != "synthetic-subscription-token" {
					t.Fatalf("unchanged preflight binding rejected: %v", err)
				}
			} else if err == nil || binding.Driver != nil || binding.Credential.Reveal() != "" {
				t.Fatal("startup accepted an unattested binding")
			}
		})
	}
	binding, err := composeRuntimeJudgments(judgmentConfig{}, "")
	if err != nil || binding.Driver != nil || binding.Provider != "unavailable" {
		t.Fatal("unconfigured startup fallback changed")
	}
}

func TestJudgmentConfigRefusesUnsafeSnapshots(t *testing.T) {
	for _, name := range []string{"missing", "public file", "public root", "symlink", "outside", "partial"} {
		t.Run(name, func(t *testing.T) {
			cfg, root := judgmentFixture(t)
			token := filepath.Join(root, "token")
			switch name {
			case "missing":
				cfg.AuthSnapshot = "absent"
			case "public file":
				if err := os.Chmod(token, 0o644); err != nil { //nolint:gosec // G302: deliberately unsafe fixture must be rejected.
					t.Fatal(err)
				}
			case "public root":
				if err := os.Chmod(root, 0o755); err != nil { //nolint:gosec // G302: deliberately unsafe root must fail credential admission.
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(token, filepath.Join(root, "link")); err != nil {
					t.Fatal(err)
				}
				cfg.AuthSnapshot = "link"
			case "outside":
				cfg.AuthSnapshot = "../token"
			case "partial":
				cfg.CLI.Model = ""
			}
			if _, _, err := composeJudgments(cfg, root); err == nil {
				t.Fatal("unsafe snapshot accepted")
			}
		})
	}
	b, digest, err := composeJudgments(judgmentConfig{}, "")
	if err != nil || b.Driver != nil || digest != "" || b.Provider != "unavailable" {
		t.Fatal("unconfigured fallback changed")
	}
}

func TestJudgmentFlags(t *testing.T) {
	cfg, root := judgmentFixture(t)
	var parsed judgmentConfig
	flags := flag.NewFlagSet("test", flag.ContinueOnError)
	judgmentFlags(flags, &parsed)
	if err := flags.Parse([]string{"-judgment-claude-bin", cfg.CLI.Binary, "-judgment-claude-sha256", cfg.CLI.SHA256, "-judgment-model", cfg.CLI.Model, "-judgment-auth-snapshot", cfg.AuthSnapshot}); err != nil {
		t.Fatal(err)
	}
	if parsed != cfg {
		t.Fatal("flag binding mismatch")
	}
	if _, _, err := composeJudgments(parsed, root); err != nil {
		t.Fatal(err)
	}
}

func TestJudgmentPreflightBindsAndRejectsCredential(t *testing.T) {
	args, environment := preflightFixture(t)
	cfg, _ := judgmentFixture(t)
	var root string
	for i, arg := range args {
		if arg == "-review-input-root" {
			root = args[i+1]
		}
	}
	if root == "" {
		t.Fatal("fixture input root missing")
	}
	tokenPath := filepath.Join(root, "judgment-token")
	if err := os.WriteFile(tokenPath, []byte("synthetic-subscription-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	args = append(args, "-judgment-claude-bin", cfg.CLI.Binary, "-judgment-claude-sha256", cfg.CLI.SHA256, "-judgment-model", cfg.CLI.Model, "-judgment-auth-snapshot", "judgment-token")
	for _, valid := range []bool{true, false} {
		if !valid {
			if err := os.WriteFile(tokenPath, []byte("invalid\ntoken"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var stdout, stderr bytes.Buffer
		err := runPreflightCommandWithEnvironment(t.Context(), args, &stdout, &stderr, environment, time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), "test-build")
		var manifest compositionManifest
		if decodeErr := json.Unmarshal(stdout.Bytes(), &manifest); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if valid && (err != nil || manifest.JudgmentConfigurationDigest == "" || checkStatus(manifest, "judgment_configuration") != compositionPassed) {
			t.Fatalf("valid judgment preflight failed: %v", err)
		}
		if !valid && (err == nil || manifest.JudgmentConfigurationDigest != "" || checkStatus(manifest, "judgment_configuration") != compositionFailed) {
			t.Fatal("invalid judgment preflight passed")
		}
		if strings.Contains(stdout.String(), "synthetic-subscription-token") || strings.Contains(stdout.String(), tokenPath) {
			t.Fatal("preflight exposed credential input")
		}
	}
}
