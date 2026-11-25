package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// rerunBoundary stands in for the token source and GitHub: it answers the
// run read with run and the rerun with postStatus and postBody, and records
// each request.
type rerunBoundary struct {
	tokenErr   error
	run        string
	postStatus int
	postBody   string
	requests   []string
}

func (b *rerunBoundary) Token(context.Context, string) (InstallationToken, error) {
	if b.tokenErr != nil {
		return InstallationToken{}, b.tokenErr
	}
	return InstallationToken{Token: Secret("rerun-fixture-token")}, nil
}

func (b *rerunBoundary) RoundTrip(req *http.Request) (*http.Response, error) {
	b.requests = append(b.requests, req.Method+" "+req.URL.EscapedPath())
	w := httptest.NewRecorder()
	switch req.Method {
	case http.MethodGet:
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, b.run)
	default:
		w.WriteHeader(b.postStatus)
		_, _ = io.WriteString(w, b.postBody)
	}
	return w.Result(), nil
}

func runJSON(head string, attempt int, status string) string {
	return fmt.Sprintf(`{"id":7,"head_sha":%q,"run_attempt":%d,"status":%q}`, head, attempt, status)
}

// newRerunHarness seeds a ready item, enqueues the intent to rerun attempt 1
// of run 7 on head, and wires a rerunner to b.
func newRerunHarness(t *testing.T, b *rerunBoundary, head string) (replyBindingFixture, *CheckRerunner, string) {
	t.Helper()
	f := seedReplyBinding(t, "feat/test-reruns", strings.Repeat("1", 40), strings.Repeat("a", 40))
	key, payload, err := CheckRerunIntent(f.item.ID, f.binding.Repo, f.binding.PRNumber, head, CheckRun{ID: 7, Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(context.Background(), key, CheckRerunKind, payload)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r, err := NewCheckRerunner(f.st, b, &http.Client{Transport: b}, "https://api.example.test")
	if err != nil {
		t.Fatal(err)
	}
	return f, r, key
}

func rerunDispatched(t *testing.T, f replyBindingFixture, key string) bool {
	t.Helper()
	var entry store.QueueEntry
	if err := f.st.Read(context.Background(), func(tx *store.ReadTx) (err error) {
		entry, err = tx.GetOutbox(context.Background(), key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckRerunBackupPayloadDigests(entry); err != nil {
		t.Fatalf("stored intent fails backup validation: %v", err)
	}
	return entry.Dispatched()
}

// TestCheckRerunSendsOnce: a completed failed attempt is rerun with one POST
// to its run's rerun-failed-jobs, and the intent completes.
func TestCheckRerunSendsOnce(t *testing.T) {
	t.Parallel()
	b := &rerunBoundary{postStatus: http.StatusCreated}
	f, r, key := newRerunHarness(t, b, strings.Repeat("a", 40))
	b.run = runJSON(f.item.PRHeadSHA, 1, "completed")
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /repos/owner/repo/actions/runs/7",
		"POST /repos/owner/repo/actions/runs/7/rerun-failed-jobs",
	}
	if strings.Join(b.requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %q, want %q", b.requests, want)
	}
	if !rerunDispatched(t, f, key) {
		t.Fatal("sent intent is still pending")
	}
	if err := r.Pass(context.Background()); err != nil || len(b.requests) != 2 {
		t.Fatalf("second pass: err %v, requests %q; want nothing sent", err, b.requests)
	}
}

// TestCheckRerunCompletesWithoutSending: an intent whose rerun already
// happened (a crash after the POST), whose run is already running again, or
// whose item left the head completes without a POST.
func TestCheckRerunCompletesWithoutSending(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	for _, tc := range []struct {
		name     string
		head     string
		run      string
		wantRead bool
	}{
		{name: "a later attempt exists", head: head, run: runJSON(head, 2, "completed"), wantRead: true},
		{name: "the attempt is running again", head: head, run: runJSON(head, 1, "in_progress"), wantRead: true},
		{name: "the item left the head", head: strings.Repeat("b", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := &rerunBoundary{run: tc.run, postStatus: http.StatusCreated}
			f, r, key := newRerunHarness(t, b, tc.head)
			if err := r.Pass(context.Background()); err != nil {
				t.Fatal(err)
			}
			if got := len(b.requests); (tc.wantRead && got != 1) || (!tc.wantRead && got != 0) {
				t.Fatalf("requests = %q, want only the run read=%t", b.requests, tc.wantRead)
			}
			if !rerunDispatched(t, f, key) {
				t.Fatal("intent is still pending")
			}
		})
	}
}

// TestCheckRerunTreatsAlreadyRunningAsDone: GitHub's refusal to rerun a run
// that is already running is the state the call asks for.
func TestCheckRerunTreatsAlreadyRunningAsDone(t *testing.T) {
	t.Parallel()
	b := &rerunBoundary{postStatus: http.StatusForbidden, postBody: `{"message":"This workflow is already running"}`}
	f, r, key := newRerunHarness(t, b, strings.Repeat("a", 40))
	b.run = runJSON(f.item.PRHeadSHA, 1, "completed")
	if err := r.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !rerunDispatched(t, f, key) {
		t.Fatal("already-running refusal left the intent pending")
	}
}

// TestCheckRerunRefusalLeavesIntentPending: a refused mint (an installation
// that has not accepted actions: write) or a refused rerun sends nothing it
// can count and keeps the intent for the next pass, and the error that
// crosses the boundary is fixed text.
func TestCheckRerunRefusalLeavesIntentPending(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		b    *rerunBoundary
	}{
		{name: "refused mint", b: &rerunBoundary{tokenErr: errors.New("mint refused: secret-ish detail")}},
		{name: "refused rerun", b: &rerunBoundary{postStatus: http.StatusForbidden, postBody: `{"message":"Resource not accessible by integration"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, r, key := newRerunHarness(t, tc.b, strings.Repeat("a", 40))
			tc.b.run = runJSON(f.item.PRHeadSHA, 1, "completed")
			err := r.Pass(context.Background())
			if err == nil || strings.Contains(err.Error(), "secret-ish") {
				t.Fatalf("Pass() = %v, want a fixed-text error", err)
			}
			if rerunDispatched(t, f, key) {
				t.Fatal("refused rerun completed its intent")
			}
		})
	}
}

// TestCheckRerunRefusesForeignRun: a run read that names another run or head
// is an error, never a reason to send or to complete.
func TestCheckRerunRefusesForeignRun(t *testing.T) {
	t.Parallel()
	b := &rerunBoundary{run: runJSON(strings.Repeat("c", 40), 1, "completed"), postStatus: http.StatusCreated}
	f, r, key := newRerunHarness(t, b, strings.Repeat("a", 40))
	if err := r.Pass(context.Background()); err == nil {
		t.Fatal("foreign run accepted")
	}
	if len(b.requests) != 1 || rerunDispatched(t, f, key) {
		t.Fatalf("requests %q; foreign run sent or completed the intent", b.requests)
	}
}
