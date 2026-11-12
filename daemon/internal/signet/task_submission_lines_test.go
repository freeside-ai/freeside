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

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
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
	reviewer := selectionChoice(domain.RoleReviewer, "codex")
	implementer := selectionChoice(domain.RoleImplementer, "claude-b")

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
	if replay.Submission == nil || !reflect.DeepEqual(*replay.Submission, *result.Submission) || replay.Revision != result.Revision {
		t.Fatalf("replay result = %+v, want %+v", replay, result)
	}

	for name, cmd := range map[string]signet.ClientCommand{
		"different selection": submitTaskCommandWithLines("cmd-1", reviewer,
			selectionChoice(domain.RoleImplementer, "codex")),
		"line removed": submitTaskCommandWithLines("cmd-1", reviewer),
		"line added": submitTaskCommandWithLines("cmd-1", reviewer, implementer,
			selectionChoice(domain.RoleSpecifier, "codex")),
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
// with lines is digested over: the lines in canonical role order, each its
// selection, under their wire names. Reordering the eligible roles or renaming a field would change
// the replay identity of every recorded submission that carries a line.
func TestSubmitTaskRequestDigestWithLinesIsPinned(t *testing.T) {
	t.Parallel()
	submitter := &fakeTaskSubmitter{}
	service, _ := newSubmitTaskService(t, submitter, domain.DeviceActive)
	cmd := submitTaskCommandWithLines("cmd-1",
		selectionChoice(domain.RoleReviewer, "codex"),
		selectionChoice(domain.RoleRemediator, "claude-b"),
		selectionChoice(domain.RoleImplementer, "codex"),
		selectionChoice(domain.RoleSpecifier, "claude-b"))
	if _, err := service.Submit(context.Background(), cmd); err != nil {
		t.Fatalf("submit: %v", err)
	}
	const claudeB = `{"enrollment_id":"line-claude-b/codex_cli","model_id":"model-claude-b","effort":"high"}`
	const codex = `{"enrollment_id":"line-codex/codex_cli","model_id":"model-codex","effort":"high"}`
	const pinned = `{"Device":"device-1","Payload":{"ProjectID":"project-1","Source":"Add a health endpoint.","Name":"Health endpoint",` +
		`"TaskLines":[{"role":"specifier","selection":` + claudeB + `},{"role":"implementer","selection":` + codex + `},` +
		`{"role":"remediator","selection":` + claudeB + `},{"role":"reviewer","selection":` + codex + `}]}}`
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
		"empty list":          {},
		"shadow reviewer":     {selectionChoice(domain.RoleShadowReviewer, "codex")},
		"wardless role":       {selectionChoice(domain.RoleTaskNamer, "codex")},
		"unknown role":        {selectionChoice("researcher", "codex")},
		"missing role":        {selectionChoice("", "codex")},
		"duplicate role":      {selectionChoice(domain.RoleReviewer, "codex"), selectionChoice(domain.RoleReviewer, "claude")},
		"missing selection":   {{Role: domain.RoleReviewer}},
		"agent name":          {{Role: domain.RoleReviewer, Agent: "codex"}},
		"agent and selection": {{Role: domain.RoleReviewer, Agent: "codex", Selection: selectionChoice(domain.RoleReviewer, "codex").Selection}},
		"malformed selection": {{Role: domain.RoleReviewer, Selection: &domain.TaskLineSelection{
			EnrollmentID: "line-codex/codex_cli", ModelID: "model codex", Effort: "high",
		}}},
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

	chosen := `,"task_lines":[` + wireChoice(t, domain.RoleReviewer, "codex") + `,` +
		wireChoice(t, domain.RoleImplementer, "claude-b") + `]`
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
	// The wire record carries the lines in canonical role order, under the
	// names the contract gives them: each its selection, with a null agent.
	recordOf := func(response *httptest.ResponseRecorder) string {
		t.Helper()
		var envelope struct {
			Record json.RawMessage `json:"record"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode record from %s: %v", response.Body.String(), err)
		}
		return string(envelope.Record)
	}
	recordedChoice := func(role domain.RoleName, name string) string {
		return strings.Replace(wireChoice(t, role, name), `,"selection"`, `,"agent":null,"selection"`, 1)
	}
	wireLines := `"task_lines":[` + recordedChoice(domain.RoleImplementer, "claude-b") + `,` +
		recordedChoice(domain.RoleReviewer, "codex") + `]`
	if !strings.Contains(recordOf(ok), wireLines) {
		t.Fatalf("record = %s, want it to carry %s", recordOf(ok), wireLines)
	}
	lines := linesOf(taskID)
	agents := map[domain.RoleName]string{}
	for _, line := range lines {
		if line.Version != 1 || line.PredecessorID != nil || line.Source != domain.TaskLineSourceSubmitTask ||
			line.SetBy != "cmd-lines" || !line.SetAt.Equal(task.CreatedAt) {
			t.Fatalf("line = %+v", line)
		}
		if line.Encoding != domain.TaskLineEncodingV2 || line.Selection == nil {
			t.Fatalf("line = %+v, want a v2 line", line)
		}
		for _, name := range testLineNames {
			if *line.Selection == lineSelectionOf(name) {
				agents[line.Role] = name
			}
		}
	}
	if want := map[domain.RoleName]string{domain.RoleReviewer: "codex", domain.RoleImplementer: "claude-b"}; !reflect.DeepEqual(agents, want) {
		t.Fatalf("recorded lines = %v, want %v", agents, want)
	}

	// Both records the command wrote hold the lines in canonical role order.
	canonical := []domain.TaskLineChoice{
		selectionChoice(domain.RoleImplementer, "claude-b"), selectionChoice(domain.RoleReviewer, "codex"),
	}
	var specificationRun domain.RunID
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		submission, err := tx.GetTaskSubmission(ctx, "cmd-lines")
		if err != nil {
			return err
		}
		specificationRun = submission.SpecificationRunID
		manual, err := tx.GetManualSubmission(ctx, "client:cmd-lines")
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(submission.TaskLines, canonical) || !reflect.DeepEqual(manual.TaskLines, canonical) {
			t.Fatalf("recorded lines = %+v and %+v, want %+v", submission.TaskLines, manual.TaskLines, canonical)
		}
		return nil
	}); err != nil {
		t.Fatalf("read the records: %v", err)
	}

	// Admission honors a line only when it holds against the command that
	// set it, so the lines this command writes must pass that read, at the
	// specification run and at a later run of the task.
	const laterRun domain.RunID = "run-later"
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutRun(ctx, domain.Run{
			ID: laterRun, ProjectID: "project-1", TaskID: taskID,
			SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
		})
	}); err != nil {
		t.Fatalf("store a later run: %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		for _, run := range []domain.RunID{specificationRun, laterRun} {
			for role, agent := range agents {
				line, found, err := engine.RunTaskLine(ctx, tx, agenttree.Tree{}, run, role)
				if err != nil || !found || line.Selection == nil || *line.Selection != lineSelectionOf(agent) {
					t.Fatalf("%s %s line at admission = %+v, %t, %v; want agent %q", run, role, line, found, err, agent)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read the lines as admission does: %v", err)
	}

	// A retry returns the same record and appends no second version.
	retry := post("cmd-lines", chosen)
	if retry.Code != http.StatusOK || recordOf(retry) != recordOf(ok) {
		t.Fatalf("retry status = %d body=%s, want the original record %s", retry.Code, retry.Body.String(), recordOf(ok))
	}
	if after := linesOf(taskID); !reflect.DeepEqual(after, lines) {
		t.Fatalf("lines after retry = %+v, want %+v", after, lines)
	}

	// The same command_id with another choice is refused, and the recorded
	// choice stands.
	changed := post("cmd-lines", `,"task_lines":[`+wireChoice(t, domain.RoleReviewer, "claude-b")+`,`+
		wireChoice(t, domain.RoleImplementer, "claude-b")+`]`)
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
	if record := recordOf(plain); strings.Contains(record, "task_lines") {
		t.Fatalf("line-less record = %s, want no task_lines", record)
	}
}

// TestSubmitTaskCommandHTTPRejectsMalformedTaskLines covers the wire shapes
// the contract forbids, and selections the daemon doesn't serve. Each
// answers 400, writes no submission, and never echoes the refused text,
// which may be a credential pasted into the wrong field.
func TestSubmitTaskCommandHTTPRejectsMalformedTaskLines(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, s := newRealSubmitTaskHandlerAndStore(t)
	const secret = "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	codex := `{"enrollment_id":"line-codex/codex_cli","model_id":"model-codex","effort":"high"}`
	for name, lines := range map[string]string{
		"empty list":        `[]`,
		"not a list":        `{"role":"reviewer","selection":` + codex + `}`,
		"shadow reviewer":   `[{"role":"shadow_reviewer","selection":` + codex + `}]`,
		"unknown role":      `[{"role":"` + secret + `","selection":` + codex + `}]`,
		"missing role":      `[{"selection":` + codex + `}]`,
		"duplicate role":    `[{"role":"reviewer","selection":` + codex + `},{"role":"reviewer","selection":` + codex + `}]`,
		"missing selection": `[{"role":"reviewer"}]`,
		"agent name":        `[{"role":"reviewer","agent":"` + secret + `"}]`,
		"malformed selection": `[{"role":"reviewer","selection":{"enrollment_id":"line-codex/codex_cli",` +
			`"model_id":"` + secret + ` x","effort":"high"}}]`,
		"unknown enrollment": `[{"role":"reviewer","selection":{"enrollment_id":"` + secret + `/codex_cli",` +
			`"model_id":"model-codex","effort":"high"}}]`,
		"unknown field": `[{"role":"reviewer","selection":` + codex + `,"prompt":"review"}]`,
		// TaskLineSelection is open in responses (api/README.md, Response
		// Tolerance); the daemon still refuses an unknown field in it.
		"unknown selection field": `[{"role":"reviewer","selection":{"enrollment_id":"line-codex/codex_cli",` +
			`"model_id":"model-codex","effort":"high","prompt":"review"}}]`,
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

// wireChoice is the choice a client sends for role run by name.
func wireChoice(t *testing.T, role domain.RoleName, name string) string {
	t.Helper()
	selection, err := json.Marshal(lineSelectionOf(name))
	if err != nil {
		t.Fatal(err)
	}
	return `{"role":"` + string(role) + `","selection":` + string(selection) + `}`
}

// TestSubmitTaskCommandHTTPRefusesAnUnservedModel: a selection naming a
// model the enrollment's discovered catalog lacks answers a classified 400
// that names the field by the client's own index, not the canonical role
// order, without the model text, and writes nothing. With no discovery
// record the same model is accepted.
func TestSubmitTaskCommandHTTPRefusesAnUnservedModel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	handler, s := newRealSubmitTaskHandlerAndStore(t)
	post := func(commandID, model string) *httptest.ResponseRecorder {
		body := `{"command_id":"` + commandID + `","device_id":"device-1","payload":{"kind":"submit_task",` +
			`"project_id":"project-1","source":"Body.","task_lines":[` +
			`{"role":"reviewer","selection":{"enrollment_id":"line-codex/codex_cli","model_id":"model-codex","effort":"high"}},` +
			`{"role":"specifier","selection":{"enrollment_id":"line-codex/codex_cli","model_id":"` + model + `","effort":"high"}}]}}`
		return authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
	}
	if accepted := post("cmd-undiscovered", "model-unlisted"); accepted.Code != http.StatusOK {
		t.Fatalf("no discovery record: status = %d body=%s, want 200", accepted.Code, accepted.Body.String())
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordDiscoveredModelCatalog(ctx, domain.DiscoveredModelCatalog{
			EnrollmentID: "line-codex/codex_cli", HarnessBuild: "codex-cli 0.50.0", ObservedAt: operatorAt,
			Models: []domain.DiscoveredModel{{ID: "model-codex", DisplayName: "Codex", EffortLevels: []string{"high"}}},
		})
	}); err != nil {
		t.Fatal(err)
	}
	refused := post("cmd-unserved", "model-unlisted")
	if refused.Code != http.StatusBadRequest || !strings.Contains(refused.Body.String(), "task_lines[1].selection.model_id") {
		t.Fatalf("status = %d body=%s, want 400 naming task_lines[1].selection.model_id", refused.Code, refused.Body.String())
	}
	if strings.Contains(refused.Body.String(), "model-unlisted") {
		t.Fatalf("400 body echoes the refused model: %s", refused.Body.String())
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, _, err := tx.GetTaskSubmissionSnapshot(ctx, "cmd-unserved")
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("submission lookup = %v, want ErrNotFound (nothing written)", err)
	}
}
