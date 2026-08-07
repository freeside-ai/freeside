package publish

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecodeResponseSizeBound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		size    int
		wantErr string
	}{
		{name: "exactly at bound", size: maxForgeResponseBytes},
		{name: "one byte over", size: maxForgeResponseBytes + 1, wantErr: "response exceeds the 16777216-byte bound"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			const prefix = `{"value":"`
			const suffix = `"}`
			body := prefix + strings.Repeat("x", tt.size-len(prefix)-len(suffix)) + suffix
			var decoded struct {
				Value string `json:"value"`
			}
			err := decodeResponse(strings.NewReader(body), &decoded)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("decodeResponse at bound: %v", err)
				}
				if len(decoded.Value) != tt.size-len(prefix)-len(suffix) {
					t.Errorf("decoded value length = %d", len(decoded.Value))
				}
				return
			}
			if err == nil || err.Error() != tt.wantErr {
				t.Fatalf("decodeResponse error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestDecodeResponseOversizeErrorDoesNotDiscloseBody(t *testing.T) {
	t.Parallel()
	const sentinel = "FORGE_RESPONSE_SECRET_SENTINEL"
	for _, body := range []string{
		`{"value":"` + sentinel + strings.Repeat("x", maxForgeResponseBytes) + `"}`,
		`{malformed:` + sentinel + strings.Repeat("x", maxForgeResponseBytes),
	} {
		var decoded any
		err := decodeResponse(strings.NewReader(body), &decoded)
		if err == nil {
			t.Fatal("decodeResponse accepted an oversized response")
		}
		if err.Error() != "response exceeds the 16777216-byte bound" {
			t.Errorf("decodeResponse error = %v, want content-free size refusal", err)
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("oversize error disclosed response content: %v", err)
		}
	}
}

func TestDecodeResponseRejectsTrailingData(t *testing.T) {
	t.Parallel()
	var decoded struct {
		Value string `json:"value"`
	}
	err := decodeResponse(strings.NewReader(`{"value":"accepted"} trailing`), &decoded)
	if err == nil || err.Error() != "trailing data after the response document" {
		t.Fatalf("decodeResponse error = %v, want trailing-data refusal", err)
	}
}

func TestExchangeCodeRejectsTrailingDataBeforeSaving(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fixture, err := os.ReadFile(filepath.Join("testdata", "conversion-response.json"))
		if err != nil {
			t.Errorf("read fixture: %v", err)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write(append(fixture, []byte(" trailing")...))
	}))
	defer server.Close()

	registrar := NewRegistrar(nil, server.Client(), server.URL, "https://github.example")
	saved := false
	_, err := registrar.exchangeCode(
		context.Background(),
		"CODE123",
		RegistrationTarget{
			Owner:        "freeside-ai",
			OwnerID:      991337,
			Organization: true,
			Visibility:   AppVisibilityPrivate,
		},
		func(AppCredentials) error {
			saved = true
			return errors.New("save must not be called")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "trailing data after the response document") {
		t.Fatalf("exchangeCode error = %v, want trailing-data refusal", err)
	}
	if saved {
		t.Error("exchangeCode saved credentials from a response with trailing data")
	}
}
