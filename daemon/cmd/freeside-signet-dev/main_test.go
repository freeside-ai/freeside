package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func startHarness(t *testing.T) (*harness, readiness) {
	t.Helper()
	h, err := run(context.Background(), config{
		DBPath:      t.TempDir() + "/signet.db",
		ListenAddr:  "127.0.0.1:0",
		ControlAddr: "127.0.0.1:0",
		NtfyURL:     "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return h, h.readiness()
}

func postJSON(t *testing.T, url, bearer string, body any) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode %s body: %v", url, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, reader)
	if err != nil {
		t.Fatalf("build %s request: %v", url, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return doRequest(t, req)
}

func getJSON(t *testing.T, url, bearer string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build %s request: %v", url, err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return doRequest(t, req)
}

func doRequest(t *testing.T, req *http.Request) (*http.Response, []byte) {
	t.Helper()
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s body: %v", req.URL, err)
	}
	return response, payload
}

func decode[T any](t *testing.T, payload []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(payload, &value); err != nil {
		t.Fatalf("decode %s: %v", payload, err)
	}
	return value
}

// pairNewDevice walks the real pairing exchange: control mint, then the
// contract POST /pairing.
func pairNewDevice(t *testing.T, r readiness, displayName string) string {
	t.Helper()
	response, payload := postJSON(t, r.ControlURL+"/control/pairing-codes", "", nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("mint status = %d body=%s, want 201", response.StatusCode, payload)
	}
	code := decode[map[string]string](t, payload)["pairing_code"]
	if code == "" {
		t.Fatalf("mint returned no pairing_code: %s", payload)
	}
	response, payload = postJSON(t, r.APIURL+"/pairing", "",
		map[string]string{"pairing_code": code, "display_name": displayName})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("pairing status = %d body=%s, want 201", response.StatusCode, payload)
	}
	token := decode[map[string]json.RawMessage](t, payload)["device_token"]
	return strings.Trim(string(token), `"`)
}

func revision(t *testing.T, r readiness, bearer string) (string, int64) {
	t.Helper()
	response, payload := getJSON(t, r.APIURL+"/sync/revision", bearer)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("revision status = %d body=%s, want 200", response.StatusCode, payload)
	}
	state := decode[struct {
		SyncEpoch string `json:"sync_epoch"`
		Revision  int64  `json:"revision"`
	}](t, payload)
	return state.SyncEpoch, state.Revision
}

// TestControlSubmitDeliveryDrivesThePipeline: the control route runs the real
// delivery pipeline against a scripted ntfy — the row comes back
// channel_accepted, the notification's deep link points into this harness's
// own contract listener, and without -ntfy-url the same route reports the
// pipeline's fail-closed refusal.
func TestControlSubmitDeliveryDrivesThePipeline(t *testing.T) {
	var (
		clicksMu sync.Mutex
		clicks   []string
	)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clicksMu.Lock()
		clicks = append(clicks, r.Header.Get("Click"))
		clicksMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fake.Close)

	h, err := run(context.Background(), config{
		DBPath:      t.TempDir() + "/signet.db",
		ListenAddr:  "127.0.0.1:0",
		ControlAddr: "127.0.0.1:0",
		NtfyURL:     fake.URL,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	r := h.readiness()
	token := pairNewDevice(t, r, "Notified device")
	deviceID := deviceIDFromToken(t, token)
	response, payload := postJSON(t, r.ControlURL+"/control/items", "",
		map[string]any{"id": "item-notify", "item_version": 1})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("seed item status = %d body=%s, want 200", response.StatusCode, payload)
	}

	response, payload = postJSON(t, r.ControlURL+"/control/deliveries", "",
		map[string]string{"item_id": "item-notify", "device_id": deviceID})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("submit delivery status = %d body=%s, want 200", response.StatusCode, payload)
	}
	delivery := decode[map[string]json.RawMessage](t, payload)["delivery"]
	if !strings.Contains(string(delivery), `"delivery_status":"channel_accepted"`) {
		t.Errorf("delivery = %s, want channel_accepted", delivery)
	}
	clicksMu.Lock()
	if len(clicks) != 1 || clicks[0] != r.APIURL+"/attention/items/item-notify?channel=ntfy&attempt=1" {
		t.Errorf("published clicks = %v, want the harness deep link with the attempt identity", clicks)
	}
	clicksMu.Unlock()

	bareHarness, err := run(context.Background(), config{
		DBPath: t.TempDir() + "/signet.db", ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("run without ntfy: %v", err)
	}
	t.Cleanup(func() {
		if err := bareHarness.Close(); err != nil {
			t.Errorf("Close bare harness: %v", err)
		}
	})
	bare := bareHarness.readiness()
	response, payload = postJSON(t, bare.ControlURL+"/control/deliveries", "",
		map[string]string{"item_id": "item-notify", "device_id": deviceID})
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no-channel status = %d body=%s, want 503", response.StatusCode, payload)
	}
}

