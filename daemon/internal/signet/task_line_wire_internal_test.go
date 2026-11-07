package signet

import (
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestChoiceEncoding: a v1 choice goes out as its agent name with a null
// selection, and a v2 choice as its selection with a null agent, so no
// projection needs the tree and a tree edit never changes what a recorded
// line reads as.
func TestChoiceEncoding(t *testing.T) {
	t.Parallel()
	selection := domain.TaskLineSelection{EnrollmentID: "claude-a/claude_code", ModelID: "claude-opus-test", Effort: "high"}
	for _, tc := range []struct {
		name          string
		choice        domain.TaskLineChoice
		wantAgent     *string
		wantSelection *domain.TaskLineSelection
	}{
		{"v1 name", domain.TaskLineChoice{Role: domain.RoleImplementer, Agent: "claude-old"}, new("claude-old"), nil},
		{"v2 selection", domain.TaskLineChoice{Role: domain.RoleImplementer, Selection: &selection}, nil, &selection},
	} {
		agent, got := choiceEncoding(tc.choice)
		if !reflect.DeepEqual(agent, tc.wantAgent) || !reflect.DeepEqual(got, tc.wantSelection) {
			t.Errorf("%s: encoding = (%v, %v), want (%v, %v)", tc.name, agent, got, tc.wantAgent, tc.wantSelection)
		}
	}
}
