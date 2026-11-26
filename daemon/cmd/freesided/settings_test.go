package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const settingsTestContainer = "/usr/local/bin/container"

func runSettings(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runSettingsCommand(ctx, args, &stdout, &stderr)
	return stdout.String(), err
}

func settingsTestSetBy(t *testing.T) string {
	t.Helper()
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return "cli:" + account.Username
}

// exerciseSettingsSetAndShow sets an instance and a project section, shows
// both, and checks the refusals a set makes, against whichever store the
// commands reach.
func exerciseSettingsSetAndShow(t *testing.T, ctx context.Context, dbPath string) {
	t.Helper()
	setBy := settingsTestSetBy(t)
	out, err := runSettings(t, ctx, "set", "-db", dbPath, "-section", "host_tools",
		"-value", `{"container":"`+settingsTestContainer+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	var set struct {
		Revision    domain.InstanceSettingsRevision `json:"revision"`
		TakesEffect string                          `json:"takes_effect"`
	}
	if err := json.Unmarshal([]byte(out), &set); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if set.Revision.Revision != 1 || set.Revision.SetBy != setBy || set.TakesEffect != settingsTakeEffect {
		t.Fatalf("set result %+v", set)
	}
	out, err = runSettings(t, ctx, "set", "-db", dbPath, "-project", "project-widgets",
		"-section", "allowed_paths", "-value", `["src/**"]`)
	if err != nil {
		t.Fatal(err)
	}
	var pathsSet settingsSetResult
	if err := json.Unmarshal([]byte(out), &pathsSet); err != nil || pathsSet.TakesEffect != nextGateTakeEffect {
		t.Fatalf("allowed_paths set result %q (%v), want it to apply %s", out, err, nextGateTakeEffect)
	}

	out, err = runSettings(t, ctx, "show", "-db", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var instance domain.InstanceSettingsRecord
	if err := json.Unmarshal([]byte(out), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.Revision != 1 || len(instance.Sections) != 1 || instance.Sections[0].SetBy != setBy {
		t.Fatalf("instance record %+v", instance)
	}
	var hostTools domain.HostToolsSettings
	if err := json.Unmarshal(instance.Sections[0].Value, &hostTools); err != nil || hostTools.Container != settingsTestContainer {
		t.Fatalf("host_tools value %s (%v)", instance.Sections[0].Value, err)
	}
	out, err = runSettings(t, ctx, "show", "-db", dbPath, "-project", "project-widgets")
	if err != nil {
		t.Fatal(err)
	}
	var project domain.ProjectSettingsRecord
	if err := json.Unmarshal([]byte(out), &project); err != nil {
		t.Fatal(err)
	}
	if project.ProjectID != "project-widgets" || project.Revision != 1 || project.Sections[0].SetBy != setBy {
		t.Fatalf("project record %+v", project)
	}

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"relative container tool": {
			[]string{"-section", "host_tools", "-value", `{"container":"container"}`},
			"host_tools.container",
		},
		"match-everything allowlist": {[]string{
			"-project", "project-widgets", "-section", "allowed_paths",
			"-value", `["**"]`,
		}, "allowed_paths"},
		"required section cleared": {[]string{"-section", "host_tools", "-clear"}, "host_tools"},
		"unknown section":          {[]string{"-section", "listen", "-value", `"x"`}, "listen"},
		"execution pause set": {
			[]string{"-section", "execution_pause", "-value", `{"state":"requested"}`},
			"freesided execution pause",
		},
		"execution pause cleared": {[]string{"-section", "execution_pause", "-clear"}, "freesided execution resume"},
	} {
		if _, err := runSettings(t, ctx, append([]string{"set", "-db", dbPath}, tc.args...)...); err == nil ||
			!strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v, want it to name %q", name, err, tc.want)
		}
	}
	if _, err := runSettings(t, ctx, "set", "-db", dbPath, "-section", "manual_submission_config",
		"-value", `"/config/manual.json"`); err != nil {
		t.Fatal(err)
	}
	if _, err := runSettings(t, ctx, "set", "-db", dbPath, "-section", "manual_submission_config", "-clear"); err != nil {
		t.Fatalf("clearing an optional section: %v", err)
	}

	// The two execution switches set and show like any section, and apply
	// at the daemon's next start.
	out, err = runSettings(t, ctx, "set", "-db", dbPath, "-section", "execution", "-value", `{"enabled":true}`)
	if err != nil {
		t.Fatal(err)
	}
	var switchSet settingsSetResult
	if err := json.Unmarshal([]byte(out), &switchSet); err != nil || switchSet.TakesEffect != settingsTakeEffect {
		t.Fatalf("execution set result %q (%v), want it to apply %s", out, err, settingsTakeEffect)
	}
	if _, err := runSettings(t, ctx, "set", "-db", dbPath, "-project", "project-widgets",
		"-section", "enabled", "-value", `{"enabled":true}`); err != nil {
		t.Fatal(err)
	}
	if out, err = runSettings(t, ctx, "show", "-db", dbPath); err != nil {
		t.Fatal(err)
	}
	instance = domain.InstanceSettingsRecord{}
	if err := json.Unmarshal([]byte(out), &instance); err != nil {
		t.Fatal(err)
	}
	if on, err := instance.ExecutionEnabled(); err != nil || !on {
		t.Fatalf("instance record %+v: execution on %t (%v), want on", instance, on, err)
	}
	if out, err = runSettings(t, ctx, "show", "-db", dbPath, "-project", "project-widgets"); err != nil {
		t.Fatal(err)
	}
	project = domain.ProjectSettingsRecord{}
	if err := json.Unmarshal([]byte(out), &project); err != nil {
		t.Fatal(err)
	}
	if on, err := project.Enabled(); err != nil || !on {
		t.Fatalf("project record %+v: enabled %t (%v), want enabled", project, on, err)
	}
}

// TestProjectSectionTakesEffect: a set of a project section says when it
// applies, one row per answer (#3209). allowed_paths stands for the sections
// the per-task gates read (hand_back_label too), the repository adds that
// its loops follow at the next start, approved_recipe that readiness does,
// writer_class applies at the next admission, and agent_image stands for
// the sections composed at start (enabled too).
func TestProjectSectionTakesEffect(t *testing.T) {
	t.Parallel()
	for section, want := range map[domain.ProjectSettingsSection]string{
		domain.ProjectSectionAllowedPaths:   nextGateTakeEffect,
		domain.ProjectSectionRepository:     repositoryTakeEffect,
		domain.ProjectSectionApprovedRecipe: recipeTakeEffect,
		domain.ProjectSectionWriterClass:    nextAdmissionTakeEffect,
		domain.ProjectSectionAgentImage:     settingsTakeEffect,
	} {
		if got := settingsSetTakesEffect("project-widgets", string(section)); got != want {
			t.Errorf("%s takes effect %q, want %q", section, got, want)
		}
	}
}

// TestSettingsSetCordonedLaunchesAppliesAtTheNextAdmission: a set of the
// cordoned launch cap reports that it applies at the next admission, not
// the daemon's next start, and a cap below one is refused.
func TestSettingsSetCordonedLaunchesAppliesAtTheNextAdmission(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	out, err := runSettings(t, t.Context(), "set", "-db", dbPath, "-section", "cordoned_launches",
		"-value", `{"max_concurrent":2}`)
	if err != nil {
		t.Fatal(err)
	}
	var set settingsSetResult
	if err := json.Unmarshal([]byte(out), &set); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if set.TakesEffect != nextAdmissionTakeEffect {
		t.Fatalf("takes effect %q, want %q", set.TakesEffect, nextAdmissionTakeEffect)
	}
	if _, err := runSettings(t, t.Context(), "set", "-db", dbPath, "-section", "cordoned_launches",
		"-value", `{"max_concurrent":0}`); err == nil || !strings.Contains(err.Error(), "max_concurrent") {
		t.Fatalf("cap of zero: error %v, want it to name max_concurrent", err)
	}
}

func TestSettingsSetAndShowWithTheDaemonStopped(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	exerciseSettingsSetAndShow(t, t.Context(), dbPath)
}

func TestSettingsSetAndShowUseTheRunningDaemonStore(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))
	exerciseSettingsSetAndShow(t, ctx, dbPath)
	if err := h.Store.Read(t.Context(), func(tx *store.ReadTx) error {
		record, err := tx.InstanceSettingsRecord(t.Context())
		if err == nil && record.Revision != 4 {
			t.Errorf("running daemon store instance revision = %d, want 4", record.Revision)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSettingsImportYieldsTheFileSettings imports the deputy fixture, with
// and without every optional key, and loads the records back: the loader
// yields the settings the file maps to, and both compose the same Claude
// driver configuration.
func TestSettingsImportYieldsTheFileSettings(t *testing.T) {
	t.Parallel()
	for name, fixture := range map[string]func(*testing.T) map[string]any{
		"fixture":      deputySettingsFixture,
		"every option": deputySettingsFixtureWithEveryOption,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dbPath := filepath.Join(t.TempDir(), "freeside.db")
			seedTemplateStore(t, dbPath)
			settingsPath, _ := writeDeputySettings(t, fixture(t), 0o600)
			out, err := runSettings(t, t.Context(), "import", "-db", dbPath, "-settings", settingsPath,
				"-container-bin", settingsTestContainer)
			if err != nil {
				t.Fatal(err)
			}
			var result settingsImportResult
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			if result.Project != "project-widgets" || result.InstanceRevision == 0 || result.ProjectRevision == 0 ||
				!slices.Contains(result.NotImported, "listen") || !slices.Contains(result.NotImported, "writers") {
				t.Fatalf("import result %+v", result)
			}
			// Each imported project section says when an edit of it
			// applies, as a set of it would.
			if result.TakesEffect.Instance != settingsTakeEffect || !slices.Contains(result.TakesEffect.Project,
				settingsSectionEffect{Section: domain.ProjectSectionAllowedPaths, TakesEffect: nextGateTakeEffect}) {
				t.Fatalf("import takes effect %+v", result.TakesEffect)
			}

			loaded, err := loadDeputySettings(settingsPath)
			if err != nil {
				t.Fatal(err)
			}
			want := loaded.runSettings(settingsTestContainer)
			st, err := store.Open(t.Context(), dbPath, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = st.Close() })
			got, err := daemoncompose.LoadRunSettings(t.Context(), st, "project-widgets")
			if err != nil {
				t.Fatal(err)
			}
			if got.InstanceRevision != result.InstanceRevision || got.ProjectRevision != result.ProjectRevision {
				t.Fatalf("loaded revisions %d and %d, import wrote %d and %d",
					got.InstanceRevision, got.ProjectRevision, result.InstanceRevision, result.ProjectRevision)
			}
			if got.Instance.Execution != nil || got.Project.Enabled != nil {
				t.Fatalf("import set a switch: execution %+v, enabled %+v", got.Instance.Execution, got.Project.Enabled)
			}
			got.InstanceRevision, got.ProjectRevision = 0, 0
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("loaded settings\n%+v\nfile settings\n%+v", got, want)
			}
			launch := daemoncompose.RunLaunch{StateDir: "/rig/daemon", ListenPort: 7340}
			if !reflect.DeepEqual(daemoncompose.NewClaudeDriverConfig(got, launch),
				daemoncompose.NewClaudeDriverConfig(want, launch)) {
				t.Fatal("the loaded and file settings compose different driver configurations")
			}

			if _, err := runSettings(t, t.Context(), "import", "-db", dbPath, "-settings", settingsPath,
				"-container-bin", settingsTestContainer); err == nil || !strings.Contains(err.Error(), "already have revision") {
				t.Fatalf("second import error %v", err)
			}
		})
	}
}

