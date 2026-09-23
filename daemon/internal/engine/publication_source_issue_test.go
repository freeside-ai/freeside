package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

type sourceIssueTestToken struct{}

func (sourceIssueTestToken) Token(context.Context, string) (publish.InstallationToken, error) {
	return publish.InstallationToken{}, nil
}

func TestPublicationSourceIssueSnapshotSurvivesRestart(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("first_read_failed_%t", failed), func(t *testing.T) {
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				first := reads.Add(1) == 1
				if failed == first {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write([]byte(`{"number":7,"state":"open","title":"Original requirements","body":"Original body"}`))
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "store.db")
			s := storetest.Open(t, path, store.Options{})
			publisher := publish.NewPublisher(sourceIssueTestToken{}, server.Client(), server.URL, nil, nil, nil, nil)
			workflow := &productionPublicationWorkflow{store: s, publisher: publisher}
			task := productionPublicationTask{RunID: "run-snapshot", HeadSHA: closureStoreHead}
			binding := closureStoreBinding(domain.ResolvedPolicy{}, "")
			first, err := workflow.publicationSourceIssueText(t.Context(), task, binding, 7)
			if (err != nil) != failed {
				t.Fatalf("first read error = %v, want failure %t", err, failed)
			}
			if !failed && first != (publish.IssueText{Title: "Original requirements", Body: "Original body"}) {
				t.Fatalf("first read = %+v", first)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			workflow = &productionPublicationWorkflow{store: storetest.Open(t, path, store.Options{}), publisher: publisher}
			replayed, err := workflow.publicationSourceIssueText(t.Context(), task, binding, 7)
			if (err != nil) != failed || replayed != first {
				t.Fatalf("restart read = (%+v, %v), want original result %+v", replayed, err, first)
			}
			if reads.Load() != 1 {
				t.Fatalf("forge reads = %d, want one across restart", reads.Load())
			}
			// A persisted observation cannot be reused for a different source,
			// even under the same candidate key.
			_, err = workflow.publicationSourceIssueText(t.Context(), task, binding, 8)
			if !errors.Is(err, errSourceIssueCheckpoint) || !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("different source error = %v, want checkpoint identity failure", err)
			}
			// A newly reviewed base starts a new observation.
			binding.admission.Base.BaseSHA = "new-base"
			_, err = workflow.publicationSourceIssueText(t.Context(), task, binding, 7)
			if (err != nil) == failed || reads.Load() != 2 {
				t.Fatalf("new base read error = %v, reads = %d", err, reads.Load())
			}
		})
	}
}

func TestPublicationSourceIssueRestartRejectsMalformedText(t *testing.T) {
	for _, title := range []string{"", " \t\n"} {
		t.Run(fmt.Sprintf("title_%q", title), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "store.db")
			s := storetest.Open(t, path, store.Options{})
			task := productionPublicationTask{RunID: "run-malformed-source", HeadSHA: closureStoreHead}
			binding := closureStoreBinding(domain.ResolvedPolicy{}, "")
			saved := productionSourceIssueCheckpoint{
				Version: "1", RunID: task.RunID, HeadSHA: task.HeadSHA,
				BaseSHA: binding.admission.Base.BaseSHA, Repo: binding.admission.Base.Repo,
				RepositoryID: binding.admission.Base.RepositoryID, IssueNumber: 7,
				Text: &publish.IssueText{Title: title},
			}
			payload, err := json.Marshal(saved)
			if err != nil {
				t.Fatal(err)
			}
			key := "production-source-issue/" + string(task.RunID) + "/" + task.HeadSHA + "/" + binding.admission.Base.BaseSHA
			if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
				_, _, err := tx.RecordInbox(t.Context(), key, productionSourceIssueCheckpointKind, payload)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			var reads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				reads.Add(1)
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer server.Close()
			workflow := &productionPublicationWorkflow{
				store:     storetest.Open(t, path, store.Options{}),
				publisher: publish.NewPublisher(sourceIssueTestToken{}, server.Client(), server.URL, nil, nil, nil, nil),
			}
			_, err = workflow.publicationSourceIssueText(t.Context(), task, binding, 7)
			if !errors.Is(err, errSourceIssueCheckpoint) || !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("malformed restored text error = %v, want checkpoint identity failure", err)
			}
			if reads.Load() != 0 {
				t.Fatalf("forge reads = %d, want no fresh observation on malformed replay", reads.Load())
			}
		})
	}
}
