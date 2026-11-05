package signet_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// TestSubmittedTaskSyncsFetchableSourceDigest drives the real submitter: the
// synced task's source names the digest the submission stored, and the
// attachments read serves the submitted text under it.
func TestSubmittedTaskSyncsFetchableSourceDigest(t *testing.T) {
	t.Parallel()
	handler := newRealSubmitTaskHandler(t)
	const source = "Add a health endpoint."

	submitted := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(
		`{"command_id":"cmd-digest","device_id":"device-1","payload":{"kind":"submit_task","project_id":"project-1","source":"`+source+`"}}`)))
	if submitted.Code != http.StatusOK {
		t.Fatalf("submit status = %d body=%s, want 200", submitted.Code, submitted.Body.String())
	}
	var envelope struct {
		Record struct {
			TaskID       domain.TaskID `json:"task_id"`
			SourceDigest domain.Digest `json:"source_digest"`
		} `json:"record"`
	}
	if err := json.Unmarshal(submitted.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode submit result: %v", err)
	}
	want := domain.Digest(contentaddr.Sum([]byte(source)))
	if envelope.Record.SourceDigest != want {
		t.Fatalf("submission source digest = %q, want %q", envelope.Record.SourceDigest, want)
	}

	response := authenticatedRequest(t, handler, http.MethodGet, "/sync/bootstrap", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("bootstrap status = %d body=%s", response.Code, response.Body.String())
	}
	var bootstrap signet.BootstrapSnapshot
	if err := json.Unmarshal(response.Body.Bytes(), &bootstrap); err != nil {
		t.Fatalf("decode bootstrap: %v", err)
	}
	if len(bootstrap.Tasks) != 1 || bootstrap.Tasks[0].Task.ID != envelope.Record.TaskID {
		t.Fatalf("bootstrap tasks = %+v, want the submitted task %q", bootstrap.Tasks, envelope.Record.TaskID)
	}
	got := bootstrap.Tasks[0].Task.Source
	if got == nil || got.Kind != domain.SpecificationSourceWorkItemArtifact || got.WorkItemArtifactDigest == nil || *got.WorkItemArtifactDigest != want {
		t.Fatalf("synced task source = %+v, want digest %q", got, want)
	}

	attachment := authenticatedRequest(t, handler, http.MethodGet, "/attachments/"+string(*got.WorkItemArtifactDigest), nil)
	if attachment.Code != http.StatusOK || attachment.Body.String() != source {
		t.Fatalf("attachment = %d %q, want 200 %q", attachment.Code, attachment.Body.String(), source)
	}
}
