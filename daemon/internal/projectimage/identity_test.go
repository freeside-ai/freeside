package projectimage

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestGitHubRepositoryResolverBindsCanonicalNameAndNumericID(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("User-Agent") == "" || r.Header.Get("X-GitHub-Api-Version") == "" {
			t.Error("identity request lacks stable GitHub headers")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Body: io.NopCloser(strings.NewReader(
				`{"id":1278475858,"full_name":"freeasinbird/gh-imgup"}`)),
		}, nil
	})}
	resolver := githubRepositoryResolver{client: client, endpoint: "https://api.github.test"}
	if err := resolver.Verify(t.Context(), "freeasinbird/gh-imgup", 1278475858); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		repo string
		id   int64
	}{
		{"wrong id", "freeasinbird/gh-imgup", 7},
		{"noncanonical name", "FreeAsInBird/gh-imgup", 1278475858},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := resolver.Verify(t.Context(), tc.repo, tc.id); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Verify = %v, want ErrInvalidRequest", err)
			}
		})
	}
}
