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
		ExecutionPause: &domain.ExecutionPauseSettings{State: domain.ExecutionPauseRequested},
	}
}

func projectSettingsFixture() domain.ProjectSettings {
	return domain.ProjectSettings{
		Repository:     domain.ProjectRepositorySettings{Repo: "acme/widgets", RepositoryID: 42, BaseRef: "main"},
		AllowedPaths:   []string{"src/**"},
		ApprovedRecipe: domain.Digest("sha256:" + strings.Repeat("b", 64)),
		AgentImage:     settingsImage("agent"),
		HandBackLabel:  new("needs-rework"),
	}
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
		{domain.ProjectSectionHandBackLabel, `""`, "hand_back_label is required"},
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
