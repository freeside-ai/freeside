package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const servedProject domain.ProjectID = "project-served"

// engineWorkflowPolicySetter binds the hook the way daemoncompose does.
func engineWorkflowPolicySetter(
	ctx context.Context, tx *store.WriteTx, change signet.WorkflowPolicyChange, now time.Time,
) (signet.WorkflowPolicyOutcome, error) {
	result, err := engine.SetWorkflowPolicyTx(ctx, tx, engine.SetWorkflowPolicyRequest{
		Scope: change.Scope, ProjectID: change.ProjectID, Key: change.Key, Value: change.Value, SetBy: change.SetBy,
	}, now)
	return signet.WorkflowPolicyOutcome{Recorded: result.Recorded, Revision: result.Revision}, err
}

var withWorkflowPolicy = []signet.Option{
	signet.WithWorkflowPolicySetter(engineWorkflowPolicySetter),
	signet.WithWorkflowPolicyProjects([]domain.ProjectID{servedProject}),
}

func workflowCommand(
	id string, scope domain.WorkflowPolicyScope, project *domain.ProjectID,
	key domain.WorkflowPolicyKey, value *string, expected *int,
) signet.ClientCommand {
	in := operatorCommand(id, domain.CommandKindSetWorkflowPolicy)
	in.SetWorkflowPolicy = signet.SetWorkflowPolicyPayload{
		Scope: scope, ProjectID: project, Key: key, Value: value, ExpectedRevision: expected,
	}
	return in
}

func workflowSetting(t *testing.T, settings []signet.WorkflowPolicySettingWire, key domain.WorkflowPolicyKey) signet.WorkflowPolicySettingWire {
	t.Helper()
	for _, setting := range settings {
		if setting.Key == key {
			return setting
		}
	}
	t.Fatalf("no setting for %s in %+v", key, settings)
	return signet.WorkflowPolicySettingWire{}
}

