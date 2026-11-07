package engine

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Task-line admission (plan §5.4, issue #1640): an attempt's role runs the
// agent its task's line names, the admission cites that line, and a line that
// cannot be honored refuses the attempt instead of running the lineup's
// agent.

const (
	agentTestSecondAgent   = "claude-second"
	agentTestSubmitCommand = "cmd-submit-lines"
	// agentTestPastedName passes the agent grammar and reads like a pasted
	// credential.
	agentTestPastedName = "sk-ant-oat01-fixture-not-a-token"
)

// addSecondAgent seeds a second adopted Claude identity and adds an agent on
// it to the tree, so a task line can name an agent other than the lineup's.
// It returns that identity and its enrollment.
func (f *agentAdmissionFixture) addSecondAgent(t *testing.T) (domain.AuthIdentity, domain.ClientEnrollment) {
	t.Helper()
	ctx := context.Background()
	identity := f.identity
	identity.ID, identity.AccountBinding = "claude-second", "acct-fixture-0002"
	identity.Interim.AuthStoreVolume = "claude-second-interim"
	enrollment := f.enrollment
	enrollment.ID, enrollment.AuthIdentityID = "claude-second/claude_code", identity.ID
	enrollment.AccountBinding = identity.AccountBinding
	manifest := agentTestDigest("6")
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordAuthIdentity(ctx, identity, agentTestAt.Add(-4*time.Minute)); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(ctx, enrollment, agentTestAt.Add(-3*time.Minute)); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity.ID, "inv-adopt-second",
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, Generation: 0,
				AuthStoreVolume: "claude-second-generation", StoreManifestDigest: manifest,
			}, agentTestAt.Add(-2*time.Minute), agentTestAt.Add(10*time.Minute))
		if err != nil {
			return err
		}
		_, err = tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: enrollment.ID, AuthStoreVolume: "claude-second-generation",
			StoreManifestDigest: manifest, LeaseFence: lease.Fence,
			AccountBinding: identity.AccountBinding, RecordedAt: agentTestAt.Add(-time.Minute),
		}, agentTestAt.Add(-time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tree := f.selection.Tree
	tree.Agents = append(slices.Clone(tree.Agents), domain.AgentSource{
		Name: agentTestSecondAgent, Enrollment: string(enrollment.ID), Route: enrollment.Route,
		Adapter: agentbaseline.ClaudeAdapterName, Offer: agentTestWriterAgent,
		Effort: domain.EffortHigh,
	})
	tree.Sort()
	files, err := agenttree.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if f.selection.LineupRevision, err = agenttree.Revision(files); err != nil {
		t.Fatal(err)
	}
	if f.selection.Tree, err = agenttree.Parse(files); err != nil {
		t.Fatal(err)
	}
	selection := f.selection
	f.engine.admission.environment.Agents = &selection
	return identity, enrollment
}

