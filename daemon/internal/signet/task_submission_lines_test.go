package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func submitTaskCommandWithLines(commandID string, lines ...domain.TaskLineChoice) signet.ClientCommand {
	cmd := submitTaskCommand(commandID, "Add a health endpoint.", "Health endpoint")
	cmd.SubmitTask.TaskLines = lines
	return cmd
}

// TestSubmitTaskCommandBindsTaskLinesToItsIdentity covers the replay rule for
// lines: the same set under the same command_id returns the recorded result
// whatever order it is sent in, and any other set is an immutable conflict.
func TestSubmitTaskCommandBindsTaskLinesToItsIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	submitter := &fakeTaskSubmitter{}
	service, _ := newSubmitTaskService(t, submitter, domain.DeviceActive)
	reviewer := domain.TaskLineChoice{Role: domain.RoleReviewer, Agent: "codex"}
	implementer := domain.TaskLineChoice{Role: domain.RoleImplementer, Agent: "claude-b"}

	result, err := service.Submit(ctx, submitTaskCommandWithLines("cmd-1", reviewer, implementer))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	// The submitter receives the lines in canonical role order.
	if want := []domain.TaskLineChoice{implementer, reviewer}; !reflect.DeepEqual(submitter.last.TaskLines, want) {
		t.Fatalf("submitter lines = %+v, want %+v", submitter.last.TaskLines, want)
	}

	replay, err := service.Submit(ctx, submitTaskCommandWithLines("cmd-1", implementer, reviewer))
	if err != nil {
		t.Fatalf("replay in another order: %v", err)
	}
	if replay.Submission == nil || *replay.Submission != *result.Submission || replay.Revision != result.Revision {
		t.Fatalf("replay result = %+v, want %+v", replay, result)
	}

	for name, cmd := range map[string]signet.ClientCommand{
		"different agent": submitTaskCommandWithLines("cmd-1", reviewer,
			domain.TaskLineChoice{Role: domain.RoleImplementer, Agent: "codex"}),
		"line removed": submitTaskCommandWithLines("cmd-1", reviewer),
		"line added": submitTaskCommandWithLines("cmd-1", reviewer, implementer,
			domain.TaskLineChoice{Role: domain.RoleSpecifier, Agent: "codex"}),
		"no lines": submitTaskCommandWithLines("cmd-1"),
	} {
		if _, err := service.Submit(ctx, cmd); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("%s: error = %v, want ErrImmutableConflict", name, err)
		}
	}

	// The reverse: a submission recorded without lines does not replay for a
	// retry that adds one.
	if _, err := service.Submit(ctx, submitTaskCommandWithLines("cmd-2")); err != nil {
		t.Fatalf("submit without lines: %v", err)
	}
	if _, err := service.Submit(ctx, submitTaskCommandWithLines("cmd-2", reviewer)); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("lines added to a line-less submission: error = %v, want ErrImmutableConflict", err)
	}
	if submitter.calls != 2 {
		t.Fatalf("submitter calls = %d, want 2 (one per accepted command)", submitter.calls)
	}
}

// TestSubmitTaskRequestDigestWithoutLinesIsUnchanged pins the request digest
// of a submission that carries no lines to the bytes it was taken over before
// the field existed. A recorded submission replays only while a retry
// reproduces its digest, so a change here strands every recorded command.
func TestSubmitTaskRequestDigestWithoutLinesIsUnchanged(t *testing.T) {
	t.Parallel()
	submitter := &fakeTaskSubmitter{}
	service, _ := newSubmitTaskService(t, submitter, domain.DeviceActive)
	if _, err := service.Submit(context.Background(), submitTaskCommandWithLines("cmd-1")); err != nil {
		t.Fatalf("submit: %v", err)
	}
	const before = `{"Device":"device-1","Payload":{"ProjectID":"project-1","Source":"Add a health endpoint.","Name":"Health endpoint"}}`
	if want := domain.Digest(contentaddr.Sum([]byte(before))); submitter.last.RequestDigest != want {
		t.Fatalf("request digest = %s, want %s", submitter.last.RequestDigest, want)
	}
}

