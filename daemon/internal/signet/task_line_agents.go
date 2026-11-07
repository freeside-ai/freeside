package signet

import (
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TaskLineAgents is the document bridge between the agent names the client
// wire carries and the selections task lines hold. The daemon's committed
// agent tree (agenttree.Tree) is one. Until #2598 moves the API to
// selections, a client names an agent, signet translates the name to the
// selection a line records, and a line goes back to the client as the agent
// its selection maps to for its role. #2599 removes the bridge.
type TaskLineAgents interface {
	SelectionOfAgent(name string) (domain.TaskLineSelection, error)
	AgentForSelection(role domain.RoleName, selection domain.TaskLineSelection) (string, error)
	AgentDigest(name string) (domain.Digest, error)
}

// ErrTaskLineAgentUnknown refuses a task line choice naming an agent the
// committed tree cannot select for the role: one it lacks, or one whose
// selection maps to another definition, so a line holding it would run
// something other than the agent the client chose.
var ErrTaskLineAgentUnknown = errors.New("task line names an agent the tree cannot select")

// WithTaskLineAgents binds the bridge. agents returns the tree in effect when
// it is called, or ErrOperatorSettingUnavailable while the daemon has none;
// a command or projection that needs it then fails closed.
func WithTaskLineAgents(agents func() (TaskLineAgents, error)) Option {
	return func(s *Service) { s.taskLineAgents = agents }
}

func (s *Service) lineAgents() (TaskLineAgents, error) {
	if s.taskLineAgents == nil {
		return nil, ErrOperatorSettingUnavailable
	}
	return s.taskLineAgents()
}

// TaskLineAgents is the bridge in effect now, nil while the daemon has none.
// A read that projects lines with no Service (ReadTaskTimeline) takes it from
// here.
func (s *Service) TaskLineAgents() TaskLineAgents {
	agents, err := s.lineAgents()
	if err != nil {
		return nil
	}
	return agents
}

// lineSelection is the selection a line for role records for the wire's
// agent name. The selection must map back to that agent's definition, so the
// line runs the agent the client chose, though it may show under another
// name for the same definition. The error never repeats the name, which is
// client text.
func (s *Service) lineSelection(role domain.RoleName, name string) (domain.TaskLineSelection, error) {
	agents, err := s.lineAgents()
	if err != nil {
		return domain.TaskLineSelection{}, err
	}
	// The tree's own error quotes the name, so it is dropped here.
	selection, err := agents.SelectionOfAgent(name)
	if err != nil {
		return domain.TaskLineSelection{}, ErrTaskLineAgentUnknown
	}
	if !sameDefinition(agents, role, selection, name) {
		return domain.TaskLineSelection{}, fmt.Errorf("%w: another agent has its selection", ErrTaskLineAgentUnknown)
	}
	return selection, nil
}

// sameDefinition reports whether a line for role holding selection runs the
// named agent's definition.
func sameDefinition(agents TaskLineAgents, role domain.RoleName, selection domain.TaskLineSelection, name string) bool {
	back, err := agents.AgentForSelection(role, selection)
	if err != nil {
		return false
	}
	if back == name {
		return true
	}
	want, err := agents.AgentDigest(name)
	if err != nil {
		return false
	}
	got, err := agents.AgentDigest(back)
	return err == nil && got == want
}

// lineSelections translates canonical name choices to the selection choices
// a record and its lines hold, in the same order.
func (s *Service) lineSelections(choices []domain.TaskLineChoice) ([]domain.TaskLineChoice, error) {
	if len(choices) == 0 {
		return choices, nil
	}
	selected := make([]domain.TaskLineChoice, 0, len(choices))
	for _, choice := range choices {
		selection, err := s.lineSelection(choice.Role, choice.Agent)
		if err != nil {
			return nil, fmt.Errorf("task line %s: %w", choice.Role, err)
		}
		selected = append(selected, domain.TaskLineChoice{Role: choice.Role, Selection: &selection})
	}
	return selected, nil
}

// choiceAgent is the agent name the wire carries for a recorded choice: the
// name a v1 choice holds, or the agent a selection maps to for its role
// through agents. A selection no one agent runs, such as one a later tree
// moved to another model or effort, or any selection while no tree is bound
// (agents nil), goes out as enrollment/model/effort, the form
// `freesided submit --task-line` takes. An agent name holds no slash, so
// that text never names an agent; a projection never fails on it, since a
// routine tree edit would otherwise hide every task that ran the old
// selection.
func choiceAgent(agents TaskLineAgents, choice domain.TaskLineChoice) string {
	if choice.Selection == nil {
		return choice.Agent
	}
	if agents != nil {
		if name, err := agents.AgentForSelection(choice.Role, *choice.Selection); err == nil {
			return name
		}
	}
	return choice.Selection.AgentText()
}

// TaskLine is the API's TaskLine: one version of a task's line as a client
// reads it, naming the agent it runs (choiceAgent). A cleared version
// never reaches the wire. ID is the stored line's own id, so a v2 line's is
// the address of its selection, not of this name.
type TaskLine struct {
	ID            domain.Digest              `json:"id"`
	TaskID        domain.TaskID              `json:"task_id"`
	Role          domain.RoleName            `json:"role"`
	Agent         string                     `json:"agent"`
	Version       int                        `json:"version"`
	PredecessorID *domain.Digest             `json:"predecessor_id"`
	Source        domain.TaskLineSource      `json:"source"`
	SetBy         string                     `json:"set_by"`
	SetAt         time.Time                  `json:"set_at"`
	Provenance    *domain.TaskLineProvenance `json:"provenance"`
}

// TaskLineChoice is the API's TaskLineChoice: a role and the agent a
// recorded choice names or maps to (choiceAgent).
type TaskLineChoice struct {
	Role  domain.RoleName `json:"role"`
	Agent string          `json:"agent"`
}

// wireTaskLine projects an uncleared line with its agent (choiceAgent).
func wireTaskLine(agents TaskLineAgents, line domain.TaskLine) TaskLine {
	return TaskLine{
		ID: line.ID, TaskID: line.TaskID, Role: line.Role, Agent: choiceAgent(agents, line.Choice()),
		Version: line.Version, PredecessorID: line.PredecessorID, Source: line.Source, SetBy: line.SetBy,
		SetAt: line.SetAt, Provenance: line.Provenance,
	}
}

// wireTaskLines projects lines that are never cleared, such as a move's.
func wireTaskLines(agents TaskLineAgents, lines []domain.TaskLine) []TaskLine {
	if lines == nil {
		return nil
	}
	wire := make([]TaskLine, 0, len(lines))
	for _, line := range lines {
		wire = append(wire, wireTaskLine(agents, line))
	}
	return wire
}

// wireTaskLineChoices projects a record's choices with their agents.
func wireTaskLineChoices(agents TaskLineAgents, choices []domain.TaskLineChoice) []TaskLineChoice {
	if choices == nil {
		return nil
	}
	wire := make([]TaskLineChoice, 0, len(choices))
	for _, choice := range choices {
		wire = append(wire, TaskLineChoice{Role: choice.Role, Agent: choiceAgent(agents, choice)})
	}
	return wire
}

// selectionAgentNames maps each uncleared v2 line to its agent
// (choiceAgent), for a projection that shows agents beside v1 lines' own
// names.
func selectionAgentNames(agents TaskLineAgents, lines []domain.TaskLine) map[domain.Digest]string {
	names := map[domain.Digest]string{}
	for _, line := range lines {
		if line.Cleared || line.Encoding == domain.TaskLineEncodingV1 {
			continue
		}
		names[line.ID] = choiceAgent(agents, line.Choice())
	}
	return names
}
