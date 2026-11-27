package signet_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestStoppedTaskRetryAnswers400 verifies that POST /commands answers 400 with
// the Stop refusal message (not 409 and not 500), leaves the card open at its
// original version, records no command, and moves no server revision.
func TestStoppedTaskRetryAnswers400(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newFixture(t)
	runID := domain.RunID("run-http-stop")
	var taskID domain.TaskID
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
			Kind: domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{
				Repo: "owner/repo", RepositoryID: 1, IssueNumber: 99,
			},
		})
		if err != nil {
			return err
		}
		taskID = task.ID
		return tx.PutRun(ctx, domain.Run{
			ID: runID, TaskID: taskID, ProjectID: "proj-1",
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		})
	}); err != nil {
		t.Fatal(err)
	}
	role := domain.RoleSpecifier
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ExecutionFailureItemID("inv-spec-http-stop"), ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason:            "specification canceled",
		RequestedDecision: []domain.Action{domain.ActionRetry, domain.ActionDiscuss, domain.ActionStop},
		ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: domain.ExecutionOutcomeCanceled, Stage: domain.StageNameSpecification,
			InvocationID: "inv-spec-http-stop", RetryOutlook: domain.RetryOutlookMaySucceed,
			Admitted: &domain.InvocationAdmissionFacts{
				InvocationID: "inv-spec-http-stop", Kind: domain.AdmissionKindExecution, Role: &role,
				AdmittedAt: *f.now,
			},
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.item = item
	seedImplementationFailure(t, f, item.ExecutionFailure)
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	_, placed := f.itemSnapshot(t)
	state, err := f.store.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		_, _, err := tx.StopTask(ctx, domain.StopTaskRequest{
			CommandID: "stop-http-retry-task", DeviceID: "device-1",
			TaskID: taskID, ProjectID: "proj-1",
			ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
		}, f.now.Add(time.Second))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	before := serverRevision(t, f.store)
	body := `{
		"command_id":"retry-stopped-http", "device_id":"device-1",
		"expected_entity_version":` + strconv.FormatInt(placed.EntityVersion, 10) + `, "expected_bindings":{},
		"payload":{"item_id":"` + string(item.ID) + `", "action":"retry",
			"item_version":1, "pr_head_sha":"", "artifact_digests":[]}}`
	response := authenticatedRequest(t, signet.NewHTTPHandler(f.service, testAuthorizer),
		http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
	var decoded map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode %q: %v", response.Body.String(), err)
	}
	message, _ := decoded["message"].(string)
	if response.Code != http.StatusBadRequest || len(decoded) != 1 || !strings.Contains(message, signet.ErrTaskStopped.Error()) {
		t.Fatalf("POST /commands = %d %s, want 400 with message containing %q alone",
			response.Code, response.Body.String(), signet.ErrTaskStopped.Error())
	}
	if serverRevision(t, f.store) != before {
		t.Fatal("refused retry moved server revision")
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, _, err := tx.GetCommandSnapshot(ctx, "retry-stopped-http")
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("decision record = %v, want none", err)
	}
	if after, snap := f.itemSnapshot(t); after.Status != domain.StatusOpen || after.DecidedAt != nil ||
		snap.EntityVersion != placed.EntityVersion {
		t.Fatalf("card after refusal = %s decided %v at version %d, want open and undecided at %d",
			after.Status, after.DecidedAt, snap.EntityVersion, placed.EntityVersion)
	}
}