// TestSetWorkflowPolicyRecordsLayersAndHoldsTheRevision: a change at either
// scope records a revision set by the device, the projection shows each
// key's value and the layer it came from, a repeat records nothing, a stale
// expectation is refused with the current projection, and a clear falls
// back to the general layer.
func TestSetWorkflowPolicyRecordsLayersAndHoldsTheRevision(t *testing.T) {
	t.Parallel()
	svc, st, _ := newOperatorService(t, withWorkflowPolicy...)
	project := servedProject

	empty, err := svc.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.WorkflowPolicy; got.Revision != 0 || len(got.General) != len(domain.AllWorkflowPolicyKeys) ||
		len(got.Projects) != 1 || got.Projects[0].ProjectID != servedProject {
		t.Fatalf("empty projection = %+v", got)
	}
	for _, setting := range empty.WorkflowPolicy.General {
		if setting.Origin != domain.WorkflowPolicyOriginDefault || setting.Value != setting.Key.Default() ||
			setting.SetBy != nil || setting.SetAt != nil {
			t.Fatalf("empty general setting = %+v, want the default", setting)
		}
	}

	general := workflowCommand("wf-1", domain.WorkflowPolicyScopeGeneral, nil,
		domain.WorkflowGateSourceIssueClosure, new("true"), nil)
	first, err := svc.Submit(t.Context(), general)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Receipt.WorkflowPolicyChange; got == nil || !got.Recorded || got.Revision != 1 {
		t.Fatalf("general change = %+v", first.Receipt)
	}
	if replay, err := svc.Submit(t.Context(), general); err != nil || !reflect.DeepEqual(replay, first) {
		t.Fatalf("replay = %+v, %v; want %+v", replay, err, first)
	}

	second, err := svc.Submit(t.Context(), workflowCommand("wf-2", domain.WorkflowPolicyScopeProject, &project,
		domain.WorkflowFollowUpMaxPerDay, new("3"), new(1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Receipt.WorkflowPolicyChange; !got.Recorded || got.Revision != 2 {
		t.Fatalf("project change = %+v", got)
	}

	snapshot, err := svc.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	policy := snapshot.WorkflowPolicy
	if policy.Revision != 2 || policy.AsOfRevision != snapshot.Revision {
		t.Fatalf("projection revisions = %d as of %d, want 2 as of %d", policy.Revision, policy.AsOfRevision, snapshot.Revision)
	}
	if got := workflowSetting(t, policy.General, domain.WorkflowGateSourceIssueClosure); got.Value != "true" ||
		got.Origin != domain.WorkflowPolicyOriginGeneral || got.SetBy == nil || *got.SetBy != "device:device-1" ||
		got.SetAt == nil || !got.SetAt.Equal(operatorAt) {
		t.Fatalf("general closure gate = %+v", got)
	}
	settings := policy.Projects[0].Settings
	if got := workflowSetting(t, settings, domain.WorkflowGateSourceIssueClosure); got.Value != "true" ||
		got.Origin != domain.WorkflowPolicyOriginGeneral {
		t.Fatalf("project closure gate = %+v, want the general layer", got)
	}
	if got := workflowSetting(t, settings, domain.WorkflowFollowUpMaxPerDay); got.Value != "3" ||
		got.Origin != domain.WorkflowPolicyOriginProject {
		t.Fatalf("project daily cap = %+v, want the project layer", got)
	}
	if got := workflowSetting(t, settings, domain.WorkflowFollowUpMaxPerRun); got.Value != "5" ||
		got.Origin != domain.WorkflowPolicyOriginDefault {
		t.Fatalf("project run cap = %+v, want the default", got)
	}

	repeat, err := svc.Submit(t.Context(), workflowCommand("wf-repeat", domain.WorkflowPolicyScopeProject, &project,
		domain.WorkflowFollowUpMaxPerDay, new("3"), new(2)))
	if err != nil {
		t.Fatal(err)
	}
	if got := repeat.Receipt.WorkflowPolicyChange; got.Recorded || got.Revision != 2 {
		t.Fatalf("repeat = %+v, want unrecorded at 2", got)
	}

	before := serverRevision(t, st)
	_, err = svc.Submit(t.Context(), workflowCommand("wf-stale", domain.WorkflowPolicyScopeGeneral, nil,
		domain.WorkflowGateIssueComment, new("true"), new(1)))
	var rejection *signet.WorkflowPolicyRejectionError
	if !errors.As(err, &rejection) || rejection.CurrentWorkflowPolicy.Revision != 2 ||
		rejection.CurrentWorkflowPolicy.AsOfRevision != before {
		t.Fatalf("stale expectation = %v, want the current projection", err)
	}
	if serverRevision(t, st) != before {
		t.Fatal("a stale change committed")
	}

	cleared, err := svc.Submit(t.Context(), workflowCommand("wf-clear", domain.WorkflowPolicyScopeProject, &project,
		domain.WorkflowFollowUpMaxPerDay, nil, new(2)))
	if err != nil {
		t.Fatal(err)
	}
	if got := cleared.Receipt.WorkflowPolicyChange; !got.Recorded || got.Revision != 3 {
		t.Fatalf("clear = %+v", got)
	}
	snapshot, err = svc.Bootstrap(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := workflowSetting(t, snapshot.WorkflowPolicy.Projects[0].Settings, domain.WorkflowFollowUpMaxPerDay); got.Value != "10" ||
		got.Origin != domain.WorkflowPolicyOriginDefault || got.SetBy != nil {
		t.Fatalf("cleared daily cap = %+v, want the default", got)
	}
}

// TestSetWorkflowPolicyRefusesBeforeRecording: an unknown key or scope, a
// value the key does not take, a scope and project that disagree, and a
// project the daemon does not serve are refused, nothing is recorded, and
// no refusal repeats the value or project.
func TestSetWorkflowPolicyRefusesBeforeRecording(t *testing.T) {
	t.Parallel()
	svc, st, _ := newOperatorService(t, withWorkflowPolicy...)
	project, pasted := servedProject, domain.ProjectID(pastedRole)
	for name, tc := range map[string]struct {
		command signet.ClientCommand
		want    error
	}{
		"unknown key": {workflowCommand("wf-key", domain.WorkflowPolicyScopeGeneral, nil,
			"gates.publish", new("true"), nil), domain.ErrInvalidWorkflowPolicyKey},
		"unknown scope": {workflowCommand("wf-scope", "team", nil,
			domain.WorkflowGateIssueComment, new("true"), nil), domain.ErrInvalidWorkflowPolicyScope},
		"gate value": {workflowCommand("wf-gate", domain.WorkflowPolicyScopeGeneral, nil,
			domain.WorkflowGateIssueComment, new(pastedRole), nil), domain.ErrInvalidWorkflowPolicyValue},
		"cap value": {workflowCommand("wf-cap", domain.WorkflowPolicyScopeGeneral, nil,
			domain.WorkflowFollowUpMaxDepth, new("0"), nil), domain.ErrInvalidWorkflowPolicyValue},
		"padded cap": {workflowCommand("wf-pad", domain.WorkflowPolicyScopeGeneral, nil,
			domain.WorkflowFollowUpMaxDepth, new("02"), nil), domain.ErrInvalidWorkflowPolicyValue},
		"project without id": {workflowCommand("wf-noid", domain.WorkflowPolicyScopeProject, nil,
			domain.WorkflowGateIssueComment, new("true"), nil), domain.ErrWorkflowPolicyProjectMismatch},
		"general with id": {workflowCommand("wf-id", domain.WorkflowPolicyScopeGeneral, &project,
			domain.WorkflowGateIssueComment, new("true"), nil), domain.ErrWorkflowPolicyProjectMismatch},
		"unserved project": {workflowCommand("wf-unserved", domain.WorkflowPolicyScopeProject, &pasted,
			domain.WorkflowGateIssueComment, new("true"), nil), domain.ErrWorkflowPolicyProjectUnserved},
		"zero expected": {workflowCommand("wf-zero", domain.WorkflowPolicyScopeGeneral, nil,
			domain.WorkflowGateIssueComment, new("true"), new(0)), domain.ErrNonPositive},
	} {
		t.Run(name, func(t *testing.T) {
			before := serverRevision(t, st)
			_, err := svc.Submit(t.Context(), tc.command)
			if !errors.Is(err, tc.want) {
				t.Fatalf("refusal = %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), pastedRole) {
				t.Fatalf("refusal repeats the request: %v", err)
			}
			if serverRevision(t, st) != before {
				t.Fatal("a refused change committed")
			}
		})
	}
}

// TestSetWorkflowPolicyHTTP: the wire record flattens the change beside its
// kind, a stale expectation is a 409 carrying current_workflow_policy, and a
// refusal is a 400.
func TestSetWorkflowPolicyHTTP(t *testing.T) {
	t.Parallel()
	svc, _, _ := newOperatorService(t, withWorkflowPolicy...)
	handler := signet.NewHTTPHandler(svc, testAuthorizer)
	post := func(t *testing.T, commandID, payload string) (int, map[string]any) {
		t.Helper()
		body := `{"command_id":"` + commandID + `","device_id":"device-1","payload":` + payload + `}`
		response := authenticatedRequest(t, handler, http.MethodPost, "/commands", bytes.NewReader([]byte(body)))
		var decoded map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("decode %q: %v", response.Body.String(), err)
		}
		return response.Code, decoded
	}

	code, body := post(t, "h-set", `{"kind":"set_workflow_policy","scope":"project","project_id":"`+string(servedProject)+
		`","key":"gates.follow_up_filing","value":"true","expected_revision":null}`)
	record, _ := body["record"].(map[string]any)
	if code != 200 || record["kind"] != "set_workflow_policy" || record["recorded"] != true || record["revision"] != float64(1) ||
		record["scope"] != "project" || record["project_id"] != string(servedProject) || record["value"] != "true" ||
		record["device_id"] != "device-1" {
		t.Fatalf("set: %d %v", code, body)
	}
	code, body = post(t, "h-stale", `{"kind":"set_workflow_policy","scope":"general","key":"gates.issue_comment","value":"true"}`)
	if current, _ := body["current_workflow_policy"].(map[string]any); code != 409 || body["message"] == "" ||
		current["revision"] != float64(1) {
		t.Fatalf("stale: %d %v", code, body)
	}
	for name, payload := range map[string]string{
		"unknown key":      `{"kind":"set_workflow_policy","scope":"general","key":"gates.publish","value":"true","expected_revision":1}`,
		"bad value":        `{"kind":"set_workflow_policy","scope":"general","key":"gates.issue_comment","value":"` + pastedRole + `","expected_revision":1}`,
		"unserved project": `{"kind":"set_workflow_policy","scope":"project","project_id":"` + pastedRole + `","key":"gates.issue_comment","value":"true","expected_revision":1}`,
		"unknown member":   `{"kind":"set_workflow_policy","scope":"general","key":"gates.issue_comment","value":"true","expected_revision":1,"force":true}`,
	} {
		t.Run(name, func(t *testing.T) {
			code, body := post(t, "h-"+strings.ReplaceAll(name, " ", "-"), payload)
			if code != 400 || strings.Contains(fmt.Sprint(body), pastedRole) {
				t.Fatalf("%d %v, want a 400 that does not repeat the request", code, body)
			}
		})
	}
}
