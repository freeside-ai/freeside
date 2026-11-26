package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// StoredProjectAuthority is the installed daemon's per-task project
// authority (#3209): it reads the project's current settings record inside
// the gate's transaction and re-checks it against current policy before any
// gate trusts it. It re-checks what LoadRunSettings checks at a start, so a
// gate and a start refuse the same records:
//   - the record folds with every required section (checkProjectRecord);
//   - every section passes the settings gate (CheckProjectSettings);
//   - the repository ID matches the project's binding
//     (CheckProjectRepositoryBinding);
//   - the project has a project image. A start composes no driver for a
//     project with a recipe binding and no image (storedDriverConfig), so
//     the gates refuse its tasks too, rather than admit one under another
//     project's driver and reach a verification with no room to run in.
//     #3627 lifts both once it builds that room; the binding then reaches
//     admission through RecipeBinding.
//
// A failed check is a configuration verdict and comes back joined with
// engine.ErrProjectSettingsRefused, so the gate holds the task until the
// record is corrected. A read or decode error comes back as it is: a bad
// stored row is a bug and fails loudly.
func StoredProjectAuthority(ctx context.Context, tx *store.ReadTx, project domain.ProjectID) (engine.ProjectAuthority, error) {
	record, err := tx.ProjectSettingsRecord(ctx, project)
	if err != nil {
		return engine.ProjectAuthority{}, err
	}
	settings, err := checkProjectRecord(record)
	if err != nil {
		return engine.ProjectAuthority{}, errors.Join(engine.ErrProjectSettingsRefused, err)
	}
	if err := tx.CheckProjectRepositoryBinding(ctx, project, settings.Repository.RepositoryID); err != nil {
		if errors.Is(err, store.ErrProjectRepositoryMismatch) {
			return engine.ProjectAuthority{}, errors.Join(engine.ErrProjectSettingsRefused, err)
		}
		return engine.ProjectAuthority{}, err
	}
	if settings.AgentImage == nil {
		return engine.ProjectAuthority{}, errors.Join(engine.ErrProjectSettingsRefused,
			fmt.Errorf("project %q binds its recipe with recipe_binding and has no agent_image: %w", project, ErrNoProjectImage))
	}
	authority := engine.ProjectAuthority{
		ProjectID:        project,
		SettingsRevision: record.Revision,
		Repo:             settings.Repository.Repo,
		RepositoryID:     settings.Repository.RepositoryID,
		BaseRef:          settings.Repository.BaseRef,
		AllowedPaths:     slices.Clone(settings.AllowedPaths),
		ApprovedRecipe:   settings.ApprovedRecipe,
		WriterClass:      settings.WriterClass,
		RecipeBinding:    settings.RecipeBinding,
	}
	if settings.HandBackLabel != nil {
		authority.HandBackLabel = *settings.HandBackLabel
	}
	return authority, nil
}

// projectAuthoritySource picks the source the driver's gates read a task's
// project authority from. A configuration composed from the store (it
// carries settings revisions) reads each project's stored settings per
// task; one composed from a settings file or flags serves its single
// project's fixed values, as it did before the gates read per project.
// The choice lives here, not in a ClaudeDriverConfig field, because the
// config is compared by value.
func projectAuthoritySource(cfg ClaudeDriverConfig) engine.ProjectAuthoritySource {
	if cfg.SettingsRevisions != nil {
		return StoredProjectAuthority
	}
	return engine.FixedProjectAuthority(fixedProjectAuthority(cfg))
}

func fixedProjectAuthority(cfg ClaudeDriverConfig) engine.ProjectAuthority {
	return engine.ProjectAuthority{
		ProjectID:      cfg.ProjectID,
		Repo:           cfg.Repo,
		RepositoryID:   cfg.RepositoryID,
		BaseRef:        cfg.BaseRef,
		AllowedPaths:   slices.Clone(cfg.AllowedPaths),
		ApprovedRecipe: cfg.ApprovedRecipe,
		HandBackLabel:  cfg.HandBackLabel,
		WriterClass:    cfg.WriterClass,
	}
}

// handBackLabelLookup adapts source to the hand-back remover and intake. A
// project whose settings grant its tasks nothing names no label: its pull
// requests keep theirs and its hand-backs start nothing, which is the
// refusal; the gates that hold its tasks report why.
func handBackLabelLookup(source engine.ProjectAuthoritySource) publish.HandBackLabelLookup {
	return func(ctx context.Context, tx *store.ReadTx, project domain.ProjectID) (string, error) {
		authority, err := source(ctx, tx, project)
		if errors.Is(err, engine.ErrProjectSettingsRefused) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return authority.HandBackLabel, nil
	}
}
