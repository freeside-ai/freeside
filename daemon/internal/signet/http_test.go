package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func testAuthorizer(r *http.Request) (domain.DeviceID, bool) {
	if r.Header.Get("Authorization") != "Bearer test-device-1" {
		return "", false
	}
	return "device-1", true
}

func TestHTTPAuthenticationFailsClosed(t *testing.T) {
	f := newFixture(t)
	for name, handler := range map[string]http.Handler{
		"nil authorizer":      signet.NewHTTPHandler(f.service, nil),
		"rejected credential": signet.NewHTTPHandler(f.service, testAuthorizer),
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/sync/revision", nil)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatalf("response = %d WWW-Authenticate %q, want 401 Bearer",
					response.Code, response.Header().Get("WWW-Authenticate"))
			}
		})
	}
}

// TestHTTPPartialRefetchPreservesRevisionGap is §5.14 test 11's server half.
// A single-entity response carries only that row's metadata, never the global
// full-snapshot cursor; the heartbeat still reveals a write the refetch did
// not cover, so the client must bootstrap or refetch every affected resource.
func TestHTTPPartialRefetchPreservesRevisionGap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	initial, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("initial Bootstrap: %v", err)
	}
	second := f.item
	second.ID = "item-2"
	if err := f.service.PutItem(ctx, second); err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := authenticatedRequest(t, handler, http.MethodGet, "/attention/items/item-1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET item status = %d body=%s", response.Code, response.Body.String())
	}
	var partial map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &partial); err != nil {
		t.Fatalf("decode partial response: %v", err)
	}
	if _, hasRevision := partial["revision"]; hasRevision {
		t.Fatal("partial entity response exposed a whole-cache revision cursor")
	}
	var asOf int64
	if err := json.Unmarshal(partial["as_of_revision"], &asOf); err != nil {
		t.Fatalf("decode as_of_revision: %v", err)
	}
	if asOf != initial.AttentionItems[0].AsOfRevision {
		t.Errorf("partial item revision = %d, want unchanged row revision %d",
			asOf, initial.AttentionItems[0].AsOfRevision)
	}

	heartbeatResponse := authenticatedRequest(t, handler, http.MethodGet, "/sync/revision", nil)
	var heartbeat signet.ServerRevision
	if err := json.Unmarshal(heartbeatResponse.Body.Bytes(), &heartbeat); err != nil {
		t.Fatalf("decode heartbeat: %v", err)
	}
	if heartbeat.Revision <= initial.Revision || heartbeat.Revision <= asOf {
		t.Fatalf("heartbeat revision = %d, initial=%d partial row=%d; want a visible gap",
			heartbeat.Revision, initial.Revision, asOf)
	}
	refreshed, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("refreshed Bootstrap: %v", err)
	}
	if refreshed.Revision != heartbeat.Revision || len(refreshed.AttentionItems) != 2 {
		t.Errorf("refreshed bootstrap = revision %d with %d items, want %d with 2",
			refreshed.Revision, len(refreshed.AttentionItems), heartbeat.Revision)
	}
}

func TestHTTPBootstrapMatchesOpenAPIEnvelope(t *testing.T) {
	f := newFixture(t)
	seedSyncResources(t, f)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := authenticatedRequest(t, handler, http.MethodGet, "/sync/bootstrap", nil)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response = %d %q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	}
	var bootstrap signet.BootstrapSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &bootstrap); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if bootstrap.SyncEpoch == "" || bootstrap.Revision < 1 ||
		len(bootstrap.AttentionItems) != 1 || len(bootstrap.AttentionDeliveries) != 1 ||
		len(bootstrap.Runs) != 1 || len(bootstrap.Conversations) != 1 {
		t.Fatalf("bootstrap envelope = %+v", bootstrap)
	}
	for _, forbidden := range []string{
		`"evidence_snapshot":null`, `"agent_claims":null`, `"artifact_digests":null`,
		`"attempts":null`, `"messages":null`,
	} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Errorf("bootstrap contains contract-invalid null array %s: %s", forbidden, response.Body.String())
		}
	}
}

