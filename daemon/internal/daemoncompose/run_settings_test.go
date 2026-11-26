package daemoncompose_test

import (
	"context"
	"database/sql"
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
	return seedRunSettingsAt(t, filepath.Join(t.TempDir(), "freeside.db"), instance, project)
}

// seedRunSettingsAt is seedRunSettings on a database at a path the caller
// knows.
func seedRunSettingsAt(
	t *testing.T, path string,
	instance map[domain.InstanceSettingsSection]string, project map[domain.ProjectSettingsSection]string,
) *store.Store {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t, path, store.Options{})
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

// TestLoadRunSettingsRefusesAStoredWriterClassNoWriterRunsOn rewrites a
// stored writer_class to the host verification class, which no append
// accepts: the loader re-checks the stored value and refuses it before a
// run composes from it.
func TestLoadRunSettingsRefusesAStoredWriterClassNoWriterRunsOn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	project := storedProjectSections()
	project[domain.ProjectSectionWriterClass] = `"fresh_vm_read_only_volume_handoff"`
	st := seedRunSettingsAt(t, path, storedInstanceSections(), project)
	settings, err := daemoncompose.LoadRunSettings(ctx, st, runSettingsProject)
	if err != nil {
		t.Fatal(err)
	}
	if got := settings.Project.WriterClass; got == nil || *got != domain.BackendFreshVMReadOnlyVolumeHandoff {
		t.Fatalf("loaded writer_class %v, want the warded class", got)
	}
	raw, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if _, err := raw.ExecContext(ctx, `UPDATE project_settings_revisions
		SET body = json_set(body, '$.value', 'credential_free_host_verification')
		WHERE project_id = ? AND section = 'writer_class'`, runSettingsProject); err != nil {
		t.Fatal(err)
	}
	if _, err := daemoncompose.LoadRunSettings(ctx, st, runSettingsProject); err == nil ||
		!strings.Contains(err.Error(), "writer_class") {
		t.Fatalf("error %v, want the stored writer_class refused", err)
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

// noImageProjectSections is storedProjectSections with the image replaced
// by a recipe binding at the approved recipe.
func noImageProjectSections() map[domain.ProjectSettingsSection]string {
	project := storedProjectSections()
	delete(project, domain.ProjectSectionAgentImage)
	project[domain.ProjectSectionRecipeBinding] = `{"repository":"acme/widgets","repository_id":42,` +
		`"commit_sha":"` + strings.Repeat("c", 40) + `","recipe_digest":"sha256:` + strings.Repeat("b", 64) + `"}`
	return project
}

// TestLoadRunSettingsReadsANoImageProject: a project with a recipe binding
// and no image loads with its binding, and the loader refuses a binding that
// names another repository or a recipe other than approved_recipe.
func TestLoadRunSettingsReadsANoImageProject(t *testing.T) {
	t.Parallel()
	st := seedRunSettings(t, storedInstanceSections(), noImageProjectSections())
	settings, err := daemoncompose.LoadRunSettings(context.Background(), st, runSettingsProject)
	if err != nil {
		t.Fatal(err)
	}
	if settings.Project.AgentImage != nil || settings.Project.RecipeBinding == nil ||
		settings.Project.RecipeBinding.RecipeDigest != settings.Project.ApprovedRecipe {
		t.Fatalf("loaded image %v and binding %+v, want the binding alone", settings.Project.AgentImage, settings.Project.RecipeBinding)
	}
	for name, tc := range map[string]struct {
		binding string
		want    string
	}{
		"another repository": {
			`{"repository":"acme/gadgets","repository_id":43,"commit_sha":"` + strings.Repeat("c", 40) +
				`","recipe_digest":"sha256:` + strings.Repeat("b", 64) + `"}`,
			"not the project's acme/widgets (42)",
		},
		"an unapproved recipe": {
			`{"repository":"acme/widgets","repository_id":42,"commit_sha":"` + strings.Repeat("c", 40) +
				`","recipe_digest":"sha256:` + strings.Repeat("e", 64) + `"}`,
			"not the approved_recipe",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			project := noImageProjectSections()
			project[domain.ProjectSectionRecipeBinding] = tc.binding
			st := seedRunSettings(t, storedInstanceSections(), project)
			settings, err := daemoncompose.LoadRunSettings(context.Background(), st, runSettingsProject)
			if err == nil || !strings.Contains(err.Error(), tc.want) || settings.ProjectID != "" {
				t.Fatalf("LoadRunSettings = %+v, %v; want a refusal containing %q", settings.ProjectID, err, tc.want)
			}
		})
	}
}

// TestSettingsGateRefusesAnAmbiguousOrWardedNoImageProject: the gate runs
// the cross-section rules a stored record can't be relied on to hold.
func TestSettingsGateRefusesAnAmbiguousOrWardedNoImageProject(t *testing.T) {
	t.Parallel()
	st := seedRunSettings(t, storedInstanceSections(), noImageProjectSections())
	loaded, err := daemoncompose.LoadRunSettings(context.Background(), st, runSettingsProject)
	if err != nil {
		t.Fatal(err)
	}
	warded := loaded.Project
	warded.WriterClass = new(domain.BackendFreshVMReadOnlyVolumeHandoff)
	both := loaded.Project
	both.AgentImage = new(domain.ImageRef(runSettingsImage("agent")))
	neither := loaded.Project
	neither.RecipeBinding = nil
	for name, tc := range map[string]struct {
		settings domain.ProjectSettings
		want     error
	}{
		"warded with no image":   {warded, domain.ErrWardedWithoutProjectImage},
		"an image and a binding": {both, domain.ErrProjectRecipeSourceAmbiguous},
		"neither":                {neither, domain.ErrSettingsSectionMissing},
	} {
		if err := daemoncompose.CheckProjectSettings(tc.settings); !errors.Is(err, tc.want) {
			t.Errorf("%s: CheckProjectSettings = %v, want %v", name, err, tc.want)
		}
	}
}
