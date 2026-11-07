package engine

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// v2 task lines (issue #2597): a line selects an enrollment, a model, and an
// effort, and admission runs the one tree agent that selection maps to
// (agenttree.Tree.LineAgent) through the path a v1 line's name takes.

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

// setTree rewrites the fixture's tree, as an operator committing a new one
// would, and has admission read it.
func (f *agentAdmissionFixture) setTree(t *testing.T, change func(tree *agenttree.Tree)) {
	t.Helper()
	tree := f.selection.Tree
	tree.Agents = slices.Clone(tree.Agents)
	change(&tree)
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
}

// addTwinOf adds an agent named twin that is agent under another name: one
// definition, as the baseline tree writes when two roles pick one model and
// effort. When repriced, the twin's offer differs in its term, so the twin
// is another definition with the same selection.
func (f *agentAdmissionFixture) addTwinOf(t *testing.T, agent, twin string, repriced bool) {
	t.Helper()
	f.setTree(t, func(tree *agenttree.Tree) {
		source, ok := tree.Agent(agent)
		if !ok {
			t.Fatalf("no agent %s", agent)
		}
		source.Name = twin
		if repriced {
			i := slices.IndexFunc(tree.Offers, func(o agenttree.Offer) bool {
				return o.Route == source.Route && o.Name == source.Offer
			})
			offer := tree.Offers[i]
			offer.Name = twin
			offer.Fragment.NotAfter = offer.Fragment.NotAfter.Add(24 * time.Hour)
			var err error
			if offer.Fragment.Digest, err = offer.Fragment.ComputeDigest(); err != nil {
				t.Fatal(err)
			}
			tree.Offers = append(slices.Clone(tree.Offers), offer)
			source.Offer = offer.Name
		}
		tree.Agents = append(tree.Agents, source)
	})
}

// TestV2TaskLineAdmitsTheAgentItSelects: a v2 line binds the agent its
// selection maps to, exactly as a v1 line naming that agent does, citing the
// v2 line.
func TestV2TaskLineAdmitsTheAgentItSelects(t *testing.T) {
	t.Parallel()
	bind := func(t *testing.T, v2 bool) (domain.AdmissionAgentBinding, domain.TaskLine) {
		t.Helper()
		f := newAgentAdmissionFixture(t)
		_, enrollment := f.addSecondAgent(t)
		var line domain.TaskLine
		if v2 {
			line = f.submitLineV2(t, domain.RoleImplementer, agentTestSecondAgent)
		} else {
			line = f.submitLine(t, domain.RoleImplementer, agentTestSecondAgent)
		}
		admission, admitted, err := f.admit(t)
		if err != nil || !admitted || admission.AgentBinding == nil {
			t.Fatalf("admitAttempt = %t, %v", admitted, err)
		}
		wantCites(t, admission, line, enrollment.ID)
		f.record(t, admission)
		return *admission.AgentBinding, line
	}
	v1Binding, v1Line := bind(t, false)
	v2Binding, v2Line := bind(t, true)
	if v1Line.Encoding != domain.TaskLineEncodingV1 || v2Line.Encoding != domain.TaskLineEncodingV2 || v1Line.ID == v2Line.ID {
		t.Fatalf("lines = %s %s and %s %s", v1Line.Encoding, v1Line.ID, v2Line.Encoding, v2Line.ID)
	}
	v1Binding.SelectionRecordID = v2Line.ID
	if !reflect.DeepEqual(v1Binding, v2Binding) {
		t.Fatalf("v2 binding = %+v, want the v1 binding %+v but for the line it cites", v2Binding, v1Binding)
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
					order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
				f.appendLine(t, domain.TaskLineInput{
					TaskID: f.task(t, f.run.ID).ID, Role: domain.RoleImplementer, Selection: foreign,
					Source: domain.TaskLineSourceRouter, SetBy: routerSetBy(f.task(t, f.run.ID).ID),
					Provenance: policyOrder(policy.Revision, routerSecondPool),
				}, agentTestAt.Add(-time.Minute))
			},
			// The router's anchor reads the routing policy's agent names
			// until #2736.
			want: agenttree.ErrNoAgentForSelection.Error(),
		},
		{
			name: "a router line selects an agent its policy does not order",
			setup: func(t *testing.T, f *agentAdmissionFixture) {
				f.addSecondAgent(t)
				policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
					order(domain.RoleImplementer, agentTestWriterAgent))
				f.appendLine(t, domain.TaskLineInput{
					TaskID: f.task(t, f.run.ID).ID, Role: domain.RoleImplementer,
					Selection: f.lineSelection(t, agentTestSecondAgent),
					Source:    domain.TaskLineSourceRouter, SetBy: routerSetBy(f.task(t, f.run.ID).ID),
					Provenance: policyOrder(policy.Revision, routerSecondPool),
				}, agentTestAt.Add(-time.Minute))
			},
			want: "does not order the line's agent for its role",
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

