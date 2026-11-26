package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var settingsSetAt = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func openSettingsStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.Open(t, filepath.Join(t.TempDir(), "settings.db"), store.Options{})
}

func appendInstanceSettings(t *testing.T, s *store.Store, section domain.InstanceSettingsSection, value string) (domain.InstanceSettingsRevision, error) {
	t.Helper()
	ctx := context.Background()
	var revision domain.InstanceSettingsRevision
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		revision, err = tx.AppendInstanceSettingsRevision(ctx, domain.InstanceSettingsInput{
			Section: section, Value: json.RawMessage(value), SetBy: "cli:operator",
		}, settingsSetAt)
		return err
	})
	return revision, err
}

func appendProjectSettings(
	t *testing.T, s *store.Store, project domain.ProjectID, section domain.ProjectSettingsSection, value string,
) (domain.ProjectSettingsRevision, error) {
	t.Helper()
	ctx := context.Background()
	var revision domain.ProjectSettingsRevision
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		revision, err = tx.AppendProjectSettingsRevision(ctx, domain.ProjectSettingsInput{
			ProjectID: project, Section: section, Value: json.RawMessage(value), SetBy: "cli:operator",
		}, settingsSetAt)
		return err
	})
	return revision, err
}

// TestInstanceSettingsAppendLeavesEarlierRevisions sets one section twice:
// the second set is a new revision, and the first reads back unchanged.
func TestInstanceSettingsAppendLeavesEarlierRevisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openSettingsStore(t)
	first, err := appendInstanceSettings(t, s, domain.InstanceSectionSeedRoot, `"/rig/seed"`)
	if err != nil {
		t.Fatal(err)
	}
	second, err := appendInstanceSettings(t, s, domain.InstanceSectionSeedRoot, `"/rig/seed-2"`)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || second.Revision != 2 {
		t.Fatalf("revisions %d and %d, want 1 and 2", first.Revision, second.Revision)
	}
	var (
		revisions []domain.InstanceSettingsRevision
		record    domain.InstanceSettingsRecord
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if revisions, err = tx.InstanceSettingsRevisions(ctx); err != nil {
			return err
		}
		record, err = tx.InstanceSettingsRecord(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 2 || !reflect.DeepEqual(revisions[0], first) {
		t.Fatalf("revisions %+v, want the first unchanged as %+v", revisions, first)
	}
	if record.Revision != 2 || len(record.Sections) != 1 || string(record.Sections[0].Value) != `"/rig/seed-2"` {
		t.Fatalf("record %+v, want seed_root at revision 2", record)
	}
}

// TestSettingsAppendRefusesABrokenValue keeps a refused set out of the
// record: the refusal names the field, and no revision is numbered.
func TestSettingsAppendRefusesABrokenValue(t *testing.T) {
	t.Parallel()
	s := openSettingsStore(t)
	if _, err := appendInstanceSettings(t, s, domain.InstanceSectionHostTools, `{"container":"container"}`); err == nil {
		t.Fatal("relative container path accepted")
	}
	if _, err := appendInstanceSettings(t, s, domain.InstanceSectionImages, `null`); !errors.Is(err, domain.ErrRequiredSettingsSectionCleared) {
		t.Fatalf("clearing images: %v, want ErrRequiredSettingsSectionCleared", err)
	}
	r, err := appendInstanceSettings(t, s, domain.InstanceSectionHostTools, `{"container":"/usr/local/bin/container"}`)
	if err != nil {
		t.Fatal(err)
	}
	if r.Revision != 1 {
		t.Fatalf("revision %d after two refusals, want 1", r.Revision)
	}
}

// TestSettingsProjectsListsEachProjectOnce: the start's read of which
// projects hold settings names each project once, in id order, however many
// revisions it has, and is empty on a store that holds none.
func TestSettingsProjectsListsEachProjectOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openSettingsStore(t)
	read := func() []domain.ProjectID {
		t.Helper()
		var projects []domain.ProjectID
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			projects, err = tx.SettingsProjects(ctx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return projects
	}
	if projects := read(); len(projects) != 0 {
		t.Fatalf("empty store lists %v, want none", projects)
	}
	for _, project := range []domain.ProjectID{"project-b", "project-a", "project-b"} {
		if _, err := appendProjectSettings(t, s, project, domain.ProjectSectionEnabled, `{"enabled":true}`); err != nil {
			t.Fatal(err)
		}
	}
	if projects := read(); !slices.Equal(projects, []domain.ProjectID{"project-a", "project-b"}) {
		t.Fatalf("settings projects %v, want [project-a project-b]", projects)
	}
}

// TestProjectSettingsNumberPerProjectAndHoldTheBinding numbers each
// project's revisions from 1 and refuses a repository the project's
// projects row does not bind it to.
func TestProjectSettingsNumberPerProjectAndHoldTheBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openSettingsStore(t)
	repository := `{"repo":"acme/widgets","repository_id":42,"base_ref":"main"}`
	for _, project := range []domain.ProjectID{"project-a", "project-b"} {
		r, err := appendProjectSettings(t, s, project, domain.ProjectSectionRepository, repository)
		if err != nil {
			t.Fatal(err)
		}
		if r.Revision != 1 {
			t.Fatalf("project %s revision %d, want 1", project, r.Revision)
		}
	}
	bound, err := domain.NewProject("project-a", "acme/widgets", 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.RegisterProject(ctx, bound) }); err != nil {
		t.Fatal(err)
	}
	if _, err := appendProjectSettings(t, s, "project-a", domain.ProjectSectionRepository,
		`{"repo":"acme/gadgets","repository_id":43,"base_ref":"main"}`); !errors.Is(err, store.ErrProjectRepositoryMismatch) {
		t.Fatalf("rebinding project-a: %v, want ErrProjectRepositoryMismatch", err)
	}
	if _, err := appendProjectSettings(t, s, "project-a", domain.ProjectSectionRepository,
		`{"repo":"acme/widgets","repository_id":42,"base_ref":"release"}`); err != nil {
		t.Fatalf("same repository, new base ref: %v", err)
	}
	var record domain.ProjectSettingsRecord
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.ProjectSettingsRecord(ctx, "project-a")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if record.Revision != 2 || len(record.Sections) != 1 {
		t.Fatalf("project-a record %+v, want head 2 with one section", record)
	}
	var registered bool
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetProject(ctx, "project-b")
		registered = err == nil
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if registered {
		t.Fatal("setting project-b's settings created its projects row")
	}
}

