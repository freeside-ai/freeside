package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func manualConfigFixture(t *testing.T) manualSubmissionConfig {
	t.Helper()
	var keys []domain.PolicyKey
	if err := json.Unmarshal([]byte(submissionPolicyBody("daemon/**", strings.Repeat("a", 64))), &keys); err != nil {
		t.Fatal(err)
	}
	return manualSubmissionConfig{Version: 1, Projects: []manualSubmissionProject{{
		ProjectID: "project-client", PolicyKeys: keys,
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 123},
	}}}
}

func writeManualConfig(t *testing.T, path string, cfg manualSubmissionConfig) {
	t.Helper()
	body, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestManualSubmissionConfigSnapshot(t *testing.T) {
	t.Parallel()
	cfg := manualConfigFixture(t)
	other := cfg.Projects[0]
	other.ProjectID = "project-other"
	other.PolicyKeys = slices.Clone(other.PolicyKeys)
	slices.Reverse(other.PolicyKeys)
	cfg.Projects = append(cfg.Projects, other)
	path := filepath.Join(t.TempDir(), "manual.json")
	writeManualConfig(t, path, cfg)
	lookup, err := loadManualSubmissionConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	first, ok := lookup("project-client")
	if !ok {
		t.Fatal("configured project absent")
	}
	second, ok := lookup("project-other")
	if !ok || !reflect.DeepEqual(first, second) {
		t.Fatal("policy order changed configuration")
	}
	if _, ok := lookup("unknown"); ok {
		t.Fatal("unknown project accepted")
	}
	first.PolicyKeys[0].Value = "mutated"
	if err := os.WriteFile(path, []byte("invalid replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	again, _ := lookup("project-client")
	if !reflect.DeepEqual(again, second) {
		t.Fatal("lookup or file mutation changed startup snapshot")
	}
	for _, disabled := range []string{"", filepath.Join(t.TempDir(), "empty.json")} {
		if disabled != "" {
			writeManualConfig(t, disabled, manualSubmissionConfig{Version: 1, Projects: []manualSubmissionProject{}})
		}
		lookup, err := loadManualSubmissionConfig(disabled)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := lookup("project-client"); ok {
			t.Fatal("disabled configuration accepted project")
		}
	}
}

func TestManualSubmissionConfigRejectsInvalidInputBeforeStartup(t *testing.T) {
	t.Parallel()
	valid, err := json.Marshal(manualConfigFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"empty file": {}, "invalid JSON": []byte("{"), "trailing JSON": append(slices.Clone(valid), []byte(" {}")...),
		"null document": []byte("null"), "missing version": []byte(`{"projects":[]}`),
		"unsupported version": []byte(`{"version":2,"projects":[]}`), "null projects": []byte(`{"version":1,"projects":null}`),
		"missing projects": []byte(`{"version":1}`), "null entry": []byte(`{"version":1,"projects":[null]}`),
		"empty entry":      []byte(`{"version":1,"projects":[{}]}`),
		"duplicate member": []byte(`{"version":1,"version":1,"projects":[]}`),
		"unknown field":    []byte(`{"version":1,"projects":[],"unknown":true}`),
		"invalid UTF8":     bytes.Replace(valid, []byte("project-client"), []byte{'x', 0xff}, 1),
		"nested duplicate": bytes.Replace(valid, []byte(`"bot_user_id":123`), []byte(`"bot_user_id":123,"bot_user_id":123`), 1),
		"nested unknown":   bytes.Replace(valid, []byte(`"bot_user_id":123`), []byte(`"bot_user_id":123,"unknown":true`), 1),
		"oversize":         append(slices.Clone(valid), bytes.Repeat([]byte(" "), maxSubmissionFileBytes)...),
	}
	mutations := map[string]func(*manualSubmissionConfig){
		"missing specification setting": func(c *manualSubmissionConfig) {
			c.Projects[0].PolicyKeys = append(c.Projects[0].PolicyKeys[:1], c.Projects[0].PolicyKeys[2:]...)
		},
		"invalid specification value": func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys[2].Value = "0" },
		"later invalid project": func(c *manualSubmissionConfig) {
			c.Projects = append(c.Projects, manualSubmissionProject{ProjectID: "invalid"})
		},
		"missing project id": func(c *manualSubmissionConfig) { c.Projects[0].ProjectID = "" },
		"blank project id":   func(c *manualSubmissionConfig) { c.Projects[0].ProjectID = "  " },
		"duplicate project":  func(c *manualSubmissionConfig) { c.Projects = append(c.Projects, c.Projects[0]) },
		"null policy":        func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys = nil },
		"empty policy":       func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys = []domain.PolicyKey{} },
		"empty key":          func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys[0].Key = "" },
		"duplicate policy key": func(c *manualSubmissionConfig) {
			c.Projects[0].PolicyKeys = append(c.Projects[0].PolicyKeys, c.Projects[0].PolicyKeys[0])
		},
		"missing provenance":        func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys[0].Provenance = domain.KeyProvenance{} },
		"invalid provenance source": func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys[0].Provenance.Source = "invented" },
		"missing provenance digest": func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys[0].Provenance.Digest = "" },
		"missing paths":             func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys = c.Projects[0].PolicyKeys[1:] },
		"unenforceable paths":       func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys[0].Value = "**" },
		"empty paths":               func(c *manualSubmissionConfig) { c.Projects[0].PolicyKeys[0].Value = "" },
		"missing author":            func(c *manualSubmissionConfig) { c.Projects[0].CommitAuthor = engine.ProductionCommitAuthor{} },
		"bad author slug":           func(c *manualSubmissionConfig) { c.Projects[0].CommitAuthor.AppSlug = "Bad Slug" },
		"nonpositive bot id":        func(c *manualSubmissionConfig) { c.Projects[0].CommitAuthor.BotUserID = -1 },
	}
	for name, mutate := range mutations {
		cfg := manualConfigFixture(t)
		mutate(&cfg)
		body, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cases[name] = body
	}
	cases["unreadable file"] = nil
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "manual.json")
			if name != "unreadable file" {
				if err := os.WriteFile(path, body, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			dbPath := filepath.Join(root, "daemon.db")
			h, err := run(context.Background(), nil, config{DBPath: dbPath, ListenAddr: "127.0.0.1:0", ManualSubmissionConfigPath: path})
			if err == nil {
				_ = h.Close()
				t.Fatal("invalid configuration started command listener")
			}
			if !strings.Contains(err.Error(), "manual submission config") {
				t.Fatalf("unrelated startup error: %v", err)
			}
			if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
				t.Fatalf("configuration rejection created database: %v", err)
			}
		})
	}
}