// TestSubmitTaskRequestDigestWithLinesIsPinned pins the bytes a submission
// with lines is digested over: the lines in canonical role order under their
// wire names. Reordering the eligible roles or renaming a field would change
// the replay identity of every recorded submission that carries a line.
func TestSubmitTaskRequestDigestWithLinesIsPinned(t *testing.T) {
	t.Parallel()
	submitter := &fakeTaskSubmitter{}
	service, _ := newSubmitTaskService(t, submitter, domain.DeviceActive)
	cmd := submitTaskCommandWithLines("cmd-1",
		domain.TaskLineChoice{Role: domain.RoleReviewer, Agent: "codex"},
		domain.TaskLineChoice{Role: domain.RoleRemediator, Agent: "claude-b"},
		domain.TaskLineChoice{Role: domain.RoleImplementer, Agent: "codex"},
		domain.TaskLineChoice{Role: domain.RoleSpecifier, Agent: "claude-b"})
	if _, err := service.Submit(context.Background(), cmd); err != nil {
		t.Fatalf("submit: %v", err)
	}
	const pinned = `{"Device":"device-1","Payload":{"ProjectID":"project-1","Source":"Add a health endpoint.","Name":"Health endpoint",` +
		`"TaskLines":[{"role":"specifier","agent":"claude-b"},{"role":"implementer","agent":"codex"},` +
		`{"role":"remediator","agent":"claude-b"},{"role":"reviewer","agent":"codex"}]}}`
	if want := domain.Digest(contentaddr.Sum([]byte(pinned))); submitter.last.RequestDigest != want {
		t.Fatalf("request digest = %s, want %s", submitter.last.RequestDigest, want)
	}
}

func TestSubmitTaskCommandRejectsMalformedTaskLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	submitter := &fakeTaskSubmitter{}
	service, _ := newSubmitTaskService(t, submitter, domain.DeviceActive)
	for name, lines := range map[string][]domain.TaskLineChoice{
		"empty list":      {},
		"shadow reviewer": {{Role: domain.RoleShadowReviewer, Agent: "codex"}},
		"wardless role":   {{Role: domain.RoleTaskNamer, Agent: "codex"}},
		"unknown role":    {{Role: "researcher", Agent: "codex"}},
		"missing role":    {{Agent: "codex"}},
		"duplicate role":  {{Role: domain.RoleReviewer, Agent: "codex"}, {Role: domain.RoleReviewer, Agent: "claude"}},
		"missing agent":   {{Role: domain.RoleReviewer}},
		"malformed agent": {{Role: domain.RoleReviewer, Agent: "../codex"}},
	} {
		cmd := submitTaskCommandWithLines("cmd-" + name)
		cmd.SubmitTask.TaskLines = lines
		if _, err := service.Submit(ctx, cmd); !errors.Is(err, signet.ErrInvalidSubmitTaskPayload) {
			t.Fatalf("%s: error = %v, want ErrInvalidSubmitTaskPayload", name, err)
		}
	}
	if submitter.calls != 0 {
		t.Fatalf("submitter calls = %d, want 0 (rejected before submission)", submitter.calls)
	}
}

