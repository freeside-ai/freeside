package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

var settingsSetAt = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

func settingsImage(name string) domain.ImageRef {
	return domain.ImageRef("registry.local/" + name + "@sha256:" + strings.Repeat("a", 64))
}

// instanceSettingsFixture sets every instance section, the optional ones
// included, so it is the golden and a validation-positive case at once.
func instanceSettingsFixture() domain.InstanceSettings {
	return domain.InstanceSettings{
		HostTools: domain.HostToolsSettings{Container: "/usr/local/bin/container"},
		Images:    domain.InstanceImageSettings{Exporter: settingsImage("exporter"), Review: settingsImage("review")},
		SeedRoot:  "/rig/seed",
		Prompts: domain.PromptSettings{
			PromptPackage: "/prompts/impl.json", SpecificationPromptPackage: "/prompts/spec.json",
			RemediationPromptPackage: "/prompts/rem.json", VendorInstructions: "/prompts/CLAUDE.md",
		},
		AgentTree: domain.AgentTreeSettings{Checkout: "/src/freeside", Commit: strings.Repeat("d", 40)},
		Review: domain.InstanceReviewSettings{
			InputRoot: "/review", AuthMode: "subscription", AuthSnapshot: "/review/auth.json",
			Instructions: "/review/AGENTS.md",
		},
		Judgment: &domain.JudgmentSettings{
			ClaudeBin: "/opt/claude/bin/claude", ClaudeSHA256: strings.Repeat("e", 64),
			PublicationAuthorPrompt: "/prompts/author.md",
		},
		Timeouts:               domain.TimeoutSettings{WriterStop: "20m", WriterStall: "0s"},
		ManualSubmissionConfig: new("/rig/manual.json"),
		Build:                  domain.BuildSettings{Proxy: "", BaseBuildRef: "", DNS: []string{}},
		MacosVerification: &domain.MacosVerificationSettings{
			Root: "/opt/macverify", Slots: []string{"fvslot1", "fvslot2"}, XcodeBuild: "17A100", Swift: "swiftlang-6.2",
		},
		ExecutionPause:   &domain.ExecutionPauseSettings{State: domain.ExecutionPauseRequested},
		CordonedLaunches: &domain.CordonedLaunchSettings{MaxConcurrent: 2},
		ImageBuilds:      &domain.ImageBuildSettings{MaxConcurrent: 2},
		Execution:        &domain.EnabledSettings{Enabled: true},
	}
}

func projectSettingsFixture() domain.ProjectSettings {
	return domain.ProjectSettings{
		Repository:     domain.ProjectRepositorySettings{Repo: "acme/widgets", RepositoryID: 42, BaseRef: "main"},
		AllowedPaths:   []string{"src/**"},
		ApprovedRecipe: domain.Digest("sha256:" + strings.Repeat("b", 64)),
		AgentImage:     new(settingsImage("agent")),
		HandBackLabel:  new("needs-rework"),
		WriterClass:    new(domain.BackendFreshVMReadOnlyVolumeHandoff),
		Enabled:        &domain.EnabledSettings{Enabled: true},
	}
}

// recipeBindingFixture is a valid binding for projectSettingsFixture's
// repository and approved recipe.
func recipeBindingFixture() domain.RecipeBinding {
	return domain.RecipeBinding{
		Repository: "acme/widgets", RepositoryID: 42, CommitSHA: strings.Repeat("c", 40),
		RecipeDigest: domain.Digest("sha256:" + strings.Repeat("b", 64)),
	}
}

// noImageProjectSettingsFixture is a project with no project image: its
// recipe is bound by recipe_binding and its writers run cordoned.
func noImageProjectSettingsFixture() domain.ProjectSettings {
	project := projectSettingsFixture()
	project.AgentImage = nil
	project.RecipeBinding = new(recipeBindingFixture())
	project.WriterClass = new(domain.BackendCordonedHostProcess)
	return project
}

