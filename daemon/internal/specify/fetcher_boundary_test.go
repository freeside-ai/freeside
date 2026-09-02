package specify

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type boundaryRoundTrip func(*http.Request) (*http.Response, error)

func (f boundaryRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestFetcherAcceptsExactProductionInputBoundary(t *testing.T) {
	const rawURL = "https://docs.example/exact"
	body, contentType := exactBoundaryEnvelopeBody(t, rawURL)
	root := t.TempDir()
	st, err := store.Open(context.Background(), root+"/state.db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(root + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	fetcher, err := NewFetcher(st, blobs, boundaryRoundTrip(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{contentType}},
			Body:       io.NopCloser(bytes.NewReader(body)), Request: request,
		}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := fetcher.Fetch(t.Context(), "inv-exact-boundary", 1,
		FetchRequest{URL: rawURL, Purpose: "test"},
		[]string{"https://docs.example"}, int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	reader, err := blobs.OpenContext(t.Context(), artifact.Artifact.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	stored, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != int(exec.ProductionMaxInputBytes) {
		t.Fatalf("stored exact-boundary envelope = %d bytes, want %d",
			len(stored), exec.ProductionMaxInputBytes)
	}
}

func exactBoundaryEnvelopeBody(t *testing.T, rawURL string) ([]byte, string) {
	t.Helper()
	limit := int(exec.ProductionMaxInputBytes)
	for padding := 0; padding < 4; padding++ {
		contentType := strings.Repeat("x", padding)
		empty, err := json.Marshal(researchEnvelope{
			URL: rawURL, Purpose: "test", FinalURL: rawURL,
			Status: http.StatusOK, ContentType: contentType, BodyBase64: "",
		})
		if err != nil {
			t.Fatal(err)
		}
		encodedBytes := limit - len(empty)
		if encodedBytes <= 0 || encodedBytes%4 != 0 {
			continue
		}
		body := bytes.Repeat([]byte("x"), encodedBytes/4*3)
		envelope, err := json.Marshal(researchEnvelope{
			URL: rawURL, Purpose: "test", FinalURL: rawURL,
			Status: http.StatusOK, ContentType: contentType,
			BodyBase64: base64.StdEncoding.EncodeToString(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(envelope) == limit {
			return body, contentType
		}
	}
	t.Fatal("could not construct an exact-boundary research envelope")
	return nil, ""
}
