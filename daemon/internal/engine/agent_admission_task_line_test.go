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
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Task-line admission (plan §5.4, issue #1640): an attempt's role runs the
// agent its task's line names, the admission cites that line, and a line that
// cannot be honored refuses the attempt instead of running the lineup's
// agent.

const (
	agentTestSecondAgent   = "claude-second"
	agentTestSubmitCommand = "cmd-submit-lines"
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
	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
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
		Adapter: agentbaseline.ClaudeAdapterName, Offer: agentbaseline.ClaudeOfferName,
		Effort: domain.EffortHarnessDefault,
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
// task: the record a submit_task line's set_by names.
func (f *agentAdmissionFixture) recordSubmitCommand(t *testing.T, commandID string, runID domain.RunID) {
	t.Helper()
	ctx := context.Background()
	task := f.task(t, runID)
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutTaskSubmissionRequest(ctx, domain.TaskSubmission{
			CommandID: commandID, DeviceID: "device-1", ProjectID: task.ProjectID,
			SourceDigest: agentTestDigest("4"), TaskID: task.ID, SpecificationRunID: runID,
			Name: domain.DisplayName{Text: "Fixture task", Source: domain.DisplayNameSourceOperator},
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
// set by a recorded command that created the task, at the task's creation
// instant.
func (f *agentAdmissionFixture) submitLine(t *testing.T, role domain.RoleName, agent string) domain.TaskLine {
	t.Helper()
	f.recordSubmitCommand(t, agentTestSubmitCommand, f.run.ID)
	task := f.task(t, f.run.ID)
	return f.appendLine(t, domain.TaskLineInput{
		TaskID: task.ID, Role: role, Agent: agent,
		Source: domain.TaskLineSourceSubmitTask, SetBy: agentTestSubmitCommand,
	}, task.CreatedAt)
}

// recordCLISubmission records a `freesided submit` that created the task of
// taskRun, and returns the identity a cli_submit line's set_by names. As the
// command does, it stores the specification run derived from the
// implementation run the submission names, and not that implementation run.
func (f *agentAdmissionFixture) recordCLISubmission(t *testing.T, id string, taskRun domain.RunID) string {
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
	t.Run("the latest version wins", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		_, enrollment := f.addSecondAgent(t)
		first := f.submitLine(t, domain.RoleImplementer, agentbaseline.ClaudeAgentName)
		second := f.submitLine(t, domain.RoleImplementer, agentTestSecondAgent)
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
		line := f.submitLine(t, domain.RoleImplementer, agentbaseline.ClaudeAgentName)
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
			Source: domain.TaskLineSourceCLISubmit, SetBy: f.recordCLISubmission(t, "submission-1", f.run.ID),
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
				f.submitLine(t, domain.RoleImplementer, "ghost")
			},
			want: `the task line names agent "ghost", which the tree lacks`,
		},
		{
			name: "the line's agent runs under a disabled identity",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				identity, _ := f.addSecondAgent(t)
				identity.Enabled = false
				if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
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
			// No writer exists for this source until #1641, so nothing can
			// back the line.
			name: "a proposal_start line",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				f.recordSubmitCommand(t, agentTestSubmitCommand, f.run.ID)
				line(t, f, domain.TaskLineSourceProposalStart, agentTestSubmitCommand, f.task(t, f.run.ID).CreatedAt)
			},
			want: "no proposal start records a task line yet",
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

// TestTaskLinesLeaveLineupOnlyRolesAlone: with a line recorded for every
// eligible role of the task, the shadow reviewer and a wardless role still
// resolve from the lineup, and the startup check still proves the lineup's
// agent. None of the three has a task in view.
func TestTaskLinesLeaveLineupOnlyRolesAlone(t *testing.T) {
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	for _, role := range domain.TaskLineRoles {
		f.submitLine(t, role, agentTestSecondAgent)
	}
	tree := f.selection.Tree
	digest, err := tree.AgentDigest(agentbaseline.ClaudeAgentName)
	if err != nil {
		t.Fatal(err)
	}
	prompt := agentTestDigest("a")
	tree.Lineup = slices.Clone(tree.Lineup)
	for _, role := range []domain.RoleName{domain.RoleShadowReviewer, domain.RoleTaskNamer} {
		tree.Lineup = append(tree.Lineup, agenttree.LineupLine{
			Key: string(role),
			Selection: domain.LineupSelection{
				AgentName: agentbaseline.ClaudeAgentName, AgentDigest: digest,
				PromptName: "lineup-only", PromptDigest: prompt,
			},
		})
	}
	for _, role := range []domain.RoleName{domain.RoleShadowReviewer, domain.RoleTaskNamer} {
		agent, err := ResolveRole(ctx, f.store, tree, role)
		if err != nil {
			t.Fatalf("ResolveRole(%s) = %v", role, err)
		}
		if agent.TaskLine != nil || agent.Line.AgentName != agentbaseline.ClaudeAgentName ||
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
		agentTestPrompts[domain.RoleImplementer].Digest, domain.ModeAttendedDev, agentTestAt)
	if err != nil || binding.SelectionSource != domain.AgentSelectionSourceLineup ||
		binding.SelectionRecordID != "" || binding.EnrollmentID != f.enrollment.ID {
		t.Fatalf("CheckRole = %+v, %v; want the lineup's agent", binding, err)
	}
}

// TestLinelessAdmissionSelectsFromTheLineup: a run whose task has no lines
// admits the binding the lineup-only resolution yields, with the lineup as
// its source and no record cited.
func TestLinelessAdmissionSelectsFromTheLineup(t *testing.T) {
	f := newAgentAdmissionFixture(t)
	admission, admitted, err := f.admit(t)
	if err != nil || !admitted || admission.AgentBinding == nil {
		t.Fatalf("admitAttempt = %t, %v", admitted, err)
	}
	lineupOnly, err := f.selection.CheckRole(context.Background(), f.store, domain.RoleImplementer,
		admission.StageInputs.PromptPackageDigest, domain.ModeAttendedDev, agentTestAt)
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
