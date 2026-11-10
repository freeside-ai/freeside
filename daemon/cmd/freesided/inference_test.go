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

	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
)

func judgmentFixture(t *testing.T) (daemoncompose.JudgmentConfig, string) {
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
	return daemoncompose.JudgmentConfig{CLI: claudeinference.Config{Binary: bin, SHA256: hex.EncodeToString(h[:])}}, root
}

// The judgment flags name the CLI pin and no model. -judgment-auth-snapshot
// is gone with the host token file it named (#1426), and -judgment-model
// with the fallback it fed (#1964): a command line that still passes either
// is refused, not silently ignored.
func TestJudgmentFlags(t *testing.T) {
	t.Parallel()
	cfg, _ := judgmentFixture(t)
	args := []string{"-judgment-claude-bin", cfg.CLI.Binary, "-judgment-claude-sha256", cfg.CLI.SHA256}
	parse := func(args []string) (daemoncompose.JudgmentConfig, error) {
		var parsed daemoncompose.JudgmentConfig
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
	if _, _, err := daemoncompose.ComposeJudgments(parsed); err != nil {
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
	t.Parallel()
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
