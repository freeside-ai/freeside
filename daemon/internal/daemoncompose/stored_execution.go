package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/topicstore"
)

// StoredExecution is how the installed daemon, prod started with no
// -driver, composes its driver from the stored settings (#2083): the launch
// inputs its environment tier fixes, which the settings records do not
// hold. Start reads the records, opens the store under the policy they
// name, and composes the driver when execution is on, exactly one project
// is enabled, that project's settings load, and prod's gated order lets it.
// Any of those that fails serves driverless with a card, never an exit:
// prod runs under KeepAlive.
type StoredExecution struct {
	// Launch is the launch half of the driver configuration; Start fills in
	// the judgment digest from the stored judgment section.
	Launch RunLaunch
	// CompositionFailure is why this process's earlier start could not
	// compose the driver from the settings (StoredCompositionError). Set,
	// the start composes none and raises the card that names it.
	CompositionFailure string
}

// StoredCompositionError is a start that chose to compose the driver from
// the stored settings and failed to. The caller starts again with the
// failure in StoredExecution.CompositionFailure, so the daemon serves
// driverless with a card instead of exiting.
type StoredCompositionError struct {
	Err error
}

func (e *StoredCompositionError) Error() string {
	return "compose the driver from the stored settings: " + e.Err.Error()
}

func (e *StoredCompositionError) Unwrap() error { return e.Err }

// storedSwitches is what the policy-free read finds: the switches and the
// recipes the store's policy approves. A switch that does not read stops the
// start at a card rather than an exit: executionRefused is the instance's,
// projectRefused the first project's.
type storedSwitches struct {
	executionOn      bool
	executionRefused error
	enabled          []domain.ProjectID
	projectRefused   error
	recipes          map[domain.Digest]bool
}

// readStoredSwitches opens the store under the caller's options, reads the
// switches and every project's approved recipe, and closes it. The store's
// open-time policy cannot come from records the store has to be open to
// read, so this read touches only the settings records, which carry no
// admission or evidence the policy gates.
func readStoredSwitches(ctx context.Context, dbPath string, opts store.Options, logger *slog.Logger) (storedSwitches, error) {
	st, _, err := topicstore.Open(ctx, dbPath, opts)
	if err != nil {
		return storedSwitches{}, err
	}
	switches := storedSwitches{recipes: map[domain.Digest]bool{}}
	err = st.Read(ctx, func(tx *store.ReadTx) error {
		instance, err := tx.InstanceSettingsRecord(ctx)
		if err != nil {
			return err
		}
		switches.executionOn, switches.executionRefused = instance.ExecutionEnabled()
		projects, err := tx.SettingsProjects(ctx)
		if err != nil {
			return err
		}
		for _, project := range projects {
			record, err := tx.ProjectSettingsRecord(ctx, project)
			if err != nil {
				return err
			}
			on, err := record.Enabled()
			if err != nil && switches.projectRefused == nil {
				switches.projectRefused = err
			}
			if on {
				switches.enabled = append(switches.enabled, project)
			}
			// Every project's recipe, enabled or not, so evidence a project
			// recorded stays readable after the operator turns it off.
			// Disabling a project is not revoking its recipe.
			recipe, ok, err := record.ApprovedRecipe()
			if err != nil {
				logger.Warn("project's approved recipe does not read; the store will not approve it",
					"project", string(project), "error", err)
				continue
			}
			if ok {
				switches.recipes[recipe] = true
			}
		}
		return nil
	})
	return switches, errors.Join(err, st.Close())
}

// storedStoreOptions is the policy a store-composed prod opens its store
// under, whether or not the driver ends up running: the unattended
// admission floor and the contained credential mode a Claude-mode store
// configures, and the recipes the settings approve beside the caller's.
// A driverless start on a root that has executed therefore still reads its
// admissions and evidence.
func storedStoreOptions(opts store.Options, recipes map[domain.Digest]bool) store.Options {
	opts.AdmissionFloors = map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeUnattended: AdmissionCapabilitySnapshot(domain.ModeUnattended),
	}
	opts.ApprovedCredentialModes = []domain.CredentialMode{domain.CredentialSubscriptionContained}
	approved := maps.Clone(opts.ApprovedRecipes)
	if approved == nil {
		approved = map[domain.Digest]bool{}
	}
	maps.Copy(approved, recipes)
	opts.ApprovedRecipes = approved
	return opts
}

// decideStoredExecution is the stored half of the start order, before prod's
// gated order: the first of these that applies stops the start at a
// driverless state with the detail its card names. It returns "" with the
// loaded settings when the gated order may run.
func decideStoredExecution(
	ctx context.Context, st *store.Store, switches storedSwitches, failure string,
) (ExecutionState, string, RunSettings, error) {
	switch {
	case failure != "":
		return ExecutionCompositionFailed, failure, RunSettings{}, nil
	case switches.executionRefused != nil:
		return ExecutionSettingsRefused, switches.executionRefused.Error(), RunSettings{}, nil
	case !switches.executionOn:
		return ExecutionNotConfigured, "", RunSettings{}, nil
	case switches.projectRefused != nil:
		return ExecutionSettingsRefused, switches.projectRefused.Error(), RunSettings{}, nil
	case len(switches.enabled) == 0:
		return ExecutionNoEnabledProject, "", RunSettings{}, nil
	case len(switches.enabled) > 1:
		names := make([]string, len(switches.enabled))
		for i, project := range switches.enabled {
			names[i] = string(project)
		}
		return ExecutionSeveralProjectsEnabled, strings.Join(names, ", "), RunSettings{}, nil
	}
	settings, err := LoadRunSettings(ctx, st, switches.enabled[0])
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "", "", RunSettings{}, err
	}
	if err != nil {
		return ExecutionSettingsRefused, err.Error(), RunSettings{}, nil
	}
	return "", "", settings, nil
}

// storedDriverConfig composes the driver configuration from the loaded
// settings and the launch, binding the digest of the stored judgment
// configuration as the deputy's preflight does for its manifest.
func storedDriverConfig(settings RunSettings, launch RunLaunch) (ClaudeDriverConfig, error) {
	cfg := NewClaudeDriverConfig(settings, launch)
	if cfg.Judgments == (JudgmentConfig{}) {
		return cfg, nil
	}
	_, digest, err := ComposeJudgments(cfg.Judgments)
	if err != nil {
		return ClaudeDriverConfig{}, fmt.Errorf("judgment configuration: %w", err)
	}
	cfg.Judgments.ExpectedDigest = digest
	return cfg, nil
}
