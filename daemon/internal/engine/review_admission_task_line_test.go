package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Reviewer-line selection (plan §5.4, issue #1981): a review for a task whose
// reviewer line names another agent on the composed harness admits that
// agent and cites the line, and a line that cannot be honored refuses the
// review by the line's id instead of running the lineup's reviewer.

// agentTestShadowPrompt is the shadow reviewer's prompt in the review tree.
var agentTestShadowPrompt = agentbaseline.Prompt{Name: "shadow", Digest: agentTestDigest("1")}

// reviewTree gives the fixture a tree with a Claude reviewer and a shadow
// reviewer on the fixture's identity, and the second agent addSecondAgent
// seeds. It returns the routed and the shadow reviewer's ReviewRole.
func (f *agentAdmissionFixture) reviewTree(t *testing.T) (routed, shadow ReviewRole) {
	t.Helper()
	f.addSecondAgent(t)
	second, ok := f.selection.Tree.Agent(agentTestSecondAgent)
	if !ok {
		t.Fatal("the fixture tree lacks the second agent")
	}
	prompts := make(map[domain.RoleName]agentbaseline.Prompt)
	for role, prompt := range agentTestPrompts {
		prompts[role] = prompt
	}
	prompts[domain.RoleReviewer] = agentbaseline.Prompt{Name: "review", Digest: agentTestDigest("6")}
	prompts[domain.RoleShadowReviewer] = agentTestShadowPrompt
	tree, err := agentbaseline.Tree(agentbaseline.TreeInput{
		ClaudeEnrollment: f.enrollment, ClaudeReviewEnrollment: &f.enrollment,
		Writers: agentTestWriters(), ReviewModel: "claude-review-test", ReviewEffort: domain.EffortHigh,
		ShadowReviewModel: "claude-shadow-test", ShadowReviewEffort: domain.EffortHigh,
		TermsBasisDate: "2026-08-01", PricingRevision: "pricing-2026-08",
		OfferNotAfter: agentTestAt.Add(24 * time.Hour), Prompts: prompts,
	})
	if err != nil {
		t.Fatal(err)
	}
	tree.Agents = append(slices.Clone(tree.Agents), second)
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
	routed = ReviewRole{
		Role: domain.RoleReviewer, PromptName: "review", PromptDigest: agentTestDigest("6"),
		HarnessClient: domain.HarnessClientClaudeCode, Vendor: domain.AgentVendorClaude,
		EffectiveEgress: agentTestEgress,
	}
	shadow = routed
	shadow.Role, shadow.PromptName, shadow.PromptDigest = domain.RoleShadowReviewer,
		agentTestShadowPrompt.Name, agentTestShadowPrompt.Digest
	return routed, shadow
}

func (f *agentAdmissionFixture) admitReview(t *testing.T, spec ReviewRole) (domain.ReviewAdmission, error) {
	t.Helper()
	f.selection.Launch = agentbaseline.RoleLaunch
	return f.selection.AdmitReview(context.Background(), f.store, spec, f.run.ID, "review-1", agentTestAt)
}

// TestReviewerLineSelectsTheReviewAgent is the acceptance fixture: the review
// runs the agent the reviewer line names, under its identity, model, and
// effort, and the snapshot cites the line.
func TestReviewerLineSelectsTheReviewAgent(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	routed, _ := f.reviewTree(t)
	line := f.submitLine(t, domain.RoleReviewer, agentTestSecondAgent)

	admission, err := f.admitReview(t, routed)
	if err != nil {
		t.Fatal(err)
	}
	checked, err := CheckReviewRole(context.Background(), f.store, f.selection.Tree, agentbaseline.RoleLaunch,
		routed, f.run.ID, agentTestAt)
	if err != nil {
		t.Fatal(err)
	}
	if checked.TaskLine == nil || checked.TaskLine.ID != line.ID || checked.Line.AgentName != agentTestSecondAgent ||
		checked.Line.PromptName != routed.PromptName {
		t.Fatalf("checked review agent = %+v, want the line's agent on the lineup's prompt", checked.RoleAgent)
	}
	binding := admission.AgentBinding
	switch {
	case binding.SelectionSource != domain.AgentSelectionSourceTaskLine || binding.SelectionRecordID != line.ID:
		t.Errorf("selection = %s %s, want task_line %s", binding.SelectionSource, binding.SelectionRecordID, line.ID)
	case admission.AuthIdentityID != "claude-second":
		t.Errorf("identity = %s, want claude-second", admission.AuthIdentityID)
	case binding.EnrollmentID != checked.Enrollment.ID || binding.EnrollmentGeneration != checked.Generation.Ordinal:
		t.Errorf("enrollment = %s@%d, want the second agent's", binding.EnrollmentID, binding.EnrollmentGeneration)
	case binding.AgentDigest != checked.Resolved.Definition.Digest:
		t.Errorf("agent digest = %s, want %s", binding.AgentDigest, checked.Resolved.Definition.Digest)
	case binding.RouteModelID != "claude-implementer-test":
		t.Errorf("model = %s, want the second agent's offer model", binding.RouteModelID)
	case binding.NativeEffort == "":
		t.Error("the snapshot passes no effort")
	}
	if admission.ModelConfiguration() != "claude-implementer-test/"+binding.NativeEffort {
		t.Errorf("model configuration = %s", admission.ModelConfiguration())
	}
}

// TestLinelessReviewAdmitsTheLineupReviewer: with no reviewer line the
// snapshot is the lineup's, and a reviewer line never reaches the shadow
// reviewer.
func TestLinelessReviewAdmitsTheLineupReviewer(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	routed, shadow := f.reviewTree(t)

	lineless, err := f.admitReview(t, routed)
	if err != nil {
		t.Fatal(err)
	}
	reviewer, _ := f.selection.Tree.Agent(agentbaseline.ClaudeReviewAgentName)
	if lineless.AgentBinding.SelectionSource != domain.AgentSelectionSourceLineup ||
		lineless.AgentBinding.SelectionRecordID != "" || lineless.AuthIdentityID != f.identity.ID ||
		lineless.AgentBinding.EnrollmentID != domain.ClientEnrollmentID(reviewer.Enrollment) ||
		lineless.AgentBinding.RouteModelID != "claude-review-test" {
		t.Fatalf("lineless review admission = %+v, want the lineup reviewer", lineless)
	}

	f.submitLine(t, domain.RoleReviewer, agentTestSecondAgent)
	shadowAdmission, err := f.admitReview(t, shadow)
	if err != nil {
		t.Fatal(err)
	}
	if shadowAdmission.AgentBinding.SelectionSource != domain.AgentSelectionSourceLineup ||
		shadowAdmission.AuthIdentityID != f.identity.ID ||
		shadowAdmission.AgentBinding.RouteModelID != "claude-shadow-test" {
		t.Fatalf("shadow admission under a reviewer line = %+v, want the shadow lineup agent", shadowAdmission)
	}
}

// TestReviewerLineRefusals: a line that cannot be honored refuses the review,
// never runs the lineup's reviewer, and names the line by its id, never by
// the agent text it holds.
func TestReviewerLineRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// A name that passes the grammar and reads like a pasted credential.
	const pasted = "sk-ant-oat01-fixture-not-a-token"
	cases := []struct {
		name  string
		setup func(t *testing.T, f *agentAdmissionFixture) domain.TaskLine
		want  string
	}{
		{
			name: "the line names an agent the tree lacks",
			setup: func(t *testing.T, f *agentAdmissionFixture) domain.TaskLine {
				return f.submitLine(t, domain.RoleReviewer, pasted)
			},
			want: "names an agent the tree lacks",
		},
		{
			name: "the line's agent runs under a disabled identity",
			setup: func(t *testing.T, f *agentAdmissionFixture) domain.TaskLine {
				var identity domain.AuthIdentity
				if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
					var err error
					identity, err = tx.GetAuthIdentity(ctx, "claude-second")
					return err
				}); err != nil {
					t.Fatal(err)
				}
				identity.Enabled = false
				if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
					return tx.RecordAuthIdentity(ctx, identity, agentTestAt)
				}); err != nil {
					t.Fatal(err)
				}
				return f.submitLine(t, domain.RoleReviewer, agentTestSecondAgent)
			},
			want: "is disabled",
		},
		{
			name: "the line does not hold against the command that set it",
			setup: func(t *testing.T, f *agentAdmissionFixture) domain.TaskLine {
				f.recordSubmitCommand(t, agentTestSubmitCommand, f.run.ID)
				task := f.task(t, f.run.ID)
				return f.appendLine(t, domain.TaskLineInput{
					TaskID: task.ID, Role: domain.RoleReviewer, Agent: agentTestSecondAgent,
					Source: domain.TaskLineSourceSubmitTask, SetBy: agentTestSubmitCommand,
				}, task.CreatedAt.Add(time.Second))
			},
			want: "its task was created at",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newAgentAdmissionFixture(t)
			routed, _ := f.reviewTree(t)
			line := tc.setup(t, f)
			admission, err := f.admitReview(t, routed)
			if !errors.Is(err, ErrAgentNotAdmissible) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("AdmitReview = %+v, %v; want a refusal containing %q", admission, err, tc.want)
			}
			if !strings.Contains(err.Error(), string(line.ID)) {
				t.Errorf("refusal %q does not name line %s", err, line.ID)
			}
			if strings.Contains(err.Error(), pasted) {
				t.Errorf("refusal %q repeats the line's agent text", err)
			}
		})
	}
}
