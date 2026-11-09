package engine

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// v2 task lines (issue #2597): a line selects an enrollment, a model, and an
// effort, and admission resolves that selection against the enrollment's
// route (domain.ResolveAgentSelection). A v1 line names a tree agent, which
// the tree no longer holds (#2599), so admission refuses it by its id.

// submitLineSelection records a submission choosing selection for role, and
// the v2 line it writes.
func (f *agentAdmissionFixture) submitLineSelection(
	t *testing.T, role domain.RoleName, selection *domain.TaskLineSelection,
) domain.TaskLine {
	t.Helper()
	commandID := agentTestSubmitCommand + "-" + string(role)
	f.recordSubmitCommand(t, commandID, f.run.ID, domain.TaskLineChoice{Role: role, Selection: selection})
	task := f.task(t, f.run.ID)
	return f.appendLine(t, domain.TaskLineInput{
		TaskID: task.ID, Role: role, Selection: selection,
		Source: domain.TaskLineSourceSubmitTask, SetBy: commandID,
	}, task.CreatedAt)
}

// TestV2TaskLineAdmitsTheSelectionItHolds: a v2 line binds the selection it
// holds on the enrollment it names, citing the line.
func TestV2TaskLineAdmitsTheSelectionItHolds(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	_, enrollment := f.addSecondAgent(t)
	line := f.submitLine(t, domain.RoleImplementer, agentTestSecondAgent)
	admission, admitted, err := f.admit(t)
	if err != nil || !admitted || admission.AgentBinding == nil {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	wantCites(t, admission, line, enrollment.ID)
	if !f.bindingRuns(t, admission.AgentBinding, agentTestSecondAgent) {
		t.Fatalf("binding = %+v, want the line's selection", admission.AgentBinding)
	}
}

// TestLegacyTaskLineRefusesByItsID: a v1 line names a tree agent, and the
// tree carries none since #2599, so admission refuses it by its id with the
// command that replaces it. It never falls back to the lineup, and the
// refusal does not repeat the agent name, which is operator text.
func TestLegacyTaskLineRefusesByItsID(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	line := f.submitLegacyLine(t, domain.RoleImplementer, agentTestPastedName)
	if line.Encoding != domain.TaskLineEncodingV1 {
		t.Fatalf("line encoding = %s", line.Encoding)
	}
	admission, admitted, err := f.admit(t)
	want := "task line " + string(line.ID) + " names a tree agent; set a v2 line with `freesided task-line`"
	if admitted || admission.AgentBinding != nil || !errors.Is(err, ErrAgentNotAdmissible) ||
		!strings.Contains(err.Error(), want) {
		t.Fatalf("admitAttempt = %t, %v; want a refusal naming %q", admitted, err, want)
	}
	if strings.Contains(err.Error(), agentTestPastedName) {
		t.Errorf("refusal %q repeats the line's agent", err)
	}
}

// TestV2TaskLineAdmissionRefusals: a v2 line runs its selection against the
// enrollment it names. A selection on an enrollment the store lacks refuses
// without falling back to the lineup, and so does a router or move line
// whose selection is not the agent its record or policy chose.
func TestV2TaskLineAdmissionRefusals(t *testing.T) {
	t.Parallel()
	foreign := &domain.TaskLineSelection{EnrollmentID: "enroll-foreign", ModelID: "model-foreign", Effort: "high"}
	cases := []struct {
		name  string
		setup func(t *testing.T, f *agentAdmissionFixture)
		want  string
	}{
		{
			name: "the selection names an enrollment the store lacks",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.submitLineSelection(t, domain.RoleImplementer, foreign)
			},
			want: "its enrollment cannot be read",
		},
		{
			name: "a router line selects what no tree agent runs",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
					order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment))
				f.appendLine(t, domain.TaskLineInput{
					TaskID: f.task(t, f.run.ID).ID, Role: domain.RoleImplementer, Selection: foreign,
					Source: domain.TaskLineSourceRouter, SetBy: routerSetBy(f.task(t, f.run.ID).ID),
					Provenance: policyOrder(policy.Revision, routerSecondPool),
				}, agentTestAt.Add(-time.Minute))
			},
			// A router line runs its enrollment on the lineup's model and
			// effort; any other selection is not one the policy orders.
			want: "does not order the line's selection for its role",
		},
		{
			name: "a router line selects an enrollment its policy does not order",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
					order(domain.RoleImplementer, agentTestEnrollment))
				f.appendLine(t, domain.TaskLineInput{
					TaskID: f.task(t, f.run.ID).ID, Role: domain.RoleImplementer,
					Selection: f.lineSelection(t, agentTestSecondAgent),
					Source:    domain.TaskLineSourceRouter, SetBy: routerSetBy(f.task(t, f.run.ID).ID),
					Provenance: policyOrder(policy.Revision, routerSecondPool),
				}, agentTestAt.Add(-time.Minute))
			},
			want: "does not order the line's selection for its role",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			tc.setup(t, f)
			admission, admitted, err := f.admit(t)
			if admitted || admission.AgentBinding != nil || !errors.Is(err, ErrAgentNotAdmissible) ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("admitAttempt = %t, %v; want a refusal naming %q", admitted, err, tc.want)
			}
			if strings.Contains(err.Error(), "enroll-foreign") {
				t.Errorf("refusal %q repeats the line's selection", err)
			}
		})
	}
}