// task returns the task the store bound the run to.
func (f *agentAdmissionFixture) task(t *testing.T, runID domain.RunID) domain.Task {
	t.Helper()
	ctx := context.Background()
	var task domain.Task
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		task, err = tx.GetTask(ctx, run.TaskID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return task
}

// recordSubmitCommand records the submit_task command that created the run's
// task, with the lines it carried: the record a submit_task line's set_by
// names.
func (f *agentAdmissionFixture) recordSubmitCommand(
	t *testing.T, commandID string, runID domain.RunID, lines ...domain.TaskLineChoice,
) {
	t.Helper()
	ctx := context.Background()
	task := f.task(t, runID)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutTaskSubmissionRequest(ctx, domain.TaskSubmission{
			CommandID: commandID, Origin: domain.TaskSubmissionOriginDevice, DeviceID: new(domain.DeviceID("device-1")), ProjectID: task.ProjectID,
			SourceDigest: agentTestDigest("4"), TaskID: task.ID, SpecificationRunID: runID,
			Name:      domain.DisplayName{Text: "Fixture task", Source: domain.DisplayNameSourceOperator},
			TaskLines: lines,
		}, agentTestDigest("c"))
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *agentAdmissionFixture) appendLine(t *testing.T, in domain.TaskLineInput, setAt time.Time) domain.TaskLine {
	t.Helper()
	ctx := context.Background()
	var line domain.TaskLine
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		line, err = tx.AppendTaskLine(ctx, in, setAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return line
}

// submitLine records a line on the fixture run's task as submit_task does:
// set by a recorded command that created the task and carried the line, at
// the task's creation instant. Each role gets its own command so that a test
// can give several roles a line.
func (f *agentAdmissionFixture) submitLine(t *testing.T, role domain.RoleName, agent string) domain.TaskLine {
	t.Helper()
	commandID := agentTestSubmitCommand + "-" + string(role)
	f.recordSubmitCommand(t, commandID, f.run.ID, domain.TaskLineChoice{Role: role, Agent: agent})
	task := f.task(t, f.run.ID)
	return f.appendLine(t, domain.TaskLineInput{
		TaskID: task.ID, Role: role, Agent: agent,
		Source: domain.TaskLineSourceSubmitTask, SetBy: commandID,
	}, task.CreatedAt)
}

// submitLineV2 records the v2 line a submission choosing the named agent writes
// now: the selection the agent reduces to in the fixture's tree.
func (f *agentAdmissionFixture) submitLineV2(t *testing.T, role domain.RoleName, agent string) domain.TaskLine {
	t.Helper()
	commandID := agentTestSubmitCommand + "-" + string(role)
	selection := f.lineSelection(t, agent)
	f.recordSubmitCommand(t, commandID, f.run.ID, domain.TaskLineChoice{Role: role, Selection: selection})
	task := f.task(t, f.run.ID)
	return f.appendLine(t, domain.TaskLineInput{
		TaskID: task.ID, Role: role, Selection: selection,
		Source: domain.TaskLineSourceSubmitTask, SetBy: commandID,
	}, task.CreatedAt)
}

// recordCLISubmission records a `freesided submit` that created the task of
// taskRun, with the lines it carried, and returns the identity a cli_submit
// line's set_by names. As the command does, it stores the specification run
// derived from the implementation run the submission names, and not that
// implementation run.
func (f *agentAdmissionFixture) recordCLISubmission(
	t *testing.T, id string, taskRun domain.RunID, lines ...domain.TaskLineChoice,
) string {
	t.Helper()
	ctx := context.Background()
	task := f.task(t, taskRun)
	implementation := domain.RunID("run-implementation-" + id)
	seedRunPolicy(t, f.store, domain.Run{
		ID: domain.SpecificationRunIDForImplementation(implementation), ProjectID: task.ProjectID,
		TaskID: task.ID, SpecDigest: agentTestDigest("9"),
	}, nil)
	digest := agentTestDigest("d")
	artifact, err := SubmissionArtifact(domain.ArtifactKindSpecification, digest, domain.EvidenceMediaTextMarkdown, 10)
	if err != nil {
		t.Fatal(err)
	}
	identity := "cli:" + id
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := RegisterSubmissionArtifact(ctx, tx, artifact); err != nil {
			return err
		}
		_, err := tx.AcceptManualSubmission(ctx, domain.ManualSubmission{
			Identity: identity, ProjectID: task.ProjectID, SourceArtifactID: artifact.ID,
			SourceDigest: digest, RequestDigest: agentTestDigest("c"), ImplementationRunID: implementation,
			TaskLines: lines,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return identity
}

func (f *agentAdmissionFixture) admit(t *testing.T) (domain.ExecutionAdmission, bool, error) {
	t.Helper()
	return f.engine.admitAttempt(context.Background(), f.binding, f.stage, f.binding.invocation.ID)
}

// TestTaskLineAdmissionBindsTheLinesAgent is the acceptance fixture: a task
// submitted with an implementer line admits the agent the line names, under
// that agent's identity and credential, and cites the line. The prompt stays
// the lineup line's.
func TestTaskLineAdmissionBindsTheLinesAgent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	identity, enrollment := f.addSecondAgent(t)
	line := f.submitLine(t, domain.RoleImplementer, agentTestSecondAgent)

	admission, admitted, err := f.admit(t)
	if err != nil || !admitted || admission.AgentBinding == nil {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	binding := admission.AgentBinding
	if binding.SelectionSource != domain.AgentSelectionSourceTaskLine || binding.SelectionRecordID != line.ID {
		t.Fatalf("selection = %q %q, want the task line %s", binding.SelectionSource, binding.SelectionRecordID, line.ID)
	}
	digest, err := f.selection.Tree.AgentDigest(agentTestSecondAgent)
	if err != nil {
		t.Fatal(err)
	}
	lineup, err := f.selection.Tree.ResolveLineup()
	if err != nil {
		t.Fatal(err)
	}
	lineupLine, _ := lineup.Line(domain.RoleImplementer)
	if binding.AgentDigest != digest || binding.AgentDigest == lineupLine.AgentDigest ||
		binding.EnrollmentID != enrollment.ID || binding.LineupRevision != f.selection.LineupRevision {
		t.Fatalf("agent binding = %+v, want the second agent %s on %s", *binding, digest, enrollment.ID)
	}
	if admission.AuthIdentityID == nil || *admission.AuthIdentityID != identity.ID {
		t.Fatalf("admitted identity = %v, want %s", admission.AuthIdentityID, identity.ID)
	}
	// A task line picks the agent only.
	if admission.StageInputs.PromptPackageDigest != lineupLine.PromptDigest {
		t.Fatalf("admitted prompt = %s, want the lineup's %s",
			admission.StageInputs.PromptPackageDigest, lineupLine.PromptDigest)
	}
	// The store's gates accept the record and resolve the line it cites, and
	// the attempt mounts the second agent's store.
	f.record(t, admission)
	var stored domain.ExecutionAdmission
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetExecutionAdmission(ctx, admission.InvocationID)
		return err
	}); err != nil || stored.ID != admission.ID {
		t.Fatalf("read back = %s, %v; want %s", stored.ID, err, admission.ID)
	}
	volume, err := f.adapters.Leaser.AuthStoreVolume(ctx, identity.ID, admission.InvocationID)
	if err != nil || volume != "claude-second-generation" {
		t.Fatalf("agent-bound volume = %q, %v", volume, err)
	}
}

// TestTaskLineAdmissionReadsTheCurrentLineOfItsRole: the latest version of a
// role's line decides, and a line for one role moves no other.
func TestTaskLineAdmissionReadsTheCurrentLineOfItsRole(t *testing.T) {
	t.Parallel()
	t.Run("the latest version wins", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		_, enrollment := f.addSecondAgent(t)
		first := f.submitLine(t, domain.RoleImplementer, agentTestWriterAgent)
		second := *f.changeLine(t, domain.RoleImplementer, agentTestSecondAgent, false).Line
		if second.Version != 2 || second.PredecessorID == nil || *second.PredecessorID != first.ID {
			t.Fatalf("second line = %+v, want version 2 superseding %s", second, first.ID)
		}
		admission, admitted, err := f.admit(t)
		if err != nil || !admitted {
			t.Fatalf("admitAttempt = %t, %v", admitted, err)
		}
		if admission.AgentBinding.SelectionRecordID != second.ID ||
			admission.AgentBinding.EnrollmentID != enrollment.ID {
			t.Fatalf("binding = %+v, want version 2 (%s) and its agent", *admission.AgentBinding, second.ID)
		}
	})

	t.Run("a line names the lineup's own agent", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		line := f.submitLine(t, domain.RoleImplementer, agentTestWriterAgent)
		admission, admitted, err := f.admit(t)
		if err != nil || !admitted {
			t.Fatalf("admitAttempt = %t, %v", admitted, err)
		}
		// The operator's choice is recorded as theirs even when it agrees
		// with the lineup.
		if admission.AgentBinding.SelectionSource != domain.AgentSelectionSourceTaskLine ||
			admission.AgentBinding.SelectionRecordID != line.ID ||
			admission.AgentBinding.EnrollmentID != f.enrollment.ID {
			t.Fatalf("binding = %+v", *admission.AgentBinding)
		}
	})

	t.Run("a line for another role", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		f.addSecondAgent(t)
		f.submitLine(t, domain.RoleSpecifier, agentTestSecondAgent)
		admission, admitted, err := f.admit(t)
		if err != nil || !admitted {
			t.Fatalf("admitAttempt = %t, %v", admitted, err)
		}
		if admission.AgentBinding.SelectionSource != domain.AgentSelectionSourceLineup ||
			admission.AgentBinding.SelectionRecordID != "" || admission.AgentBinding.EnrollmentID != f.enrollment.ID {
			t.Fatalf("implementer binding = %+v, want the lineup's agent and no record", *admission.AgentBinding)
		}
	})

	t.Run("a CLI submission's line", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		_, enrollment := f.addSecondAgent(t)
		task := f.task(t, f.run.ID)
		line := f.appendLine(t, domain.TaskLineInput{
			TaskID: task.ID, Role: domain.RoleImplementer, Agent: agentTestSecondAgent,
			Source: domain.TaskLineSourceCLISubmit, SetBy: f.recordCLISubmission(t, "submission-1", f.run.ID,
				domain.TaskLineChoice{Role: domain.RoleImplementer, Agent: agentTestSecondAgent}),
		}, task.CreatedAt)
		admission, admitted, err := f.admit(t)
		if err != nil || !admitted {
			t.Fatalf("admitAttempt = %t, %v", admitted, err)
		}
		if admission.AgentBinding.SelectionRecordID != line.ID || admission.AgentBinding.EnrollmentID != enrollment.ID {
			t.Fatalf("binding = %+v, want the CLI line %s and its agent", *admission.AgentBinding, line.ID)
		}
	})
}

