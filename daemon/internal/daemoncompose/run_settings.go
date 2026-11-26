package daemoncompose

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/claudeinference"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// RunSettings are the settings one project's runs take from configuration:
// the instance settings every project shares and the project's own. The
// deputy maps its settings file to them and the installed daemon loads them
// from the store, so both compose the Claude driver through one function
// (NewClaudeDriverConfig). The revisions are 0 for settings from a file.
type RunSettings struct {
	Instance         domain.InstanceSettings
	InstanceRevision int
	ProjectID        domain.ProjectID
	Project          domain.ProjectSettings
	ProjectRevision  int
	// WriterStopTimeout and WriterStallInterval are the instance timeouts
	// parsed, by whoever built the settings after checking them.
	WriterStopTimeout   time.Duration
	WriterStallInterval time.Duration
}

// LoadRunSettings is the gate between the stored settings records and a run.
// The records hold authority written under the policy of their day (the
// approved recipe, the allowed paths, the publication repository), so the
// loader trusts none of it as stored. In one read it folds the instance's
// and the project's revisions, refuses a scope that lacks a required
// section and names the section, and re-checks every section against
// current policy: the domain section rules (the container tool's absolute,
// clean path; the recipe's digest shape), the settings gate
// (engine.ExplicitAllowedPaths on the allowed paths, the transport's
// repository, branch, and label forms, the review auth modes, the build
// inputs), and the repository against the project's projects binding. On any
// failure it returns no settings.
func LoadRunSettings(ctx context.Context, st *store.Store, project domain.ProjectID) (RunSettings, error) {
	var (
		instanceRecord domain.InstanceSettingsRecord
		projectRecord  domain.ProjectSettingsRecord
		settings       RunSettings
	)
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if instanceRecord, err = tx.InstanceSettingsRecord(ctx); err != nil {
			return err
		}
		if projectRecord, err = tx.ProjectSettingsRecord(ctx, project); err != nil {
			return err
		}
		if settings, err = checkRunSettings(instanceRecord, projectRecord); err != nil {
			return err
		}
		return tx.CheckProjectRepositoryBinding(ctx, project, settings.Project.Repository.RepositoryID)
	})
	if err != nil {
		return RunSettings{}, fmt.Errorf("load run settings: %w", err)
	}
	return settings, nil
}

// revisions is nil for settings from a file, whose revisions are both 0.
// Any other pair is returned as it is, so a half-set one reaches
// ClaudeDriverConfig.Validate and is refused there.
func (s RunSettings) revisions() *domain.SettingsRevisions {
	if s.InstanceRevision == 0 && s.ProjectRevision == 0 {
		return nil
	}
	return &domain.SettingsRevisions{Instance: s.InstanceRevision, Project: s.ProjectRevision}
}

func checkRunSettings(instanceRecord domain.InstanceSettingsRecord, projectRecord domain.ProjectSettingsRecord) (RunSettings, error) {
	instance, err := instanceRecord.Settings()
	if err != nil {
		return RunSettings{}, err
	}
	if err := CheckInstanceSettings(instance); err != nil {
		return RunSettings{}, fmt.Errorf("instance settings: %w", err)
	}
	project, err := checkProjectRecord(projectRecord)
	if err != nil {
		return RunSettings{}, err
	}
	stop, stall, err := instance.Timeouts.Durations()
	if err != nil {
		return RunSettings{}, fmt.Errorf("instance settings: %w", err)
	}
	return RunSettings{
		Instance: instance, InstanceRevision: instanceRecord.Revision,
		ProjectID: projectRecord.ProjectID, Project: project, ProjectRevision: projectRecord.Revision,
		WriterStopTimeout: stop, WriterStallInterval: stall,
	}, nil
}

// checkProjectRecord is the project half of the settings gate on a folded
// record: it refuses a record that lacks a required section, naming it, and
// re-checks every section against current policy (CheckProjectSettings).
// The run settings loader and the per-task project authority
// (StoredProjectAuthority) share it, so a start and a gate refuse the same
// records.
func checkProjectRecord(record domain.ProjectSettingsRecord) (domain.ProjectSettings, error) {
	project, err := record.Settings()
	if err != nil {
		return domain.ProjectSettings{}, err
	}
	if err := CheckProjectSettings(project); err != nil {
		return domain.ProjectSettings{}, fmt.Errorf("project %q settings: %w", record.ProjectID, err)
	}
	return project, nil
}

// RunLaunch is what composing the Claude driver needs beyond the stored
// settings: the inputs of one launch (the state paths, the rig token, the
// App directories, the listen port, the judgment digest the launch's
// manifest names) and the launcher's fixed bindings.
type RunLaunch struct {
	StateDir              string
	RigTokenFile          string
	AppStateDir           string
	AppCredentialsDir     string
	ListenPort            int
	JudgmentDigest        string
	ProviderEndpoints     []string
	ReviewWorkspaceSizeMB int64
	RunConformance        bool
	OperatingMode         domain.OperatingMode
}

