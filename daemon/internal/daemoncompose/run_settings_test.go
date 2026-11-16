package daemoncompose_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const runSettingsProject domain.ProjectID = "project-widgets"

var runSettingsSetAt = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func runSettingsImage(name string) string {
	return "registry.local/" + name + "@sha256:" + strings.Repeat("a", 64)
}

// storedInstanceSections and storedProjectSections are a complete, valid
// value for every required section.
func storedInstanceSections() map[domain.InstanceSettingsSection]string {
	return map[domain.InstanceSettingsSection]string{
		domain.InstanceSectionHostTools: `{"container":"/usr/local/bin/container"}`,
		domain.InstanceSectionImages: `{"exporter":"` + runSettingsImage("exporter") + `","review":"` +
			runSettingsImage("review") + `"}`,
		domain.InstanceSectionSeedRoot: `"/rig/seed"`,
		domain.InstanceSectionPrompts: `{"prompt_package":"/prompts/impl.json","specification_prompt_package":"/prompts/spec.json",` +
			`"remediation_prompt_package":"/prompts/rem.json","vendor_instructions":"/prompts/CLAUDE.md"}`,
		domain.InstanceSectionAgentTree: `{"checkout":"/src/freeside","commit":"` + strings.Repeat("d", 40) + `"}`,
		domain.InstanceSectionReview: `{"input_root":"/review","auth_mode":"subscription",` +
			`"auth_snapshot":"/review/auth.json","instructions":"/review/AGENTS.md"}`,
		domain.InstanceSectionTimeouts: `{"writer_stop":"20m","writer_stall":"0s"}`,
		domain.InstanceSectionBuild:    `{"proxy":"","base_build_ref":"","dns":[]}`,
	}
}

func storedProjectSections() map[domain.ProjectSettingsSection]string {
	return map[domain.ProjectSettingsSection]string{
		domain.ProjectSectionRepository:     `{"repo":"acme/widgets","repository_id":42,"base_ref":"main"}`,
		domain.ProjectSectionAllowedPaths:   `["src/**"]`,
		domain.ProjectSectionApprovedRecipe: `"sha256:` + strings.Repeat("b", 64) + `"`,
		domain.ProjectSectionAgentImage:     `"` + runSettingsImage("agent") + `"`,
	}
}

// seedRunSettings stores every section given, in registration order, and
// returns the store.
func seedRunSettings(
	t *testing.T, instance map[domain.InstanceSettingsSection]string, project map[domain.ProjectSettingsSection]string,
) *store.Store {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, section := range domain.AllInstanceSettingsSections {
			value, ok := instance[section]
			if !ok {
				continue
			}
			if _, err := tx.AppendInstanceSettingsRevision(ctx, domain.InstanceSettingsInput{
				Section: section, Value: json.RawMessage(value), SetBy: "cli:operator",
			}, runSettingsSetAt); err != nil {
				return err
			}
		}
		for _, section := range domain.AllProjectSettingsSections {
			value, ok := project[section]
			if !ok {
				continue
			}
			if _, err := tx.AppendProjectSettingsRevision(ctx, domain.ProjectSettingsInput{
				ProjectID: runSettingsProject, Section: section, Value: json.RawMessage(value), SetBy: "cli:operator",
			}, runSettingsSetAt); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestLoadRunSettingsReadsCompleteRecords(t *testing.T) {
	t.Parallel()
	st := seedRunSettings(t, storedInstanceSections(), storedProjectSections())
	settings, err := daemoncompose.LoadRunSettings(context.Background(), st, runSettingsProject)
	if err != nil {
		t.Fatal(err)
	}
	if settings.InstanceRevision != len(storedInstanceSections()) || settings.ProjectRevision != len(storedProjectSections()) {
		t.Fatalf("revisions %d and %d", settings.InstanceRevision, settings.ProjectRevision)
	}
	if settings.ProjectID != runSettingsProject || settings.Project.Repository.RepositoryID != 42 ||
		settings.Instance.HostTools.Container != "/usr/local/bin/container" || settings.WriterStopTimeout != 20*time.Minute {
		t.Fatalf("settings %+v", settings)
	}
}

// TestNewClaudeDriverConfigCarriesTheSettingsRevisions: a composition from
// stored settings names their revisions, and one from a file names none.
func TestNewClaudeDriverConfigCarriesTheSettingsRevisions(t *testing.T) {
	t.Parallel()
	st := seedRunSettings(t, storedInstanceSections(), storedProjectSections())
	settings, err := daemoncompose.LoadRunSettings(context.Background(), st, runSettingsProject)
	if err != nil {
		t.Fatal(err)
	}
	cfg := daemoncompose.NewClaudeDriverConfig(settings, daemoncompose.RunLaunch{})
	want := domain.SettingsRevisions{Instance: settings.InstanceRevision, Project: settings.ProjectRevision}
	if cfg.SettingsRevisions == nil || *cfg.SettingsRevisions != want {
		t.Fatalf("composed revisions = %v, want %+v", cfg.SettingsRevisions, want)
	}
	settings.InstanceRevision, settings.ProjectRevision = 0, 0
	if file := daemoncompose.NewClaudeDriverConfig(settings, daemoncompose.RunLaunch{}); file.SettingsRevisions != nil {
		t.Fatalf("file settings composed revisions %+v", *file.SettingsRevisions)
	}
}

// TestLoadRunSettingsRefusesWhatCurrentPolicyRefuses stores values that
// domain validation accepts and the settings gate refuses; the loader
// returns no settings.
func TestLoadRunSettingsRefusesWhatCurrentPolicyRefuses(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		instance map[domain.InstanceSettingsSection]string
		project  map[domain.ProjectSettingsSection]string
		want     string
	}{
		"match-everything allowlist": {project: map[domain.ProjectSettingsSection]string{
			domain.ProjectSectionAllowedPaths: `["**"]`,
		}, want: "allowed_paths must name explicit declared paths"},
		"refs base ref": {project: map[domain.ProjectSettingsSection]string{
			domain.ProjectSectionRepository: `{"repo":"acme/widgets","repository_id":42,"base_ref":"refs/heads/main"}`,
		}, want: "repository.base_ref is not a branch name"},
		"padded label": {project: map[domain.ProjectSettingsSection]string{
			domain.ProjectSectionHandBackLabel: `" needs-rework"`,
		}, want: "hand_back_label is not a label name"},
		"unknown review auth mode": {instance: map[domain.InstanceSettingsSection]string{
			domain.InstanceSectionReview: `{"input_root":"/review","auth_mode":"password",` +
				`"auth_snapshot":"/review/auth.json","instructions":"/review/AGENTS.md"}`,
		}, want: "review.auth_mode"},
		"bad rebuild ref": {instance: map[domain.InstanceSettingsSection]string{
			domain.InstanceSectionBuild: `{"proxy":"","base_build_ref":"not a ref","dns":[]}`,
		}, want: "build:"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			instance, project := storedInstanceSections(), storedProjectSections()
			for section, value := range tc.instance {
				instance[section] = value
			}
			for section, value := range tc.project {
				project[section] = value
			}
			st := seedRunSettings(t, instance, project)
			settings, err := daemoncompose.LoadRunSettings(context.Background(), st, runSettingsProject)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want it to contain %q", err, tc.want)
			}
			if settings.ProjectID != "" {
				t.Fatalf("loader returned settings %+v with its refusal", settings)
			}
		})
	}
}