func instanceRevision(t *testing.T, revision int, section domain.InstanceSettingsSection, value json.RawMessage) domain.InstanceSettingsRevision {
	t.Helper()
	r, err := domain.NewInstanceSettingsRevision(domain.InstanceSettingsInput{
		Section: section, Value: value, SetBy: "cli:operator",
	}, revision, settingsSetAt)
	if err != nil {
		t.Fatalf("NewInstanceSettingsRevision: %v", err)
	}
	return r
}

func projectRevision(t *testing.T, revision int, section domain.ProjectSettingsSection, value json.RawMessage) domain.ProjectSettingsRevision {
	t.Helper()
	r, err := domain.NewProjectSettingsRevision(domain.ProjectSettingsInput{
		ProjectID: "project-widgets", Section: section, Value: value, SetBy: "cli:operator",
	}, revision, settingsSetAt)
	if err != nil {
		t.Fatalf("NewProjectSettingsRevision: %v", err)
	}
	return r
}

func assertSettingsGolden(t *testing.T, name string, value any) {
	t.Helper()
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, name, append(body, '\n'))
}

// TestSettingsGolden pins each settings shape; each fixture is also a
// validation-positive case.
func TestSettingsGolden(t *testing.T) {
	instance := instanceSettingsFixture()
	if err := instance.Validate(); err != nil {
		t.Fatalf("instance Validate: %v", err)
	}
	assertSettingsGolden(t, "instance_settings", instance)
	project := projectSettingsFixture()
	if err := project.Validate(); err != nil {
		t.Fatalf("project Validate: %v", err)
	}
	assertSettingsGolden(t, "project_settings", project)
	noImage := noImageProjectSettingsFixture()
	if err := noImage.Validate(); err != nil {
		t.Fatalf("no-image project Validate: %v", err)
	}
	assertSettingsGolden(t, "project_settings_recipe_binding", noImage)
	binding := recipeBindingFixture()
	if err := binding.Validate(); err != nil {
		t.Fatalf("recipe binding Validate: %v", err)
	}
	assertSettingsGolden(t, "recipe_binding", binding)

	set := instanceRevision(t, 1, domain.InstanceSectionHostTools, json.RawMessage(`{ "container": "/usr/local/bin/container" }`))
	clear := instanceRevision(t, 2, domain.InstanceSectionJudgment, nil)
	projectSet := projectRevision(t, 1, domain.ProjectSectionAllowedPaths, json.RawMessage(`["src/**", "docs/**"]`))
	projectClear := projectRevision(t, 2, domain.ProjectSectionHandBackLabel, json.RawMessage(`null`))
	for _, tc := range []struct {
		name  string
		value interface{ Validate() error }
	}{
		{"instance_settings_revision_set", set},
		{"instance_settings_revision_clear", clear},
		{"project_settings_revision_set", projectSet},
		{"project_settings_revision_clear", projectClear},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.value.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			assertSettingsGolden(t, tc.name, tc.value)
		})
	}
}

// TestSettingsRevisionRoundTrip decodes each stored body back to an equal
// revision, so the canonical value a revision stores survives the store.
func TestSettingsRevisionRoundTrip(t *testing.T) {
	set := instanceRevision(t, 1, domain.InstanceSectionHostTools, json.RawMessage(`{ "container": "/usr/local/bin/container" }`))
	if string(set.Value) != `{"container":"/usr/local/bin/container"}` {
		t.Fatalf("value %s, want the canonical encoding", set.Value)
	}
	for _, r := range []domain.InstanceSettingsRevision{set, instanceRevision(t, 2, domain.InstanceSectionJudgment, nil)} {
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var back domain.InstanceSettingsRevision
		if err := json.Unmarshal(body, &back); err != nil {
			t.Fatal(err)
		}
		if err := back.Validate(); err != nil {
			t.Fatalf("decoded Validate: %v", err)
		}
		if back.Cleared() != r.Cleared() || !back.SetAt.Equal(r.SetAt) {
			t.Fatalf("decoded %+v, want %+v", back, r)
		}
	}
}

