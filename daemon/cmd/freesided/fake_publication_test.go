package main

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func TestFakePublicationCommandConfigDefaults(t *testing.T) {
	cfg := fakePublicationCommandConfig{
		DBPath: "/tmp/freeside.db", StateDir: "/tmp/state",
		CredentialsDir: "/tmp/credentials", WorkspaceDir: "/tmp/workspace",
		RecipeFile: "/tmp/recipe.json", Repo: "owner/repo",
		BaseRef: "main", BaseSHA: "1111111111111111111111111111111111111111",
		AllowedPaths: []string{" docs/** ", "Sources/**"},
	}
	if err := cfg.withDefaultsAndValidate(); err != nil {
		t.Fatalf("withDefaultsAndValidate: %v", err)
	}
	if cfg.RecipeRepoPath != verify.DefaultRecipePath ||
		cfg.ReconcileInterval != defaultReconcileInterval ||
		cfg.JanitorInterval != defaultJanitorInterval {
		t.Fatalf("defaults = %+v", cfg)
	}
	if cfg.AllowedPaths[0] != "docs/**" {
		t.Fatalf("trimmed allowlist = %v", cfg.AllowedPaths)
	}
}

func TestFakePublicationCommandConfigRequiresAttendedInputs(t *testing.T) {
	var cfg fakePublicationCommandConfig
	if err := cfg.withDefaultsAndValidate(); err == nil {
		t.Fatal("empty config succeeded")
	}
}
