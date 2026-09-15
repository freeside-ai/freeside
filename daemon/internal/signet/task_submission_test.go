package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// fakeTaskSubmitter stands in for the engine-backed submitter. It creates a
// task and specification run inside the accepting transaction so
// PutTaskSubmission's foreign keys are satisfied, records how often it ran, and
// returns a configurable error. It keys the task by a fixed issue source, which
// is enough to exercise the signet handler; the real intake-key convergence is
// covered by the engine's end-to-end tests.
type fakeTaskSubmitter struct {
	calls int
	err   error
}

func (f *fakeTaskSubmitter) SubmitTask(ctx context.Context, tx *store.WriteTx, in signet.TaskSubmissionInput) (signet.TaskSubmissionResult, error) {
	f.calls++
	if f.err != nil {
		return signet.TaskSubmissionResult{}, f.err
	}
	source := domain.SpecificationSource{
		Kind:         domain.SpecificationSourceIssueSubject,
		IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 1},
	}
	task, err := tx.GetOrCreateTask(ctx, in.ProjectID, source)
	if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	runID := domain.RunID("run-fake-1")
	if _, err := tx.GetRun(ctx, runID); errors.Is(err, store.ErrNotFound) {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: in.ProjectID, TaskID: task.ID,
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
		}); err != nil {
			return signet.TaskSubmissionResult{}, err
		}
	} else if err != nil {
		return signet.TaskSubmissionResult{}, err
	}
	name := domain.DisplayName{Text: string(task.ID), Source: domain.DisplayNameSourceIdentifier}
	if in.OperatorName != "" {
		name = domain.DisplayName{Text: in.OperatorName, Source: domain.DisplayNameSourceOperator}
	}
	return signet.TaskSubmissionResult{TaskID: task.ID, SpecificationRunID: runID, Name: name}, nil
}

func newSubmitTaskService(t *testing.T, submitter signet.TaskSubmitter, deviceStatus domain.DeviceStatus) (*signet.Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	s := storetest.Open(t, t.TempDir()+"/signet.db", store.Options{})
	t.Cleanup(func() { _ = s.Close() })
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	pairedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	device := domain.Device{ID: "device-1", DisplayName: "Test Mac", Status: deviceStatus, PairedAt: pairedAt}
	if deviceStatus == domain.DeviceRevoked {
		revokedAt := pairedAt.Add(time.Hour)
		device.RevokedAt = &revokedAt
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, device)
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	opts := []signet.Option{signet.WithBlobStore(blobs)}
	if submitter != nil {
		opts = append(opts, signet.WithTaskSubmitter(submitter))
	}
	return signet.NewService(s, opts...), s
}

func submitTaskCommand(commandID, source, name string) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: commandID, DeviceID: "device-1", Kind: domain.CommandKindSubmitTask,
		SubmitTask: signet.SubmitTaskPayload{ProjectID: "project-1", Source: source, Name: name},
	}
}

func TestSubmitTaskCommandCreatesAndReplays(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	submitter := &fakeTaskSubmitter{}
	service, _ := newSubmitTaskService(t, submitter, domain.DeviceActive)

	result, err := service.Submit(ctx, submitTaskCommand("cmd-1", "Add a health endpoint.", "Health endpoint"))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if result.Submission == nil {
		t.Fatalf("result carries no submission: %+v", result)
	}
	wantDigest := domain.Digest(contentaddr.Sum([]byte("Add a health endpoint.")))
	if result.Submission.CommandID != "cmd-1" || result.Submission.ProjectID != "project-1" ||
		result.Submission.SourceDigest != wantDigest || result.Submission.SpecificationRunID != "run-fake-1" ||
		result.Submission.TaskID == "" ||
		result.Submission.Name != (domain.DisplayName{Text: "Health endpoint", Source: domain.DisplayNameSourceOperator}) {
		t.Fatalf("submission = %+v", *result.Submission)
	}
	if result.Revision < 1 {
		t.Fatalf("revision = %d", result.Revision)
	}
	if submitter.calls != 1 {
		t.Fatalf("submitter calls = %d, want 1", submitter.calls)
	}

	// A retried command_id returns the recorded result and starts no second run.
	replay, err := service.Submit(ctx, submitTaskCommand("cmd-1", "Add a health endpoint.", "Ignored on replay"))
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replay.Submission == nil || *replay.Submission != *result.Submission || replay.Revision != result.Revision {
		t.Fatalf("replay result = %+v, want %+v", replay, result)
	}
	if submitter.calls != 1 {
		t.Fatalf("submitter calls after replay = %d, want 1", submitter.calls)
	}
}

