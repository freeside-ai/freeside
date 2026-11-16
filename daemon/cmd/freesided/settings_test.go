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
	if _, err := runSettings(t, ctx, "set", "-db", dbPath, "-project", "project-widgets",
		"-section", "allowed_paths", "-value", `["src/**"]`); err != nil {
		t.Fatal(err)
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
		if err == nil && record.Revision != 3 {
			t.Errorf("running daemon store instance revision = %d, want 3", record.Revision)
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