// TestTaskLineAdmissionRefusals: a line that cannot be honored refuses the
// attempt through the ordinary hold. It never admits the lineup's agent,
// which in every case here would pass.
func TestTaskLineAdmissionRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// otherTask seeds a second run, and so a second task, in the project.
	otherTask := func(t *testing.T, f *agentAdmissionFixture) domain.RunID {
		t.Helper()
		seedRunPolicy(t, f.store, domain.Run{ID: "run-other", ProjectID: f.run.ProjectID, SpecDigest: agentTestDigest("9")}, nil)
		if f.task(t, "run-other").ID == f.task(t, f.run.ID).ID {
			t.Fatal("the second run shares the fixture run's task")
		}
		return "run-other"
	}
	// line appends an implementer line naming the second agent, which admits.
	line := func(t *testing.T, f *agentAdmissionFixture, source domain.TaskLineSource, setBy string, setAt time.Time) {
		t.Helper()
		f.appendLine(t, domain.TaskLineInput{
			TaskID: f.task(t, f.run.ID).ID, Role: domain.RoleImplementer, Agent: agentTestSecondAgent,
			Source: source, SetBy: setBy,
		}, setAt)
	}
	cases := []struct {
		name  string
		setup func(t *testing.T, f *agentAdmissionFixture)
		want  string
	}{
		{
			name: "the line names an agent the tree lacks",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.submitLine(t, domain.RoleImplementer, agentTestPastedName)
			},
			want: "names an agent the tree lacks",
		},
		{
			name: "the line's agent runs under a disabled identity",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				identity, _ := f.addSecondAgent(t)
				identity.Enabled = false
				if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
					return tx.RecordAuthIdentity(ctx, identity, agentTestAt)
				}); err != nil {
					t.Fatal(err)
				}
				f.submitLine(t, domain.RoleImplementer, agentTestSecondAgent)
			},
			want: "is disabled",
		},
		{
			name: "the role has no lineup line to take its prompt from",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.submitLine(t, domain.RoleImplementer, agentTestSecondAgent)
				agents := f.engine.admission.environment.Agents
				agents.Tree.Lineup = slices.DeleteFunc(slices.Clone(agents.Tree.Lineup), func(l agenttree.LineupLine) bool {
					return l.Key == string(domain.RoleImplementer)
				})
			},
			want: "the lineup has no line for it",
		},
		{
			name: "set_by names no recorded command",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				line(t, f, domain.TaskLineSourceSubmitTask, "cmd-never-recorded", f.task(t, f.run.ID).CreatedAt)
			},
			want: "not found",
		},
		{
			name: "set_by names a command that created another task",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.recordSubmitCommand(t, "cmd-other-task", otherTask(t, f))
				line(t, f, domain.TaskLineSourceSubmitTask, "cmd-other-task", f.task(t, f.run.ID).CreatedAt)
			},
			want: "that command created task",
		},
		{
			name: "set_at is not the task's creation instant",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.recordSubmitCommand(t, agentTestSubmitCommand, f.run.ID)
				line(t, f, domain.TaskLineSourceSubmitTask, agentTestSubmitCommand,
					f.task(t, f.run.ID).CreatedAt.Add(time.Second))
			},
			want: "its task was created at",
		},
		{
			name: "a CLI line names a submission never recorded",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				line(t, f, domain.TaskLineSourceCLISubmit, "cli:never-recorded", f.task(t, f.run.ID).CreatedAt)
			},
			want: "not found",
		},
		{
			name: "a CLI line names a submission whose run belongs to another task",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				identity := f.recordCLISubmission(t, "submission-other", otherTask(t, f))
				line(t, f, domain.TaskLineSourceCLISubmit, identity, f.task(t, f.run.ID).CreatedAt)
			},
			want: "that submission created task",
		},
		{
			// The swapped-agent attack: a real command, the right instant,
			// and another credential. Both agents are pasted text, so the
			// loop's check also proves the refusal repeats neither.
			name: "the command recorded another agent for the role",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.recordSubmitCommand(t, agentTestSubmitCommand, f.run.ID, domain.TaskLineChoice{
					Role: domain.RoleImplementer, Agent: agentTestPastedName + "-recorded",
				})
				task := f.task(t, f.run.ID)
				f.appendLine(t, domain.TaskLineInput{
					TaskID: task.ID, Role: domain.RoleImplementer, Agent: agentTestPastedName,
					Source: domain.TaskLineSourceSubmitTask, SetBy: agentTestSubmitCommand,
				}, task.CreatedAt)
			},
			want: "that command's record names another agent for this role",
		},
		{
			name: "the CLI submission recorded another agent for the role",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				identity := f.recordCLISubmission(t, "submission-1", f.run.ID, domain.TaskLineChoice{
					Role: domain.RoleImplementer, Agent: agentTestPastedName + "-recorded",
				})
				task := f.task(t, f.run.ID)
				f.appendLine(t, domain.TaskLineInput{
					TaskID: task.ID, Role: domain.RoleImplementer, Agent: agentTestPastedName,
					Source: domain.TaskLineSourceCLISubmit, SetBy: identity,
				}, task.CreatedAt)
			},
			want: "that command's record names another agent for this role",
		},
		{
			name: "the command recorded no line for the role",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.recordSubmitCommand(t, agentTestSubmitCommand, f.run.ID, domain.TaskLineChoice{
					Role: domain.RoleReviewer, Agent: agentTestSecondAgent,
				})
				line(t, f, domain.TaskLineSourceSubmitTask, agentTestSubmitCommand, f.task(t, f.run.ID).CreatedAt)
			},
			want: "that command's record names no agent for this role",
		},
		{
			name: "the CLI submission recorded no line for the role",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				identity := f.recordCLISubmission(t, "submission-1", f.run.ID, domain.TaskLineChoice{
					Role: domain.RoleReviewer, Agent: agentTestSecondAgent,
				})
				line(t, f, domain.TaskLineSourceCLISubmit, identity, f.task(t, f.run.ID).CreatedAt)
			},
			want: "that command's record names no agent for this role",
		},
		{
			// A forged later version naming the recorded agent would undo the
			// operator's change to the role.
			name: "a later version cites the submission",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.submitLine(t, domain.RoleImplementer, agentTestSecondAgent)
				f.changeLine(t, domain.RoleImplementer, agentTestWriterAgent, false)
				task := f.task(t, f.run.ID)
				f.appendLine(t, domain.TaskLineInput{
					TaskID: task.ID, Role: domain.RoleImplementer, Agent: agentTestSecondAgent,
					Source: domain.TaskLineSourceSubmitTask, SetBy: agentTestSubmitCommand + "-" + string(domain.RoleImplementer),
				}, task.CreatedAt)
			},
			want: "a submission sets only a role's first version",
		},
		{
			// A record from before the field decodes with no lines, so it
			// backs no line (TestTaskLineChangeRecoversAPreFieldLine).
			name: "the command predates recorded lines",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.recordSubmitCommand(t, agentTestSubmitCommand, f.run.ID)
				line(t, f, domain.TaskLineSourceSubmitTask, agentTestSubmitCommand, f.task(t, f.run.ID).CreatedAt)
			},
			want: "that command's record names no agent for this role",
		},
		{
			name: "the CLI submission predates recorded lines",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				identity := f.recordCLISubmission(t, "submission-1", f.run.ID)
				line(t, f, domain.TaskLineSourceCLISubmit, identity, f.task(t, f.run.ID).CreatedAt)
			},
			want: "that command's record names no agent for this role",
		},
		{
			// submit_task records a manual submission too, under client:. A
			// line claiming the CLI must not anchor to it.
			name: "a CLI line names a record of another kind",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.recordSubmitCommand(t, agentTestSubmitCommand, f.run.ID)
				line(t, f, domain.TaskLineSourceCLISubmit, agentTestSubmitCommand, f.task(t, f.run.ID).CreatedAt)
			},
			want: "not a CLI submission identity",
		},
		{
			name: "a submit_task line names a CLI submission",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				identity := f.recordCLISubmission(t, "submission-1", f.run.ID)
				line(t, f, domain.TaskLineSourceSubmitTask, identity, f.task(t, f.run.ID).CreatedAt)
			},
			want: "not found",
		},
		{
			name: "a proposal_start line names a submit_task command",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.recordSubmitCommand(t, agentTestSubmitCommand, f.run.ID)
				line(t, f, domain.TaskLineSourceProposalStart, agentTestSubmitCommand, f.task(t, f.run.ID).CreatedAt)
			},
			want: "not found",
		},
		{
			name: "a router line names another task's routing",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour), domain.RoutingPolicyRole{
					Role: domain.RoleImplementer, Agents: []string{agentTestSecondAgent},
				})
				routerLine(t, f, "router:task-other", policyOrder(1, routerSecondPool), agentTestAt)
			},
			want: "it does not name its own task's routing",
		},
		{
			name: "a router line is a later version",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour), domain.RoutingPolicyRole{
					Role: domain.RoleImplementer, Agents: []string{agentTestSecondAgent},
				})
				f.submitLine(t, domain.RoleImplementer, agentTestWriterAgent)
				routerLine(t, f, routerSetBy(f.task(t, f.run.ID).ID), policyOrder(1, routerSecondPool), agentTestAt)
			},
			want: "the router sets only a role's first version",
		},
		{
			name: "a router line cites a policy revision never set",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				routerLine(t, f, routerSetBy(f.task(t, f.run.ID).ID), policyOrder(1, routerSecondPool), agentTestAt)
			},
			want: "routing policy revision 1 was never set",
		},
		{
			name: "a router line predates the policy revision it cites",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.setRoutingPolicy(t, agentTestAt.Add(time.Second), domain.RoutingPolicyRole{
					Role: domain.RoleImplementer, Agents: []string{agentTestSecondAgent},
				})
				routerLine(t, f, routerSetBy(f.task(t, f.run.ID).ID), policyOrder(1, routerSecondPool), agentTestAt)
			},
			want: "routing policy revision 1 was set after the line",
		},
		{
			name: "the cited revision does not order the line's agent for its role",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour), domain.RoutingPolicyRole{
					Role: domain.RoleImplementer, Agents: []string{agentTestWriterAgent},
				}, domain.RoutingPolicyRole{
					Role: domain.RoleReviewer, Agents: []string{agentTestSecondAgent},
				})
				routerLine(t, f, routerSetBy(f.task(t, f.run.ID).ID), policyOrder(1, routerSecondPool), agentTestAt)
			},
			want: "routing policy revision 1 does not order the line's agent for its role",
		},
		{
			name: "the cited revision orders no agent the line passed over",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour), domain.RoutingPolicyRole{
					Role: domain.RoleImplementer, Agents: []string{agentTestSecondAgent},
				})
				provenance := policyOrder(1, routerSecondPool)
				provenance.PassedOver = &domain.TaskLinePassedOverMark{
					Agent: agentTestPastedName, Pool: routerMainPool, Window: "five_hour",
					Source: domain.ExhaustedMarkProviderRefusal, MarkedAt: agentTestAt.Add(-time.Minute),
				}
				routerLine(t, f, routerSetBy(f.task(t, f.run.ID).ID), provenance, agentTestAt)
			},
			want: "routing policy revision 1 orders no agent the line passed over",
		},
		{
			name: "the cited revision drains no account first",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour), domain.RoutingPolicyRole{
					Role: domain.RoleImplementer, Agents: []string{agentTestSecondAgent},
				})
				provenance := policyOrder(1, routerSecondPool)
				provenance.Tier = domain.TaskLineSelectionDrainFirstAccount
				routerLine(t, f, routerSetBy(f.task(t, f.run.ID).ID), provenance, agentTestAt)
			},
			want: "routing policy revision 1 drains no such account first",
		},
		{
			name: "the cited revision drains another account first",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.setDrainingPolicy(t, agentTestAt.Add(-time.Hour), new(routerMainPool), domain.RoutingPolicyRole{
					Role: domain.RoleImplementer, Agents: []string{agentTestSecondAgent},
				})
				provenance := policyOrder(1, routerSecondPool)
				provenance.Tier = domain.TaskLineSelectionDrainFirstAccount
				routerLine(t, f, routerSetBy(f.task(t, f.run.ID).ID), provenance, agentTestAt)
			},
			want: "routing policy revision 1 drains no such account first",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			// Without the line the attempt admits through the lineup.
			if _, admitted, err := f.admit(t); err != nil || !admitted {
				t.Fatalf("admitAttempt before the line = %t, %v", admitted, err)
			}
			tc.setup(t, f)
			admission, admitted, err := f.admit(t)
			if admitted || admission.AgentBinding != nil || !errors.Is(err, ErrAgentNotAdmissible) ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("admitAttempt = %t, %v; want a refusal naming %q", admitted, err, tc.want)
			}
			if strings.Contains(err.Error(), agentTestPastedName) {
				t.Errorf("refusal %q repeats the line's agent text", err)
			}
			// The ordinary hold and card path, and nothing recorded.
			if !invocationDispatchHold(err) {
				t.Fatal("the refusal was not classified as an invocation hold")
			}
			if reason, ok := dispatchHoldReason(err); !ok || reason != domain.HoldAdmissionPolicyRefused {
				t.Fatalf("hold reason = %q, %t", reason, ok)
			}
			err = f.store.Read(ctx, func(tx *store.ReadTx) error {
				_, err := tx.GetExecutionAdmission(ctx, f.binding.invocation.ID)
				return err
			})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("stored admission after a refusal = %v, want %v", err, store.ErrNotFound)
			}
		})
	}
}

