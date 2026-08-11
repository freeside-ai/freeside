package elaborate_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestFetcherStoresDigestAddressedResearch(t *testing.T) {
	st := openStore(t)
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		if request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" {
			t.Fatal("credential-bearing header reached research transport")
		}
		return response(request, http.StatusOK, "text/plain", "bounded facts"), nil
	})
	fetcher, err := elaborate.NewFetcher(st, blobs, transport)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fetcher.Fetch(t.Context(), "inv-elab-1", 1, elaborate.FetchRequest{
		URL: "https://DOCS.example/fact?q=1", Purpose: "confirm the contract",
	}, []string{"https://docs.example"}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if got.Artifact.Type != domain.ArtifactKindResearch ||
		got.Artifact.Provenance.ProducerClass != domain.ProducerDaemon ||
		got.FinalURL != "https://DOCS.example/fact?q=1" {
		t.Fatalf("result = %+v", got)
	}
	if ok, err := blobs.Verify(got.Artifact.Digest); err != nil || !ok {
		t.Fatalf("blob verify = %v, %v", ok, err)
	}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		stored, err := tx.GetArtifact(t.Context(), got.Artifact.ID)
		if err == nil && stored.Digest != got.Artifact.Digest {
			t.Fatalf("stored digest = %s", stored.Digest)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	recovered, err := fetcher.Fetch(t.Context(), "inv-elab-1", 1, elaborate.FetchRequest{
		URL: "https://DOCS.example/fact?q=1", Purpose: "confirm the contract",
	}, []string{"https://docs.example"}, 1024)
	if err != nil || recovered.Artifact.Digest != got.Artifact.Digest || calls.Load() != 1 {
		t.Fatalf("recovery = %+v, %v; transport calls = %d", recovered, err, calls.Load())
	}
	_, err = fetcher.Fetch(t.Context(), "inv-elab-1", 1, elaborate.FetchRequest{
		URL: "https://DOCS.example/fact?q=1", Purpose: "retarget the persisted ordinal",
	}, []string{"https://docs.example"}, 1024)
	if !errors.Is(err, domain.ErrParentKeyMismatch) || calls.Load() != 1 {
		t.Fatalf("retarget recovery = %v; transport calls = %d", err, calls.Load())
	}
}

func TestFetcherAdversarialURLSpace(t *testing.T) {
	st := openStore(t)
	blobs, _ := signet.NewBlobStore(t.TempDir())
	var calls atomic.Int64
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		return response(request, http.StatusOK, "text/plain", "ok"), nil
	})
	fetcher, _ := elaborate.NewFetcher(st, blobs, transport)
	cases := []struct {
		name  string
		url   string
		allow []string
		ok    bool
	}{
		{"host case", "https://DOCS.EXAMPLE/a", []string{"https://docs.example"}, true},
		{"default port", "https://docs.example:443/a", []string{"https://docs.example"}, true},
		{"different port", "https://docs.example:8443/a", []string{"https://docs.example"}, false},
		{"allowed port", "https://docs.example:8443/a", []string{"https://docs.example:8443"}, true},
		{"IP literal", "https://127.0.0.1/a", []string{"https://docs.example"}, false},
		{"IPv6 literal", "https://[::1]/a", []string{"https://docs.example"}, false},
		{"userinfo", "https://user:pass@docs.example/a", []string{"https://docs.example"}, false},
		{"HTTP", "http://docs.example/a", []string{"https://docs.example"}, false},
		{"suffix", "https://docs.example.evil/a", []string{"https://docs.example"}, false},
		{"fragment", "https://docs.example/a#secret", []string{"https://docs.example"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := calls.Load()
			_, err := fetcher.Fetch(t.Context(), domain.InvocationID("inv-"+strings.ReplaceAll(tc.name, " ", "-")), 1,
				elaborate.FetchRequest{URL: tc.url, Purpose: "test"}, tc.allow, 16)
			if tc.ok && err != nil {
				t.Fatal(err)
			}
			if !tc.ok && !errors.Is(err, elaborate.ErrResearchURLRefused) {
				t.Fatalf("error = %v, want ErrResearchURLRefused", err)
			}
			if !tc.ok && calls.Load() != before {
				t.Fatal("refused URL reached transport")
			}
		})
	}
}

