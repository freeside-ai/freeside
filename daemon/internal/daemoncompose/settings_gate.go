package daemoncompose

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// CheckInstanceSettingsSection is the settings gate for one instance
// section: it checks the section's value against current policy, so an
// operator set and a load at start refuse the same values. It re-runs the
// domain section rules (among them the container tool's absolute, clean
// path), then the rules domain cannot hold: review.auth_mode is one of
// ReviewAuthModes, and the build section passes projectimage's proxy and
// rebuild-input checks. Each refusal names the field.
func CheckInstanceSettingsSection(settings domain.InstanceSettings, section domain.InstanceSettingsSection) error {
	if err := settings.ValidateSection(section); err != nil {
		return err
	}
	switch section {
	case domain.InstanceSectionReview:
		if !slices.Contains(ReviewAuthModes, ward.CodexAuthMode(settings.Review.AuthMode)) {
			return fmt.Errorf("review.auth_mode %q is not one of %v", settings.Review.AuthMode, ReviewAuthModes)
		}
	case domain.InstanceSectionBuild:
		return checkBuildSettings(settings.Build)
	case domain.InstanceSectionHostTools, domain.InstanceSectionImages, domain.InstanceSectionSeedRoot,
		domain.InstanceSectionPrompts, domain.InstanceSectionAgentTree, domain.InstanceSectionJudgment,
		domain.InstanceSectionTimeouts, domain.InstanceSectionManualSubmissionConfig,
		domain.InstanceSectionMacosVerification, domain.InstanceSectionExecutionPause,
		domain.InstanceSectionCordonedLaunches, domain.InstanceSectionExecution:
	}
	return nil
}

// checkBuildSettings holds the settings file's build rule: without a base
// ref the proxy still names preflight's build egress, so it must be a valid
// proxy or empty; with one, the inputs are the daemon's rebuild flags.
func checkBuildSettings(build domain.BuildSettings) error {
	if build.BaseBuildRef == "" {
		if err := projectimage.ValidateBuildProxy(build.Proxy); err != nil {
			return fmt.Errorf("build.proxy: %w", err)
		}
		return nil
	}
	if err := RebuildInputs(build).Validate(); err != nil {
		return fmt.Errorf("build: %w", err)
	}
	return nil
}

// RebuildInputs are the daemon's rebuild flags, which it takes only with a
// base ref.
func RebuildInputs(build domain.BuildSettings) projectimage.BuildInputs {
	if build.BaseBuildRef == "" {
		return projectimage.BuildInputs{}
	}
	return projectimage.BuildInputs{BaseBuildRef: build.BaseBuildRef, BuildProxy: build.Proxy, DNS: build.DNS}
}

// CheckProjectSettingsSection is the settings gate for one project section:
// it checks the section's value against current policy. It re-runs the
// domain section rules, then the rules domain cannot hold: the repository
// is a valid transport repository and its base ref a branch name, the
// allowed paths pass engine.ExplicitAllowedPaths (the rule the Claude
// driver refuses at start), the approved recipe is a sha256 digest, and the
// hand-back label is a label name. The writer class's only rule is the
// domain's: it names one of the two writer classes. Each refusal names the
// field. The
// repository's binding to the project's projects row is store state, which
// the loader and the store's append check.
func CheckProjectSettingsSection(settings domain.ProjectSettings, section domain.ProjectSettingsSection) error {
	if err := settings.ValidateSection(section); err != nil {
		return err
	}
	switch section {
	case domain.ProjectSectionRepository:
		if publish.ValidateRepository(settings.Repository.Repo) != nil {
			return errors.New("repository.repo is not owner/name")
		}
		if strings.HasPrefix(settings.Repository.BaseRef, "refs/") ||
			publish.ValidateBranchName(settings.Repository.BaseRef) != nil {
			return errors.New("repository.base_ref is not a branch name")
		}
	case domain.ProjectSectionAllowedPaths:
		if !engine.ExplicitAllowedPaths(settings.AllowedPaths) {
			return errors.New("allowed_paths must name explicit declared paths, such as src/**, not a match-everything or malformed pattern")
		}
	case domain.ProjectSectionApprovedRecipe:
		if !contentaddr.Valid(string(settings.ApprovedRecipe)) {
			return errors.New("approved_recipe is not a sha256 digest")
		}
	case domain.ProjectSectionHandBackLabel:
		if publish.ValidateLabelName(*settings.HandBackLabel) != nil {
			return errors.New("hand_back_label is not a label name")
		}
	case domain.ProjectSectionAgentImage, domain.ProjectSectionWriterClass, domain.ProjectSectionEnabled:
	}
	return nil
}

// CheckInstanceSettings runs the instance gate on every section that is
// set.
func CheckInstanceSettings(settings domain.InstanceSettings) error {
	for _, section := range domain.AllInstanceSettingsSections {
		value, err := settings.SectionValue(section)
		if err != nil {
			return err
		}
		if value == nil {
			continue
		}
		if err := CheckInstanceSettingsSection(settings, section); err != nil {
			return err
		}
	}
	return nil
}

// CheckProjectSettings runs the project gate on every section that is set.
func CheckProjectSettings(settings domain.ProjectSettings) error {
	for _, section := range domain.AllProjectSettingsSections {
		value, err := settings.SectionValue(section)
		if err != nil {
			return err
		}
		if value == nil {
			continue
		}
		if err := CheckProjectSettingsSection(settings, section); err != nil {
			return err
		}
	}
	return nil
}