// TestSubmitTaskCommandHTTPRecordsTaskLines runs the engine-backed submitter
// behind the HTTP handler: an accepted command records one line per role in
// the transaction that creates the task, and a retry appends none.
func TestSubmitTaskCommandHTTPRecordsTaskLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, s := newRealSubmitTaskHandlerAndStore(t)
	post := func(commandID, lines string) *httptest.ResponseRecorder {
		body := `{"command_id":"` + commandID + `","device_id":"device-1","payload":{"kind":"submit_task",` +
			`"project_id":"project-1","source":"Add a health endpoint."` + lines + `}}`
		return authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
	}
	taskOf := func(response *httptest.ResponseRecorder) domain.TaskID {
		t.Helper()
		var envelope struct {
			Record struct {
				TaskID domain.TaskID `json:"task_id"`
			} `json:"record"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || envelope.Record.TaskID == "" {
			t.Fatalf("decode task id from %s: %v", response.Body.String(), err)
		}
		return envelope.Record.TaskID
	}
	linesOf := func(taskID domain.TaskID) []domain.TaskLine {
		t.Helper()
		var lines []domain.TaskLine
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			lines, err = tx.TaskLines(ctx, taskID)
			return err
		}); err != nil {
			t.Fatalf("read task lines: %v", err)
		}
		return lines
	}

	const chosen = `,"task_lines":[{"role":"reviewer","agent":"codex"},{"role":"implementer","agent":"claude-b"}]`
	ok := post("cmd-lines", chosen)
	if ok.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", ok.Code, ok.Body.String())
	}
	taskID := taskOf(ok)
	var task domain.Task
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		task, err = tx.GetTask(ctx, taskID)
		return err
	}); err != nil {
		t.Fatalf("read task: %v", err)
	}
	lines := linesOf(taskID)
	agents := map[domain.RoleName]string{}
	for _, line := range lines {
		if line.Version != 1 || line.PredecessorID != nil || line.Source != domain.TaskLineSourceSubmitTask ||
			line.SetBy != "cmd-lines" || !line.SetAt.Equal(task.CreatedAt) {
			t.Fatalf("line = %+v", line)
		}
		agents[line.Role] = line.Agent
	}
	if want := map[domain.RoleName]string{domain.RoleReviewer: "codex", domain.RoleImplementer: "claude-b"}; !reflect.DeepEqual(agents, want) {
		t.Fatalf("recorded lines = %v, want %v", agents, want)
	}

	// Admission honors a line only when it holds against the command that
	// set it, so the lines this command writes must pass that read.
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		submission, err := tx.GetTaskSubmission(ctx, "cmd-lines")
		if err != nil {
			return err
		}
		for role, agent := range agents {
			line, found, err := engine.RunTaskLine(ctx, tx, submission.SpecificationRunID, role)
			if err != nil || !found || line.Agent != agent {
				t.Fatalf("%s line at admission = %+v, %t, %v; want agent %q", role, line, found, err, agent)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read the lines as admission does: %v", err)
	}

	// A retry returns the same task and appends no second version.
	retry := post("cmd-lines", chosen)
	if retry.Code != http.StatusOK || taskOf(retry) != taskID {
		t.Fatalf("retry status = %d body=%s, want the original task", retry.Code, retry.Body.String())
	}
	if after := linesOf(taskID); !reflect.DeepEqual(after, lines) {
		t.Fatalf("lines after retry = %+v, want %+v", after, lines)
	}

	// The same command_id with another choice is refused, and the recorded
	// choice stands.
	changed := post("cmd-lines", `,"task_lines":[{"role":"reviewer","agent":"claude-b"},{"role":"implementer","agent":"claude-b"}]`)
	if changed.Code != http.StatusBadRequest {
		t.Fatalf("changed lines status = %d body=%s, want 400", changed.Code, changed.Body.String())
	}
	if after := linesOf(taskID); !reflect.DeepEqual(after, lines) {
		t.Fatalf("lines after refused change = %+v, want %+v", after, lines)
	}

	// A submission without lines records none.
	plain := post("cmd-plain", "")
	if plain.Code != http.StatusOK {
		t.Fatalf("line-less status = %d body=%s, want 200", plain.Code, plain.Body.String())
	}
	if got := linesOf(taskOf(plain)); len(got) != 0 {
		t.Fatalf("line-less submission recorded %+v", got)
	}
}

// TestSubmitTaskCommandHTTPRejectsMalformedTaskLines covers the wire shapes
// the contract forbids. Each answers 400, writes no submission, and never
// echoes the agent text, which may be a credential pasted into the wrong
// field.
func TestSubmitTaskCommandHTTPRejectsMalformedTaskLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, s := newRealSubmitTaskHandlerAndStore(t)
	const secret = "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	for name, lines := range map[string]string{
		"empty list":      `[]`,
		"not a list":      `{"role":"reviewer","agent":"codex"}`,
		"shadow reviewer": `[{"role":"shadow_reviewer","agent":"codex"}]`,
		"unknown role":    `[{"role":"` + secret + `","agent":"codex"}]`,
		"missing role":    `[{"agent":"codex"}]`,
		"duplicate role":  `[{"role":"reviewer","agent":"codex"},{"role":"reviewer","agent":"claude"}]`,
		"missing agent":   `[{"role":"reviewer"}]`,
		"malformed agent": `[{"role":"reviewer","agent":"` + secret + `"}]`,
		"unknown field":   `[{"role":"reviewer","agent":"codex","prompt":"review"}]`,
	} {
		commandID := "cmd-" + strings.ReplaceAll(name, " ", "-")
		body := `{"command_id":"` + commandID + `","device_id":"device-1","payload":{"kind":"submit_task",` +
			`"project_id":"project-1","source":"Body.","task_lines":` + lines + `}}`
		response := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body=%s, want 400", name, response.Code, response.Body.String())
		}
		if strings.Contains(response.Body.String(), secret) {
			t.Fatalf("%s: 400 body echoes the refused text: %s", name, response.Body.String())
		}
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			_, _, err := tx.GetTaskSubmissionSnapshot(ctx, commandID)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: submission lookup = %v, want ErrNotFound (nothing written)", name, err)
		}
	}
}
