package engine

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Operator line changes (plan §5.4, issue #1601): a change writes a record and
// a line version citing it, an admission after it honors the version only
// against that record, and a role with an attempt running keeps its line.

func (f *agentAdmissionFixture) changeLine(t *testing.T, role domain.RoleName, agent string, clear bool) TaskLineChangeResult {
	t.Helper()
	result, err := ChangeTaskLine(context.Background(), f.store, ChangeTaskLineRequest{
		TaskID: f.task(t, f.run.ID).ID, Role: role, Agent: agent, Clear: clear,
	}, agentTestAt)
	if err != nil {
		t.Fatalf("change the %s line: %v", role, err)
	}
	return result
}

// lineVersions returns every version of the role's line on the fixture task.
func (f *agentAdmissionFixture) lineVersions(t *testing.T, role domain.RoleName) []domain.TaskLine {
	t.Helper()
	ctx := context.Background()
	taskID := f.task(t, f.run.ID).ID
	var versions []domain.TaskLine
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		lines, err := tx.TaskLines(ctx, taskID)
		for _, line := range lines {
			if line.Role == role {
				versions = append(versions, line)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return versions
}

func (f *agentAdmissionFixture) taskRevision(t *testing.T) store.Snapshot {
	t.Helper()
	ctx := context.Background()
	taskID := f.task(t, f.run.ID).ID
	var snapshot store.Snapshot
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		task, err := tx.GetTaskSnapshot(ctx, taskID)
		snapshot = task.Snapshot
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// TestChangedLineSelectsAtTheNextAdmission is the acceptance fixture: a change
// writes its record and a version citing it, and the next admission binds the
// new agent and cites that version, which the store's gates accept.
func TestChangedLineSelectsAtTheNextAdmission(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	identity, _ := f.addSecondAgent(t)
	first := f.submitLine(t, domain.RoleImplementer, agentTestWriterAgent)

	result := f.changeLine(t, domain.RoleImplementer, agentTestSecondAgent, false)
	line := result.Line
	if !result.Recorded || line == nil || line.Version != 2 || line.Agent != agentTestSecondAgent ||
		line.Source != domain.TaskLineSourceCLILineChange || line.PredecessorID == nil || *line.PredecessorID != first.ID {
		t.Fatalf("change = %+v, %+v", result, line)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		change, err := tx.GetTaskLineChange(ctx, domain.Digest(line.SetBy))
		if err == nil && !change.Backs(*line) {
			t.Errorf("record %+v does not back the line it set", change)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	admission, admitted, err := f.admit(t)
	if err != nil || !admitted {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	binding := admission.AgentBinding
	if binding.SelectionSource != domain.AgentSelectionSourceTaskLine || binding.SelectionRecordID != line.ID {
		t.Fatalf("selection = %s %s, want the changed line %s", binding.SelectionSource, binding.SelectionRecordID, line.ID)
	}
	if admission.AuthIdentityID == nil || *admission.AuthIdentityID != identity.ID {
		t.Fatalf("identity = %v, want %s", admission.AuthIdentityID, identity.ID)
	}
	f.record(t, admission)
}

// TestFirstChangeOfALinelessRole: a role with no line takes one the same way,
// a first version that replaces nothing.
func TestFirstChangeOfALinelessRole(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	line := f.changeLine(t, domain.RoleImplementer, agentTestSecondAgent, false).Line
	if line == nil || line.Version != 1 || line.PredecessorID != nil {
		t.Fatalf("line = %+v, want version 1", line)
	}
	admission, admitted, err := f.admit(t)
	if err != nil || !admitted || admission.AgentBinding.SelectionRecordID != line.ID {
		t.Fatalf("admitAttempt = %+v, %t, %v", admission.AgentBinding, admitted, err)
	}
}

// TestClearReturnsARoleToTheLineup: a clear cites its record, the next
// admission binds the lineup's selection, and a later change takes the role
// off the lineup again.
func TestClearReturnsARoleToTheLineup(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	f.submitLine(t, domain.RoleImplementer, agentTestSecondAgent)

	cleared := f.changeLine(t, domain.RoleImplementer, "", true)
	if !cleared.Recorded || cleared.Line == nil || !cleared.Line.Cleared || cleared.Line.Agent != "" || cleared.Line.Version != 2 {
		t.Fatalf("clear = %+v, %+v", cleared, cleared.Line)
	}
	admission, admitted, err := f.admit(t)
	if err != nil || !admitted {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	if binding := admission.AgentBinding; binding.SelectionSource != domain.AgentSelectionSourceLineup {
		t.Fatalf("selection = %s %s, want the lineup", binding.SelectionSource, binding.SelectionRecordID)
	}

	again := f.changeLine(t, domain.RoleImplementer, agentTestSecondAgent, false)
	if !again.Recorded || again.Line.Version != 3 {
		t.Fatalf("a change after the clear = %+v", again.Line)
	}
	admission, admitted, err = f.admit(t)
	if err != nil || !admitted || admission.AgentBinding.SelectionRecordID != again.Line.ID {
		t.Fatalf("admitAttempt = %+v, %t, %v", admission.AgentBinding, admitted, err)
	}
}

// TestAskingForTheCurrentLineRecordsNothing: the same agent, or a clear of a
// role with no line or a cleared one, writes nothing and moves no revision.
func TestAskingForTheCurrentLineRecordsNothing(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	unchanged := func(name string, role domain.RoleName, agent string, clear bool) {
		t.Helper()
		before, versions := f.taskRevision(t), f.lineVersions(t, role)
		result := f.changeLine(t, role, agent, clear)
		if result.Recorded {
			t.Errorf("%s: recorded %+v", name, result.Line)
		}
		if after := f.taskRevision(t); after != before {
			t.Errorf("%s: task moved from %+v to %+v", name, before, after)
		}
		if after := f.lineVersions(t, role); len(after) != len(versions) {
			t.Errorf("%s: %d versions, want %d", name, len(after), len(versions))
		}
	}
	unchanged("a clear of a role with no line", domain.RoleImplementer, "", true)
	line := f.submitLine(t, domain.RoleImplementer, agentTestSecondAgent)
	unchanged("the same agent", domain.RoleImplementer, agentTestSecondAgent, false)
	if got := f.changeLine(t, domain.RoleImplementer, agentTestSecondAgent, false); got.Line == nil || got.Line.ID != line.ID {
		t.Errorf("the report names %+v, want the current line %s", got.Line, line.ID)
	}
	f.changeLine(t, domain.RoleImplementer, "", true)
	unchanged("a clear of a cleared role", domain.RoleImplementer, "", true)
}

// TestChangeIsRefusedWhileItsRoleRuns: a recorded admission holds its role's
// line until the attempt's outcome, with its dispatch in any state, and no
// other role's.
func TestChangeIsRefusedWhileItsRoleRuns(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	line := f.submitLine(t, domain.RoleImplementer, agentTestWriterAgent)
	admission, admitted, err := f.admit(t)
	if err != nil || !admitted {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	f.record(t, admission)

	before := f.taskRevision(t)
	for name, request := range map[string]ChangeTaskLineRequest{
		"a change": {Role: domain.RoleImplementer, Agent: agentTestSecondAgent},
		"a clear":  {Role: domain.RoleImplementer, Clear: true},
	} {
		request.TaskID = f.task(t, f.run.ID).ID
		if result, err := ChangeTaskLine(ctx, f.store, request, agentTestAt); !errors.Is(err, domain.ErrTaskLineRoleRunning) {
			t.Fatalf("%s while the role runs = %+v, %v", name, result, err)
		}
	}
	if versions := f.lineVersions(t, domain.RoleImplementer); len(versions) != 1 || versions[0].ID != line.ID {
		t.Fatalf("versions = %+v, want the original line only", versions)
	}
	if after := f.taskRevision(t); after != before {
		t.Fatalf("task moved from %+v to %+v", before, after)
	}
	// The running line is what the role already has, so naming it is no change.
	if result := f.changeLine(t, domain.RoleImplementer, agentTestWriterAgent, false); result.Recorded {
		t.Fatalf("naming the running line recorded %+v", result.Line)
	}
	for _, role := range []domain.RoleName{domain.RoleSpecifier, domain.RoleRemediator, domain.RoleReviewer} {
		if result := f.changeLine(t, role, agentTestSecondAgent, false); !result.Recorded {
			t.Errorf("the %s line did not change while the implementer runs", role)
		}
	}

	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordExecutionOutcome(ctx, domain.ExecutionOutcome{
			InvocationID: admission.InvocationID, AdmissionID: admission.ID,
			Status: domain.ExecutionOutcomeFailed, Summary: "driver failure",
			Cause: domain.FailureCauseUnknown, RecordedAt: agentTestAt.Add(time.Minute),
		})
	}); err != nil {
		t.Fatal(err)
	}
	if result := f.changeLine(t, domain.RoleImplementer, agentTestSecondAgent, false); !result.Recorded {
		t.Fatal("the line did not change after the attempt's outcome")
	}
}

// TestAReviewRequestHoldsTheReviewerLine: a review runs from its request until
// its record or failure, and holds no writer's line.
func TestAReviewRequestHoldsTheReviewerLine(t *testing.T) {
	ctx := context.Background()
	const round, baseSHA, headSHA = 1, "base", "head"
	invocation := ProductionReviewInvocationID("run-1", round)
	finishes := map[string]func(t *testing.T, tx *store.WriteTx) error{
		"failure": func(_ *testing.T, tx *store.WriteTx) error {
			return tx.PutReviewFailure(ctx, domain.ReviewFailure{
				InvocationID: invocation, RunID: "run-1", Round: round, BaseSHA: baseSHA, HeadSHA: headSHA,
				Class: domain.ReviewFailureConfiguration, Reason: "refused at launch", ObservedAt: agentTestAt,
			})
		},
		"record": func(t *testing.T, tx *store.WriteTx) error {
			t.Helper()
			record, err := domain.NewReviewRecord(domain.ReviewRecord{
				InvocationID: invocation, RunID: "run-1", Round: round,
				Admission:           execfake.RecordAdmission(domain.RoleReviewer, "run-1", invocation),
				ConfigurationDigest: agentTestDigest("7"), InstructionDigest: agentTestDigest("8"),
				BaseSHA: baseSHA, HeadSHA: headSHA, CompletedAt: agentTestAt,
				CompletionEvidence: agentTestDigest("9"), Outcome: domain.ReviewClean,
			})
			if err != nil {
				return err
			}
			return tx.PutReviewRecord(ctx, record, nil)
		},
	}
	for name, finish := range finishes {
		t.Run(name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			f.addSecondAgent(t)
			task := f.task(t, f.run.ID)
			if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutReviewRequest(ctx, domain.ReviewRequestRecord{
					InvocationID: invocation, RunID: f.run.ID, Round: round,
					BaseSHA: baseSHA, HeadSHA: headSHA, RequestedAt: agentTestAt,
				})
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := ChangeTaskLine(ctx, f.store, ChangeTaskLineRequest{
				TaskID: task.ID, Role: domain.RoleReviewer, Agent: agentTestSecondAgent,
			}, agentTestAt); !errors.Is(err, domain.ErrTaskLineRoleRunning) {
				t.Fatalf("a change while the review runs: %v", err)
			}
			if result := f.changeLine(t, domain.RoleImplementer, agentTestSecondAgent, false); !result.Recorded {
				t.Fatal("the implementer line did not change while the reviewer runs")
			}
			if err := f.store.Write(ctx, func(tx *store.WriteTx) error { return finish(t, tx) }); err != nil {
				t.Fatal(err)
			}
			if result := f.changeLine(t, domain.RoleReviewer, agentTestSecondAgent, false); !result.Recorded {
				t.Fatal("the reviewer line did not change after the review finished")
			}
		})
	}
}

// TestClosedWorkflowHoldsNoLine: an abandoned episode advances nothing, so its
// open attempt and its open review may never record an end. The line stays
// changeable, or the reattempt that recovers the task would run on it.
func TestClosedWorkflowHoldsNoLine(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	admission, admitted, err := f.admit(t)
	if err != nil || !admitted {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	f.record(t, admission)
	invocation := ProductionReviewInvocationID(f.run.ID, 1)
	task := f.task(t, f.run.ID)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordTaskLifecycleFact(ctx, task.ID, domain.TaskLifecycleFact{
			Kind: domain.TaskLifecycleStarted, RunID: f.run.ID, SourceID: "start-1", RecordedAt: agentTestAt,
		}); err != nil {
			return err
		}
		return tx.PutReviewRequest(ctx, domain.ReviewRequestRecord{
			InvocationID: invocation, RunID: f.run.ID, Round: 1,
			BaseSHA: "base", HeadSHA: "head", RequestedAt: agentTestAt,
		})
	}); err != nil {
		t.Fatal(err)
	}
	roles := []domain.RoleName{domain.RoleImplementer, domain.RoleReviewer}
	for _, role := range roles {
		if _, err := ChangeTaskLine(ctx, f.store, ChangeTaskLineRequest{
			TaskID: task.ID, Role: role, Agent: agentTestSecondAgent,
		}, agentTestAt); !errors.Is(err, domain.ErrTaskLineRoleRunning) {
			t.Fatalf("the %s line while the episode is open: %v", role, err)
		}
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.AbandonTask(ctx, task.ID, agentTestAt); err != nil {
			return err
		}
		current, err := tx.GetTask(ctx, task.ID)
		if err != nil {
			return err
		}
		_, _, err = tx.CloseTaskWorkflow(ctx, task.ID, current.CurrentStart().Ordinal, agentTestAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, role := range roles {
		if result := f.changeLine(t, role, agentTestSecondAgent, false); !result.Recorded {
			t.Errorf("the %s line did not change after the workflow closed", role)
		}
	}
}

// TestChangeRequestRefusals: a request that can't be recorded fails before
// writing, and no refusal repeats the text it refused.
func TestChangeRequestRefusals(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	task := f.task(t, f.run.ID)
	// Fails the agent grammar and reads like a pasted credential.
	const pasted = "sk-ant-oat01 fixture not a token"
	for name, request := range map[string]ChangeTaskLineRequest{
		"an ineligible role":  {TaskID: task.ID, Role: domain.RoleShadowReviewer, Agent: agentTestSecondAgent},
		"an unknown role":     {TaskID: task.ID, Role: pasted, Agent: agentTestSecondAgent},
		"an ineligible clear": {TaskID: task.ID, Role: pasted, Clear: true},
		"a malformed agent":   {TaskID: task.ID, Role: domain.RoleImplementer, Agent: pasted},
		"no agent":            {TaskID: task.ID, Role: domain.RoleImplementer},
		"a clear with text":   {TaskID: task.ID, Role: domain.RoleImplementer, Agent: pasted, Clear: true},
		"an unknown task":     {TaskID: "task-unknown", Role: domain.RoleImplementer, Agent: agentTestSecondAgent},
	} {
		result, err := ChangeTaskLine(ctx, f.store, request, agentTestAt)
		if err == nil || result.Recorded {
			t.Errorf("%s = %+v, %v; want a refusal", name, result, err)
		} else if strings.Contains(err.Error(), pasted) {
			t.Errorf("%s: the refusal repeats the request text: %v", name, err)
		}
	}
}

// TestForgedLineChangesRefuseAdmission: a cli_line_change version holds only
// against the record it cites. Each forgery starts from a real record, removes
// the version it wrote, and appends one that disagrees in a single fact, so
// the refusal can't come from a chain check instead.
func TestForgedLineChangesRefuseAdmission(t *testing.T) {
	ctx := context.Background()
	type forged struct {
		line  domain.TaskLineInput
		setAt time.Time
	}
	const role = domain.RoleImplementer
	// recordThenRemove records a change and removes its line, leaving the
	// record as the role's only trace.
	recordThenRemove := func(t *testing.T, f *agentAdmissionFixture, taskID domain.TaskID, role domain.RoleName) domain.Digest {
		t.Helper()
		result, err := ChangeTaskLine(ctx, f.store, ChangeTaskLineRequest{
			TaskID: taskID, Role: role, Agent: agentTestSecondAgent,
		}, agentTestAt)
		if err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", f.storePath)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = db.Close() }()
		if _, err := db.ExecContext(ctx, `DELETE FROM task_lines WHERE id = ?`, result.Line.ID); err != nil {
			t.Fatal(err)
		}
		return domain.Digest(result.Line.SetBy)
	}
	base := func(t *testing.T, f *agentAdmissionFixture, setBy domain.Digest) domain.TaskLineInput {
		t.Helper()
		return domain.TaskLineInput{
			TaskID: f.task(t, f.run.ID).ID, Role: role, Agent: agentTestSecondAgent,
			Source: domain.TaskLineSourceCLILineChange, SetBy: string(setBy),
		}
	}
	cases := []struct {
		name  string
		forge func(t *testing.T, f *agentAdmissionFixture) forged
		want  string
	}{
		{
			name: "no record",
			forge: func(t *testing.T, f *agentAdmissionFixture) forged {
				return forged{base(t, f, agentTestDigest("a")), agentTestAt}
			},
			want: "not found",
		},
		{
			name: "a set_by that is no digest",
			forge: func(t *testing.T, f *agentAdmissionFixture) forged {
				return forged{base(t, f, "not-a-digest"), agentTestAt}
			},
			want: "not found",
		},
		{
			name: "a clear with no record",
			forge: func(t *testing.T, f *agentAdmissionFixture) forged {
				f.submitLine(t, role, agentTestSecondAgent)
				in := base(t, f, agentTestDigest("a"))
				in.Agent, in.Cleared = "", true
				return forged{in, agentTestAt}
			},
			want: "not found",
		},
		{
			name: "a record of another task",
			forge: func(t *testing.T, f *agentAdmissionFixture) forged {
				seedRunPolicy(t, f.store, domain.Run{ID: "run-other", ProjectID: f.run.ProjectID, SpecDigest: agentTestDigest("9")}, nil)
				id := recordThenRemove(t, f, f.task(t, "run-other").ID, role)
				return forged{base(t, f, id), agentTestAt}
			},
			want: "names another",
		},
		{
			name: "a record of another role",
			forge: func(t *testing.T, f *agentAdmissionFixture) forged {
				id := recordThenRemove(t, f, f.task(t, f.run.ID).ID, domain.RoleRemediator)
				return forged{base(t, f, id), agentTestAt}
			},
			want: "names another",
		},
		{
			name: "a record of another agent",
			forge: func(t *testing.T, f *agentAdmissionFixture) forged {
				id := recordThenRemove(t, f, f.task(t, f.run.ID).ID, role)
				in := base(t, f, id)
				in.Agent = agentTestWriterAgent
				return forged{in, agentTestAt}
			},
			want: "names another",
		},
		{
			name: "a record that set an agent, on a clear",
			forge: func(t *testing.T, f *agentAdmissionFixture) forged {
				f.submitLine(t, role, agentTestWriterAgent)
				id := recordThenRemove(t, f, f.task(t, f.run.ID).ID, role)
				in := base(t, f, id)
				in.Agent, in.Cleared = "", true
				return forged{in, agentTestAt}
			},
			want: "names another",
		},
		{
			name: "a record that replaced another version",
			forge: func(t *testing.T, f *agentAdmissionFixture) forged {
				f.submitLine(t, role, agentTestWriterAgent)
				line := f.changeLine(t, role, agentTestSecondAgent, false).Line
				f.changeLine(t, role, agentTestWriterAgent, false)
				return forged{base(t, f, domain.Digest(line.SetBy)), agentTestAt}
			},
			want: "names another",
		},
		{
			name: "a line set at another instant",
			forge: func(t *testing.T, f *agentAdmissionFixture) forged {
				id := recordThenRemove(t, f, f.task(t, f.run.ID).ID, role)
				return forged{base(t, f, id), agentTestAt.Add(time.Second)}
			},
			want: "names another",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			f.addSecondAgent(t)
			got := tc.forge(t, f)
			line := f.appendLine(t, got.line, got.setAt)
			admission, admitted, err := f.admit(t)
			if !errors.Is(err, ErrAgentNotAdmissible) || admitted || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("admitAttempt = %+v, %t, %v; want a refusal containing %q", admission, admitted, err, tc.want)
			}
			if !strings.Contains(err.Error(), string(line.ID)) {
				t.Errorf("refusal %q does not name line %s", err, line.ID)
			}
		})
	}
}

// TestReviewerLineChangeLeavesARefusal: the line a review refused is the one
// operator's way out, because a refused review requested nothing and so
// holds no line. The next review admission accepts the changed agent.
func TestReviewerLineChangeLeavesARefusal(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	routed, _ := f.reviewTree(t)
	refused := f.submitLine(t, domain.RoleReviewer, "not-in-the-tree")
	if _, err := f.admitReview(t, routed); !errors.Is(err, ErrAgentNotAdmissible) {
		t.Fatalf("the refused line admits: %v", err)
	}
	changed := f.changeLine(t, domain.RoleReviewer, agentTestSecondAgent, false)
	if !changed.Recorded || *changed.Line.PredecessorID != refused.ID {
		t.Fatalf("change = %+v", changed.Line)
	}
	admission, err := f.admitReview(t, routed)
	if err != nil {
		t.Fatal(err)
	}
	if binding := admission.AgentBinding; binding.SelectionSource != domain.AgentSelectionSourceTaskLine ||
		binding.SelectionRecordID != changed.Line.ID || admission.AuthIdentityID != "claude-second" {
		t.Fatalf("admission = %+v, want the changed line %s", admission, changed.Line.ID)
	}
}

// TestClearedReviewerLineReviewsFromTheLineup: a clear returns the reviewer to
// the lineup too, and a clear nothing backs refuses the review.
func TestClearedReviewerLineReviewsFromTheLineup(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	routed, _ := f.reviewTree(t)
	f.submitLine(t, domain.RoleReviewer, agentTestSecondAgent)
	f.changeLine(t, domain.RoleReviewer, "", true)
	admission, err := f.admitReview(t, routed)
	if err != nil {
		t.Fatal(err)
	}
	if binding := admission.AgentBinding; binding.SelectionSource != domain.AgentSelectionSourceLineup {
		t.Fatalf("selection = %s %s, want the lineup", binding.SelectionSource, binding.SelectionRecordID)
	}
}