// TestSettingsImportRefusesAProjectAlreadySet refuses an import into a
// project that has a revision, though the instance has none, and writes
// nothing.
func TestSettingsImportRefusesAProjectAlreadySet(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	if _, err := runSettings(t, t.Context(), "set", "-db", dbPath, "-project", "project-widgets",
		"-section", "allowed_paths", "-value", `["src/**"]`); err != nil {
		t.Fatal(err)
	}
	settingsPath, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	if _, err := runSettings(t, t.Context(), "import", "-db", dbPath, "-settings", settingsPath,
		"-container-bin", settingsTestContainer); err == nil || !strings.Contains(err.Error(), `project "project-widgets"`) {
		t.Fatalf("import error %v", err)
	}
	out, err := runSettings(t, t.Context(), "show", "-db", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	var instance domain.InstanceSettingsRecord
	if err := json.Unmarshal([]byte(out), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.Revision != 0 {
		t.Fatalf("refused import wrote instance revision %d", instance.Revision)
	}
}

// TestSettingsImportAcceptsAStoreOnlyEverPausedCappedOrSwitched: an
// execution pause, a cordoned launch cap, and the two execution switches
// aren't configuration the deputy's file carries, so a store that only ever
// held them still takes an import, and the import leaves the switches as
// the operator set them.
func TestSettingsImportAcceptsAStoreOnlyEverPausedCappedOrSwitched(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	st, err := store.Open(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		for _, input := range []domain.InstanceSettingsInput{
			{Section: domain.InstanceSectionExecutionPause, Value: json.RawMessage(`{"state":"requested"}`)},
			{Section: domain.InstanceSectionExecutionPause},
			{Section: domain.InstanceSectionCordonedLaunches, Value: json.RawMessage(`{"max_concurrent":2}`)},
			{Section: domain.InstanceSectionExecution, Value: json.RawMessage(`{"enabled":false}`)},
		} {
			input.SetBy = "cli:operator"
			if _, err := tx.AppendInstanceSettingsRevision(t.Context(), input, time.Now().UTC()); err != nil {
				return err
			}
		}
		_, err := tx.AppendProjectSettingsRevision(t.Context(), domain.ProjectSettingsInput{
			ProjectID: "project-widgets", Section: domain.ProjectSectionEnabled,
			Value: json.RawMessage(`{"enabled":true}`), SetBy: "cli:operator",
		}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	settingsPath, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	out, err := runSettings(t, t.Context(), "import", "-db", dbPath, "-settings", settingsPath,
		"-container-bin", settingsTestContainer)
	if err != nil {
		t.Fatalf("import into a store only ever paused, capped, or switched: %v", err)
	}
	var result settingsImportResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.InstanceRevision <= 4 || result.ProjectRevision <= 1 {
		t.Fatalf("import result %+v, want revisions after the four instance and one project revision", result)
	}
	st, err = store.Open(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	loaded, err := daemoncompose.LoadRunSettings(t.Context(), st, "project-widgets")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Instance.Execution == nil || loaded.Instance.Execution.Enabled ||
		loaded.Project.Enabled == nil || !loaded.Project.Enabled.Enabled {
		t.Fatalf("switches after import: execution %+v, enabled %+v; want them as set before it",
			loaded.Instance.Execution, loaded.Project.Enabled)
	}
}

func TestSettingsImportRefusesARelativeContainerTool(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	settingsPath, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	if _, err := runSettings(t, t.Context(), "import", "-db", dbPath, "-settings", settingsPath,
		"-container-bin", "bin/container"); err == nil || !strings.Contains(err.Error(), "-container-bin") {
		t.Fatalf("import error %v", err)
	}
}

// TestSettingsImportNeedsTheContainerToolOnPath sets PATH, so it cannot run
// in parallel.
func TestSettingsImportNeedsTheContainerToolOnPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	settingsPath, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	t.Setenv("PATH", "")
	if _, err := runSettings(t, t.Context(), "import", "-db", dbPath, "-settings", settingsPath); err == nil ||
		!strings.Contains(err.Error(), "not on PATH") {
		t.Fatalf("import error %v", err)
	}
}

// TestSettingsImportThroughTheRunningDaemon imports without -container-bin
// through a running daemon: the command resolves the tool on its PATH and
// the daemon stores that path. It sets PATH, so it cannot run in parallel.
func TestSettingsImportThroughTheRunningDaemon(t *testing.T) {
	toolDir := t.TempDir()
	container := filepath.Join(toolDir, deputyContainerBin)
	execfixture.Write(t, container, []byte("#!/bin/sh\n"), 0o700)
	t.Setenv("PATH", toolDir)
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	seedTemplateStore(t, dbPath)
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral, DBPath: dbPath, ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx := context.WithValue(t.Context(), directStoreOpenerKey{}, directStoreOpener(
		func(context.Context, string, store.Options, storeOpenMode) (*store.Store, error) {
			return nil, errors.New("unexpected direct store open")
		}))
	settingsPath, _ := writeDeputySettings(t, deputySettingsFixture(t), 0o600)
	if _, err := runSettings(t, ctx, "import", "-db", dbPath, "-settings", settingsPath); err != nil {
		t.Fatal(err)
	}
	settings, err := daemoncompose.LoadRunSettings(t.Context(), h.Store, "project-widgets")
	if err != nil {
		t.Fatal(err)
	}
	if settings.Instance.HostTools.Container != container {
		t.Fatalf("stored container tool %q, want %q", settings.Instance.HostTools.Container, container)
	}
}
