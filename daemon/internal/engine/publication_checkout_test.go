package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

type stubPublicationCheckout struct{ dir, baseSHA, baseRef, repo string }

func (c stubPublicationCheckout) Dir() string     { return c.dir }
func (c stubPublicationCheckout) BaseSHA() string { return c.baseSHA }
func (c stubPublicationCheckout) BaseRef() string { return c.baseRef }
func (c stubPublicationCheckout) Repo() string    { return c.repo }

// fetchBaseTransport serves FetchBase only; any other call panics on the nil
// embedded interface, which fails the test.
type fetchBaseTransport struct {
	PublicationTransport
	fetch func(dir string) (PublicationCheckout, error)
}

func (t fetchBaseTransport) FetchBase(
	_ context.Context, _, _, _, dir string,
) (PublicationCheckout, error) {
	return t.fetch(dir)
}

func TestFetchAdmittedBaseMarksOnlyFetchFailures(t *testing.T) {
	t.Parallel()
	base := domain.BaseRevision{
		Repo: "acme/widget", BaseRef: "main", BaseSHA: "1111111111111111111111111111111111111111",
	}
	boundCheckout := func(dir string) stubPublicationCheckout {
		return stubPublicationCheckout{dir: dir, baseSHA: base.BaseSHA, baseRef: base.BaseRef, repo: base.Repo}
	}
	errTransport := errors.New("remote hung up")
	tests := []struct {
		name string
		// fetch runs as FetchBase; it creates dir when the case needs a real
		// checkout directory.
		fetch          func(dir string) (PublicationCheckout, error)
		scratchMissing bool
		wantMarked     bool
		wantIs         error
	}{
		{
			name:           "missing scratch directory",
			fetch:          func(string) (PublicationCheckout, error) { return nil, errTransport },
			scratchMissing: true,
			wantIs:         fs.ErrNotExist,
		},
		{
			name:       "fetch error",
			fetch:      func(string) (PublicationCheckout, error) { return nil, errTransport },
			wantMarked: true,
			wantIs:     errTransport,
		},
		{
			// The marker is the type, not the sentinel's absence: a transport
			// error that wraps ErrParentKeyMismatch is still a fetch failure.
			name: "fetch error wrapping the binding sentinel",
			fetch: func(string) (PublicationCheckout, error) {
				return nil, fmt.Errorf("fetch: %w", domain.ErrParentKeyMismatch)
			},
			wantMarked: true,
			wantIs:     domain.ErrParentKeyMismatch,
		},
		{
			name: "checkout of another base",
			fetch: func(dir string) (PublicationCheckout, error) {
				if err := os.Mkdir(dir, 0o700); err != nil {
					return nil, err
				}
				checkout := boundCheckout(dir)
				checkout.baseSHA = "2222222222222222222222222222222222222222"
				return checkout, nil
			},
			wantIs: domain.ErrParentKeyMismatch,
		},
		{
			name:   "nil checkout",
			fetch:  func(string) (PublicationCheckout, error) { return nil, nil },
			wantIs: domain.ErrParentKeyMismatch,
		},
		{
			name: "checkout outside the requested directory",
			fetch: func(dir string) (PublicationCheckout, error) {
				if err := os.Mkdir(dir, 0o700); err != nil {
					return nil, err
				}
				return boundCheckout(filepath.Dir(dir)), nil
			},
			wantIs: domain.ErrParentKeyMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			w := &productionPublicationWorkflow{transport: fetchBaseTransport{fetch: tt.fetch}}
			scratch := t.TempDir()
			if tt.scratchMissing {
				scratch = filepath.Join(scratch, "missing")
			}
			checkout, dir, err := w.fetchAdmittedBase(t.Context(), scratch, base)
			if err == nil {
				t.Fatalf("fetchAdmittedBase() = %v, %q, nil; want an error", checkout, dir)
			}
			if !errors.Is(err, tt.wantIs) {
				t.Fatalf("fetchAdmittedBase() error = %v, want errors.Is %v", err, tt.wantIs)
			}
			var fetchErr *publicationBaseFetchError
			if marked := errors.As(err, &fetchErr); marked != tt.wantMarked {
				t.Fatalf("fetchAdmittedBase() error %v marked as a fetch failure = %v, want %v",
					err, marked, tt.wantMarked)
			}
			if tt.wantMarked && err.Error() != fetchErr.err.Error() {
				t.Fatalf("marked error text = %q, want the fetch error's %q", err.Error(), fetchErr.err.Error())
			}
		})
	}
}

func TestFetchAdmittedBaseReturnsTheBoundCheckout(t *testing.T) {
	t.Parallel()
	base := domain.BaseRevision{
		Repo: "acme/widget", BaseRef: "main", BaseSHA: "1111111111111111111111111111111111111111",
	}
	scratch := t.TempDir()
	w := &productionPublicationWorkflow{transport: fetchBaseTransport{
		fetch: func(dir string) (PublicationCheckout, error) {
			if err := os.Mkdir(dir, 0o700); err != nil {
				return nil, err
			}
			return stubPublicationCheckout{dir: dir, baseSHA: base.BaseSHA, baseRef: base.BaseRef, repo: base.Repo}, nil
		},
	}}
	checkout, dir, err := w.fetchAdmittedBase(t.Context(), scratch, base)
	if err != nil {
		t.Fatalf("fetchAdmittedBase() error = %v", err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(scratch, "checkout"))
	if err != nil {
		t.Fatal(err)
	}
	if dir != want || checkout.BaseSHA() != base.BaseSHA {
		t.Fatalf("fetchAdmittedBase() = %q at %s, want %q at %s", dir, checkout.BaseSHA(), want, base.BaseSHA)
	}
}
