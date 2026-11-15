package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
