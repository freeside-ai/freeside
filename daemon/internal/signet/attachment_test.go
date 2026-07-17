package signet_test

import (
	"bytes"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// TestGetAttachmentServesStoredBytes pins the digest-addressed read path
// (api/openapi.yaml getAttachment; handler daemon/internal/signet/http.go). The
// route landed with the conversation surface, but only the PUT half was under
// test; these cases cover #128's acceptance 1 (verbatim octet-stream behind the
// device gate) and 2 (the contract 404 for an unknown digest).
func TestGetAttachmentServesStoredBytes(t *testing.T) {
	f := newConversationFixture(t)
	handler := signet.NewHTTPHandler(f.service, headerDeviceAuthorizer)

	blob := []byte("card-image-bytes")
	digest := domain.Digest("sha256:" + hex.EncodeToString(sha256sum(blob)))
	if _, err := f.blobs.Put(digest, bytes.NewReader(blob)); err != nil {
		t.Fatalf("Put blob: %v", err)
	}

	get := func(path, device string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if device != "" {
			req.Header.Set("X-Test-Device", device)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	t.Run("stored digest serves verbatim octet-stream", func(t *testing.T) {
		rec := get("/attachments/"+string(digest), "device-1")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET = %d: %s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
			t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
		}
		if !bytes.Equal(rec.Body.Bytes(), blob) {
			t.Fatalf("body = %q, want %q", rec.Body.Bytes(), blob)
		}
	})

	t.Run("unknown digest returns the contract 404", func(t *testing.T) {
		// Well-formed but unstored: hits ErrBlobNotFound (404), not the
		// ErrInvalidDigest (400) branch a malformed digest would take.
		unknown := "sha256:" + strings.Repeat("11", 32)
		rec := get("/attachments/"+unknown, "device-1")
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET unknown = %d, want 404: %s", rec.Code, rec.Body.String())
		}
		if want := "{\"message\":\"not found\"}\n"; rec.Body.String() != want {
			t.Fatalf("404 body = %q, want %q", rec.Body.String(), want)
		}
	})

	t.Run("missing device credential is rejected before the store", func(t *testing.T) {
		rec := get("/attachments/"+string(digest), "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("GET unauthenticated = %d, want 401: %s", rec.Code, rec.Body.String())
		}
	})
}
