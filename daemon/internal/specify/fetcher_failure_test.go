package specify_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
)

// failureTransport answers each docs.example path with one outcome a
// specifier's research can meet.
func failureTransport(calls *atomic.Int64) http.RoundTripper {
	return roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls.Add(1)
		switch request.URL.Path {
		case "/server-error":
			return response(request, http.StatusServiceUnavailable, "text/plain", "unavailable body"), nil
		case "/network-error":
			return nil, errors.New("network unavailable")
		case "/loop":
			loop := response(request, http.StatusFound, "", "")
			loop.Header.Set("Location", "https://docs.example/loop")
			return loop, nil
		case "/offsite":
			offsite := response(request, http.StatusFound, "", "")
			offsite.Header.Set("Location", "https://evil.example/landing")
			return offsite, nil
		case "/forged-final":
			forged := request.Clone(request.Context())
			forged.URL, _ = request.URL.Parse("https://evil.example/landing")
			return response(forged, http.StatusNotFound, "text/plain", "forged"), nil
		case "/no-final":
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(""))}, nil
		case "/large":
			return response(request, http.StatusOK, "text/plain", "too large"), nil
		case "/large-content-type":
			return response(request, http.StatusOK, strings.Repeat("x", 9<<10), "ok"), nil
		case "/non-utf8":
			return &http.Response{
				StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/plain"}},
				Body: io.NopCloser(bytes.NewReader([]byte{0xff})), Request: request,
			}, nil
		case "/odd-status":
			return response(request, 999, "text/plain", "odd"), nil
		default:
			return response(request, http.StatusOK, "text/plain", "ok"), nil
		}
	})
}

func newFailureFetcher(t *testing.T) (*specify.Fetcher, *signet.BlobStore, *atomic.Int64) {
	t.Helper()
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int64{}
	fetcher, err := specify.NewFetcher(openStore(t), blobs, failureTransport(calls))
	if err != nil {
		t.Fatal(err)
	}
	return fetcher, blobs, calls
}

func recordFailure(t *testing.T, fetcher *specify.Fetcher, id, path string) (specify.ResearchArtifact, error) {
	t.Helper()
	return fetcher.FetchOrRecordFailure(t.Context(), domain.InvocationID(id), 1,
		specify.FetchRequest{URL: "https://docs.example/" + path, Purpose: "test"},
		specify.ManagedRepository{}, []string{"https://docs.example"}, 100)
}

// TestFetchOrRecordFailureStoresAFailureArtifact covers the outcomes that
// hand back to the specifier: an error status and a transport error each
// become a replayable research artifact with no body and a named cause.
func TestFetchOrRecordFailureStoresAFailureArtifact(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path   string
		status int
		want   string
	}{
		{"server-error", http.StatusServiceUnavailable, `fetch research "https://docs.example/server-error": HTTP 503`},
		{"network-error", 0, "network unavailable"},
		{"loop", 0, "research redirect limit 5 exceeded"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			fetcher, blobs, calls := newFailureFetcher(t)
			got, err := recordFailure(t, fetcher, "inv-"+tc.path, tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if got.Artifact.Type != domain.ArtifactKindResearch || got.FinalURL == "" {
				t.Fatalf("result = %+v", got)
			}
			envelope := storedEnvelope(t, blobs, got)
			evidence, err := specify.DecodeResearchEvidence(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if evidence.Source.Status != tc.status || evidence.Body != "" || evidence.Source.ContentType != "" ||
				!strings.Contains(evidence.Source.Failure, tc.want) {
				t.Fatalf("evidence = %+v, want status %d naming %q", evidence, tc.status, tc.want)
			}
			if strings.Contains(string(envelope), "unavailable body") {
				t.Fatal("failure envelope stored the server's error body")
			}
			before := calls.Load()
			recovered, err := recordFailure(t, fetcher, "inv-"+tc.path, tc.path)
			if err != nil || recovered.Artifact.Digest != got.Artifact.Digest || calls.Load() != before {
				t.Fatalf("replay = %+v, %v; transport calls %d -> %d", recovered, err, before, calls.Load())
			}
		})
	}
}

// TestFetchKeepsFailingOnAFailedFetch pins the task namer's contract: Fetch
// still fails on what FetchOrRecordFailure records.
func TestFetchKeepsFailingOnAFailedFetch(t *testing.T) {
	t.Parallel()
	fetcher, _, _ := newFailureFetcher(t)
	for _, path := range []string{"server-error", "network-error"} {
		_, err := fetcher.Fetch(t.Context(), domain.InvocationID("inv-"+path), 1,
			specify.FetchRequest{URL: "https://docs.example/" + path, Purpose: "test"},
			specify.ManagedRepository{}, []string{"https://docs.example"}, 100)
		if !errors.Is(err, specify.ErrResearchFetchFailed) {
			t.Errorf("%s: Fetch error = %v, want ErrResearchFetchFailed", path, err)
		}
	}
}

