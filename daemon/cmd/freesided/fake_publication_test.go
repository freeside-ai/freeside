package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func TestFakePublicationTerminalReadableWaitsForJanitorCoverage(t *testing.T) {
	fatal := errors.New("reconcile failed")
	tests := []struct {
		name         string
		reconcileErr error
		wantReadable bool
		wantReturned error
	}{
		{name: "completed pass", wantReadable: true},
		{name: "inactive janitor", reconcileErr: publish.ErrJanitorInactive},
		{name: "wrapped inactive janitor", reconcileErr: errors.Join(
			errors.New("recover terminal task"), publish.ErrJanitorInactive,
		)},
		{name: "fatal reconciliation", reconcileErr: fatal, wantReturned: fatal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			readable, err := fakePublicationTerminalReadable(tt.reconcileErr)
			if readable != tt.wantReadable || !errors.Is(err, tt.wantReturned) {
				t.Fatalf("disposition = %t, %v; want %t, %v",
					readable, err, tt.wantReadable, tt.wantReturned)
			}
		})
	}
}

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

func TestPrepareFakePublicationConfigUsesDurablePathsBeforeResolvingAliases(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "pending", true: "completed"}[dispatched], func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "workspace")
			workDir := filepath.Join(root, "durable-work")
			credentials := filepath.Join(root, "credentials")
			if err := os.Mkdir(workspace, 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(workDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(credentials, 0o700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(root, "workspace-alias")
			if err := os.Symlink(credentials, alias); err != nil {
				t.Fatal(err)
			}
			workAlias := filepath.Join(root, "work-alias")
			if err := os.Symlink(workspace, workAlias); err != nil {
				t.Fatal(err)
			}
			recipeAlias := filepath.Join(root, "recipe-alias")
			if err := os.Symlink(workspace, recipeAlias); err != nil {
				t.Fatal(err)
			}
			cfg := fakePublicationCommandConfig{
				DBPath: filepath.Join(root, "freeside.db"), StateDir: filepath.Join(root, "state"),
				CredentialsDir: credentials, WorkspaceDir: alias,
				WorkDir: workAlias, RecipeFile: recipeAlias, Repo: "owner/repo",
				BaseRef: "main", BaseSHA: "1111111111111111111111111111111111111111",
			}
			found, err := prepareFakePublicationConfig(
				t.Context(),
				&cfg,
				func(
					_ context.Context,
					received fakePublicationCommandConfig,
				) (engine.FakePublicationReplayBinding, bool, error) {
					if received.WorkspaceDir != alias {
						t.Fatalf("loader received workspace %q, want unresolved alias %q",
							received.WorkspaceDir, alias)
					}
					if received.WorkDir != workAlias {
						t.Fatalf("loader received work root %q, want unresolved alias %q",
							received.WorkDir, workAlias)
					}
					return engine.FakePublicationReplayBinding{
						WorkspaceDir: workspace,
						WorkDir:      workDir,
						Dispatched:   dispatched,
					}, true, nil
				},
			)
			if err != nil {
				t.Fatalf("prepare replay config: %v", err)
			}
			resolvedWorkspace, err := resolvePublicationPath(workspace)
			if err != nil {
				t.Fatal(err)
			}
			if !found || cfg.WorkspaceDir != resolvedWorkspace {
				t.Fatalf("prepared workspace = %q, found %t, want durable %q",
					cfg.WorkspaceDir, found, resolvedWorkspace)
			}
			resolvedWorkDir, err := resolvePublicationPath(workDir)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.WorkDir != resolvedWorkDir {
				t.Fatalf("prepared work root = %q, want durable %q",
					cfg.WorkDir, resolvedWorkDir)
			}
			if cfg.RecipeFile != recipeAlias {
				t.Fatalf("replay recipe path = %q, want unused ambient alias %q",
					cfg.RecipeFile, recipeAlias)
			}
		})
	}
}

func TestPrepareFakePublicationConfigValidatesAmbientRecipeForNewRun(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o750); err != nil {
		t.Fatal(err)
	}
	recipeAlias := filepath.Join(root, "recipe-alias")
	if err := os.Symlink(workspace, recipeAlias); err != nil {
		t.Fatal(err)
	}
	cfg := fakePublicationCommandConfig{
		DBPath: filepath.Join(root, "freeside.db"), StateDir: filepath.Join(root, "state"),
		CredentialsDir: filepath.Join(root, "credentials"), WorkspaceDir: workspace,
		RecipeFile: recipeAlias, Repo: "owner/repo",
		BaseRef: "main", BaseSHA: "1111111111111111111111111111111111111111",
	}
	found, err := prepareFakePublicationConfig(
		t.Context(),
		&cfg,
		func(
			context.Context,
			fakePublicationCommandConfig,
		) (engine.FakePublicationReplayBinding, bool, error) {
			return engine.FakePublicationReplayBinding{}, false, nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "-publication-recipe overlaps") {
		t.Fatalf("new-run ambient recipe validation error = %v", err)
	}
	if found {
		t.Fatal("new run reported a durable replay binding")
	}
}

func TestExistingFakePublicationResultReturnsDurableTerminalItem(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	attention := signet.NewService(st)
	runID := domain.RunID("run-terminal")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: engine.FakePublicationBlockedItemID(runID), ProjectID: "project-terminal",
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