// pairNewDeviceWithTopic walks the real pairing exchange like pairNewDevice
// but also returns the private ntfy topic the one-time grant promises, so a
// test can later assert the delivery pipeline publishes to that same topic.
func pairNewDeviceWithTopic(t *testing.T, r readiness, displayName string) (token, topic string) {
	t.Helper()
	response, payload := postJSON(t, r.ControlURL+"/control/pairing-codes", "", nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("mint status = %d body=%s, want 201", response.StatusCode, payload)
	}
	code := decode[map[string]string](t, payload)["pairing_code"]
	response, payload = postJSON(t, r.APIURL+"/pairing", "",
		map[string]string{"pairing_code": code, "display_name": displayName})
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("pairing status = %d body=%s, want 201", response.StatusCode, payload)
	}
	grant := decode[struct {
		DeviceToken      string `json:"device_token"`
		NtfySubscription struct {
			Topic string `json:"topic"`
		} `json:"ntfy_subscription"`
	}](t, payload)
	if grant.NtfySubscription.Topic == "" {
		t.Fatalf("pairing grant carried no ntfy topic: %s", payload)
	}
	return grant.DeviceToken, grant.NtfySubscription.Topic
}

// TestTopicSurvivesRestart is issue #133's proof (criteria 1, 2, 4): with a
// persisted -topic-key-file, the topic handed to a device in its one-time
// pairing grant still equals the topic SubmitDelivery publishes to after the
// harness is torn down and rebuilt against the same store. The store and key
// files live in separate directories, showing the credential need not (and
// does not) sit in the store's backup surface.
func TestTopicSurvivesRestart(t *testing.T) {
	var (
		topicsMu sync.Mutex
		topics   []string
	)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topicsMu.Lock()
		topics = append(topics, strings.TrimPrefix(r.URL.Path, "/"))
		topicsMu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(fake.Close)

	dbPath := filepath.Join(t.TempDir(), "signet.db")
	keyPath := filepath.Join(t.TempDir(), "topic.key")
	cfg := config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0",
		NtfyURL: fake.URL, TopicKeyFile: keyPath,
	}

	// Run 1: pair a device and capture the topic its grant promises.
	h1, err := run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	r1 := h1.readiness()
	token, grantTopic := pairNewDeviceWithTopic(t, r1, "Persistent device")
	deviceID := deviceIDFromToken(t, token)
	if err := h1.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}

	// The key is credential-grade and lives outside the store's directory.
	info, err := os.Lstat(keyPath)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() || info.Mode().Perm() != 0o600 {
		t.Errorf("key file = mode %v, want a 0600 regular file", info.Mode())
	}
	if filepath.Dir(keyPath) == filepath.Dir(dbPath) {
		t.Errorf("key file shares the store directory (its backup surface)")
	}

	// Run 2: same db + key file. A delivery must publish to the pre-restart
	// grant's topic, not a freshly rekeyed one.
	h2, err := run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run 2 (restart): %v", err)
	}
	t.Cleanup(func() {
		if err := h2.Close(); err != nil {
			t.Errorf("Close 2: %v", err)
		}
	})
	r2 := h2.readiness()
	response, payload := postJSON(t, r2.ControlURL+"/control/items", "",
		map[string]any{"id": "item-notify", "item_version": 1})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("seed item status = %d body=%s, want 200", response.StatusCode, payload)
	}
	response, payload = postJSON(t, r2.ControlURL+"/control/deliveries", "",
		map[string]string{"item_id": "item-notify", "device_id": deviceID})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("submit delivery status = %d body=%s, want 200", response.StatusCode, payload)
	}

	topicsMu.Lock()
	defer topicsMu.Unlock()
	if len(topics) != 1 {
		t.Fatalf("published %d notifications, want 1", len(topics))
	}
	if topics[0] != grantTopic {
		t.Errorf("post-restart published topic = %q, pre-restart grant topic = %q", topics[0], grantTopic)
	}
}