func TestHTTPCommandBindsAuthenticatedDevice(t *testing.T) {
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	before := f.revision(t)

	mismatched := commandJSON("cmd-wrong-device", "device-2", 1, domain.ActionStop)
	response := authenticatedRequest(t, handler, http.MethodPost, "/commands", mismatched)
	if response.Code != http.StatusForbidden {
		t.Fatalf("mismatched device status = %d body=%s, want 403", response.Code, response.Body.String())
	}
	if after := f.revision(t); after != before {
		t.Fatalf("rejected identity moved revision %d -> %d", before, after)
	}

	response = authenticatedRequest(t, handler, http.MethodPost, "/commands",
		commandJSON("cmd-valid", "device-1", 1, domain.ActionOpenPR))
	if response.Code != http.StatusOK {
		t.Fatalf("valid command status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode command result: %v", err)
	}
	if _, ok := result["record"]; !ok {
		t.Fatalf("command result keys = %v, want OpenAPI record field", result)
	}
	if _, ok := result["revision"]; !ok {
		t.Fatalf("command result keys = %v, want OpenAPI revision field", result)
	}
	if strings.Contains(response.Body.String(), `"artifact_digests":null`) {
		t.Fatalf("command result contains a contract-invalid null digest array: %s", response.Body.String())
	}
}