func TestSubmitTaskCommandRejectsReusedCommandID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	submitter := &fakeTaskSubmitter{}
	service, _ := newSubmitTaskService(t, submitter, domain.DeviceActive)

	if _, err := service.Submit(ctx, submitTaskCommand("cmd-1", "Add a health endpoint.", "Health endpoint")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if submitter.calls != 1 {
		t.Fatalf("submitter calls = %d, want 1", submitter.calls)
	}

	// A command_id reused for a different submission is an immutable conflict,
	// never a silent replay of the first task. Only the optional name may
	// differ (that convergence is covered by CreatesAndReplays).
	for _, tc := range []struct {
		name string
		cmd  signet.ClientCommand
	}{
		{"different source", submitTaskCommand("cmd-1", "Add a metrics endpoint.", "Health endpoint")},
		{"different project", signet.ClientCommand{
			CommandID: "cmd-1", DeviceID: "device-1", Kind: domain.CommandKindSubmitTask,
			SubmitTask: signet.SubmitTaskPayload{ProjectID: "project-2", Source: "Add a health endpoint.", Name: "Health endpoint"},
		}},
		{"different device", signet.ClientCommand{
			CommandID: "cmd-1", DeviceID: "device-2", Kind: domain.CommandKindSubmitTask,
			SubmitTask: signet.SubmitTaskPayload{ProjectID: "project-1", Source: "Add a health endpoint.", Name: "Health endpoint"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.Submit(ctx, tc.cmd); !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("error = %v, want ErrImmutableConflict", err)
			}
		})
	}
	if submitter.calls != 1 {
		t.Fatalf("submitter calls after conflicts = %d, want 1 (no second run)", submitter.calls)
	}
}

func TestSubmitRejectsUnknownCommandKind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _ := newSubmitTaskService(t, &fakeTaskSubmitter{}, domain.DeviceActive)
	// The dispatch handles decision, the legacy zero value, and submit_task; a
	// nonempty unknown kind (which the HTTP boundary already rejects) fails
	// closed at the exhaustive-switch fallback rather than routing to decision.
	_, err := service.Submit(ctx, signet.ClientCommand{
		CommandID: "cmd-x", DeviceID: "device-1", Kind: domain.CommandKind("bogus"),
	})
	if !errors.Is(err, domain.ErrInvalidCommandKind) {
		t.Fatalf("error = %v, want ErrInvalidCommandKind", err)
	}
}

func TestSubmitTaskCommandRejectsRevokedDevice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	submitter := &fakeTaskSubmitter{}
	service, _ := newSubmitTaskService(t, submitter, domain.DeviceRevoked)
	if _, err := service.Submit(ctx, submitTaskCommand("cmd-1", "Some work.", "")); !errors.Is(err, signet.ErrDeviceNotActive) {
		t.Fatalf("revoked device error = %v, want ErrDeviceNotActive", err)
	}
	if submitter.calls != 0 {
		t.Fatalf("submitter calls = %d, want 0 (rejected before submission)", submitter.calls)
	}
}

func TestSubmitTaskCommandFailsClosedWithoutSubmitter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, _ := newSubmitTaskService(t, nil, domain.DeviceActive)
	if _, err := service.Submit(ctx, submitTaskCommand("cmd-1", "Some work.", "")); !errors.Is(err, signet.ErrTaskSubmissionUnavailable) {
		t.Fatalf("no submitter error = %v, want ErrTaskSubmissionUnavailable", err)
	}
}

func TestSubmitTaskCommandRejectsMalformedPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	submitter := &fakeTaskSubmitter{}
	service, _ := newSubmitTaskService(t, submitter, domain.DeviceActive)
	for _, tc := range []struct {
		name string
		cmd  signet.ClientCommand
	}{
		{"empty project", signet.ClientCommand{
			CommandID: "cmd-1", DeviceID: "device-1", Kind: domain.CommandKindSubmitTask,
			SubmitTask: signet.SubmitTaskPayload{Source: "work"},
		}},
		{"empty source", signet.ClientCommand{
			CommandID: "cmd-1", DeviceID: "device-1", Kind: domain.CommandKindSubmitTask,
			SubmitTask: signet.SubmitTaskPayload{ProjectID: "project-1"},
		}},
		{"envelope present", signet.ClientCommand{
			CommandID: "cmd-1", DeviceID: "device-1", Kind: domain.CommandKindSubmitTask,
			ExpectedEntityVersion: 1, SubmitTask: signet.SubmitTaskPayload{ProjectID: "project-1", Source: "work"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.Submit(ctx, tc.cmd); !errors.Is(err, signet.ErrInvalidSubmitTaskPayload) {
				t.Fatalf("error = %v, want ErrInvalidSubmitTaskPayload", err)
			}
		})
	}
	if submitter.calls != 0 {
		t.Fatalf("submitter calls = %d, want 0 (rejected before submission)", submitter.calls)
	}
}

