package publish_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// TestMergeBase proves the read asks the compare API for the target branch
// against the exact head and returns the merge base it names, and refuses a
// response that names no full commit or a status other than 200.
func TestMergeBase(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	base := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"merge base", http.StatusOK, `{"status":"diverged","merge_base_commit":{"sha":"` + base + `"},"files":[]}`, base},
		{"short commit", http.StatusOK, `{"merge_base_commit":{"sha":"bbbb"}}`, ""},
		{"no commit", http.StatusOK, `{"status":"diverged"}`, ""},
		{"not found", http.StatusNotFound, `{}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Method + " " + r.URL.RequestURI()
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			r := publish.NewReconciler(testTokenSource(), srv.Client(), srv.URL)
			sha, err := r.MergeBase(context.Background(), "owner/repo", "release/2026", head)
			if want := "GET /repos/owner/repo/compare/release%2F2026..." + head + "?per_page=1"; got != want {
				t.Fatalf("request = %q, want %q", got, want)
			}
			if tc.want == "" {
				if err == nil {
					t.Fatalf("merge base = %q, want an error", sha)
				}
				return
			}
			if err != nil || sha != tc.want {
				t.Fatalf("merge base = %q, %v; want %q", sha, err, tc.want)
			}
		})
	}
	r := publish.NewReconciler(testTokenSource(), http.DefaultClient, "http://127.0.0.1:1")
	if _, err := r.MergeBase(context.Background(), "owner/repo", "main", "HEAD"); err == nil {
		t.Fatal("merge base of a symbolic head: want an error before any request")
	}
}

func TestValidateLabelName(t *testing.T) {
	t.Parallel()
	for _, label := range []string{"needs-rework", "needs rework", "bug: 🐛", strings.Repeat("x", 50)} {
		if err := publish.ValidateLabelName(label); err != nil {
			t.Errorf("ValidateLabelName(%q) = %v", label, err)
		}
	}
	for _, label := range []string{"", " padded", "padded ", "tab\there", strings.Repeat("x", 51), "bad\xff"} {
		if err := publish.ValidateLabelName(label); err == nil {
			t.Errorf("ValidateLabelName(%q) accepted", label)
		}
	}
}