// TestLoadRunSettingsRefusesARepositoryTheProjectIsNotBoundTo stores a
// repository section, then binds the project to another repository: the
// binding is current state the stored section no longer agrees with.
func TestLoadRunSettingsRefusesARepositoryTheProjectIsNotBoundTo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := seedRunSettings(t, storedInstanceSections(), storedProjectSections())
	bound, err := domain.NewProject(runSettingsProject, "acme/gadgets", 43)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.RegisterProject(ctx, bound) }); err != nil {
		t.Fatal(err)
	}
	if _, err := daemoncompose.LoadRunSettings(ctx, st, runSettingsProject); !errors.Is(err, store.ErrProjectRepositoryMismatch) {
		t.Fatalf("error %v, want ErrProjectRepositoryMismatch", err)
	}
}

func TestLoadRunSettingsNamesAMissingSection(t *testing.T) {
	t.Parallel()
	instance := storedInstanceSections()
	delete(instance, domain.InstanceSectionPrompts)
	st := seedRunSettings(t, instance, storedProjectSections())
	_, err := daemoncompose.LoadRunSettings(context.Background(), st, runSettingsProject)
	if !errors.Is(err, domain.ErrSettingsSectionMissing) || !strings.Contains(err.Error(), "prompts") {
		t.Fatalf("error %v, want prompts named as missing", err)
	}
	project := storedProjectSections()
	delete(project, domain.ProjectSectionApprovedRecipe)
	st = seedRunSettings(t, storedInstanceSections(), project)
	_, err = daemoncompose.LoadRunSettings(context.Background(), st, runSettingsProject)
	if !errors.Is(err, domain.ErrSettingsSectionMissing) || !strings.Contains(err.Error(), "approved_recipe") {
		t.Fatalf("error %v, want approved_recipe named as missing", err)
	}
}

// TestSettingsGateChecksOneSection runs the gate on one set section, as an
// operator set does, without the rest of the settings.
func TestSettingsGateChecksOneSection(t *testing.T) {
	t.Parallel()
	settings, _, err := domain.DecodeProjectSection(domain.ProjectSectionAllowedPaths, json.RawMessage(`["**"]`))
	if err != nil {
		t.Fatal(err)
	}
	if err := daemoncompose.CheckProjectSettingsSection(settings, domain.ProjectSectionAllowedPaths); err == nil {
		t.Fatal("match-everything allowlist passed the gate")
	}
	instance, _, err := domain.DecodeInstanceSection(domain.InstanceSectionHostTools,
		json.RawMessage(`{"container":"/usr/local/bin/container"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := daemoncompose.CheckInstanceSettingsSection(instance, domain.InstanceSectionHostTools); err != nil {
		t.Fatal(err)
	}
}