func TestSubmitTaskCommandHTTPRoundTripAndEnvelopeRejection(t *testing.T) {
	t.Parallel()
	service, _ := newSubmitTaskService(t, &fakeTaskSubmitter{}, domain.DeviceActive)
	handler := signet.NewHTTPHandler(service, testAuthorizer)

	post := func(body string) *httptest.ResponseRecorder {
		return authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
	}

	ok := post(`{"command_id":"cmd-1","device_id":"device-1","payload":{"kind":"submit_task","project_id":"project-1","source":"Add a health endpoint.","name":"Health endpoint"}}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("submit_task status = %d body=%s, want 200", ok.Code, ok.Body.String())
	}
	var envelope struct {
		Record   json.RawMessage `json:"record"`
		Revision int64           `json:"revision"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	var kinded struct {
		Kind      domain.CommandKind `json:"kind"`
		TaskID    domain.TaskID      `json:"task_id"`
		ProjectID domain.ProjectID   `json:"project_id"`
	}
	if err := json.Unmarshal(envelope.Record, &kinded); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if kinded.Kind != domain.CommandKindSubmitTask || kinded.TaskID == "" || kinded.ProjectID != "project-1" {
		t.Fatalf("record = %+v", kinded)
	}

	// A submit_task command carrying the decision envelope is malformed.
	bad := post(`{"command_id":"cmd-2","device_id":"device-1","expected_entity_version":1,"payload":{"kind":"submit_task","project_id":"project-1","source":"work"}}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("envelope-carrying submit_task status = %d, want 400", bad.Code)
	}
}

// newRealSubmitTaskHandler wires the engine-backed submitter behind the HTTP
// handler so the operator-name validation runs end to end (the fake submitter
// stores names verbatim). It shares one blob store between the service and the
// submitter, as the daemon composition does.
func newRealSubmitTaskHandler(t *testing.T) http.Handler {
	t.Helper()
	ctx := context.Background()
	s := storetest.Open(t, t.TempDir()+"/signet-real.db", store.Options{})
	t.Cleanup(func() { _ = s.Close() })
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: "device-1", DisplayName: "Mac", Status: domain.DeviceActive,
			PairedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	prov := domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: domain.Digest(contentaddr.Sum([]byte("submit-task-http-policy")))}
	initiator := engine.ManualInitiator{
		PolicyKeys: []domain.PolicyKey{
			{Key: specify.PolicySpecApproval, Value: "true", Provenance: prov},
			{Key: specify.PolicyMaxIterations, Value: "4", Provenance: prov},
			{Key: specify.PolicyStageActiveTime, Value: "1m", Provenance: prov},
			{Key: specify.PolicyApprovalWait, Value: "1m", Provenance: prov},
			{Key: specify.PolicyResearchAllowlist, Value: "https://docs.example", Provenance: prov},
			{Key: specify.PolicyResearchMaxBytes, Value: "1024", Provenance: prov},
			{Key: "paths", Value: "daemon/**", Provenance: prov},
		},
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	}
	submitter := engine.NewTaskSubmitter(blobs, func(domain.ProjectID) (engine.ManualInitiator, bool) {
		return initiator, true
	})
	service := signet.NewService(s, signet.WithBlobStore(blobs), signet.WithTaskSubmitter(submitter))
	return signet.NewHTTPHandler(service, testAuthorizer)
}

func TestSubmitTaskCommandHTTPValidatesOperatorName(t *testing.T) {
	t.Parallel()
	handler := newRealSubmitTaskHandler(t)
	post := func(body string) *httptest.ResponseRecorder {
		return authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
	}

	// A padded name answers 200 and records the trimmed operator name.
	ok := post(`{"command_id":"cmd-ok","device_id":"device-1","payload":{"kind":"submit_task","project_id":"project-1","source":"Add a health endpoint.","name":"  Health endpoint  "}}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("padded name status = %d body=%s, want 200", ok.Code, ok.Body.String())
	}
	var envelope struct {
		Record json.RawMessage `json:"record"`
	}
	if err := json.Unmarshal(ok.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	var record struct {
		Name domain.DisplayName `json:"name"`
	}
	if err := json.Unmarshal(envelope.Record, &record); err != nil {
		t.Fatalf("decode record: %v", err)
	}
	if record.Name != (domain.DisplayName{Text: "Health endpoint", Source: domain.DisplayNameSourceOperator}) {
		t.Fatalf("recorded name = %+v, want the trimmed operator name", record.Name)
	}

	// Each invalid name answers 400 with a message that never contains the name
	// (the name may be the refused secret). The exactly-empty case is covered by
	// the HTTP boundary check; these non-empty names reach the submitter.
	for _, bad := range []struct{ label, name string }{
		{"whitespace only", "   "},
		{"multiline", "line one\nline two"},
		{"too long", strings.Repeat("é", 61)},
		{"credential", "ghp_" + strings.Repeat("A", 36)},
	} {
		t.Run(bad.label, func(t *testing.T) {
			payload, err := json.Marshal(map[string]any{
				"command_id": "cmd-" + bad.label, "device_id": "device-1",
				"payload": map[string]any{
					"kind": "submit_task", "project_id": "project-1", "source": "Body.", "name": bad.name,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			resp := post(string(payload))
			if resp.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s, want 400", resp.Code, resp.Body.String())
			}
			if strings.Contains(resp.Body.String(), bad.name) {
				t.Fatalf("400 body echoes the submitted name: %s", resp.Body.String())
			}
		})
	}
}

// TestSubmitTaskCommandHTTPRejectsInvalidUTF8Name covers the raw wire path a Go
// string literal cannot: an invalid UTF-8 byte in payload.name must be rejected
// with 400, not silently substituted with U+FFFD and stored. The submit_task
// arm decodes with RejectInvalidUTF8, so the whole body is refused before the
// name reaches operatorTaskName. A "\xff" Go literal would already be a valid
// Go string by the time the namer ran, so the byte has to ride in the raw body.
func TestSubmitTaskCommandHTTPRejectsInvalidUTF8Name(t *testing.T) {
	t.Parallel()
	handler := newRealSubmitTaskHandler(t)

	body := []byte(`{"command_id":"cmd-badutf8","device_id":"device-1","payload":{"kind":"submit_task","project_id":"project-1","source":"Body.","name":"`)
	body = append(body, 0xff) // a lone continuation byte: never valid UTF-8
	body = append(body, []byte(`"}}`)...)

	resp := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader(body))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("invalid-UTF-8 name status = %d body=%s, want 400", resp.Code, resp.Body.String())
	}
	// The name must not leak, neither the raw byte nor the U+FFFD it would have
	// become under tolerant decoding.
	if bytes.Contains(resp.Body.Bytes(), []byte{0xff}) || strings.ContainsRune(resp.Body.String(), '�') {
		t.Fatalf("400 body leaks the submitted name bytes: %s", resp.Body.String())
	}
}
