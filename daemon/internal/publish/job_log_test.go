package publish_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

const jobLogPath = "/repos/o/r/actions/jobs/41/logs"

// jobLogServers is GitHub's API, which redirects the job log read, and the
// storage host the redirect names. download records every request the
// storage host saw.
type jobLogServers struct {
	api      *httptest.Server
	storage  *httptest.Server
	mu       sync.Mutex
	download []*http.Request
}

func newJobLogServers(t *testing.T, location func(storage string) string, serve http.HandlerFunc) *jobLogServers {
	t.Helper()
	s := &jobLogServers{}
	s.storage = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.download = append(s.download, r.Clone(context.Background()))
		s.mu.Unlock()
		serve(w, r)
	}))
	t.Cleanup(s.storage.Close)
	s.api = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != jobLogPath || r.Header.Get("Authorization") == "" {
			http.Error(w, "unexpected", http.StatusBadRequest)
			return
		}
		w.Header().Set("Location", location(s.storage.URL))
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(s.api.Close)
	return s
}

func (s *jobLogServers) reconciler() *publish.Reconciler {
	r := publish.NewReconciler(testTokenSource(), s.storage.Client(), s.api.URL)
	target, _ := url.Parse(s.storage.URL)
	r.AllowJobLogHost(target.Hostname())
	return r
}

func signedLogURL(storage string) string { return storage + "/logs/41?sig=signed-secret" }

// TestReadJobLogTailFollowsTheRedirectWithoutTheToken: the token goes to
// GitHub only. The signed URL is fetched once with no credential, and the
// read keeps the log's tail.
func TestReadJobLogTailFollowsTheRedirectWithoutTheToken(t *testing.T) {
	t.Parallel()
	log := append(bytes.Repeat([]byte("x"), publish.MaxJobLogTailBytes), []byte("\n--- FAIL: TestHandle\n")...)
	s := newJobLogServers(t, signedLogURL, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(log)
	})
	tail, err := s.reconciler().ReadJobLogTail(context.Background(), "o/r", 41)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != publish.MaxJobLogTailBytes || !bytes.HasSuffix(tail, []byte("--- FAIL: TestHandle\n")) {
		t.Fatalf("tail is %d bytes ending %q", len(tail), tail[max(0, len(tail)-32):])
	}
	if len(s.download) != 1 {
		t.Fatalf("storage saw %d requests, want 1", len(s.download))
	}
	for name := range s.download[0].Header {
		if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Cookie") {
			t.Fatalf("the download carried %s", name)
		}
	}
	if s.download[0].URL.RawQuery != "sig=signed-secret" {
		t.Fatalf("download query = %q", s.download[0].URL.RawQuery)
	}
	if got, want := s.download[0].Header.Get("Range"), "bytes=-"+strconv.Itoa(publish.MaxJobLogTailBytes); got != want {
		t.Fatalf("download Range = %q, want %q", got, want)
	}
}

// TestReadJobLogTailHonorsTheRange: a storage host that answers the tail
// request with the tail alone is read as is, and one that finds an empty log
// to take no tail of reads as an empty log.
func TestReadJobLogTailHonorsTheRange(t *testing.T) {
	t.Parallel()
	s := newJobLogServers(t, signedLogURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("--- FAIL: TestHandle\n"))
	})
	if tail, err := s.reconciler().ReadJobLogTail(context.Background(), "o/r", 41); err != nil ||
		string(tail) != "--- FAIL: TestHandle\n" {
		t.Fatalf("partial content = %q, %v", tail, err)
	}
	empty := newJobLogServers(t, signedLogURL, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	})
	if tail, err := empty.reconciler().ReadJobLogTail(context.Background(), "o/r", 41); err != nil || len(tail) != 0 {
		t.Fatalf("unsatisfiable range = %q, %v; want an empty log", tail, err)
	}
}