func taskLineProposal(t *testing.T, f *agentAdmissionFixture) (ProposalAdmissionResult, *signet.Service, signet.ClientCommand) {
	t.Helper()
	ctx := t.Context()
	request := proposalAdmissionFixture(t, "admit-task-line")
	seedProposalAdmissionPolicy(t, ctx, f.store, request)
	engine := newProposalAdmissionEngine(t, f.store)
	proposal, err := engine.admitProposalAt(ctx, request, agentTestAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: "device-lines", DisplayName: "Mac", Status: domain.DeviceActive, PairedAt: agentTestAt.Add(-time.Hour),
		})
	}); err != nil {
		t.Fatal(err)
	}
	service := signet.NewService(f.store, signet.WithBlobStore(engine.artifacts.(*signet.BlobStore)),
		signet.WithClock(func() time.Time { return agentTestAt.Add(time.Minute) }))
	return proposal, service, signet.ClientCommand{
		CommandID: "start-task-line", DeviceID: "device-lines", ExpectedEntityVersion: 1,
		Payload: signet.DecisionPayload{
			ItemID: proposal.Item.ID, ItemVersion: proposal.Item.ItemVersion, Action: domain.ActionStart,
			ArtifactDigests: proposal.Item.ArtifactDigests,
		},
	}
}