// TestRestartWithoutTopicKeyFails covers issue #133 criterion 3: if the key
// file is lost but the store persists, the harness refuses to start rather
// than mint a fresh key that would silently rekey every paired device.
func TestRestartWithoutTopicKeyFails(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "signet.db")
	keyPath := filepath.Join(t.TempDir(), "topic.key")
	cfg := config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0",
		NtfyURL: "http://127.0.0.1:1", TopicKeyFile: keyPath,
	}
	h, err := run(context.Background(), cfg)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove key file: %v", err)
	}

	h, err = run(context.Background(), cfg)
	if err == nil {
		_ = h.Close()
		t.Fatal("restart with a lost key file succeeded, want a fail-closed refusal")
	}
	if !errors.Is(err, errTopicKeyAbsentForStore) {
		t.Fatalf("restart error = %v, want errTopicKeyAbsentForStore", err)
	}
}

// TestRunWithBadTopicKeyLeavesNoStore covers Codex round-5: a bad
// -topic-key-file on a fresh -db must fail before store.Open creates the
// database, so the operator's corrected retry still sees a fresh store instead
// of being refused as a possible rekey.
func TestRunWithBadTopicKeyLeavesNoStore(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "signet.db")
	badKey := filepath.Join(dbPath+".blobs", "topic.key") // rejected: inside the blob tree
	h, err := run(context.Background(), config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0",
		NtfyURL: "http://127.0.0.1:1", TopicKeyFile: badKey,
	})
	if err == nil {
		_ = h.Close()
		t.Fatal("run accepted a key path inside the store, want a refusal")
	}
	if _, statErr := os.Stat(dbPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("store was created despite the topic-key failure: stat err = %v", statErr)
	}
	// The corrected retry against the still-fresh store succeeds.
	h, err = run(context.Background(), config{
		DBPath: dbPath, ListenAddr: "127.0.0.1:0", ControlAddr: "127.0.0.1:0",
		NtfyURL: "http://127.0.0.1:1", TopicKeyFile: filepath.Join(t.TempDir(), "topic.key"),
	})
	if err != nil {
		t.Fatalf("corrected retry against a fresh store failed: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
}

// deviceIDFromToken recovers the device identity the pairing grant embeds in
// its `fsd1.<device_id_b64>.<secret>` bearer token.
func deviceIDFromToken(t *testing.T, token string) string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("device token has %d segments, want 3", len(parts))
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 {
		t.Fatalf("decode device id from token: %v", err)
	}
	return string(raw)
}

// TestRunRefusesNonLoopback: the harness fails closed on any non-loopback
// bind, for either listener (plan §5.2; the control surface is
// unauthenticated by design and must never be reachable off-host).
func TestRunRefusesNonLoopback(t *testing.T) {
	for name, cfg := range map[string]config{
		"contract": {DBPath: t.TempDir() + "/signet.db", ListenAddr: "0.0.0.0:0", ControlAddr: "127.0.0.1:0"},
		"control":  {DBPath: t.TempDir() + "/signet.db", ListenAddr: "127.0.0.1:0", ControlAddr: "0.0.0.0:0"},
	} {
		if h, err := run(context.Background(), cfg); err == nil {
			_ = h.Close()
			t.Errorf("%s: run accepted a non-loopback address", name)
		} else if !strings.Contains(err.Error(), "non-loopback") {
			t.Errorf("%s: error = %v, want a non-loopback refusal", name, err)
		}
	}
}

// TestHarnessPairingFlowOverTheWire: a control-minted code is redeemable on
// the contract surface exactly once, and the granted token authorizes reads.
func TestHarnessPairingFlowOverTheWire(t *testing.T) {
	_, r := startHarness(t)

	if response, _ := getJSON(t, r.APIURL+"/sync/revision", ""); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated revision status = %d, want 401", response.StatusCode)
	}
	token := pairNewDevice(t, r, "Convergence A")
	if epoch, rev := revision(t, r, token); epoch == "" || rev < 1 {
		t.Fatalf("revision = (%q, %d), want a seeded epoch and revision", epoch, rev)
	}
}

