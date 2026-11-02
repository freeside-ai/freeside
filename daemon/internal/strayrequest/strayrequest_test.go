package strayrequest_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/strayrequest"
)

// seen is what the wrapped handler observed of one request.
type seen struct {
	method, path, rawQuery, host, header, body string
}

// TestIgnore drives a real server so the request forms (origin-form and a
// proxy client's absolute-form) are the ones a listener actually parses.
func TestIgnore(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		hits []seen
	)
	server := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		mu.Lock()
		hits = append(hits, seen{r.Method, r.URL.Path, r.URL.RawQuery, r.URL.Host, r.Header.Get("X-Probe"), string(body)})
		mu.Unlock()
		w.WriteHeader(http.StatusTeapot)
	})))
	t.Cleanup(server.Close)
	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxied := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(serverURL)}}

	tests := []struct {
		name   string
		client *http.Client
		method string
		target string
		body   string
		want   *seen // nil: the request must not reach the handler
	}{
		{name: "root GET", client: server.Client(), method: http.MethodGet, target: server.URL + "/"},
		{name: "bare origin GET", client: server.Client(), method: http.MethodGet, target: server.URL},
		{name: "root GET with query", client: server.Client(), method: http.MethodGet, target: server.URL + "/?x=1"},
		{
			name: "non-root GET", client: server.Client(), method: http.MethodGet, target: server.URL + "/repos/o/r?page=2",
			want: &seen{method: http.MethodGet, path: "/repos/o/r", rawQuery: "page=2", header: "kept"},
		},
		{
			name: "root POST", client: server.Client(), method: http.MethodPost, target: server.URL + "/", body: "payload",
			want: &seen{method: http.MethodPost, path: "/", header: "kept", body: "payload"},
		},
		{
			name: "root HEAD", client: server.Client(), method: http.MethodHead, target: server.URL + "/",
			want: &seen{method: http.MethodHead, path: "/", header: "kept"},
		},
		{
			name: "proxied root GET", client: proxied, method: http.MethodGet, target: "http://upstream.test/",
			want: &seen{method: http.MethodGet, path: "/", host: "upstream.test", header: "kept"},
		},
	}
	for _, tc := range tests {
		mu.Lock()
		hits = nil
		mu.Unlock()

		req, err := http.NewRequestWithContext(t.Context(), tc.method, tc.target, strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		req.Header.Set("X-Probe", "kept")
		resp, err := tc.client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		_ = resp.Body.Close()

		mu.Lock()
		got := hits
		mu.Unlock()
		if tc.want == nil {
			if resp.StatusCode != http.StatusNotFound || len(got) != 0 {
				t.Errorf("%s: status %d, handler saw %v; want 404 and no handler call", tc.name, resp.StatusCode, got)
			}
			continue
		}
		if resp.StatusCode != http.StatusTeapot || len(got) != 1 || got[0] != *tc.want {
			t.Errorf("%s: status %d, handler saw %v; want 418 and %v", tc.name, resp.StatusCode, got, *tc.want)
		}
	}
}

// TestProbe: the probe is the stray request Ignore exists for.
func TestProbe(t *testing.T) {
	t.Parallel()
	var (
		mu  sync.Mutex
		got []string
	)
	// Unwrapped, so a real local probe may arrive too; it looks the same.
	bare := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.Method+" "+r.RequestURI+" "+r.UserAgent())
		mu.Unlock()
	}))
	t.Cleanup(bare.Close)
	strayrequest.Probe(t, bare.URL)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Contains(got, "GET / node") {
		t.Errorf("unwrapped server saw %v, want GET / node", got)
	}

	wrapped := httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("wrapped handler saw %s %s", r.Method, r.RequestURI)
	})))
	t.Cleanup(wrapped.Close)
	strayrequest.Probe(t, wrapped.URL)
}
