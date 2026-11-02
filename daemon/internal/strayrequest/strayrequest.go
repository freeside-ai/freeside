// Package strayrequest keeps requests that other local programs send to a
// test's loopback server away from the fake behind it.
//
// Some desktop tools probe listening loopback ports: T3 Code's Node
// process sends GET / to them (#2465). A daemon fake on an ephemeral
// 127.0.0.1 port treats every request as the code under test's, so one
// probe fails a test that counts, records, or refuses requests.
//
// Ignore answers exactly an origin-form GET / (any query string) with 404
// and passes every other request through, so a fake still sees each
// request the code under test sends. No daemon client requests the root
// path; a fake whose code under test does must stay unwrapped. A proxied
// request (absolute-form, such as GET http://host/) always reaches the
// fake.
//
// Usage (from any _test.go), for every httptest server in daemon/:
//
//	server := httptest.NewServer(strayrequest.Ignore(handler))
//
// A regression test sends the probe itself with Probe before the operation
// under test.
package strayrequest

import (
	"io"
	"net/http"
	"testing"
)

// Ignore returns a handler that answers an origin-form GET / with 404 and
// passes every other request to h unchanged.
func Ignore(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Host == "" && r.URL.Path == "/" {
			http.NotFound(w, r)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Probe sends serverURL the request T3 Code was seen sending, GET / with a
// node user agent, and fails the test only if the request cannot be sent:
// what the server answers is the wrapped fake's business.
func Probe(t *testing.T, serverURL string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, serverURL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("User-Agent", "node")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stray probe: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