// TestV2TaskLineTwinsAreOneAgent: names that share a definition are one
// agent to a v2 line. An operator's line selecting it admits, and so does a
// router line the policy chose by the other name. Admission reads no agent,
// so two definitions that run one selection admit too.
func TestV2TaskLineTwinsAreOneAgent(t *testing.T) {
	t.Parallel()
	t.Run("operator line", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		_, enrollment := f.addSecondAgent(t)
		f.addTwinOf(t, agentTestSecondAgent, "claude-second-twin", false)
		line := f.submitLineV2(t, domain.RoleImplementer, "claude-second-twin")
		admission, admitted, err := f.admit(t)
		if err != nil || !admitted {
			t.Fatalf("admitAttempt = %t, %v", admitted, err)
		}
		wantCites(t, admission, line, enrollment.ID)
	})
	t.Run("two definitions of one selection", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		_, enrollment := f.addSecondAgent(t)
		f.addTwinOf(t, agentTestSecondAgent, "claude-second-twin", true)
		line := f.submitLineSelection(t, domain.RoleImplementer, f.lineSelection(t, agentTestSecondAgent))
		admission, admitted, err := f.admit(t)
		if err != nil || !admitted {
			t.Fatalf("admitAttempt = %t, %v", admitted, err)
		}
		wantCites(t, admission, line, enrollment.ID)
	})
	t.Run("router line", func(t *testing.T) {
		f := newAgentAdmissionFixture(t)
		_, enrollment := f.addSecondAgent(t)
		f.addTwinOf(t, agentTestSecondAgent, "claude-second-twin", false)
		f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour), order(domain.RoleImplementer, "claude-second-twin"))
		admission, err := f.admitRun(t, f.run, agentTestAt)
		if err != nil {
			t.Fatal(err)
		}
		lines := f.taskLines(t, f.run.ID)
		if len(lines) != 1 || lines[0].Source != domain.TaskLineSourceRouter ||
			*lines[0].Selection != *f.lineSelection(t, "claude-second-twin") {
			t.Fatalf("router lines = %+v", lines)
		}
		wantCites(t, admission, lines[0], enrollment.ID)
	})
}

// TestMovedV2LineRefusesAnotherAgent: a moved line's selection must still map
// to the agent its record moved the role to. A tree that has since renamed
// that agent maps it to another name, and the anchor refuses.
func TestMovedV2LineRefusesAnotherAgent(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Minute),
		order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
	line := f.moveTask(t, policy, nil, false)
	if line.Encoding != domain.TaskLineEncodingV2 || f.lineAgent(line) != agentTestSecondAgent {
		t.Fatalf("moved line = %+v", line)
	}
	f.setTree(t, func(tree *agenttree.Tree) {
		for i := range tree.Agents {
			if tree.Agents[i].Name == agentTestSecondAgent {
				tree.Agents[i].Name = "claude-renamed"
			}
		}
	})
	admission, err := f.admitRun(t, f.run, agentTestAt.Add(time.Minute))
	if err == nil || !strings.Contains(err.Error(), "its record moved the role to another agent") {
		t.Fatalf("admission of a move the tree renamed = %+v, %v; want its anchor refused", admission.AgentBinding, err)
	}
}