// NewClaudeDriverConfig composes the Claude driver's configuration from run
// settings and one launch's inputs. It checks nothing itself:
// ClaudeDriverConfig.Validate does at start, and refuses a half-set pair of
// revisions.
func NewClaudeDriverConfig(settings RunSettings, launch RunLaunch) ClaudeDriverConfig {
	instance, project := settings.Instance, settings.Project
	var handBackLabel string
	if project.HandBackLabel != nil {
		handBackLabel = *project.HandBackLabel
	}
	// A project with no image leaves AgentImage empty, which
	// ClaudeDriverConfig.Validate refuses; the installed daemon refuses one
	// earlier, in storedDriverConfig, with a host check.
	var agentImage domain.ImageRef
	if project.AgentImage != nil {
		agentImage = *project.AgentImage
	}
	return ClaudeDriverConfig{
		AgentImage: agentImage, ExporterImage: string(instance.Images.Exporter),
		ContainerBin: instance.HostTools.Container, SeedRoot: instance.SeedRoot,
		WriterStopTimeout: settings.WriterStopTimeout, WriterStallInterval: settings.WriterStallInterval,
		Rebuild: RebuildInputs(instance.Build), ImageBuildLimit: instance.ImageBuildLimit(),
		StateDir: launch.StateDir, RigTokenFile: launch.RigTokenFile,
		Judgments:                      newJudgmentConfig(instance.Judgment, launch.JudgmentDigest),
		ProviderEndpoints:              launch.ProviderEndpoints,
		PromptPackageFile:              instance.Prompts.PromptPackage,
		SpecificationPromptPackageFile: instance.Prompts.SpecificationPromptPackage,
		RemediationPromptPackageFile:   instance.Prompts.RemediationPromptPackage,
		VendorInstructions:             instance.Prompts.VendorInstructions,
		Repo:                           project.Repository.Repo, RepositoryID: project.Repository.RepositoryID,
		BaseRef:           project.Repository.BaseRef,
		HandBackLabel:     handBackLabel,
		AgentTreeCheckout: instance.AgentTree.Checkout, AgentTreeCommit: instance.AgentTree.Commit,
		AllowedPaths:   project.AllowedPaths,
		RunConformance: launch.RunConformance,
		StateRoot:      launch.AppStateDir, CredentialsDir: launch.AppCredentialsDir,
		OperatingMode: launch.OperatingMode,
		ReviewImage:   string(instance.Images.Review), ReviewInputRoot: instance.Review.InputRoot,
		ReviewAuthMode:     ward.CodexAuthMode(instance.Review.AuthMode),
		ReviewAuthSnapshot: instance.Review.AuthSnapshot, ReviewInstructions: instance.Review.Instructions,
		ReviewWorkspaceSizeMB: launch.ReviewWorkspaceSizeMB,
		HostVerification:      NewMacverifyExecutorConfig(instance.MacosVerification, launch.ListenPort),
		SettingsRevisions:     settings.revisions(),
		ProjectID:             settings.ProjectID,
		ApprovedRecipe:        project.ApprovedRecipe,
		WriterClass:           project.WriterClass,
	}
}

// newJudgmentConfig maps the judgment section; an absent section turns
// subscription judgments off.
func newJudgmentConfig(judgment *domain.JudgmentSettings, expectedDigest string) JudgmentConfig {
	if judgment == nil {
		return JudgmentConfig{}
	}
	return JudgmentConfig{
		CLI:                     claudeinference.Config{Binary: judgment.ClaudeBin, SHA256: judgment.ClaudeSHA256},
		PublicationAuthorPrompt: judgment.PublicationAuthorPrompt,
		ExpectedDigest:          expectedDigest,
	}
}

// NewMacverifyExecutorConfig maps the macOS verification section and the
// daemon's listen port to the executor configuration. An absent section maps
// to the zero value, whose empty Root configures no executor, as an empty
// -macverify-root does; an absent port base is macverify.DefaultPortBase.
func NewMacverifyExecutorConfig(section *domain.MacosVerificationSettings, listenPort int) MacverifyExecutorConfig {
	if section == nil {
		return MacverifyExecutorConfig{}
	}
	portBase := section.PortBase
	if portBase == 0 {
		portBase = macverify.DefaultPortBase
	}
	return MacverifyExecutorConfig{
		Root: section.Root, Slots: slices.Clone(section.Slots), XcodeBuild: section.XcodeBuild, Swift: section.Swift,
		PortBase: portBase, ListenPort: listenPort,
	}
}