func TestManualSubmissionHTTPRestartAndReplay(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manual.json")
	dbPath := filepath.Join(root, "daemon.db")
	cfg := manualConfigFixture(t)
	writeManualConfig(t, path, cfg)
	var h *daemon
	start := func(configPath string, labels []intakeInitiator) {
		t.Helper()
		if h != nil {
			if err := h.Close(); err != nil {
				t.Fatal(err)
			}
		}
		runConfig := config{DBPath: dbPath, ListenAddr: "127.0.0.1:0", ManualSubmissionConfigPath: configPath, IntakeInitiators: labels}
		var err error
		h, err = run(context.Background(), nil, runConfig)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(runConfig.IntakeInitiators, labels) {
			t.Fatal("manual config changed label initiators")
		}
	}
	start(path, nil)
	t.Cleanup(func() {
		if h != nil {
			if err := h.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	payload, _ := json.Marshal(map[string]string{"pairing_code": h.readiness().PairingCode, "display_name": "Client"})
	response, err := http.Post(h.readiness().APIURL+"/pairing", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	var grant signet.PairingGrant
	if response.StatusCode != http.StatusCreated {
		_ = response.Body.Close()
		t.Fatal("pairing failed")
	}
	if err := json.NewDecoder(response.Body).Decode(&grant); err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	request := func(method, path string, body []byte, status int, result any) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), method, h.readiness().APIURL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+grant.DeviceToken)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != status {
			t.Fatalf("%s %s status %d, want %d", method, path, resp.StatusCode, status)
		}
		if result != nil {
			if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
				t.Fatal(err)
			}
		}
	}
	submit := func(id, project, source, name string, status int) domain.TaskSubmission {
		t.Helper()
		body, err := json.Marshal(map[string]any{
			"command_id": id, "device_id": grant.Device.Device.ID,
			"payload": map[string]string{"kind": "submit_task", "project_id": project, "source": source, "name": name},
		})
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Record domain.TaskSubmission `json:"record"`
		}
		request(http.MethodPost, "/commands", body, status, &result)
		return result.Record
	}
	counts := func(tasks, runs, commands int) {
		t.Helper()
		db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		for _, check := range []struct {
			query string
			want  int
		}{
			{"SELECT count(*) FROM tasks", tasks}, {"SELECT count(*) FROM runs", runs}, {"SELECT count(*) FROM task_submission_commands", commands},
		} {
			var count int
			if err := db.QueryRow(check.query).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != check.want {
				t.Fatalf("%s = %d, want %d", check.query, count, check.want)
			}
		}
	}
	submit("unknown", "unknown", "Unknown source", "Unknown", http.StatusNotFound)
	counts(0, 0, 0)
	first := submit("first", "project-client", "# New client task", "First name", http.StatusOK)
	if first.TaskID == "" || first.SpecificationRunID == "" {
		t.Fatal("no task or specification run")
	}
	counts(1, 1, 1)
	var bootstrap signet.BootstrapSnapshot
	request(http.MethodGet, "/sync/bootstrap", nil, http.StatusOK, &bootstrap)
	if len(bootstrap.Tasks) != 1 || bootstrap.Tasks[0].Task.ID != first.TaskID || !slices.Contains(bootstrap.Tasks[0].Task.RunIDs, first.SpecificationRunID) {
		t.Fatal("sync did not return submitted task and run")
	}
	var originalAttempt domain.ProductionAttempt
	var originalRun domain.Run
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		task, err := tx.GetTask(context.Background(), first.TaskID)
		if err != nil {
			return err
		}
		originalAttempt, err = tx.GetProductionAttempt(context.Background(), task.CampaignIDs[0], 1)
		if err != nil {
			return err
		}
		originalRun, err = tx.GetRun(context.Background(), first.SpecificationRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	reused := func(got domain.TaskSubmission) {
		t.Helper()
		if got.TaskID != first.TaskID || got.SpecificationRunID != first.SpecificationRunID || got.Name != first.Name {
			t.Fatal("replay changed task, run, or first name")
		}
	}
	reused(submit("first", "project-client", "# New client task", "Ignored", http.StatusOK))
	counts(1, 1, 1)
	reused(submit("second", "project-client", "# New client task", "Ignored", http.StatusOK))
	counts(1, 1, 2)
	cfg.Projects[0].CommitAuthor.BotUserID = 456
	cfg.Projects[0].PolicyKeys[0].Value = "scripts/**"
	writeManualConfig(t, path, cfg)
	start(path, nil)
	reused(submit("first", "project-client", "# New client task", "Ignored", http.StatusOK))
	reused(submit("third", "project-client", "# New client task", "Ignored", http.StatusOK))
	counts(1, 1, 3)
	fresh := submit("fresh", "project-client", "# Different source", "New name", http.StatusOK)
	counts(2, 2, 4)
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		old, err := tx.GetProductionAttempt(context.Background(), originalAttempt.CampaignID, 1)
		if err != nil {
			return err
		}
		oldRun, err := tx.GetRun(context.Background(), first.SpecificationRunID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(old, originalAttempt) || !reflect.DeepEqual(oldRun, originalRun) {
			return fmt.Errorf("restart changed immutable bindings")
		}
		task, err := tx.GetTask(context.Background(), fresh.TaskID)
		if err != nil {
			return err
		}
		attempt, err := tx.GetProductionAttempt(context.Background(), task.CampaignIDs[0], 1)
		if err != nil {
			return err
		}
		freshRun, err := tx.GetRun(context.Background(), fresh.SpecificationRunID)
		if err != nil {
			return err
		}
		if attempt.PublicationDigest == old.PublicationDigest || freshRun.PolicyDigest == oldRun.PolicyDigest {
			return fmt.Errorf("new task did not use changed configuration")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A separately configured label project cannot become a manual fallback.
	start("", []intakeInitiator{{ProjectID: "label-only", PolicyKeys: cfg.Projects[0].PolicyKeys, CommitAuthor: cfg.Projects[0].CommitAuthor}})
	reused(submit("first", "project-client", "# New client task", "Ignored", http.StatusOK))
	reused(submit("fourth", "project-client", "# New client task", "Ignored", http.StatusOK))
	counts(2, 2, 5)
	submit("disabled", "project-client", "# Never created", "Refused", http.StatusNotFound)
	submit("label", "label-only", "# Label only", "Refused", http.StatusNotFound)
	counts(2, 2, 5)
}