func TestHTTPCommandRendersStaleReplacement(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	advanced := f.item
	advanced.ItemVersion++
	advanced.Reason = "canonical state changed"
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, advanced)
	}); err != nil {
		t.Fatalf("advance item: %v", err)
	}

	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := authenticatedRequest(t, handler, http.MethodPost, "/commands",
		commandJSON("cmd-stale", "device-1", 1, domain.ActionStop))
	if response.Code != http.StatusConflict {
		t.Fatalf("stale command status = %d body=%s, want 409", response.Code, response.Body.String())
	}
	var rejection struct {
		Message         string                       `json:"message"`
		ReplacementItem signet.AttentionItemSnapshot `json:"replacement_item"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rejection); err != nil {
		t.Fatalf("decode rejection: %v", err)
	}
	if rejection.Message == "" || rejection.ReplacementItem.Item.ItemVersion != 2 ||
		rejection.ReplacementItem.EntityVersion != 2 {
		t.Fatalf("rejection = %+v, want canonical item v2/entity_version 2", rejection)
	}
}

func TestHTTPCommandRejectsMalformedBodiesWithoutStateChange(t *testing.T) {
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	valid := string(mustJSON(map[string]any{
		"command_id": "cmd-1", "device_id": "device-1", "expected_entity_version": 1,
		"expected_bindings": map[string]string{},
		"payload": map[string]any{
			"item_id": "item-1", "action": "open_pr", "item_version": 1,
			"pr_head_sha": "cafebabe", "artifact_digests": []string{},
		},
	}))
	for _, test := range []struct {
		name   string
		body   string
		status int
	}{
		{"unknown field", strings.Replace(valid, `"command_id"`, `"unexpected":true,"command_id"`, 1), http.StatusBadRequest},
		{"missing expected bindings", strings.Replace(valid, `"expected_bindings":{},`, "", 1), http.StatusBadRequest},
		{"empty expected binding", strings.Replace(valid, `"expected_bindings":{}`, `"expected_bindings":{"evidence":""}`, 1), http.StatusBadRequest},
		{"null expected binding", strings.Replace(valid, `"expected_bindings":{}`, `"expected_bindings":{"evidence":null}`, 1), http.StatusBadRequest},
		{"missing pr head", strings.Replace(valid, `,"pr_head_sha":"cafebabe"`, "", 1), http.StatusBadRequest},
		{"null pr head", strings.Replace(valid, `"pr_head_sha":"cafebabe"`, `"pr_head_sha":null`, 1), http.StatusBadRequest},
		{"missing artifact digests", strings.Replace(valid, `,"artifact_digests":[]`, "", 1), http.StatusBadRequest},
		{"null artifact digests", strings.Replace(valid, `"artifact_digests":[]`, `"artifact_digests":null`, 1), http.StatusBadRequest},
		{"multiple objects", valid + valid, http.StatusBadRequest},
		{"oversized", `{"padding":"` + strings.Repeat("x", maxTestCommandBodyBytes) + `"}`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := f.revision(t)
			response := authenticatedRequest(t, handler, http.MethodPost, "/commands",
				bytes.NewReader([]byte(test.body)))
			if response.Code != test.status {
				t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), test.status)
			}
			if after := f.revision(t); after != before {
				t.Fatalf("malformed request moved revision %d -> %d", before, after)
			}
		})
	}
}

const maxTestCommandBodyBytes = (1 << 20) + 1

// bearerRequest performs one request under an arbitrary Authorization header
// (or none, when header is empty); the pairing and revocation flows exercise
// the real authorizer rather than the test stub.
func bearerRequest(t *testing.T, handler http.Handler, method, target, header string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if header != "" {
		request.Header.Set("Authorization", header)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

// TestHTTPPairingFlowEndToEnd walks the whole device lifecycle over the wire
// with the real request authorizer: exchange a minted code (201, the
// OpenAPI PairingGrant envelope), read with the granted token, revoke the
// device (200, DeviceSnapshot), and observe the credential stop working
// (401) while a second device's re-revoke returns the recorded snapshot.
func TestHTTPPairingFlowEndToEnd(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, signet.NewRequestAuthorizer(f.store))

	plaintext, _, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	response := bearerRequest(t, handler, http.MethodPost, "/pairing", "",
		mustJSON(map[string]string{"pairing_code": plaintext, "display_name": "Ben's iPhone"}))
	if response.Code != http.StatusCreated {
		t.Fatalf("pairing status = %d body=%s, want 201", response.Code, response.Body.String())
	}
	var grant signet.PairingGrant
	if err := json.Unmarshal(response.Body.Bytes(), &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	for _, key := range []string{"device_token", "device", "ntfy_subscription"} {
		if _, ok := envelope[key]; !ok {
			t.Errorf("grant envelope is missing %q: %s", key, response.Body.String())
		}
	}
	if grant.DeviceToken == "" || grant.Device.EntityVersion != 1 || grant.NtfySubscription.Topic == "" {
		t.Fatalf("grant = %+v, want a token, private topic, and a version-1 device snapshot", grant)
	}
	deviceID := grant.Device.Device.ID

	authorized := bearerRequest(t, handler, http.MethodGet, "/sync/revision", "Bearer "+grant.DeviceToken, nil)
	if authorized.Code != http.StatusOK {
		t.Fatalf("granted token read status = %d, want 200", authorized.Code)
	}

	revoke := bearerRequest(t, handler, http.MethodPost, "/devices/"+string(deviceID)+"/revoke",
		"Bearer "+grant.DeviceToken, nil)
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke status = %d body=%s, want 200", revoke.Code, revoke.Body.String())
	}
	var snapshot signet.DeviceSnapshot
	if err := json.Unmarshal(revoke.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode revoke snapshot: %v", err)
	}
	if snapshot.Device.Status != domain.DeviceRevoked || snapshot.EntityVersion != 2 {
		t.Fatalf("revoke snapshot = %+v, want revoked at entity_version 2", snapshot)
	}

	if after := bearerRequest(t, handler, http.MethodGet, "/sync/revision", "Bearer "+grant.DeviceToken, nil); after.Code != http.StatusUnauthorized {
		t.Fatalf("revoked token read status = %d, want 401", after.Code)
	}

	// A surviving device re-revokes the dead one: same recorded snapshot.
	secondToken, _ := pairedDevice(t, f, "Second device")
	again := bearerRequest(t, handler, http.MethodPost, "/devices/"+string(deviceID)+"/revoke",
		"Bearer "+secondToken, nil)
	if again.Code != http.StatusOK || again.Body.String() != revoke.Body.String() {
		t.Fatalf("re-revoke = %d %s, want the recorded 200 %s",
			again.Code, again.Body.String(), revoke.Body.String())
	}
}

// TestHTTPPairingRejectionsAreUndifferentiated: unknown, expired, and
// consumed codes produce byte-identical 403 responses (the anti-probing
// contract), and malformed requests fail 400 before touching pairing state.
func TestHTTPPairingRejectionsAreUndifferentiated(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, signet.NewRequestAuthorizer(f.store))

	expired, _, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("mint expired-case code: %v", err)
	}
	*f.now = f.now.Add(10 * time.Minute)
	consumed, _, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("mint consumed-case code: %v", err)
	}
	if _, err := f.service.Pair(ctx, consumed, "First device"); err != nil {
		t.Fatalf("consume code: %v", err)
	}

	pairBody := func(code string) []byte {
		return mustJSON(map[string]string{"pairing_code": code, "display_name": "Probe"})
	}
	responses := map[string]*httptest.ResponseRecorder{}
	for name, code := range map[string]string{
		"unknown": "AAAAAAAA", "expired": expired, "consumed": consumed,
	} {
		response := bearerRequest(t, handler, http.MethodPost, "/pairing", "", pairBody(code))
		if response.Code != http.StatusForbidden {
			t.Fatalf("%s code status = %d body=%s, want 403", name, response.Code, response.Body.String())
		}
		responses[name] = response
	}
	for name, response := range responses {
		if response.Body.String() != responses["unknown"].Body.String() {
			t.Errorf("%s rejection %q differs from unknown %q: the 403 must not distinguish causes",
				name, response.Body.String(), responses["unknown"].Body.String())
		}
	}

	for name, body := range map[string][]byte{
		"missing code":  mustJSON(map[string]string{"display_name": "Probe"}),
		"empty code":    pairBody(""),
		"missing label": mustJSON(map[string]string{"pairing_code": "AAAAAAAA"}),
		"unknown field": mustJSON(map[string]string{"pairing_code": "AAAAAAAA", "display_name": "P", "extra": "x"}),
	} {
		t.Run(name, func(t *testing.T) {
			response := bearerRequest(t, handler, http.MethodPost, "/pairing", "", body)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", response.Code, response.Body.String())
			}
		})
	}
}

// TestHTTPRevokeRequiresAuthAndKnownDevice: revocation sits behind the
// credential (only /pairing is unauthenticated), and an unknown device is
// 404.
func TestHTTPRevokeRequiresAuthAndKnownDevice(t *testing.T) {
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)

	unauthenticated := bearerRequest(t, handler, http.MethodPost, "/devices/device-1/revoke", "", nil)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated revoke status = %d, want 401", unauthenticated.Code)
	}
	unknown := authenticatedRequest(t, handler, http.MethodPost, "/devices/device-ghost/revoke", nil)
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown device revoke status = %d, want 404", unknown.Code)
	}
}

// TestHTTPRevokedDeviceCommandRejected is §5.14 test 15 over the wire: with
// the real authorizer, a revoked device's prepared command dies at the
// credential (401) before the service gate even runs.
func TestHTTPRevokedDeviceCommandRejected(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, signet.NewRequestAuthorizer(f.store))
	token, deviceID := pairedDevice(t, f, "Doomed device")

	if _, err := f.service.Revoke(ctx, f.device.ID, deviceID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	before := f.revision(t)
	prepared := commandJSON("cmd-prepared", deviceID, 1, domain.ActionStop)
	preparedBody := make([]byte, prepared.Len())
	if _, err := prepared.Read(preparedBody); err != nil {
		t.Fatalf("read prepared body: %v", err)
	}
	response := bearerRequest(t, handler, http.MethodPost, "/commands", "Bearer "+token, preparedBody)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked device command status = %d body=%s, want 401", response.Code, response.Body.String())
	}
	if after := f.revision(t); after != before {
		t.Errorf("rejected command moved revision %d -> %d", before, after)
	}
}

// TestHTTPReportDeliveryOpened is #130's wire path: a paired device reports
// the opened receipt on its own attempt (200, the recorded snapshot, the
// item's timing aggregates moved in the same transaction), a replay returns
// the byte-identical snapshot without consuming revision, and the failure
// surface stays closed — a malformed attempt is 400, an unknown or
// other-device attempt is 404 (the device comes from the credential, never
// the path).
func TestHTTPReportDeliveryOpened(t *testing.T) {
	f := newFixture(t)
	seedSubmittedDelivery(t, f, f.device.ID, 1, *f.now)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	*f.now = f.now.Add(time.Minute)

	target := "/attention/items/" + string(f.item.ID) + "/deliveries/ntfy/1/opened"
	response := authenticatedRequest(t, handler, http.MethodPut, target, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("report opened status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	var snapshot signet.AttentionDeliverySnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if snapshot.Delivery.Status != domain.DeliveryOpened || snapshot.Delivery.OpenedAt == nil {
		t.Fatalf("snapshot delivery = %+v, want opened with a receipt", snapshot.Delivery)
	}
	item, _ := f.itemSnapshot(t)
	if item.Timing.FirstOpenedAt == nil || !item.Timing.FirstOpenedAt.Equal(*snapshot.Delivery.OpenedAt) {
		t.Errorf("timing first_opened_at = %v, want %v", item.Timing.FirstOpenedAt, snapshot.Delivery.OpenedAt)
	}

	before := f.revision(t)
	*f.now = f.now.Add(time.Hour)
	replay := authenticatedRequest(t, handler, http.MethodPut, target, nil)
	if replay.Code != http.StatusOK || replay.Body.String() != response.Body.String() {
		t.Fatalf("replay = %d %s, want the recorded 200 %s",
			replay.Code, replay.Body.String(), response.Body.String())
	}
	if after := f.revision(t); after != before {
		t.Errorf("replay moved the revision %d → %d", before, after)
	}

	f.seedDevice(t, "device-2")
	seedSubmittedDelivery(t, f, "device-2", 2, *f.now)
	for name, probe := range map[string]struct {
		target string
		want   int
	}{
		"garbage attempt":        {"/attention/items/" + string(f.item.ID) + "/deliveries/ntfy/junk/opened", http.StatusBadRequest},
		"zero attempt":           {"/attention/items/" + string(f.item.ID) + "/deliveries/ntfy/0/opened", http.StatusBadRequest},
		"negative attempt":       {"/attention/items/" + string(f.item.ID) + "/deliveries/ntfy/-1/opened", http.StatusBadRequest},
		"unknown attempt":        {"/attention/items/" + string(f.item.ID) + "/deliveries/ntfy/9/opened", http.StatusNotFound},
		"unknown channel":        {"/attention/items/" + string(f.item.ID) + "/deliveries/carrier-pigeon/1/opened", http.StatusNotFound},
		"unknown item":           {"/attention/items/item-ghost/deliveries/ntfy/1/opened", http.StatusNotFound},
		"another device attempt": {"/attention/items/" + string(f.item.ID) + "/deliveries/ntfy/2/opened", http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			before := f.revision(t)
			response := authenticatedRequest(t, handler, http.MethodPut, probe.target, nil)
			if response.Code != probe.want {
				t.Fatalf("status = %d body=%s, want %d", response.Code, response.Body.String(), probe.want)
			}
			if after := f.revision(t); after != before {
				t.Errorf("refused receipt moved the revision %d → %d", before, after)
			}
		})
	}
	if row, err := readDelivery(f, f.item.ID, "device-2", "ntfy", 2); err != nil || row.Status != domain.DeliverySubmitted {
		t.Errorf("device-2 row = %+v (%v), want untouched submitted: device-1 must not open it", row, err)
	}
}

// TestHTTPReportDeliveryOpenedRevokedDevice is §5.14 test 15's posture for
// receipts: with the real authorizer a revoked credential dies at 401 before
// the service runs, and when an authorizer still vouches for the revoked
// device (a revocation racing authentication) the in-transaction gate refuses
// with 403 and no effect.
func TestHTTPReportDeliveryOpenedRevokedDevice(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, signet.NewRequestAuthorizer(f.store))
	token, deviceID := pairedDevice(t, f, "Doomed device")
	seedSubmittedDelivery(t, f, deviceID, 1, *f.now)
	if _, err := f.service.Revoke(ctx, f.device.ID, deviceID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	before := f.revision(t)

	target := "/attention/items/" + string(f.item.ID) + "/deliveries/ntfy/1/opened"
	response := bearerRequest(t, handler, http.MethodPut, target, "Bearer "+token, nil)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("revoked credential status = %d body=%s, want 401", response.Code, response.Body.String())
	}

	permissive := signet.NewHTTPHandler(f.service, func(*http.Request) (domain.DeviceID, bool) {
		return deviceID, true
	})
	raced := bearerRequest(t, permissive, http.MethodPut, target, "", nil)
	if raced.Code != http.StatusForbidden {
		t.Fatalf("raced revoked device status = %d body=%s, want 403", raced.Code, raced.Body.String())
	}

	if after := f.revision(t); after != before {
		t.Errorf("refused receipt moved the revision %d → %d", before, after)
	}
	row, err := readDelivery(f, f.item.ID, deviceID, "ntfy", 1)
	if err != nil {
		t.Fatalf("readDelivery: %v", err)
	}
	if row.Status != domain.DeliverySubmitted || row.OpenedAt != nil {
		t.Errorf("row = %+v, want untouched submitted: the refused receipt must have no effect", row)
	}
}

// TestHTTPLateNotificationDeepLinksToCanonicalState is §5.14 test 9 over the
// wire: a notification goes out to a second device, the item is resolved on
// another device before the notification is acted on, and the late
// notification still leads only to truth. Its deep link (the published Click
// URL followed against the real handler) returns the canonical resolved
// snapshot, and the decision the notification had invited — prepared against
// the notified state — is refused with the canonical replacement and no side
// effect. The notification was a read-only hint; canonical state lives only
// behind the deep link.
func TestHTTPLateNotificationDeepLinksToCanonicalState(t *testing.T) {
	ctx := context.Background()
	f := newDeliveryFixture(t)
	f.seedDevice(t, "device-2")
	handler := signet.NewHTTPHandler(f.service, func(r *http.Request) (domain.DeviceID, bool) {
		switch r.Header.Get("Authorization") {
		case "Bearer test-device-1":
			return "device-1", true
		case "Bearer test-device-2":
			return "device-2", true
		}
		return "", false
	})

	if _, err := f.service.SubmitDelivery(ctx, f.item.ID, "device-2"); err != nil {
		t.Fatalf("SubmitDelivery: %v", err)
	}
	notifiedItem, notifiedSnap := f.itemSnapshot(t)

	resolve := f.command("cmd-resolve", domain.ActionStop)
	resolve.Payload.ItemVersion = notifiedItem.ItemVersion
	resolve.ExpectedEntityVersion = notifiedSnap.EntityVersion
	if _, err := f.service.Submit(ctx, resolve); err != nil {
		t.Fatalf("Submit(resolve): %v", err)
	}
	resolvedItem, resolvedSnap := f.itemSnapshot(t)
	if resolvedItem.Status != domain.StatusResolved {
		t.Fatalf("item status = %q, want resolved", resolvedItem.Status)
	}

	requests := f.ntfy.recorded(t)
	if len(requests) != 1 {
		t.Fatalf("published %d notifications, want 1", len(requests))
	}
	click, err := url.Parse(requests[0].click)
	if err != nil {
		t.Fatalf("parse click URL %q: %v", requests[0].click, err)
	}
	linked := bearerRequest(t, handler, http.MethodGet, click.Path, "Bearer test-device-2", nil)
	if linked.Code != http.StatusOK {
		t.Fatalf("deep link status = %d body=%s, want 200", linked.Code, linked.Body.String())
	}
	var canonical signet.AttentionItemSnapshot
	if err := json.Unmarshal(linked.Body.Bytes(), &canonical); err != nil {
		t.Fatalf("decode deep-link response: %v", err)
	}
	if canonical.Item.Status != domain.StatusResolved || canonical.Item.ItemVersion != resolvedItem.ItemVersion {
		t.Errorf("deep link = status %q version %d, want the resolved item at version %d",
			canonical.Item.Status, canonical.Item.ItemVersion, resolvedItem.ItemVersion)
	}
	if canonical.EntityVersion != resolvedSnap.EntityVersion {
		t.Errorf("deep link entity_version = %d, want the canonical %d",
			canonical.EntityVersion, resolvedSnap.EntityVersion)
	}

	before := f.revision(t)
	prepared := `{
		"command_id":"cmd-from-notification",
		"device_id":"device-2",
		"expected_entity_version":` + jsonNumber(notifiedSnap.EntityVersion) + `,
		"expected_bindings":{},
		"payload":{
			"item_id":"` + string(f.item.ID) + `",
			"action":"stop",
			"item_version":` + jsonNumber(int64(notifiedItem.ItemVersion)) + `,
			"pr_head_sha":"cafebabe",
			"artifact_digests":[]
		}
	}`
	refused := bearerRequest(t, handler, http.MethodPost, "/commands", "Bearer test-device-2", []byte(prepared))
	if refused.Code != http.StatusConflict {
		t.Fatalf("stale prepared command status = %d body=%s, want 409", refused.Code, refused.Body.String())
	}
	var conflict struct {
		ReplacementItem signet.AttentionItemSnapshot `json:"replacement_item"`
	}
	if err := json.Unmarshal(refused.Body.Bytes(), &conflict); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if conflict.ReplacementItem.Item.Status != domain.StatusResolved {
		t.Errorf("replacement status = %q, want the canonical resolved item", conflict.ReplacementItem.Item.Status)
	}
	if after := f.revision(t); after != before {
		t.Errorf("refused command moved the revision %d → %d", before, after)
	}
	if finalItem, finalSnap := f.itemSnapshot(t); finalItem.ItemVersion != resolvedItem.ItemVersion ||
		finalSnap.EntityVersion != resolvedSnap.EntityVersion {
		t.Errorf("refused command changed the item: version %d entity %d, want %d/%d",
			finalItem.ItemVersion, finalSnap.EntityVersion, resolvedItem.ItemVersion, resolvedSnap.EntityVersion)
	}
}

func authenticatedRequest(t *testing.T, handler http.Handler, method, target string, body *bytes.Reader) *httptest.ResponseRecorder {
	t.Helper()
	var request *http.Request
	if body == nil {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, body)
	}
	request.Header.Set("Authorization", "Bearer test-device-1")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func commandJSON(commandID string, deviceID domain.DeviceID, entityVersion int64, action domain.Action) *bytes.Reader {
	body := `{
		"command_id":"` + commandID + `",
		"device_id":"` + string(deviceID) + `",
		"expected_entity_version":` + jsonNumber(entityVersion) + `,
		"expected_bindings":{},
		"payload":{
			"item_id":"item-1",
			"action":"` + string(action) + `",
			"item_version":1,
			"pr_head_sha":"cafebabe",
			"artifact_digests":[]
		}
	}`
	return bytes.NewReader([]byte(body))
}

func jsonNumber(value int64) string {
	return strings.TrimSpace(string(mustJSON(value)))
}

func mustJSON(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}
