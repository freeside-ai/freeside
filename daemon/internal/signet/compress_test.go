package signet_test

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// compressRequest sends one request with the given Authorization and
// Accept-Encoding headers, each omitted when empty.
func compressRequest(handler http.Handler, target, authorization, acceptEncoding string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, target, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	if acceptEncoding != "" {
		request.Header.Set("Accept-Encoding", acceptEncoding)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func gunzip(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	return decoded
}

// TestHTTPGzipsJSONForAcceptingClients covers #2523: a JSON response is
// gzipped when the client accepts it, decodes to exactly the plain body, and
// varies on Accept-Encoding either way.
func TestHTTPGzipsJSONForAcceptingClients(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	seedSyncResources(t, f)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)

	plain := compressRequest(handler, "/sync/bootstrap", "Bearer test-device-1", "")
	if plain.Code != http.StatusOK {
		t.Fatalf("plain bootstrap = %d: %s", plain.Code, plain.Body.String())
	}
	if got := plain.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("plain Content-Encoding = %q, want none", got)
	}
	if got := plain.Header().Get("Vary"); got != "Accept-Encoding" {
		t.Fatalf("plain Vary = %q, want Accept-Encoding", got)
	}

	for _, acceptEncoding := range []string{"gzip", "deflate, gzip;q=0.8", "*"} {
		t.Run(acceptEncoding, func(t *testing.T) {
			compressed := compressRequest(handler, "/sync/bootstrap", "Bearer test-device-1", acceptEncoding)
			if compressed.Code != http.StatusOK {
				t.Fatalf("compressed bootstrap = %d", compressed.Code)
			}
			for header, want := range map[string]string{
				"Content-Type":     "application/json",
				"Content-Encoding": "gzip",
				"Vary":             "Accept-Encoding",
			} {
				if got := compressed.Header().Get(header); got != want {
					t.Fatalf("%s = %q, want %q", header, got, want)
				}
			}
			if decoded := gunzip(t, compressed.Body.Bytes()); !bytes.Equal(decoded, plain.Body.Bytes()) {
				t.Fatalf("gunzipped body = %s, want %s", decoded, plain.Body.Bytes())
			}
		})
	}

	for _, acceptEncoding := range []string{"identity", "gzip;q=0", "*;q=0"} {
		t.Run(acceptEncoding, func(t *testing.T) {
			refused := compressRequest(handler, "/sync/bootstrap", "Bearer test-device-1", acceptEncoding)
			if got := refused.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want none", got)
			}
			if !bytes.Equal(refused.Body.Bytes(), plain.Body.Bytes()) {
				t.Fatalf("body = %s, want the plain body", refused.Body.Bytes())
			}
		})
	}
}

// TestHTTPGzipKeepsErrorStatusAndHeaders pins that a JSON error is
// compressed like any other JSON response without losing its status or the
// headers its handler set before writing.
func TestHTTPGzipKeepsErrorStatusAndHeaders(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)

	plain := compressRequest(handler, "/sync/revision", "", "")
	compressed := compressRequest(handler, "/sync/revision", "", "gzip")
	if compressed.Code != http.StatusUnauthorized || compressed.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("response = %d WWW-Authenticate %q, want 401 Bearer",
			compressed.Code, compressed.Header().Get("WWW-Authenticate"))
	}
	if got := compressed.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if decoded := gunzip(t, compressed.Body.Bytes()); !bytes.Equal(decoded, plain.Body.Bytes()) {
		t.Fatalf("gunzipped body = %s, want %s", decoded, plain.Body.Bytes())
	}
}

// TestHTTPNeverGzipsAttachments pins the non-goal: attachment blobs are
// already-compressed images, so they are served verbatim even to a client
// that accepts gzip.
func TestHTTPNeverGzipsAttachments(t *testing.T) {
	t.Parallel()
	f := newConversationFixture(t)
	handler := signet.NewHTTPHandler(f.service, headerDeviceAuthorizer)
	blob := []byte("card-image-bytes")
	digest := domain.Digest("sha256:" + hex.EncodeToString(sha256sum(blob)))
	if _, err := f.blobs.Put(digest, bytes.NewReader(blob)); err != nil {
		t.Fatalf("Put blob: %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/attachments/"+string(digest), nil)
	request.Header.Set("X-Test-Device", "device-1")
	request.Header.Set("Accept-Encoding", "gzip")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("GET = %d: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Encoding"); got != "" {
		t.Fatalf("Content-Encoding = %q, want none", got)
	}
	if got := response.Header().Get("Vary"); got != "" {
		t.Fatalf("Vary = %q, want none", got)
	}
	if !bytes.Equal(response.Body.Bytes(), blob) {
		t.Fatalf("body = %q, want %q", response.Body.Bytes(), blob)
	}
}