// TestControlPutItemAdvancesRevision: seeding and advancing an item through
// the control surface is client-visible (revision bump, item readable), the
// real analogue of the mock's advance hook.
func TestControlPutItemAdvancesRevision(t *testing.T) {
	_, r := startHarness(t)
	token := pairNewDevice(t, r, "Convergence A")
	_, before := revision(t, r, token)

	for version := 1; version <= 2; version++ {
		response, payload := postJSON(t, r.ControlURL+"/control/items", "",
			putItemRequest{ID: "item-conv-1", ItemVersion: version})
		if response.StatusCode != http.StatusOK {
			t.Fatalf("put item v%d status = %d body=%s, want 200", version, response.StatusCode, payload)
		}
	}
	if _, after := revision(t, r, token); after != before+2 {
		t.Fatalf("revision after two puts = %d, want %d", after, before+2)
	}
	response, payload := getJSON(t, r.APIURL+"/attention/items/item-conv-1", token)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("get item status = %d body=%s, want 200", response.StatusCode, payload)
	}
	snapshot := decode[struct {
		EntityVersion int64 `json:"entity_version"`
		Item          struct {
			ItemVersion int `json:"item_version"`
		} `json:"item"`
	}](t, payload)
	if snapshot.Item.ItemVersion != 2 {
		t.Fatalf("item_version = %d, want the advanced 2", snapshot.Item.ItemVersion)
	}

	response, payload = postJSON(t, r.ControlURL+"/control/items", "",
		putItemRequest{ID: "", ItemVersion: 1})
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid item status = %d body=%s, want 400 from the domain gate", response.StatusCode, payload)
	}
}

// TestControlEpochRotation: POST /control/epoch rotates the epoch on its own
// (the minimal §5.14 test-8 stimulus) without bumping the revision. The real
// data restore is covered by TestControlCheckpointRestore.
func TestControlEpochRotation(t *testing.T) {
	_, r := startHarness(t)
	token := pairNewDevice(t, r, "Convergence A")
	beforeEpoch, beforeRevision := revision(t, r, token)

	response, payload := postJSON(t, r.ControlURL+"/control/epoch", "", nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d body=%s, want 200", response.StatusCode, payload)
	}
	afterEpoch, afterRevision := revision(t, r, token)
	if afterEpoch == beforeEpoch {
		t.Fatalf("epoch did not rotate from %q", beforeEpoch)
	}
	if afterRevision != beforeRevision {
		t.Fatalf("revision = %d, want unchanged %d (the epoch change is the invalidation)", afterRevision, beforeRevision)
	}
}

// TestControlCheckpointRestore drives the real §5.14 restore through the
// control surface: checkpoint a state, advance past it, then restore. The
// restore rolls the revision back below the advanced world and rotates the
// epoch in one call, so the epoch a client cached from the advanced state is
// now stale even though its cached revision is the higher one.
func TestControlCheckpointRestore(t *testing.T) {
	_, r := startHarness(t)
	token := pairNewDevice(t, r, "Convergence A")

	response, payload := postJSON(t, r.ControlURL+"/control/items", "",
		map[string]any{"id": "item-1", "item_version": 1})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("seed item status = %d body=%s, want 200", response.StatusCode, payload)
	}
	checkpointEpoch, checkpointRevision := revision(t, r, token)

	response, payload = postJSON(t, r.ControlURL+"/control/checkpoint", "", nil)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("checkpoint status = %d body=%s, want 201", response.StatusCode, payload)
	}
	checkpointPath := decode[map[string]string](t, payload)["checkpoint"]
	if checkpointPath == "" {
		t.Fatalf("checkpoint returned no path: %s", payload)
	}

	// Advance past the checkpoint: a client caches this higher-revision world.
	response, payload = postJSON(t, r.ControlURL+"/control/items", "",
		map[string]any{"id": "item-1", "item_version": 2})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("advance item status = %d body=%s, want 200", response.StatusCode, payload)
	}
	_, advancedRevision := revision(t, r, token)
	if advancedRevision <= checkpointRevision {
		t.Fatalf("advance did not move revision %d -> %d", checkpointRevision, advancedRevision)
	}

	response, payload = postJSON(t, r.ControlURL+"/control/restore", "",
		map[string]string{"checkpoint": checkpointPath})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("restore status = %d body=%s, want 200", response.StatusCode, payload)
	}
	restored := decode[struct {
		SyncEpoch string `json:"sync_epoch"`
		Revision  int64  `json:"revision"`
	}](t, payload)

	if restored.SyncEpoch == checkpointEpoch {
		t.Fatalf("restore did not rotate the epoch from %q", checkpointEpoch)
	}
	if restored.Revision != checkpointRevision {
		t.Fatalf("restore revision = %d, want checkpoint revision %d", restored.Revision, checkpointRevision)
	}
	if restored.Revision >= advancedRevision {
		t.Fatalf("restore revision %d did not regress below the advanced %d", restored.Revision, advancedRevision)
	}
	// The daemon now serves the restored state to clients.
	served, servedRevision := revision(t, r, token)
	if served != restored.SyncEpoch || servedRevision != restored.Revision {
		t.Fatalf("served state = %q/%d, want restored %q/%d", served, servedRevision, restored.SyncEpoch, restored.Revision)
	}
}

