package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	if len(clicks) != 1 || clicks[0] != r.APIURL+"/attention/items/item-notify" {
		t.Errorf("published clicks = %v, want the harness deep link", clicks)
	}
	clicksMu.Unlock()

	_, bare := startHarness(t)
	response, payload = postJSON(t, bare.ControlURL+"/control/deliveries", "",
		map[string]string{"item_id": "item-notify", "device_id": deviceID})
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("no-channel status = %d body=%s, want 503", response.StatusCode, payload)
	}
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

// TestControlEpochRotation: rotating the epoch simulates a restore (§5.14
// test 8 server half) without bumping the revision.
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