func TestProposalTaskLineAdmitsChosenSpecifier(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	_, enrollment := f.addSecondAgent(t)
	_, service, command := taskLineProposal(t, f)
	command.Payload.TaskLines = []domain.TaskLineChoice{{
		Role: domain.RoleSpecifier, Selection: f.lineSelection(t, agentTestSecondAgent),
	}}
	if _, err := service.Submit(t.Context(), command); err != nil {
		t.Fatal(err)
	}
	runID := proposalAdmissionPolicy(t).RunID
	var line domain.TaskLine
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var found bool
		var err error
		line, found, err = RunTaskLine(t.Context(), tx, agenttree.Tree{}, runID, domain.RoleSpecifier)
		if err == nil && !found {
			t.Fatal("the proposal's specification run has no line")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	admission, err := resolveAgentAdmission(t.Context(), f.store, f.selection, runID, domain.RoleSpecifier,
		agentTestPrompts[domain.RoleSpecifier].Digest, agentTestAt)
	if err != nil {
		t.Fatal(err)
	}
	if admission.binding.SelectionSource != domain.AgentSelectionSourceTaskLine ||
		admission.binding.SelectionRecordID != line.ID || admission.binding.EnrollmentID != enrollment.ID {
		t.Fatalf("binding = %+v, want the proposal line and its agent's enrollment", admission.binding)
	}
}

func TestProposalTaskLineAnchorRefusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
	}{
		{"missing command", "not found"},
		{"wrong action", "not a proposal start"},
		{"unattended start", "unattended start"},
		{"no decision", "not the proposal's recorded decision"},
		{"different decision", "not the proposal's recorded decision"},
		{"different task", "that proposal starts task"},
		{"different instant", "decision instant"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			f.addSecondAgent(t)
			proposal, service, command := taskLineProposal(t, f)
			task := f.task(t, proposalAdmissionPolicy(t).RunID)
			setBy := command.CommandID
			setAt := agentTestAt.Add(time.Minute)
			switch tc.name {
			case "missing command":
				setBy = "never-recorded"
			case "wrong action", "no decision", "different decision":
				action := domain.ActionStart
				if tc.name == "wrong action" {
					action = domain.ActionDecline
				}
				if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
					record, err := domain.NewCommand(domain.CommandInput{
						CommandID: command.CommandID, DeviceID: command.DeviceID, ItemID: proposal.Item.ID,
						ItemVersion: proposal.Item.ItemVersion, ArtifactDigests: proposal.Item.ArtifactDigests, Action: action,
					})
					if err != nil {
						return err
					}
					return tx.PutCommand(t.Context(), record)
				}); err != nil {
					t.Fatal(err)
				}
				if tc.name == "different decision" {
					command.CommandID = "actual-decision"
					if _, err := service.Submit(t.Context(), command); err != nil {
						t.Fatal(err)
					}
				}
			case "unattended start":
				if started, err := service.StartTaskProposalUnattended(t.Context(), proposal.Item.ID, command.CommandID); err != nil || !started {
					t.Fatalf("unattended start = %t, %v", started, err)
				}
			case "different task", "different instant":
				if _, err := service.Submit(t.Context(), command); err != nil {
					t.Fatal(err)
				}
				if tc.name == "different task" {
					task = f.task(t, f.run.ID)
				} else {
					setAt = setAt.Add(time.Nanosecond)
				}
			}
			f.appendLine(t, domain.TaskLineInput{
				TaskID: task.ID, Role: domain.RoleImplementer, Agent: agentTestSecondAgent,
				Source: domain.TaskLineSourceProposalStart, SetBy: setBy,
			}, setAt)
			runID := proposalAdmissionPolicy(t).RunID
			if tc.name == "different task" {
				runID = f.run.ID
			}
			admission, err := resolveAgentAdmission(t.Context(), f.store, f.selection, runID, domain.RoleImplementer,
				agentTestPrompts[domain.RoleImplementer].Digest, agentTestAt)
			if !errors.Is(err, ErrAgentNotAdmissible) || !strings.Contains(err.Error(), tc.want) || admission.binding.AgentDigest != "" {
				t.Fatalf("admission = %+v, %v; want refusal naming %q", admission.binding, err, tc.want)
			}
		})
	}
}