// TestControlRestoreRejectsNonIssuedCheckpoint: /control/restore accepts only a
// checkpoint this harness issued (a 32-hex .db name resolved inside its own
// checkpoint dir), so a loopback control caller cannot make the raw table copy
// in Store.Restore replace the store from an arbitrary database.
func TestControlRestoreRejectsNonIssuedCheckpoint(t *testing.T) {
	_, r := startHarness(t)
	for name, checkpoint := range map[string]string{
		"a non-issued name":             "not-a-checkpoint",
		"a path traversal":              "../../etc/passwd",
		"a valid name in a foreign dir": "/tmp/" + strings.Repeat("a", 32) + ".db",
	} {
		response, payload := postJSON(t, r.ControlURL+"/control/restore", "",
			map[string]string{"checkpoint": checkpoint})
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: restore status = %d body=%s, want 400", name, response.StatusCode, payload)
		}
	}
}

// TestRunRejectsLooseCheckpointDir: a pre-existing, group/world-readable
// checkpoint directory must fail run() closed rather than receive full store
// snapshots (device credentials, pairing rows) — the path is predictable and
// persists across runs, so MkdirAll's silent accept of an existing loose dir
// is a credential-leak surface.
func TestRunRejectsLooseCheckpointDir(t *testing.T) {
	// The gate requires exactly 0700: group/other bits are a credential leak,
	// and a missing owner write/execute bit would only fail closed later, at
	// the first checkpoint write, instead of here at startup.
	for name, mode := range map[string]os.FileMode{
		"group-readable (0750)": 0o750,
		"non-writable (0500)":   0o500,
	} {
		t.Run(name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "signet.db")
			if err := os.MkdirAll(dbPath+".checkpoints", mode); err != nil {
				t.Fatalf("pre-create checkpoint dir: %v", err)
			}
			h, err := run(context.Background(), config{
				DBPath:      dbPath,
				ListenAddr:  "127.0.0.1:0",
				ControlAddr: "127.0.0.1:0",
			})
			if err == nil {
				_ = h.Close()
				t.Fatalf("run accepted a mode-%04o checkpoint dir, want fail-closed", mode)
			}
			// The rejection must happen before store.Open, so no fresh,
			// never-paired store is left behind to strand the operator's retry
			// behind the issue #133 topic-key gate.
			if _, statErr := os.Stat(dbPath); !os.IsNotExist(statErr) {
				t.Fatalf("run created the store at %s before rejecting the checkpoint dir; stat err = %v", dbPath, statErr)
			}
		})
	}
}

// TestReadinessAddressesServe: the readiness line's URLs are the served
// listeners, and Close stops both.
func TestReadinessAddressesServe(t *testing.T) {
	h, err := run(context.Background(), config{
		DBPath:      t.TempDir() + "/signet.db",
		ListenAddr:  "127.0.0.1:0",
		ControlAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	r := h.readiness()
	for _, url := range []string{r.APIURL, r.ControlURL} {
		if !strings.HasPrefix(url, "http://127.0.0.1:") {
			t.Errorf("readiness url = %q, want a loopback http URL", url)
		}
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, url := range []string{r.APIURL + "/sync/revision", r.ControlURL + "/control/epoch"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		if response, err := http.DefaultClient.Do(req); err == nil {
			_ = response.Body.Close()
			t.Errorf("%s still serving after Close", url)
		}
	}
}