// TestSettingsSectionValuesRoundTrip encodes every section of the fixtures
// and folds them back to equal settings.
func TestSettingsSectionValuesRoundTrip(t *testing.T) {
	instance := instanceSettingsFixture()
	var instanceRevisions []domain.InstanceSettingsRevision
	for _, section := range domain.AllInstanceSettingsSections {
		value, err := instance.SectionValue(section)
		if err != nil {
			t.Fatal(err)
		}
		instanceRevisions = append(instanceRevisions, instanceRevision(t, len(instanceRevisions)+1, section, value))
	}
	record, err := domain.NewInstanceSettingsRecord(instanceRevisions)
	if err != nil {
		t.Fatal(err)
	}
	gotInstance, err := record.Settings()
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, gotInstance, instance)

	project := projectSettingsFixture()
	var projectRevisions []domain.ProjectSettingsRevision
	for _, section := range domain.AllProjectSettingsSections {
		value, err := project.SectionValue(section)
		if err != nil {
			t.Fatal(err)
		}
		projectRevisions = append(projectRevisions, projectRevision(t, len(projectRevisions)+1, section, value))
	}
	projectRecord, err := domain.NewProjectSettingsRecord("project-widgets", projectRevisions)
	if err != nil {
		t.Fatal(err)
	}
	gotProject, err := projectRecord.Settings()
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, gotProject, project)

	noImage := noImageProjectSettingsFixture()
	var noImageRevisions []domain.ProjectSettingsRevision
	for _, section := range domain.AllProjectSettingsSections {
		value, err := noImage.SectionValue(section)
		if err != nil {
			t.Fatal(err)
		}
		noImageRevisions = append(noImageRevisions, projectRevision(t, len(noImageRevisions)+1, section, value))
	}
	noImageRecord, err := domain.NewProjectSettingsRecord("project-widgets", noImageRevisions)
	if err != nil {
		t.Fatal(err)
	}
	gotNoImage, err := noImageRecord.Settings()
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, gotNoImage, noImage)
	gotBinding, err := noImageRecord.RecipeBinding()
	if err != nil || gotBinding == nil || *gotBinding != recipeBindingFixture() {
		t.Fatalf("record RecipeBinding = %+v, %v; want the fixture", gotBinding, err)
	}
}