func TestRepeatedProposalStartsKeepTaskLineHistory(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	_, service, first := taskLineProposal(t, f)
	first.Payload.TaskLines = []domain.TaskLineChoice{{
		Role: domain.RoleSpecifier, Selection: f.lineSelection(t, agentTestSecondAgent),
	}}
	result, err := service.Submit(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	runID := proposalAdmissionPolicy(t).RunID
	readLine := func() domain.TaskLine {
		t.Helper()
		var line domain.TaskLine
		if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			line, _, err = RunTaskLine(t.Context(), tx, agenttree.Tree{}, runID, domain.RoleSpecifier)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return line
	}
	original := readLine()
	engine := newProposalAdmissionEngine(t, f.store)
	for i, choices := range [][]domain.TaskLineChoice{
		nil, {{Role: domain.RoleSpecifier, Selection: f.lineSelection(t, agentTestWriterAgent)}},
	} {
		id := []string{"lineless-repeat", "changed-repeat"}[i]
		request := proposalAdmissionFixture(t, id)
		proposal, err := engine.admitProposalAt(t.Context(), request, agentTestAt)
		if err != nil {
			t.Fatal(err)
		}
		command := first
		command.CommandID = id
		command.Payload.ItemID = proposal.Item.ID
		command.Payload.ArtifactDigests = proposal.Item.ArtifactDigests
		command.Payload.TaskLines = choices
		if _, err := service.Submit(t.Context(), command); err != nil {
			t.Fatal(err)
		}
		line := readLine()
		if i == 0 && line.ID != original.ID {
			t.Fatal("a lineless start replaced the reused task's earlier choice")
		}
		if i == 1 && (line.Version != 2 || line.PredecessorID == nil || *line.PredecessorID != original.ID || f.lineAgent(line) != agentTestWriterAgent) {
			t.Fatalf("changed start did not supersede the earlier line: %+v", line)
		}
	}
	if replay, err := service.Submit(t.Context(), first); err != nil || replay.Revision != result.Revision {
		t.Fatalf("replay after supersession = %+v, %v; want original result", replay, err)
	}
}

// TestTaskLinesLeaveLineupOnlyRolesAlone: with a line recorded for every
// eligible role of the task, the shadow reviewer and a wardless role still
// resolve from the lineup, and the startup check still proves the lineup's
// agent. None of the three has a task in view.
func TestTaskLinesLeaveLineupOnlyRolesAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	for _, role := range domain.TaskLineRoles {
		f.submitLine(t, role, agentTestSecondAgent)
	}
	tree := f.selection.Tree
	digest, err := tree.AgentDigest(agentTestWriterAgent)
	if err != nil {
		t.Fatal(err)
	}
	prompt := agentTestDigest("a")
	tree.Lineup = slices.Clone(tree.Lineup)
	for _, role := range []domain.RoleName{domain.RoleShadowReviewer, domain.RoleTaskNamer} {
		tree.Lineup = append(tree.Lineup, agenttree.LineupLine{
			Key: string(role),
			Selection: domain.LineupSelection{
				AgentName: agentTestWriterAgent, AgentDigest: digest,
				PromptName: "lineup-only", PromptDigest: prompt,
			},
		})
	}
	for _, role := range []domain.RoleName{domain.RoleShadowReviewer, domain.RoleTaskNamer} {
		agent, err := ResolveRole(ctx, f.store, tree, role)
		if err != nil {
			t.Fatalf("ResolveRole(%s) = %v", role, err)
		}
		if agent.TaskLine != nil || agent.Line.AgentName != agentTestWriterAgent ||
			agent.Enrollment.ID != f.enrollment.ID {
			t.Fatalf("%s resolved %+v under %s, want the lineup's agent", role, agent.Line, agent.Enrollment.ID)
		}
	}

	// The wardless admission takes its agent from the line it is handed, and
	// the lineup's is the only one a caller can resolve for it.
	namer, err := ResolveRole(ctx, f.store, tree, domain.RoleTaskNamer)
	if err != nil {
		t.Fatal(err)
	}
	wardless, err := domain.AdmitWardlessRole(domain.WardlessAdmissionInput{
		Role: domain.RoleTaskNamer, Line: namer.Line, LineupRevision: f.selection.LineupRevision,
		Agent: namer.Resolved.Definition, Route: namer.Resolved.Route,
		Adapter: namer.Resolved.Adapter, Offer: namer.Resolved.Offer,
		PromptDigest: prompt, Enrollment: namer.Enrollment, Generation: namer.Generation,
		Deadline: agentTestAt.Add(time.Hour), ExpiryMargin: time.Minute,
		LaunchProof: &domain.InterimCallLaunchAudit{
			AdapterDigest: namer.Resolved.Adapter.Digest, HarnessBuild: namer.Resolved.Adapter.HarnessBuild,
			AuditedOn: "2026-09-01",
		},
	})
	if err != nil || wardless.AgentDigest != digest || wardless.EnrollmentID != f.enrollment.ID {
		t.Fatalf("AdmitWardlessRole = %+v, %v; want the lineup's agent %s", wardless, err, digest)
	}

	binding, err := f.selection.CheckRole(ctx, f.store, domain.RoleImplementer,
		agentTestPrompts[domain.RoleImplementer].Digest, agentTestAt)
	if err != nil || binding.SelectionSource != domain.AgentSelectionSourceLineup ||
		binding.SelectionRecordID != "" || binding.EnrollmentID != f.enrollment.ID {
		t.Fatalf("CheckRole = %+v, %v; want the lineup's agent", binding, err)
	}
}