func TestFetcherRedirectAndSizePolicies(t *testing.T) {
	st := openStore(t)
	blobs, _ := signet.NewBlobStore(t.TempDir())
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.Path {
		case "/offsite":
			result := response(request, http.StatusFound, "", "")
			result.Header.Set("Location", "https://evil.example/landing")
			return result, nil
		case "/large":
			return response(request, http.StatusOK, "text/plain", "too large"), nil
		case "/forged-final":
			forged := request.Clone(request.Context())
			forged.URL, _ = request.URL.Parse("https://evil.example/landing")
			return response(forged, http.StatusOK, "text/plain", "forged"), nil
		case "/server-error":
			return response(request, http.StatusServiceUnavailable, "text/plain", "unavailable"), nil
		case "/network-error":
			return nil, errors.New("network unavailable")
		default:
			return response(request, http.StatusOK, "text/plain", "ok"), nil
		}
	})
	fetcher, _ := elaborate.NewFetcher(st, blobs, transport)
	_, err := fetcher.Fetch(t.Context(), "inv-redirect", 1,
		elaborate.FetchRequest{URL: "https://docs.example/offsite", Purpose: "test"},
		[]string{"https://docs.example"}, 100)
	if !errors.Is(err, elaborate.ErrResearchURLRefused) {
		t.Fatalf("redirect error = %v", err)
	}
	if !elaborate.IsResearchRequestFailure(err) {
		t.Fatalf("redirect is not classified as a request failure: %v", err)
	}
	_, err = fetcher.Fetch(t.Context(), "inv-large", 1,
		elaborate.FetchRequest{URL: "https://docs.example/large", Purpose: "test"},
		[]string{"https://docs.example"}, 3)
	if !errors.Is(err, elaborate.ErrResearchTooLarge) {
		t.Fatalf("size error = %v", err)
	}
	if !elaborate.IsResearchRequestFailure(err) {
		t.Fatalf("oversized response is not classified as a request failure: %v", err)
	}
	_, err = fetcher.Fetch(t.Context(), "inv-forged-final", 1,
		elaborate.FetchRequest{URL: "https://docs.example/forged-final", Purpose: "test"},
		[]string{"https://docs.example"}, 100)
	if !errors.Is(err, elaborate.ErrResearchURLRefused) {
		t.Fatalf("forged final URL error = %v", err)
	}
	for _, path := range []string{"server-error", "network-error"} {
		_, err = fetcher.Fetch(t.Context(), domain.InvocationID("inv-"+path), 1,
			elaborate.FetchRequest{URL: "https://docs.example/" + path, Purpose: "test"},
			[]string{"https://docs.example"}, 100)
		if !elaborate.IsResearchRequestFailure(err) {
			t.Errorf("%s error is not classified as a request failure: %v", path, err)
		}
	}
	_, err = fetcher.Fetch(t.Context(), "inv-unbounded", 1,
		elaborate.FetchRequest{URL: "https://docs.example/large", Purpose: "test"},
		[]string{"https://docs.example"}, elaborate.MaxResearchResponseBytes+1)
	if err == nil {
		t.Fatal("fetcher accepted a response bound above its hard maximum")
	}
	if elaborate.IsResearchRequestFailure(err) {
		t.Fatalf("invalid daemon response bound classified as request failure: %v", err)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/state.db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func response(request *http.Request, status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status, Header: http.Header{"Content-Type": []string{contentType}},
		Body: io.NopCloser(strings.NewReader(body)), Request: request,
	}
}