// TestFetchOrRecordFailureStillFailsOnPolicyAndLimits covers the outcomes
// that are not a fetch the specifier can learn from: they fail the run
// exactly as Fetch does.
func TestFetchOrRecordFailureStillFailsOnPolicyAndLimits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		path string
		want error
	}{
		{"offsite", specify.ErrResearchURLRefused},
		{"forged-final", specify.ErrResearchURLRefused},
		{"no-final", specify.ErrResearchFetchFailed},
		{"large-content-type", specify.ErrResearchTooLarge},
		{"non-utf8", specify.ErrResearchFetchFailed},
		{"odd-status", specify.ErrResearchFetchFailed},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			fetcher, _, _ := newFailureFetcher(t)
			if _, err := recordFailure(t, fetcher, "inv-"+tc.path, tc.path); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
	fetcher, _, calls := newFailureFetcher(t)
	_, err := fetcher.FetchOrRecordFailure(t.Context(), "inv-large", 1,
		specify.FetchRequest{URL: "https://docs.example/large", Purpose: "test"},
		specify.ManagedRepository{}, []string{"https://docs.example"}, 3)
	if !errors.Is(err, specify.ErrResearchTooLarge) {
		t.Fatalf("oversized body error = %v, want ErrResearchTooLarge", err)
	}
	_, err = fetcher.FetchOrRecordFailure(t.Context(), "inv-off-allowlist", 1,
		specify.FetchRequest{URL: "https://evil.example/page", Purpose: "test"},
		specify.ManagedRepository{}, []string{"https://docs.example"}, 100)
	if !errors.Is(err, specify.ErrResearchURLRefused) || calls.Load() != 1 {
		t.Fatalf("off-allowlist error = %v after %d transport calls", err, calls.Load())
	}
}

// TestFetchOrRecordFailureRefusesANonPublicAddress drives the production
// transport: a host that resolves to loopback is refused, not recorded.
func TestFetchOrRecordFailureRefusesANonPublicAddress(t *testing.T) {
	t.Parallel()
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fetcher, err := specify.NewFetcher(openStore(t), blobs, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fetcher.FetchOrRecordFailure(t.Context(), "inv-loopback", 1,
		specify.FetchRequest{URL: "https://localhost/page", Purpose: "test"},
		specify.ManagedRepository{}, []string{"https://localhost"}, 100)
	if !errors.Is(err, specify.ErrResearchURLRefused) || !strings.Contains(err.Error(), "non-public address") {
		t.Fatalf("loopback error = %v, want a non-public-address refusal", err)
	}
}

func TestDecodeResearchEvidenceRejectsMixedOutcomes(t *testing.T) {
	t.Parallel()
	type envelope struct {
		URL         string `json:"url"`
		Purpose     string `json:"purpose"`
		FinalURL    string `json:"final_url"`
		Status      int    `json:"status"`
		ContentType string `json:"content_type"`
		BodyBase64  string `json:"body_base64"`
		Failure     string `json:"failure,omitempty"`
	}
	base := envelope{URL: "https://docs.example/a", Purpose: "test", FinalURL: "https://docs.example/a"}
	body := base64.StdEncoding.EncodeToString([]byte("text"))
	for name, tc := range map[string]struct {
		mutate func(*envelope)
		valid  bool
	}{
		"fetched":                 {func(e *envelope) { e.Status, e.BodyBase64 = 200, body }, true},
		"recorded status":         {func(e *envelope) { e.Status, e.Failure = 404, "HTTP 404" }, true},
		"recorded no response":    {func(e *envelope) { e.Failure = "network unavailable" }, true},
		"failure with a body":     {func(e *envelope) { e.Status, e.Failure, e.BodyBase64 = 404, "HTTP 404", body }, false},
		"failure with 2xx":        {func(e *envelope) { e.Status, e.Failure = 200, "HTTP 200" }, false},
		"failure with a type":     {func(e *envelope) { e.Status, e.Failure, e.ContentType = 404, "HTTP 404", "text/plain" }, false},
		"failure with 1xx":        {func(e *envelope) { e.Status, e.Failure = 101, "HTTP 101" }, false},
		"error status unrecorded": {func(e *envelope) { e.Status = 404 }, false},
		"oversized failure":       {func(e *envelope) { e.Status, e.Failure = 404, strings.Repeat("x", (8<<10)+1) }, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			value := base
			tc.mutate(&value)
			data, err := json.Marshal(value)
			if err != nil {
				t.Fatal(err)
			}
			evidence, err := specify.DecodeResearchEvidence(data)
			if tc.valid && (err != nil || evidence.Source.Failure != value.Failure) {
				t.Fatalf("valid envelope = %+v, %v", evidence, err)
			}
			if !tc.valid && !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("mixed envelope error = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

// TestDecodeResearchEvidenceAcceptsAPreFailureEnvelope pins the bytes of an
// envelope stored before the failure field existed: it still decodes, so its
// digest still names valid research.
func TestDecodeResearchEvidenceAcceptsAPreFailureEnvelope(t *testing.T) {
	t.Parallel()
	const stored = `{"url":"https://docs.example/a","purpose":"test","final_url":"https://docs.example/a",` +
		`"status":200,"content_type":"text/plain","body_base64":"dGV4dA=="}`
	evidence, err := specify.DecodeResearchEvidence([]byte(stored))
	if err != nil || evidence.Body != "text" || evidence.Source.Failure != "" {
		t.Fatalf("pre-failure envelope = %+v, %v", evidence, err)
	}
}
