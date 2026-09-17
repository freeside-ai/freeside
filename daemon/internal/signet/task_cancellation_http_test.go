package signet_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestStopTaskHTTPAuthenticationAndPayloads(t *testing.T) {
	svc, s := newSubmitTaskService(t, nil, domain.DeviceActive)
	id := seedStopTask(t, s)
	state, _ := s.ServerState(t.Context())
	handler := signet.NewHTTPHandler(svc, testAuthorizer)
	base := map[string]any{"command_id": "stop-http", "device_id": "device-1", "expected_entity_version": state.Revision, "payload": map[string]any{"kind": "stop_task", "task_id": id, "project_id": "project-1", "expected_sync_epoch": state.SyncEpoch}}
	for name, change := range map[string]func(map[string]any){
		"foreign device":      func(b map[string]any) { b["device_id"] = "device-2" },
		"missing version":     func(b map[string]any) { delete(b, "expected_entity_version") },
		"decision bindings":   func(b map[string]any) { b["expected_bindings"] = map[string]string{} },
		"client confirmation": func(b map[string]any) { b["payload"].(map[string]any)["state"] = "confirmed" },
		"client run":          func(b map[string]any) { b["payload"].(map[string]any)["run_id"] = "run-1" },
		"missing task":        func(b map[string]any) { b["payload"].(map[string]any)["task_id"] = "missing" },
		"foreign project":     func(b map[string]any) { b["payload"].(map[string]any)["project_id"] = "foreign" },
		"empty epoch":         func(b map[string]any) { b["payload"].(map[string]any)["expected_sync_epoch"] = "" },
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(base)
			var b map[string]any
			if err := json.Unmarshal(raw, &b); err != nil {
				t.Fatal(err)
			}
			change(b)
			raw, _ = json.Marshal(b)
			response := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader(raw))
			want := http.StatusBadRequest
			if name == "foreign device" {
				want = http.StatusForbidden
			}
			if name == "missing task" || name == "foreign project" {
				want = http.StatusNotFound
			}
			if response.Code != want {
				t.Fatalf("status %d want %d: %s", response.Code, want, response.Body.String())
			}
			after, _ := s.ServerState(t.Context())
			if after != state {
				t.Fatal("rejected request committed effects")
			}
		})
	}
	raw, _ := json.Marshal(base)
	response := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader(raw))
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	replay := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader(raw))
	if response.Body.String() != replay.Body.String() {
		t.Fatal("HTTP replay changed")
	}
	base["command_id"] = "stale-http"
	raw, _ = json.Marshal(base)
	stale := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader(raw))
	if stale.Code != 409 {
		t.Fatal(stale.Body.String())
	}
	var conflict signet.StaleTaskError
	if err := json.Unmarshal(stale.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.SyncEpoch != state.SyncEpoch || conflict.ReplacementTask.Task.ID != id || conflict.ReplacementTask.EntityVersion != state.Revision+1 {
		t.Fatalf("conflict: %+v", conflict)
	}
}

func TestStopCommandCrossKindCollisions(t *testing.T) {
	for _, firstKind := range domain.AllCommandKinds {
		for _, secondKind := range domain.AllCommandKinds {
			if firstKind == secondKind {
				continue
			}
			t.Run(string(firstKind)+"/"+string(secondKind), func(t *testing.T) {
				f := newFixture(t)
				id := seedStopTask(t, f.store)
				blobs, err := signet.NewBlobStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				svc := signet.NewService(f.store, signet.WithBlobStore(blobs), signet.WithTaskSubmitter(&fakeTaskSubmitter{}))
				command := func(kind domain.CommandKind) signet.ClientCommand {
					switch kind {
					case domain.CommandKindDecision:
						return f.command("shared", domain.ActionOpenPR)
					case domain.CommandKindSubmitTask:
						return submitTaskCommand("shared", "source", "")
					case domain.CommandKindStopTask:
						return stopCommand(t, f.store, id, "shared")
					}
					t.Fatal("unknown kind")
					return signet.ClientCommand{}
				}
				original, err := svc.Submit(t.Context(), command(firstKind))
				if err != nil {
					t.Fatal(err)
				}
				before, _ := f.store.ServerState(t.Context())
				if _, err := svc.Submit(t.Context(), command(secondKind)); !errors.Is(err, store.ErrImmutableConflict) {
					t.Fatalf("cross-kind collision: %v", err)
				}
				after, _ := f.store.ServerState(t.Context())
				if before != after {
					t.Fatal("collision committed effects")
				}
				// Other kinds keep their existing replay semantics. Stop's exact original
				// request is covered separately because rebuilding its version changes it.
				if firstKind != domain.CommandKindStopTask {
					again, err := svc.Submit(t.Context(), command(firstKind))
					if err != nil || !reflect.DeepEqual(again, original) {
						t.Fatalf("original receipt damaged: %v", err)
					}
				}
				if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error { _, err := tx.GetTask(t.Context(), id); return err }); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
