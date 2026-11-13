package publish

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// labelBoundary stands in for the token source and GitHub: it records each
// request and answers with status.
type labelBoundary struct {
	status   int
	requests []string
}

func (*labelBoundary) Token(context.Context, string) (InstallationToken, error) {
	return InstallationToken{Token: Secret("label-fixture-token")}, nil
}

func (b *labelBoundary) RoundTrip(req *http.Request) (*http.Response, error) {
	b.requests = append(b.requests, req.Method+" "+req.URL.EscapedPath())
	w := httptest.NewRecorder()
	w.WriteHeader(b.status)
	return w.Result(), nil
}

func newLabelHarness(t *testing.T, status int) (replyBindingFixture, *labelBoundary, *HandBackLabelRemover) {
	t.Helper()
	f := seedReplyBinding(t, "feat/test-labels", strings.Repeat("1", 40), strings.Repeat("a", 40))
	b := &labelBoundary{status: status}
	r, err := NewHandBackLabelRemover(f.st, b, &http.Client{Transport: b}, "https://api.example.test", "needs rework")
	if err != nil {
		t.Fatal(err)
	}
	return f, b, r
}

func removalRecorded(t *testing.T, f replyBindingFixture) bool {
	t.Helper()
	var recorded bool
	if err := f.st.Read(context.Background(), func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(context.Background(), handBackLabelRemovalKey(f.binding.ItemID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		recorded = true
		_, err = HandBackLabelBackupPayloadDigests(entry)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return recorded
}

// TestHandBackLabelRemoval: the remover sends one DELETE for exactly the
// configured label, escaped, on the ready item's pull request, and records
// the removal once GitHub confirms it. A 404 (the label is already gone) is
// the state the call asks for.
func TestHandBackLabelRemoval(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusOK, http.StatusNotFound} {
		f, b, r := newLabelHarness(t, status)
		if err := r.removeFor(context.Background(), f.binding); err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if len(b.requests) != 1 || b.requests[0] != "DELETE /repos/owner/repo/issues/450/labels/needs%20rework" {
			t.Fatalf("status %d: requests = %q", status, b.requests)
		}
		if !removalRecorded(t, f) {
			t.Fatalf("status %d: removal not recorded", status)
		}
	}
}

// TestHandBackLabelRemovalRefused: a refusal records nothing, so the next
// pass sends again.
func TestHandBackLabelRemovalRefused(t *testing.T) {
	t.Parallel()
	f, b, r := newLabelHarness(t, http.StatusForbidden)
	if err := r.removeFor(context.Background(), f.binding); err == nil {
		t.Fatal("a refused removal succeeded")
	}
	if len(b.requests) != 1 || removalRecorded(t, f) {
		t.Fatalf("requests = %q, recorded = %t", b.requests, removalRecorded(t, f))
	}
}

// TestHandBackLabelPassSkipsRootReadyItem: a run whose ready item no
// hand-back cycle earned sends nothing; the store decides what is due.
func TestHandBackLabelPassSkipsRootReadyItem(t *testing.T) {
	t.Parallel()
	_, b, r := newLabelHarness(t, http.StatusOK)
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(b.requests) != 0 {
		t.Fatalf("requests = %q", b.requests)
	}
}