// TestProjectSettingsNeedExactlyOneRecipeSource refuses settings with
// neither an image nor a recipe binding, naming both sections, and settings
// with both, and refuses the warded class for a project with no image.
func TestProjectSettingsNeedExactlyOneRecipeSource(t *testing.T) {
	neither := projectSettingsFixture()
	neither.AgentImage = nil
	err := neither.Validate()
	if !errors.Is(err, domain.ErrSettingsSectionMissing) ||
		!strings.Contains(err.Error(), "agent_image") || !strings.Contains(err.Error(), "recipe_binding") {
		t.Errorf("neither: Validate = %v, want ErrSettingsSectionMissing naming agent_image and recipe_binding", err)
	}
	both := projectSettingsFixture()
	both.RecipeBinding = new(recipeBindingFixture())
	if err := both.Validate(); !errors.Is(err, domain.ErrProjectRecipeSourceAmbiguous) {
		t.Errorf("both: Validate = %v, want ErrProjectRecipeSourceAmbiguous", err)
	}
	warded := noImageProjectSettingsFixture()
	warded.WriterClass = new(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if err := warded.Validate(); !errors.Is(err, domain.ErrWardedWithoutProjectImage) ||
		!strings.Contains(err.Error(), "project image") {
		t.Errorf("warded with no image: Validate = %v, want ErrWardedWithoutProjectImage naming the image", err)
	}
	unsetClass := noImageProjectSettingsFixture()
	unsetClass.WriterClass = nil
	if err := unsetClass.Validate(); err != nil {
		t.Errorf("no image, writer_class unset: Validate = %v, want nil", err)
	}

	// The record check reads only the three sections, so it refuses each
	// pair before the record is complete, in either order of writing.
	image, err := json.Marshal(settingsImage("agent"))
	if err != nil {
		t.Fatal(err)
	}
	binding, err := json.Marshal(recipeBindingFixture())
	if err != nil {
		t.Fatal(err)
	}
	wardedValue := json.RawMessage(`"fresh_vm_read_only_volume_handoff"`)
	for name, tc := range map[string]struct {
		sections []domain.ProjectSettingsSection
		values   []json.RawMessage
		want     error
	}{
		"image then binding": {
			[]domain.ProjectSettingsSection{domain.ProjectSectionAgentImage, domain.ProjectSectionRecipeBinding},
			[]json.RawMessage{image, binding},
			domain.ErrProjectRecipeSourceAmbiguous,
		},
		"binding then warded": {
			[]domain.ProjectSettingsSection{domain.ProjectSectionRecipeBinding, domain.ProjectSectionWriterClass},
			[]json.RawMessage{binding, wardedValue},
			domain.ErrWardedWithoutProjectImage,
		},
		"warded then binding": {
			[]domain.ProjectSettingsSection{domain.ProjectSectionWriterClass, domain.ProjectSectionRecipeBinding},
			[]json.RawMessage{wardedValue, binding},
			domain.ErrWardedWithoutProjectImage,
		},
		"image cleared then binding": {
			[]domain.ProjectSettingsSection{
				domain.ProjectSectionAgentImage, domain.ProjectSectionAgentImage, domain.ProjectSectionRecipeBinding,
			},
			[]json.RawMessage{image, nil, binding},
			nil,
		},
	} {
		var revisions []domain.ProjectSettingsRevision
		for i, section := range tc.sections {
			revisions = append(revisions, projectRevision(t, i+1, section, tc.values[i]))
		}
		record, err := domain.NewProjectSettingsRecord("project-widgets", revisions)
		if err != nil {
			t.Fatal(err)
		}
		if err := record.CheckRecipeSource(); !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
			t.Errorf("%s: CheckRecipeSource = %v, want %v", name, err, tc.want)
		}
	}
}

func assertSameJSON(t *testing.T, got, want any) {
	t.Helper()
	gotBody, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	wantBody, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != string(wantBody) {
		t.Fatalf("got %s\nwant %s", gotBody, wantBody)
	}
}