// TestSettingsRecordsRoundTripEverySection sets every section of both
// scopes and reads back equal settings, so each enum member survives the
// store.
func TestSettingsRecordsRoundTripEverySection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openSettingsStore(t)
	instance := map[domain.InstanceSettingsSection]string{
		domain.InstanceSectionHostTools: `{"container":"/usr/local/bin/container"}`,
		domain.InstanceSectionImages:    `{"exporter":"r/e@sha256:` + hex64 + `","review":"r/r@sha256:` + hex64 + `"}`,
		domain.InstanceSectionSeedRoot:  `"/rig/seed"`,
		domain.InstanceSectionPrompts: `{"prompt_package":"/p/i","specification_prompt_package":"/p/s",` +
			`"remediation_prompt_package":"/p/r","vendor_instructions":"/p/v"}`,
		domain.InstanceSectionAgentTree: `{"checkout":"/src/freeside","commit":"abc"}`,
		domain.InstanceSectionReview: `{"input_root":"/r","auth_mode":"subscription","auth_snapshot":"/r/a",` +
			`"instructions":"/r/i"}`,
		domain.InstanceSectionJudgment:               `{"claude_bin":"/c","claude_sha256":"x","publication_author_prompt":"/p"}`,
		domain.InstanceSectionTimeouts:               `{"writer_stop":"20m","writer_stall":"0s"}`,
		domain.InstanceSectionManualSubmissionConfig: `"/rig/manual.json"`,
		domain.InstanceSectionBuild:                  `{"proxy":"","base_build_ref":"","dns":[]}`,
		domain.InstanceSectionMacosVerification:      `{"root":"/m","slots":["a"],"xcode_build":"1","swift":"s"}`,
		domain.InstanceSectionCordonedLaunches:       `{"max_concurrent":2}`,
		domain.InstanceSectionImageBuilds:            `{"max_concurrent":2}`,
		domain.InstanceSectionExecution:              `{"enabled":true}`,
	}
	for _, section := range domain.AllInstanceSettingsSections {
		if _, err := appendInstanceSettings(t, s, section, instance[section]); err != nil {
			t.Fatalf("%s: %v", section, err)
		}
	}
	project := map[domain.ProjectSettingsSection]string{
		domain.ProjectSectionRepository:     `{"repo":"acme/widgets","repository_id":42,"base_ref":"main"}`,
		domain.ProjectSectionAllowedPaths:   `["src/**"]`,
		domain.ProjectSectionApprovedRecipe: `"sha256:` + hex64 + `"`,
		domain.ProjectSectionAgentImage:     `"r/a@sha256:` + hex64 + `"`,
		domain.ProjectSectionHandBackLabel:  `"needs-rework"`,
		domain.ProjectSectionWriterClass:    `"cordoned_host_process"`,
		domain.ProjectSectionEnabled:        `{"enabled":true}`,
	}
	for _, section := range domain.AllProjectSettingsSections {
		if _, err := appendProjectSettings(t, s, "project-a", section, project[section]); err != nil {
			t.Fatalf("%s: %v", section, err)
		}
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		instanceRecord, err := tx.InstanceSettingsRecord(ctx)
		if err != nil {
			return err
		}
		if _, err := instanceRecord.Settings(); err != nil {
			return err
		}
		for _, r := range instanceRecord.Sections {
			if string(r.Value) != instance[r.Section] {
				t.Errorf("%s reads back %s, want %s", r.Section, r.Value, instance[r.Section])
			}
		}
		projectRecord, err := tx.ProjectSettingsRecord(ctx, "project-a")
		if err != nil {
			return err
		}
		_, err = projectRecord.Settings()
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

const hex64 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestProjectSettingsAppendRefusesAConflictingRecipeSource refuses an append
// that would leave a project holding both an image and a recipe binding, or
// a binding and the warded writer class, in either order of writing, and a
// binding for another repository than the project's binding.
func TestProjectSettingsAppendRefusesAConflictingRecipeSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	image := `"r/a@sha256:` + hex64 + `"`
	binding := `{"repository":"acme/widgets","repository_id":42,"commit_sha":"` + hex64[:40] +
		`","recipe_digest":"sha256:` + hex64 + `"}`
	warded := `"fresh_vm_read_only_volume_handoff"`
	for name, tc := range map[string]struct {
		first, second domain.ProjectSettingsSection
		firstValue    string
		secondValue   string
		want          error
	}{
		"binding over an image": {
			domain.ProjectSectionAgentImage, domain.ProjectSectionRecipeBinding, image, binding,
			domain.ErrProjectRecipeSourceAmbiguous,
		},
		"image over a binding": {
			domain.ProjectSectionRecipeBinding, domain.ProjectSectionAgentImage, binding, image,
			domain.ErrProjectRecipeSourceAmbiguous,
		},
		"warded over a binding": {
			domain.ProjectSectionRecipeBinding, domain.ProjectSectionWriterClass, binding, warded,
			domain.ErrWardedWithoutProjectImage,
		},
		"binding over warded": {
			domain.ProjectSectionWriterClass, domain.ProjectSectionRecipeBinding, warded, binding,
			domain.ErrWardedWithoutProjectImage,
		},
	} {
		s := openSettingsStore(t)
		if _, err := appendProjectSettings(t, s, "project-a", tc.first, tc.firstValue); err != nil {
			t.Fatalf("%s: first append: %v", name, err)
		}
		if _, err := appendProjectSettings(t, s, "project-a", tc.second, tc.secondValue); !errors.Is(err, tc.want) {
			t.Errorf("%s: second append = %v, want %v", name, err, tc.want)
		}
	}

	// Clearing the image first lets the binding replace it.
	s := openSettingsStore(t)
	for _, step := range []struct {
		section domain.ProjectSettingsSection
		value   string
	}{
		{domain.ProjectSectionAgentImage, image},
		{domain.ProjectSectionAgentImage, "null"},
		{domain.ProjectSectionRecipeBinding, binding},
	} {
		if _, err := appendProjectSettings(t, s, "project-a", step.section, step.value); err != nil {
			t.Fatalf("%s %s: %v", step.section, step.value, err)
		}
	}

	bound, err := domain.NewProject("project-b", "acme/widgets", 43)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.RegisterProject(ctx, bound) }); err != nil {
		t.Fatal(err)
	}
	if _, err := appendProjectSettings(t, s, "project-b", domain.ProjectSectionRecipeBinding, binding); !errors.Is(err, store.ErrProjectRepositoryMismatch) {
		t.Fatalf("binding another repository: %v, want ErrProjectRepositoryMismatch", err)
	}
}
