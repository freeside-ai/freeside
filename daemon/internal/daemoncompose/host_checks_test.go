package daemoncompose

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
)

// TestCheckHostNamesTheFirstFailure: a host where every start check passes
// composes, and each check that fails stops the start under its own name.
func TestCheckHostNamesTheFirstFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	base, _ := promptPackagesFixture(t)
	base.ContainerBin = filepath.Join(root, "container")
	base.ExporterImage = "registry.local/exporter@sha256:" + strings.Repeat("a", 64)
	base.ReviewImage = "registry.local/reviewer@sha256:" + strings.Repeat("b", 64)
	base.SeedRoot = filepath.Join(root, "seed")
	base.ReviewInstructions = filepath.Join(root, "review.md")
	base.ReviewAuthSnapshot = filepath.Join(root, "codex-auth")
	execfixture.Write(t, base.ContainerBin, []byte("#!/bin/sh\n"), 0o700)
	if err := os.WriteFile(base.ReviewInstructions, []byte("Review the change.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(base.ReviewAuthSnapshot, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	passing := func() hostProbes {
		return hostProbes{
			imageInStore: func(context.Context, string, string) error { return nil },
			lookPath:     func(string) (string, error) { return "/usr/bin/git", nil },
			seedClean:    func(context.Context, string) error { return nil },
		}
	}
	if err := checkHost(t.Context(), base, passing()); err != nil {
		t.Fatalf("passing host = %v, want nil", err)
	}

	failed := errors.New("failed")
	for _, tc := range []struct {
		want   string
		change func(*ClaudeDriverConfig, *hostProbes)
	}{
		{"host_tools.container", func(cfg *ClaudeDriverConfig, _ *hostProbes) {
			cfg.ContainerBin = filepath.Join(root, "absent", "container")
		}},
		{"host_tools.container", func(cfg *ClaudeDriverConfig, _ *hostProbes) { cfg.ContainerBin = root }},
		{"host_tools.git", func(_ *ClaudeDriverConfig, probes *hostProbes) {
			probes.lookPath = func(string) (string, error) { return "", failed }
		}},
		{"reviewer_image", func(cfg *ClaudeDriverConfig, probes *hostProbes) {
			probes.imageInStore = func(_ context.Context, _, ref string) error {
				if ref == cfg.ReviewImage {
					return failed
				}
				return nil
			}
		}},
		{"seed_root", func(_ *ClaudeDriverConfig, probes *hostProbes) {
			probes.seedClean = func(context.Context, string) error { return failed }
		}},
		{"prompt_packages", func(cfg *ClaudeDriverConfig, _ *hostProbes) {
			cfg.RemediationPromptPackageFile = filepath.Join(root, "absent.md")
		}},
		{"review_instructions", func(cfg *ClaudeDriverConfig, _ *hostProbes) {
			cfg.ReviewInstructions = cfg.ReviewAuthSnapshot
		}},
	} {
		cfg, probes := base, passing()
		tc.change(&cfg, &probes)
		var named *HostCheckError
		if err := checkHost(t.Context(), cfg, probes); !errors.As(err, &named) || named.Check != tc.want ||
			named.Fix == "" {
			t.Errorf("%s: got %v, want a HostCheckError naming it and its fix", tc.want, err)
		}
	}
}