// TestReadJobLogTailKeepsTheSignedURLOutOfErrors: the signed URL grants the
// log to whoever holds it, so a failed download names only its host.
func TestReadJobLogTailKeepsTheSignedURLOutOfErrors(t *testing.T) {
	t.Parallel()
	s := newJobLogServers(t, signedLogURL, func(w http.ResponseWriter, _ *http.Request) {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	_, err := s.reconciler().ReadJobLogTail(context.Background(), "o/r", 41)
	if err == nil || errors.Is(err, publish.ErrJobLogUnavailable) {
		t.Fatalf("dropped download = %v, want a retryable error", err)
	}
	if strings.Contains(err.Error(), "signed-secret") || strings.Contains(err.Error(), "/logs/41") {
		t.Fatalf("download error %q names the signed URL", err)
	}
}

// TestReadTail: the tail survives reads that split it, a body within the
// limit is kept whole, and one past it is refused as too long.
func TestReadTail(t *testing.T) {
	t.Parallel()
	body := bytes.Repeat([]byte("0123456789"), 20_000)
	tail, err := publish.ReadTail(iotest.HalfReader(bytes.NewReader(body)), 1000, len(body))
	if err != nil || !bytes.Equal(tail, body[len(body)-1000:]) {
		t.Fatalf("tail = %d bytes, %v; want the last 1000", len(tail), err)
	}
	if whole, err := publish.ReadTail(bytes.NewReader(body[:10]), 1000, 10); err != nil || !bytes.Equal(whole, body[:10]) {
		t.Fatalf("short body = %q, %v", whole, err)
	}
	if _, err := publish.ReadTail(bytes.NewReader(body), 1000, len(body)-1); !errors.Is(err, publish.ErrJobLogTooLong) {
		t.Fatalf("body past the limit = %v, want ErrJobLogTooLong", err)
	}
}

// TestReadJobLogTailRefusesUntrustedRedirects: the Location is GitHub's
// answer, so it is refused unless it is an https URL on a public name; a
// refused target is never requested. The storage host's own redirect is not
// followed.
func TestReadJobLogTailRefusesUntrustedRedirects(t *testing.T) {
	t.Parallel()
	for name, location := range map[string]func(string) string{
		"plain http": func(storage string) string { return strings.Replace(signedLogURL(storage), "https://", "http://", 1) },
		"user info": func(storage string) string {
			return strings.Replace(signedLogURL(storage), "https://", "https://u:p@", 1)
		},
		"relative": func(string) string { return "/logs/41" },
		"empty":    func(string) string { return "" },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newJobLogServers(t, location, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte("log"))
			})
			if _, err := s.reconciler().ReadJobLogTail(context.Background(), "o/r", 41); err == nil ||
				errors.Is(err, publish.ErrJobLogUnavailable) {
				t.Fatalf("redirect %q = %v, want a refusal", location("https://storage.example"), err)
			}
			if len(s.download) != 0 {
				t.Fatalf("a refused redirect was requested %d times", len(s.download))
			}
		})
	}
	t.Run("loopback host", func(t *testing.T) {
		t.Parallel()
		s := newJobLogServers(t, signedLogURL, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("log"))
		})
		r := publish.NewReconciler(testTokenSource(), s.storage.Client(), s.api.URL)
		if _, err := r.ReadJobLogTail(context.Background(), "o/r", 41); err == nil {
			t.Fatal("a redirect to a loopback address was followed")
		}
		if len(s.download) != 0 {
			t.Fatal("a loopback address was requested")
		}
	})
	t.Run("storage redirect", func(t *testing.T) {
		t.Parallel()
		s := newJobLogServers(t, signedLogURL, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/elsewhere" {
				_, _ = w.Write([]byte("log"))
				return
			}
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		})
		if _, err := s.reconciler().ReadJobLogTail(context.Background(), "o/r", 41); err == nil {
			t.Fatal("the storage host's redirect was followed")
		}
		if len(s.download) != 1 {
			t.Fatalf("storage saw %d requests, want the one", len(s.download))
		}
	})
}

// TestReadJobLogTailReportsAnExpiredLog: a 404 or 410 from either host is a
// log GitHub no longer keeps, which no retry brings back.
func TestReadJobLogTailReportsAnExpiredLog(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusGone} {
		s := newJobLogServers(t, signedLogURL, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		})
		if _, err := s.reconciler().ReadJobLogTail(context.Background(), "o/r", 41); !errors.Is(err, publish.ErrJobLogUnavailable) {
			t.Fatalf("storage %d = %v, want ErrJobLogUnavailable", status, err)
		}
	}
	api := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(api.Close)
	r := publish.NewReconciler(testTokenSource(), api.Client(), api.URL)
	if _, err := r.ReadJobLogTail(context.Background(), "o/r", 41); !errors.Is(err, publish.ErrJobLogUnavailable) {
		t.Fatalf("API 404 = %v, want ErrJobLogUnavailable", err)
	}
}

func TestPublicLogHost(t *testing.T) {
	t.Parallel()
	for host, want := range map[string]bool{
		"pipelinesghubeus2.actions.githubusercontent.com": true,
		"results-receiver.actions.githubusercontent.com.": true,
		"127.0.0.1":       false,
		"::1":             false,
		"169.254.169.254": false,
		"localhost":       false,
		"api.localhost":   false,
		"metadata":        false,
		"LOCALHOST":       false,
	} {
		if got := publish.PublicLogHost(host); got != want {
			t.Errorf("PublicLogHost(%q) = %t, want %t", host, got, want)
		}
	}
}
