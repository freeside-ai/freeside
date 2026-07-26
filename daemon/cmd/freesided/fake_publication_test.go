package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func TestFakePublicationCommandConfigDefaults(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	cfg := fakePublicationCommandConfig{
		DBPath: filepath.Join(root, "freeside.db"), StateDir: filepath.Join(root, "state"),
		CredentialsDir: filepath.Join(root, "credentials"), WorkspaceDir: workspace,
		RecipeFile: filepath.Join(root, "recipe.json"), Repo: "owner/repo",
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

func TestFakePublicationCommandConfigRejectsWorkspaceStateOverlap(t *testing.T) {
	root := t.TempDir()
	newConfig := func(t *testing.T) fakePublicationCommandConfig {
		t.Helper()
		workspace := filepath.Join(root, "workspace")
		if err := os.MkdirAll(workspace, 0o750); err != nil {
			t.Fatal(err)
		}
		return fakePublicationCommandConfig{
			DBPath:   filepath.Join(root, "freeside.db"),
			StateDir: filepath.Join(root, "state"), CredentialsDir: filepath.Join(root, "credentials"),
			WorkDir: filepath.Join(root, "work"), FakeDriverDir: filepath.Join(root, "driver"),
			WorkspaceDir: workspace, RecipeFile: filepath.Join(root, "recipe.json"),
			Repo: "owner/repo", BaseRef: "main",
			BaseSHA: "1111111111111111111111111111111111111111",
		}
	}
	tests := []struct {
		name   string
		mutate func(*fakePublicationCommandConfig)
	}{
		{"database", func(cfg *fakePublicationCommandConfig) {
			cfg.DBPath = filepath.Join(cfg.WorkspaceDir, "freeside.db")
		}},
		{"artifact store", func(cfg *fakePublicationCommandConfig) {
			cfg.DBPath = filepath.Join(root, "candidate-state")
			cfg.WorkspaceDir = cfg.DBPath + ".blobs"
		}},
		{"work directory", func(cfg *fakePublicationCommandConfig) {
			cfg.WorkDir = filepath.Join(cfg.WorkspaceDir, "work")
		}},
		{"fake driver", func(cfg *fakePublicationCommandConfig) {
			cfg.FakeDriverDir = filepath.Join(cfg.WorkspaceDir, "driver")
		}},
		{"state directory", func(cfg *fakePublicationCommandConfig) {
			cfg.StateDir = filepath.Join(cfg.WorkspaceDir, "state")
		}},
		{"credentials directory", func(cfg *fakePublicationCommandConfig) {
			cfg.CredentialsDir = filepath.Join(cfg.WorkspaceDir, "credentials")
		}},
		{"recipe", func(cfg *fakePublicationCommandConfig) {
			cfg.RecipeFile = filepath.Join(cfg.WorkspaceDir, "recipe.json")
		}},
		{"workspace beneath state", func(cfg *fakePublicationCommandConfig) {
			cfg.StateDir = root
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := newConfig(t)
			tt.mutate(&cfg)
			if err := cfg.withDefaultsAndValidate(); err == nil {
				t.Fatal("overlapping config succeeded")
			}
		})
	}
}

func TestFakePublicationCommandConfigResolvesSymlinksBeforeContainment(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "workspace-link")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	cfg := fakePublicationCommandConfig{
		DBPath:   filepath.Join(root, "freeside.db"),
		StateDir: filepath.Join(link, "state"), CredentialsDir: filepath.Join(root, "credentials"),
		WorkDir: filepath.Join(root, "work"), FakeDriverDir: filepath.Join(root, "driver"),
		WorkspaceDir: workspace, RecipeFile: filepath.Join(root, "recipe.json"),
		Repo: "owner/repo", BaseRef: "main",
		BaseSHA: "1111111111111111111111111111111111111111",
	}
	if err := cfg.withDefaultsAndValidate(); err == nil {
		t.Fatal("symlinked overlap succeeded")
	}
}

func TestExistingFakePublicationResultReturnsDurableTerminalItem(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	attention := signet.NewService(st)
	runID := domain.RunID("run-terminal")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "publish-blocked-run-terminal", ProjectID: "project-terminal",
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID,
		},
		Type: domain.AttentionPublishBlocked, Priority: domain.PriorityHigh,
		Reason: "publication was blocked",
		RequestedDecision: []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := attention.PutItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	cfg := fakePublicationCommandConfig{
		RunID: runID, ProjectID: "project-terminal", Repo: "owner/repo",
	}
	result, ok, err := existingFakePublicationResult(ctx, attention, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || result.ItemID != item.ID || result.ItemType != item.Type ||
		result.OperatingMode != "attended_dev" || result.IsolationClass != "process_local" {
		t.Fatalf("terminal result = %+v, %t", result, ok)
	}
}

func TestFakePublicationCommandConfigRequiresAttendedInputs(t *testing.T) {
	var cfg fakePublicationCommandConfig
	if err := cfg.withDefaultsAndValidate(); err == nil {
		t.Fatal("empty config succeeded")
	}
}