// TestSettingsSectionRefusalsNameTheField sets a section to a value that
// breaks one of its rules; the refusal names the field.
func TestSettingsSectionRefusalsNameTheField(t *testing.T) {
	for _, tc := range []struct {
		section domain.InstanceSettingsSection
		value   string
		want    string
	}{
		{domain.InstanceSectionHostTools, `{"container":"container"}`, "host_tools.container must be an absolute path"},
		{domain.InstanceSectionHostTools, `{"container":"/usr/local/../bin/container"}`, "host_tools.container must be a clean path"},
		{domain.InstanceSectionHostTools, `{}`, "host_tools: container: required settings key is absent"},
		{domain.InstanceSectionBuild, `{"base_build_ref":"base:local","dns":[]}`, "build: proxy: required settings key is absent"},
		{
			domain.InstanceSectionImages, `{"exporter":"registry.local/exporter:latest","review":"` + string(settingsImage("review")) + `"}`,
			"images.exporter is not a digest-pinned image reference",
		},
		{domain.InstanceSectionSeedRoot, `"seed"`, "seed_root must be an absolute path"},
		{
			domain.InstanceSectionPrompts, `{"prompt_package":"/p","specification_prompt_package":"/p","remediation_prompt_package":"/p","vendor_instructions":"/"}`,
			"prompts.vendor_instructions must not be the root directory",
		},
		{domain.InstanceSectionAgentTree, `{"checkout":"/src/freeside","commit":""}`, "agent_tree.commit is required"},
		{domain.InstanceSectionReview, `{"input_root":"/r","auth_mode":"","auth_snapshot":"/r/a","instructions":"/r/i"}`, "review.auth_mode is required"},
		{domain.InstanceSectionJudgment, `{"claude_bin":"/c","claude_sha256":"","publication_author_prompt":"/p"}`, "judgment.claude_sha256 is required"},
		{domain.InstanceSectionTimeouts, `{"writer_stop":"-1s","writer_stall":"0s"}`, "timeouts.writer_stop: must not be negative"},
		{domain.InstanceSectionTimeouts, `{"writer_stop":"20m","writer_stall":"soon"}`, "timeouts.writer_stall: not a duration"},
		{domain.InstanceSectionManualSubmissionConfig, `"manual.json"`, "manual_submission_config must be an absolute path"},
		{domain.InstanceSectionBuild, `{"proxy":"","base_build_ref":"","dns":["1.1.1.1"]}`, "build.dns requires build.base_build_ref"},
		{domain.InstanceSectionMacosVerification, `{"root":"/m","slots":[],"xcode_build":"1","swift":"s"}`, "macos_verification.slots is empty"},
		{domain.InstanceSectionExecutionPause, `{"state":"paused"}`, `execution_pause.state "paused" is not requested or released`},
		{domain.InstanceSectionExecutionPause, `{}`, "execution_pause: state: required settings key is absent"},
		{domain.InstanceSectionCordonedLaunches, `{"max_concurrent":0}`, "cordoned_launches.max_concurrent 0 must be at least 1"},
		{domain.InstanceSectionCordonedLaunches, `{"max_concurrent":-1}`, "cordoned_launches.max_concurrent -1 must be at least 1"},
		{domain.InstanceSectionCordonedLaunches, `{}`, "cordoned_launches: max_concurrent: required settings key is absent"},
		{domain.InstanceSectionImageBuilds, `{"max_concurrent":0}`, "image_builds.max_concurrent 0 must be at least 1"},
		{domain.InstanceSectionImageBuilds, `{}`, "image_builds: max_concurrent: required settings key is absent"},
		{domain.InstanceSectionExecution, `{}`, "execution: enabled: required settings key is absent"},
		{domain.InstanceSectionExecution, `true`, "execution: "},
		{domain.InstanceSectionHostTools, `{"container":"/c","extra":1}`, "unknown"},
		{domain.InstanceSectionHostTools, `{"container":"/c","container":"/d"}`, "duplicate"},
	} {
		_, err := domain.NewInstanceSettingsRevision(domain.InstanceSettingsInput{
			Section: tc.section, Value: json.RawMessage(tc.value), SetBy: "cli:operator",
		}, 1, settingsSetAt)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: error %v, want it to contain %q", tc.section, tc.value, err, tc.want)
		}
	}
	for _, tc := range []struct {
		section domain.ProjectSettingsSection
		value   string
		want    string
	}{
		{domain.ProjectSectionRepository, `{"repo":"widgets","repository_id":42,"base_ref":"main"}`, "repository.repo is not owner/name"},
		{domain.ProjectSectionRepository, `{"repo":"acme/widgets","repository_id":0,"base_ref":"main"}`, "repository.repository_id must be positive"},
		{domain.ProjectSectionRepository, `{"repo":"acme/widgets","repository_id":42,"base_ref":""}`, "repository.base_ref is required"},
		{domain.ProjectSectionRepository, `{"repo":"acme/widgets","repository_id":42}`, "repository: base_ref: required settings key is absent"},
		{domain.ProjectSectionAllowedPaths, `[]`, "allowed_paths is empty"},
		{domain.ProjectSectionAllowedPaths, `["src/**,docs/**"]`, "allowed_paths holds an empty entry or a comma"},
		{domain.ProjectSectionApprovedRecipe, `"sha256:abc"`, "approved_recipe is not a sha256 digest"},
		{domain.ProjectSectionAgentImage, `"registry.local/agent:latest"`, "agent_image is not a digest-pinned image reference"},
		{
			domain.ProjectSectionRecipeBinding,
			`{"repository":"acme/widgets","repository_id":42,"commit_sha":"abc","recipe_digest":"sha256:` + strings.Repeat("b", 64) + `"}`,
			"recipe_binding: recipe binding commit_sha",
		},
		{
			domain.ProjectSectionRecipeBinding,
			`{"repository":"acme/widgets","repository_id":42,"commit_sha":"` + strings.Repeat("c", 40) + `"}`,
			"recipe_binding: recipe_digest: required settings key is absent",
		},
		{domain.ProjectSectionHandBackLabel, `""`, "hand_back_label is required"},
		{domain.ProjectSectionWriterClass, `"credential_free_host_verification"`, `writer_class "credential_free_host_verification" is not`},
		{domain.ProjectSectionWriterClass, `"warded"`, `writer_class "warded" is not`},
		{domain.ProjectSectionWriterClass, `""`, `writer_class "" is not`},
		{domain.ProjectSectionEnabled, `{}`, "enabled: enabled: required settings key is absent"},
		{domain.ProjectSectionEnabled, `{"enabled":"yes"}`, "enabled: "},
	} {
		_, err := domain.NewProjectSettingsRevision(domain.ProjectSettingsInput{
			ProjectID: "project-widgets", Section: tc.section, Value: json.RawMessage(tc.value), SetBy: "cli:operator",
		}, 1, settingsSetAt)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %s: error %v, want it to contain %q", tc.section, tc.value, err, tc.want)
		}
	}
}

