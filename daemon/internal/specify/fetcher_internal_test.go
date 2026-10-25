package specify

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestPublicResearchIPRejectsSpecialUseSpace(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"0.1.2.3",
		"10.0.0.1",
		"100.64.0.1",
		"127.0.0.1",
		"169.254.1.1",
		"192.0.2.1",
		"198.18.0.1",
		"198.51.100.1",
		"203.0.113.1",
		"240.0.0.1",
		"::1",
		"64:ff9b::c0a8:1",
		"100::1",
		"2001:db8::1",
		"2002:c0a8:1::",
		"fc00::1",
		"fe80::1",
	} {
		if publicResearchIP(net.ParseIP(raw)) {
			t.Errorf("publicResearchIP(%q) = true, want false", raw)
		}
	}
	for _, raw := range []string{"93.184.216.34", "2606:4700:4700::1111"} {
		if !publicResearchIP(net.ParseIP(raw)) {
			t.Errorf("publicResearchIP(%q) = false, want true", raw)
		}
	}
}

// TestRecoverRejectsAMixedResearchEnvelope plants an envelope that carries
// both a body and a failure under a fetch ordinal: replay refuses it rather
// than handing either reading to the specifier, and never refetches.
func TestRecoverRejectsAMixedResearchEnvelope(t *testing.T) {
	t.Parallel()
	st, err := store.Open(context.Background(), t.TempDir()+"/state.db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	transported := false
	fetcher, err := NewFetcher(st, blobs, roundTripper(func(*http.Request) (*http.Response, error) {
		transported = true
		return nil, errors.New("unexpected fetch")
	}))
	if err != nil {
		t.Fatal(err)
	}
	request := FetchRequest{URL: "https://docs.example/a", Purpose: "test"}
	if _, err := fetcher.register(t.Context(), "research-inv-mixed-1", "inv-mixed", request, researchEnvelope{
		URL: request.URL, Purpose: request.Purpose, FinalURL: request.URL, Status: http.StatusNotFound,
		BodyBase64: base64.StdEncoding.EncodeToString([]byte("text")), Failure: "HTTP 404",
	}, 100); err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.FetchOrRecordFailure(t.Context(), "inv-mixed", 1, request,
		ManagedRepository{}, []string{"https://docs.example"}, 100)
	if !errors.Is(err, domain.ErrParentKeyMismatch) || transported {
		t.Fatalf("mixed envelope replay = %v, transported = %t", err, transported)
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
