package signet

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// wireTaskLineV1 projects a v1 fixture, which names its agent and needs no
// tree.
func wireTaskLineV1(line domain.TaskLine) TaskLine {
	return wireTaskLine(nil, line)
}

// oneLineAgent is a TaskLineAgents whose only agent runs one selection.
type oneLineAgent struct {
	name      string
	selection domain.TaskLineSelection
}

func (o oneLineAgent) SelectionOfAgent(string) (domain.TaskLineSelection, error) {
	return o.selection, nil
}

func (o oneLineAgent) AgentForSelection(_ domain.RoleName, selection domain.TaskLineSelection) (string, error) {
	if selection != o.selection {
		return "", errors.New("no agent runs the selection")
	}
	return o.name, nil
}

func (o oneLineAgent) AgentDigest(string) (domain.Digest, error) { return "definition", nil }

// TestChoiceAgent: a v2 choice goes out as its agent while the tree runs it,
// and as its enrollment/model/effort otherwise, so a tree edit or a missing
// tree never fails a projection. A v1 choice keeps its own name.
func TestChoiceAgent(t *testing.T) {
	t.Parallel()
	runs := domain.TaskLineSelection{EnrollmentID: "claude-a/claude_code", ModelID: "claude-opus-test", Effort: "high"}
	moved := runs
	moved.Effort = "max"
	tree := oneLineAgent{name: "claude-implementer", selection: runs}
	for _, tc := range []struct {
		name   string
		agents TaskLineAgents
		choice domain.TaskLineChoice
		want   string
	}{
		{"v1 name", nil, domain.TaskLineChoice{Role: domain.RoleImplementer, Agent: "claude-old"}, "claude-old"},
		{"selection the tree runs", tree, domain.TaskLineChoice{Role: domain.RoleImplementer, Selection: &runs}, "claude-implementer"},
		{
			name: "selection the tree no longer runs", agents: tree,
			choice: domain.TaskLineChoice{Role: domain.RoleImplementer, Selection: &moved},
			want:   "claude-a/claude_code/claude-opus-test/max",
		},
		{
			name: "no tree bound", choice: domain.TaskLineChoice{Role: domain.RoleImplementer, Selection: &runs},
			want: "claude-a/claude_code/claude-opus-test/high",
		},
	} {
		if got := choiceAgent(tc.agents, tc.choice); got != tc.want {
			t.Errorf("%s: agent = %q, want %q", tc.name, got, tc.want)
		}
	}
}
