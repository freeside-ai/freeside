package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestRunServesSignetAndStops(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := run(ctx, config{
		DBPath:        filepath.Join(root, "freeside.db"),
		FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr:    "127.0.0.1:0", ReconcileInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	client := &http.Client{Timeout: time.Second}
	response, err := client.Get(h.readiness().APIURL + "/sync/revision")
	if err != nil {
		t.Fatalf("GET /sync/revision: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /sync/revision status = %d, want 401 from the live authorizer", response.StatusCode)
	}

	cancel()
	if err := h.Wait(ctx); err != nil {
		t.Fatalf("Wait after cancellation: %v", err)
	}
}

func TestRunPairsFreshDevice(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := run(ctx, config{
		DBPath: filepath.Join(root, "freeside.db"), ListenAddr: "127.0.0.1:0",
		ReconcileInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ready := h.readiness()
	if ready.PairingCode == "" {
		t.Fatal("readiness carried no startup pairing code")
	}
	payload, err := json.Marshal(map[string]string{
		"pairing_code": ready.PairingCode, "display_name": "Fresh device",
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	response, err := http.Post(ready.APIURL+"/pairing", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("POST /pairing: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("POST /pairing status = %d body=%s, want 201", response.StatusCode, body)
	}
	var grant struct {
		DeviceToken      string `json:"device_token"`
		NtfySubscription struct {
			ServerURL string `json:"server_url"`
			Topic     string `json:"topic"`
		} `json:"ntfy_subscription"`
	}
	if err := json.NewDecoder(response.Body).Decode(&grant); err != nil {
		t.Fatalf("decode pairing grant: %v", err)
	}
	if grant.DeviceToken == "" || grant.NtfySubscription.Topic == "" ||
		grant.NtfySubscription.ServerURL != defaultNtfyURL {
		t.Fatalf("pairing grant = %#v, want token and hosted ntfy capability", grant)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, ready.APIURL+"/sync/revision", nil)
	if err != nil {
		t.Fatalf("new authenticated request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+grant.DeviceToken)
	authorized, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("authenticated GET /sync/revision: %v", err)
	}
	defer func() { _ = authorized.Body.Close() }()
	if authorized.StatusCode != http.StatusOK {
		t.Fatalf("authenticated GET /sync/revision status = %d, want 200", authorized.StatusCode)
	}
}

func TestTopicKeyPersistsAndRejectsUntrustedFiles(t *testing.T) {
	t.Run("persists privately", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		first, err := loadOrCreateTopicKey(dbPath, false)
		if err != nil {
			t.Fatalf("create topic key: %v", err)
		}
		second, err := loadOrCreateTopicKey(dbPath, true)
		if err != nil {
			t.Fatalf("load topic key: %v", err)
		}
		if !slices.Equal(first, second) {
			t.Fatal("reloaded topic key differs")
		}
		info, err := os.Stat(dbPath + topicKeySuffix)
		if err != nil {
			t.Fatalf("stat topic key: %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("topic key mode = %04o, want 0600", info.Mode().Perm())
		}
	})

	t.Run("missing for existing store", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyMissing) {
			t.Fatalf("load error = %v, want errTopicKeyMissing", err)
		}
	})

	t.Run("widened permissions", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		path := dbPath + topicKeySuffix
		if err := os.WriteFile(path, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
			t.Fatalf("write topic key: %v", err)
		}
		if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // intentionally widened adversarial fixture
			t.Fatalf("chmod topic key: %v", err)
		}
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyPermissions) {
			t.Fatalf("load error = %v, want errTopicKeyPermissions", err)
		}
	})

	t.Run("malformed length", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "freeside.db")
		if err := os.WriteFile(dbPath+topicKeySuffix, []byte("short"), 0o600); err != nil {
			t.Fatalf("write topic key: %v", err)
		}
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyMalformed) {
			t.Fatalf("load error = %v, want errTopicKeyMalformed", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "freeside.db")
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Symlink(target, dbPath+topicKeySuffix); err != nil {
			t.Fatalf("symlink topic key: %v", err)
		}
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyPermissions) {
			t.Fatalf("load error = %v, want errTopicKeyPermissions", err)
		}
	})

	t.Run("hard link", func(t *testing.T) {
		root := t.TempDir()
		dbPath := filepath.Join(root, "freeside.db")
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, bytes.Repeat([]byte{1}, 32), 0o600); err != nil {
			t.Fatalf("write target: %v", err)
		}
		if err := os.Link(target, dbPath+topicKeySuffix); err != nil {
			t.Fatalf("hard link topic key: %v", err)
		}
		if _, err := loadOrCreateTopicKey(dbPath, true); !errors.Is(err, errTopicKeyPermissions) {
			t.Fatalf("load error = %v, want errTopicKeyPermissions", err)
		}
	})
}

func TestRunValidatesConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := run(ctx, config{ListenAddr: "127.0.0.1:0"}); err == nil {
		t.Fatal("run accepted an empty database path")
	}
	if _, err := run(ctx, config{
		DBPath:     filepath.Join(t.TempDir(), "freeside.db"),
		ListenAddr: "127.0.0.1:0", ReconcileInterval: -time.Second,
	}); err == nil {
		t.Fatal("run accepted a negative reconcile interval")
	}
}

func TestRunDrivesFakeWorkflow(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := run(ctx, config{
		DBPath:        filepath.Join(root, "freeside.db"),
		FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr:    "127.0.0.1:0", ReconcileInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	pairedAt := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: "device-1", DisplayName: "Device 1",
			Status: domain.DeviceActive, PairedAt: pairedAt,
		})
	}); err != nil {
		t.Fatalf("put device: %v", err)
	}
	approval, err := h.attention.GetAttentionItem(ctx, "approval-"+domain.ItemID(defaultFakeRunID))
	if err != nil {
		t.Fatalf("get approval: %v", err)
	}
	if _, err := h.attention.Submit(ctx, signet.ClientCommand{
		CommandID: "approve-main", DeviceID: "device-1", ExpectedEntityVersion: approval.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: approval.Item.ID, Action: domain.ActionApprove,
			ItemVersion: approval.Item.ItemVersion,
		},
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	feedback := waitForItem(t, h.attention, "feedback-"+domain.ItemID(defaultFakeRunID))
	if _, err := h.attention.Submit(ctx, signet.ClientCommand{
		CommandID: "discuss-main", DeviceID: "device-1", ExpectedEntityVersion: feedback.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: feedback.Item.ID, Action: domain.ActionDiscuss,
			ItemVersion: feedback.Item.ItemVersion, Message: "advance the workflow",
		},
	}); err != nil {
		t.Fatalf("discuss: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		run, err := h.attention.GetRun(ctx, defaultFakeRunID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if len(run.Run.Stages) == 1 && len(run.Run.Stages[0].Attempts) == 1 {
			item, err := h.attention.GetAttentionItem(ctx, feedback.Item.ID)
			if err != nil {
				t.Fatalf("get feedback: %v", err)
			}
			conversation, err := h.attention.GetConversation(ctx, *item.Item.ConversationID)
			if err != nil {
				t.Fatalf("get conversation: %v", err)
			}
			if len(conversation.Conversation.Messages) == 2 &&
				conversation.Conversation.Status == domain.ConversationIdle {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("freesided did not accept the fake result within 2s")
}

func waitForItem(t *testing.T, attention *signet.Service, id domain.ItemID) signet.AttentionItemSnapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		item, err := attention.GetAttentionItem(context.Background(), id)
		if err == nil {
			return item
		}
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get item %q: %v", id, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("item %q did not appear within 2s", id)
	return signet.AttentionItemSnapshot{}
}

func TestListenLoopbackRejectsWildcard(t *testing.T) {
	t.Parallel()
	if listener, err := listenLoopback("0.0.0.0:0"); err == nil {
		_ = listener.Close()
		t.Fatal("listenLoopback accepted a wildcard address")
	}
}