// TestSettingsClearOnlyOptionalSections clears each section: an optional
// one clears, a required one is refused.
func TestSettingsClearOnlyOptionalSections(t *testing.T) {
	for _, section := range domain.AllInstanceSettingsSections {
		_, err := domain.NewInstanceSettingsRevision(domain.InstanceSettingsInput{Section: section, SetBy: "cli:operator"}, 1, settingsSetAt)
		if section.Optional() != (err == nil) {
			t.Errorf("clear instance %s: error %v, optional %t", section, err, section.Optional())
		}
		if err != nil && !errors.Is(err, domain.ErrRequiredSettingsSectionCleared) {
			t.Errorf("clear instance %s: error %v, want ErrRequiredSettingsSectionCleared", section, err)
		}
	}
	for _, section := range domain.AllProjectSettingsSections {
		_, err := domain.NewProjectSettingsRevision(domain.ProjectSettingsInput{
			ProjectID: "project-widgets", Section: section, SetBy: "cli:operator",
		}, 1, settingsSetAt)
		if section.Optional() != (err == nil) {
			t.Errorf("clear project %s: error %v, optional %t", section, err, section.Optional())
		}
	}
}

// TestSettingsRecordFoldsAndNamesAMissingSection folds a later value over an
// earlier one and a clear over a value, and refuses settings that lack a
// required section, naming it.
func TestSettingsRecordFoldsAndNamesAMissingSection(t *testing.T) {
	revisions := []domain.InstanceSettingsRevision{
		instanceRevision(t, 1, domain.InstanceSectionSeedRoot, json.RawMessage(`"/rig/seed"`)),
		instanceRevision(t, 2, domain.InstanceSectionManualSubmissionConfig, json.RawMessage(`"/rig/manual.json"`)),
		instanceRevision(t, 3, domain.InstanceSectionSeedRoot, json.RawMessage(`"/rig/seed-2"`)),
		instanceRevision(t, 4, domain.InstanceSectionManualSubmissionConfig, nil),
	}
	record, err := domain.NewInstanceSettingsRecord(revisions)
	if err != nil {
		t.Fatal(err)
	}
	if record.Revision != 4 || len(record.Sections) != 1 || record.Sections[0].Revision != 3 {
		t.Fatalf("record %+v, want head 4 holding seed_root at revision 3", record)
	}
	_, err = record.Settings()
	if !errors.Is(err, domain.ErrSettingsSectionMissing) || !strings.Contains(err.Error(), "host_tools") {
		t.Fatalf("Settings error %v, want host_tools named as missing", err)
	}

	if _, err := domain.NewInstanceSettingsRecord(revisions[1:]); !errors.Is(err, domain.ErrSettingsRevisionOrder) {
		t.Fatalf("fold from revision 2: error %v, want ErrSettingsRevisionOrder", err)
	}

	projectRecord, err := domain.NewProjectSettingsRecord("project-widgets", []domain.ProjectSettingsRevision{
		projectRevision(t, 1, domain.ProjectSectionAllowedPaths, json.RawMessage(`["src/**"]`)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := projectRecord.Settings(); !errors.Is(err, domain.ErrSettingsSectionMissing) ||
		!strings.Contains(err.Error(), "repository") {
		t.Fatalf("project Settings error %v, want repository named as missing", err)
	}
	if _, err := domain.NewProjectSettingsRecord("project-other", projectRecord.Sections); err == nil {
		t.Fatal("folding another project's revisions succeeded")
	}
}

func TestSettingsRevisionRefusesAMalformedSetter(t *testing.T) {
	for name, edit := range map[string]func(*domain.InstanceSettingsRevision){
		"no set_by":       func(r *domain.InstanceSettingsRevision) { r.SetBy = "" },
		"no set_at":       func(r *domain.InstanceSettingsRevision) { r.SetAt = time.Time{} },
		"set_at not UTC":  func(r *domain.InstanceSettingsRevision) { r.SetAt = r.SetAt.In(time.FixedZone("X", 3600)) },
		"zero revision":   func(r *domain.InstanceSettingsRevision) { r.Revision = 0 },
		"unknown section": func(r *domain.InstanceSettingsRevision) { r.Section = "listen" },
	} {
		r := instanceRevision(t, 1, domain.InstanceSectionSeedRoot, json.RawMessage(`"/rig/seed"`))
		edit(&r)
		if err := r.Validate(); err == nil {
			t.Errorf("%s: Validate passed", name)
		}
	}
	if _, err := domain.ParseInstanceSettingsSection("listen"); !errors.Is(err, domain.ErrUnknownSettingsSection) {
		t.Errorf("ParseInstanceSettingsSection(listen): %v", err)
	}
	if _, err := domain.ParseProjectSettingsSection("checkout"); !errors.Is(err, domain.ErrUnknownSettingsSection) {
		t.Errorf("ParseProjectSettingsSection(checkout): %v", err)
	}
}

// TestExecutionPauseReadsAloneAndFollowsTheFold pins the pause read prod's
// start makes: it answers from a record that holds no run section, follows a
// later state, and reads none after a clear.
func TestExecutionPauseReadsAloneAndFollowsTheFold(t *testing.T) {
	var revisions []domain.InstanceSettingsRevision
	for i, value := range []string{`{"state":"requested"}`, `{"state":"released"}`, ``} {
		revisions = append(revisions, instanceRevision(t, i+1, domain.InstanceSectionExecutionPause, json.RawMessage(value)))
		record, err := domain.NewInstanceSettingsRecord(revisions)
		if err != nil {
			t.Fatal(err)
		}
		pause, err := record.ExecutionPause()
		if err != nil {
			t.Fatal(err)
		}
		want := map[int]domain.ExecutionPauseState{0: domain.ExecutionPauseRequested, 1: domain.ExecutionPauseReleased}[i]
		if want == "" {
			if pause != nil {
				t.Fatalf("after the clear, pause = %+v, want none", pause)
			}
			continue
		}
		if pause == nil || pause.State != want {
			t.Fatalf("revision %d: pause = %+v, want %s", i+1, pause, want)
		}
	}
}

// TestCordonedLaunchesReadsAloneAndFollowsTheFold pins the cap read the
// admitting transaction makes: it answers from a record that holds no run
// section, follows a later cap, and reads none after a clear.
func TestCordonedLaunchesReadsAloneAndFollowsTheFold(t *testing.T) {
	var revisions []domain.InstanceSettingsRevision
	for i, value := range []string{`{"max_concurrent":1}`, `{"max_concurrent":3}`, ``} {
		revisions = append(revisions, instanceRevision(t, i+1, domain.InstanceSectionCordonedLaunches, json.RawMessage(value)))
		record, err := domain.NewInstanceSettingsRecord(revisions)
		if err != nil {
			t.Fatal(err)
		}
		launches, err := record.CordonedLaunches()
		if err != nil {
			t.Fatal(err)
		}
		want := map[int]int{0: 1, 1: 3}[i]
		if want == 0 {
			if launches != nil {
				t.Fatalf("after the clear, cap = %+v, want none", launches)
			}
			continue
		}
		if launches == nil || launches.MaxConcurrent != want {
			t.Fatalf("revision %d: cap = %+v, want %d", i+1, launches, want)
		}
	}
}

// TestExecutionSwitchesReadAloneAndDefaultOff pins the start's reads of the
// two switches: each answers from a record that holds no run section, an
// unset section and an explicit false read as off, and a clear turns a set
// switch back off.
func TestExecutionSwitchesReadAloneAndDefaultOff(t *testing.T) {
	values := []string{`{"enabled":true}`, `{"enabled":false}`, `{"enabled":true}`, ``}
	want := []bool{true, false, true, false}
	var instanceRevisions []domain.InstanceSettingsRevision
	var projectRevisions []domain.ProjectSettingsRevision
	for _, check := range []func() (bool, error){
		func() (bool, error) {
			record, err := domain.NewInstanceSettingsRecord(nil)
			if err != nil {
				return false, err
			}
			return record.ExecutionEnabled()
		},
		func() (bool, error) {
			record, err := domain.NewProjectSettingsRecord("project-widgets", nil)
			if err != nil {
				return false, err
			}
			return record.Enabled()
		},
	} {
		if on, err := check(); err != nil || on {
			t.Fatalf("unset switch: on %t, error %v; want off", on, err)
		}
	}
	for i, value := range values {
		instanceRevisions = append(instanceRevisions,
			instanceRevision(t, i+1, domain.InstanceSectionExecution, json.RawMessage(value)))
		instanceRecord, err := domain.NewInstanceSettingsRecord(instanceRevisions)
		if err != nil {
			t.Fatal(err)
		}
		if on, err := instanceRecord.ExecutionEnabled(); err != nil || on != want[i] {
			t.Fatalf("execution after revision %d: on %t, error %v; want %t", i+1, on, err, want[i])
		}
		projectRevisions = append(projectRevisions,
			projectRevision(t, i+1, domain.ProjectSectionEnabled, json.RawMessage(value)))
		projectRecord, err := domain.NewProjectSettingsRecord("project-widgets", projectRevisions)
		if err != nil {
			t.Fatal(err)
		}
		if on, err := projectRecord.Enabled(); err != nil || on != want[i] {
			t.Fatalf("enabled after revision %d: on %t, error %v; want %t", i+1, on, err, want[i])
		}
	}
}

// TestApprovedRecipeReadsAlone pins the read the store's open-time policy
// takes: the recipe answers from a record that holds no other section, and
// an unset section answers not set rather than an error.
func TestApprovedRecipeReadsAlone(t *testing.T) {
	record, err := domain.NewProjectSettingsRecord("project-widgets", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := record.ApprovedRecipe(); ok || err != nil {
		t.Fatalf("unset recipe: ok %t, error %v; want not set", ok, err)
	}
	recipe := domain.Digest("sha256:" + strings.Repeat("a", 64))
	record, err = domain.NewProjectSettingsRecord("project-widgets", []domain.ProjectSettingsRevision{
		projectRevision(t, 1, domain.ProjectSectionApprovedRecipe, json.RawMessage(`"`+string(recipe)+`"`)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok, err := record.ApprovedRecipe(); !ok || err != nil || got != recipe {
		t.Fatalf("recipe: %q, ok %t, error %v; want %q", got, ok, err, recipe)
	}
}

// TestImageBuildLimitReadsOneWhenUnset pins the limit the daemon reads at
// start: one when the section is unset, the section's value when set.
func TestImageBuildLimitReadsOneWhenUnset(t *testing.T) {
	if limit := (domain.InstanceSettings{}).ImageBuildLimit(); limit != 1 {
		t.Fatalf("unset limit = %d, want 1", limit)
	}
	if limit := instanceSettingsFixture().ImageBuildLimit(); limit != 2 {
		t.Fatalf("set limit = %d, want 2", limit)
	}
}
