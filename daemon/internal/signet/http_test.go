package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