// TestLinelessAdmissionSelectsFromTheLineup: a run whose task has no lines
// admits the binding the lineup-only resolution yields, with the lineup as
// its source and no record cited.
func TestLinelessAdmissionSelectsFromTheLineup(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	admission, admitted, err := f.admit(t)
	if err != nil || !admitted || admission.AgentBinding == nil {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	lineupOnly, err := f.selection.CheckRole(context.Background(), f.store, domain.RoleImplementer,
		admission.StageInputs.PromptPackageDigest, agentTestAt)
	if err != nil {
		t.Fatal(err)
	}
	if lineupOnly.SelectionSource != domain.AgentSelectionSourceLineup || lineupOnly.SelectionRecordID != "" {
		t.Fatalf("lineup-only selection = %q %q", lineupOnly.SelectionSource, lineupOnly.SelectionRecordID)
	}
	if !reflect.DeepEqual(*admission.AgentBinding, lineupOnly) {
		t.Fatalf("lineless binding = %+v, want the lineup-only resolution %+v", *admission.AgentBinding, lineupOnly)
	}
}

// agentTestEscalatedAgent is the specifier's escalated counterpart the
// triage tests add to the tree.
const agentTestEscalatedAgent = "claude-escalated"

// agentTestEscalatedModel is the escalated counterpart's own model.
const agentTestEscalatedModel = "claude-escalated-test"

// addEscalatedSpecifier adds to the tree an escalated counterpart of the
// specifier's lineup agent, on the same enrollment and adapter with an offer
// of its own, and, when withLine, the escalated line pairing the two.
func (f *agentAdmissionFixture) addEscalatedSpecifier(t *testing.T, withLine bool) {
	t.Helper()
	f.addEscalatedSpecifierOn(t, withLine, agentTestEscalatedModel)
}

// addEscalatedSpecifierOn is addEscalatedSpecifier with the counterpart's
// offer on model.
func (f *agentAdmissionFixture) addEscalatedSpecifierOn(t *testing.T, withLine bool, model string) {
	t.Helper()
	tree := f.selection.Tree
	specifierAgent := agentbaseline.WriterAgentName(domain.RoleSpecifier)
	specifier, ok := tree.Agent(specifierAgent)
	if !ok {
		t.Fatalf("the tree has no specifier agent %s", specifierAgent)
	}
	offers := slices.Clone(tree.Offers)
	i := slices.IndexFunc(offers, func(o agenttree.Offer) bool { return o.Name == specifier.Offer })
	if i < 0 {
		t.Fatalf("the tree has no offer %s", specifier.Offer)
	}
	offer := offers[i]
	offer.Name = agentTestEscalatedAgent
	offer.Fragment.RouteModelID = model
	var err error
	if offer.Fragment.Digest, err = offer.Fragment.ComputeDigest(); err != nil {
		t.Fatal(err)
	}
	tree.Offers = append(offers, offer)
	escalated := specifier
	escalated.Name, escalated.Offer = agentTestEscalatedAgent, offer.Name
	tree.Agents = append(slices.Clone(tree.Agents), escalated)
	tree.Sort()
	if withLine {
		line, err := lineupLine(tree, domain.RoleSpecifier)
		if err != nil {
			t.Fatal(err)
		}
		line.AgentName = agentTestEscalatedAgent
		if line.AgentDigest, err = tree.AgentDigest(agentTestEscalatedAgent); err != nil {
			t.Fatal(err)
		}
		key, err := domain.LineupEscalatedKey(domain.RoleSpecifier, specifierAgent)
		if err != nil {
			t.Fatal(err)
		}
		tree.Lineup = append(slices.Clone(tree.Lineup), agenttree.LineupLine{
			Key: strings.TrimPrefix(key, domain.LineupRoleKeyPrefix), Selection: line,
		})
	}
	f.useTree(t, tree)
}

// staleEscalatedDigest repins the escalated line to the specifier agent's
// digest, as if the counterpart's definition changed after the line pinned
// it. The tree still parses: nothing but admission compares the two.
func (f *agentAdmissionFixture) staleEscalatedDigest(t *testing.T) {
	t.Helper()
	tree := f.selection.Tree
	specifierAgent := agentbaseline.WriterAgentName(domain.RoleSpecifier)
	stale, err := tree.AgentDigest(specifierAgent)
	if err != nil {
		t.Fatal(err)
	}
	key, err := domain.LineupEscalatedKey(domain.RoleSpecifier, specifierAgent)
	if err != nil {
		t.Fatal(err)
	}
	tree.Lineup = slices.Clone(tree.Lineup)
	i := slices.IndexFunc(tree.Lineup, func(l agenttree.LineupLine) bool {
		return l.Key == strings.TrimPrefix(key, domain.LineupRoleKeyPrefix)
	})
	if i < 0 {
		t.Fatal("the tree has no escalated line")
	}
	tree.Lineup[i].Selection.AgentDigest = stale
	f.useTree(t, tree)
}

// useTree renders and reparses tree and makes it the admission's agents.
func (f *agentAdmissionFixture) useTree(t *testing.T, tree agenttree.Tree) {
	t.Helper()
	files, err := agenttree.Render(tree)
	if err != nil {
		t.Fatal(err)
	}
	if f.selection.LineupRevision, err = agenttree.Revision(files); err != nil {
		t.Fatal(err)
	}
	if f.selection.Tree, err = agenttree.Parse(files); err != nil {
		t.Fatal(err)
	}
	selection := f.selection
	f.engine.admission.environment.Agents = &selection
}

// recordTriage stores the fixture task's triage record.
func (f *agentAdmissionFixture) recordTriage(t *testing.T, record domain.TaskTriage) {
	t.Helper()
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordTaskTriage(t.Context(), record)
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *agentAdmissionFixture) admitRole(t *testing.T, role domain.RoleName) (agentAdmission, error) {
	t.Helper()
	return resolveAgentAdmission(t.Context(), f.store, f.selection, f.run.ID, role,
		agentTestPrompts[role].Digest, agentTestAt)
}

// A triage line admits the specifier on the escalated counterpart the
// lineup names for its lineup agent, on that agent's enrollment, and the
// admission cites the line.
func TestTriageTaskLineAdmitsTheEscalatedSpecifier(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addEscalatedSpecifier(t, true)
	task := f.task(t, f.run.ID)
	f.recordTriage(t, domain.TaskTriage{
		TaskID: task.ID, Outcome: domain.TriageOutcomeCallFailed, Escalated: true, RecordedAt: agentTestAt,
	})
	line := f.appendLine(t, domain.TaskLineInput{
		TaskID: task.ID, Role: domain.RoleSpecifier, Agent: agentTestEscalatedAgent,
		Source: domain.TaskLineSourceTriage, SetBy: string(task.ID),
	}, agentTestAt)

	admission, err := f.admitRole(t, domain.RoleSpecifier)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := f.selection.Tree.AgentDigest(agentTestEscalatedAgent)
	if err != nil {
		t.Fatal(err)
	}
	binding := admission.binding
	if binding.SelectionSource != domain.AgentSelectionSourceTaskLine || binding.SelectionRecordID != line.ID ||
		binding.AgentDigest != digest || binding.EnrollmentID != f.enrollment.ID {
		t.Fatalf("binding = %+v, want the triage line on %s under %s", binding, digest, f.enrollment.ID)
	}
}

// Each check of a triage line's anchor refuses on its own, and a refused
// line never falls back to the lineup's agent.
func TestTriageTaskLineAnchorRefusals(t *testing.T) {
	t.Parallel()
	escalated := domain.TaskTriage{Outcome: domain.TriageOutcomeBudgetSpent, Escalated: true, RecordedAt: agentTestAt}
	standard := domain.TaskTriage{
		Outcome: domain.TriageOutcomeAnswered, Tier: new(domain.TriageTierStandard), RecordedAt: agentTestAt,
	}
	for _, tc := range []struct {
		name   string
		role   domain.RoleName
		agent  string
		setBy  string // empty: the line's own task
		record *domain.TaskTriage
		noLine bool // the lineup carries no escalated line
		stale  bool // the escalated line pins a stale counterpart digest
		prior  bool // a triage line already holds the specifier's first version
		setAt  time.Time
		want   string
	}{
		{
			name: "another role", role: domain.RoleImplementer, agent: agentTestEscalatedAgent, record: &escalated,
			want: "sets only the specifier",
		},
		{
			name: "another task", role: domain.RoleSpecifier, agent: agentTestEscalatedAgent, setBy: "task-other",
			record: &escalated, want: "cites another",
		},
		{
			name: "no triage record", role: domain.RoleSpecifier, agent: agentTestEscalatedAgent,
			want: "no triage record",
		},
		{
			name: "record did not escalate", role: domain.RoleSpecifier, agent: agentTestEscalatedAgent,
			record: &standard, want: "did not escalate",
		},
		{
			name: "not the counterpart", role: domain.RoleSpecifier, agent: agentTestSecondAgent, record: &escalated,
			want: "an agent other than " + agentTestEscalatedAgent,
		},
		{
			name: "no escalated line", role: domain.RoleSpecifier, agent: agentTestEscalatedAgent, record: &escalated,
			noLine: true, want: "no escalated counterpart",
		},
		{
			name: "stale counterpart digest", role: domain.RoleSpecifier, agent: agentTestEscalatedAgent,
			record: &escalated, stale: true, want: "the escalated line selects agent digest",
		},
		{
			name: "not the first version", role: domain.RoleSpecifier, agent: agentTestEscalatedAgent,
			record: &escalated, prior: true, want: "only the specifier's first version",
		},
		{
			name: "not the triage instant", role: domain.RoleSpecifier, agent: agentTestEscalatedAgent,
			record: &escalated, setAt: agentTestAt.Add(time.Second), want: "not set at its task's triage instant",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			f.addSecondAgent(t)
			f.addEscalatedSpecifier(t, !tc.noLine)
			if tc.stale {
				f.staleEscalatedDigest(t)
			}
			task := f.task(t, f.run.ID)
			if tc.record != nil {
				record := *tc.record
				record.TaskID = task.ID
				f.recordTriage(t, record)
			}
			setBy := tc.setBy
			if setBy == "" {
				setBy = string(task.ID)
			}
			in := domain.TaskLineInput{
				TaskID: task.ID, Role: tc.role, Agent: tc.agent, Source: domain.TaskLineSourceTriage, SetBy: setBy,
			}
			if tc.prior {
				f.appendLine(t, in, agentTestAt)
			}
			setAt := agentTestAt
			if !tc.setAt.IsZero() {
				setAt = tc.setAt
			}
			f.appendLine(t, in, setAt)
			admission, err := f.admitRole(t, tc.role)
			if !errors.Is(err, ErrAgentNotAdmissible) || !strings.Contains(err.Error(), tc.want) ||
				admission.binding.AgentDigest != "" {
				t.Fatalf("admission = %+v, %v; want refusal naming %q", admission.binding, err, tc.want)
			}
		})
	}
}

// Until the triager exists, the operator escalates a task by hand: a
// cli_line_change line naming the escalated agent admits the specifier on
// it, with no triage record behind it.
func TestOperatorLineEscalatesTheSpecifierByHand(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addEscalatedSpecifier(t, true)
	task := f.task(t, f.run.ID)
	change, err := domain.NewTaskLineChange(task.ID, domain.RoleSpecifier, f.lineSelection(t, agentTestEscalatedAgent), nil, agentTestAt)
	if err != nil {
		t.Fatal(err)
	}
	var line domain.TaskLine
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		line, err = tx.RecordTaskLineChange(t.Context(), change, domain.TaskLineSourceCLILineChange)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	admission, err := f.admitRole(t, domain.RoleSpecifier)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := f.selection.Tree.AgentDigest(agentTestEscalatedAgent)
	if err != nil {
		t.Fatal(err)
	}
	if admission.binding.SelectionRecordID != line.ID || admission.binding.AgentDigest != digest {
		t.Fatalf("binding = %+v, want the operator's line on the escalated agent", admission.binding)
	}
}
