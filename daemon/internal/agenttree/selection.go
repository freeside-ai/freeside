package agenttree

import (
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The document bridge. A v2 task line selects an enrollment, a model, and an
// effort (domain.TaskLineSelection), while admission, routing policy, and the
// lineup still name tree agents. These functions translate between the two
// from the tree alone, with no store read, so the selection a v2 line holds
// is the one domain.ReduceAgentSelection gives for the agent it maps to.
// #2599 removes the bridge when admission resolves a selection without a
// tree agent.

var (
	// ErrNoAgentForSelection marks a selection no tree agent reduces to.
	ErrNoAgentForSelection = errors.New("no tree agent has the selection")
	// ErrAmbiguousAgentForSelection marks a selection more than one tree
	// agent reduces to. The choice is never left to tree order: either agent
	// could carry another credential path or prompt contract.
	ErrAmbiguousAgentForSelection = errors.New("more than one tree agent has the selection")
)

// SelectionOfAgent is the enrollment, model, and effort the named agent
// reduces to: its enrollment, its offer's route model, and the effort its
// adapter sends for its effort level.
func (t Tree) SelectionOfAgent(name string) (domain.TaskLineSelection, error) {
	source, ok := t.Agent(name)
	if !ok {
		return domain.TaskLineSelection{}, fmt.Errorf("agent %q: %w", name, ErrUnknownAgent)
	}
	return t.selectionOf(source)
}

func (t Tree) selectionOf(source domain.AgentSource) (domain.TaskLineSelection, error) {
	closure, err := t.closure(source)
	if err != nil {
		return domain.TaskLineSelection{}, err
	}
	effort, err := domain.TranslateEffort(closure.adapter.ClientKind, source.Effort)
	if err != nil {
		return domain.TaskLineSelection{}, fmt.Errorf("agent %s: %w", source.Name, err)
	}
	return domain.TaskLineSelection{
		EnrollmentID: domain.ClientEnrollmentID(source.Enrollment),
		ModelID:      closure.offer.RouteModelID,
		Effort:       effort.Effective(),
	}, nil
}

// AgentForSelection is the tree agent that runs the selection for role.
// Agents that reduce to the selection and share an agent digest
// (AgentDigest) are one definition under several names: they run the same
// enrollment, route, adapter, offer, and effort, and the baseline tree
// writes such twins whenever two roles pick one model and effort. Among
// those reducing to the selection, the role's lineup agent wins, then its
// escalated counterpart, then the first name of a sole definition. A
// selection that two definitions share and the lineup does not settle
// refuses as ambiguous, never by tree order: the definitions could differ
// in adapter, and so in launch. An agent whose fragments the tree lacks
// reduces to nothing and is skipped: admission refuses it by name anyway.
// The errors never repeat the selection, which is operator text.
func (t Tree) AgentForSelection(role domain.RoleName, selection domain.TaskLineSelection) (string, error) {
	digests := map[string]domain.Digest{}
	var found []string
	for _, source := range t.Agents {
		reduced, err := t.selectionOf(source)
		if err != nil || reduced != selection {
			continue
		}
		digest, err := t.AgentDigest(source.Name)
		if err != nil {
			continue
		}
		digests[source.Name] = digest
		found = append(found, source.Name)
	}
	if len(found) == 0 {
		return "", ErrNoAgentForSelection
	}
	if preferred, ok := t.lineupAgent(role, digests); ok {
		return preferred, nil
	}
	slices.Sort(found)
	for _, name := range found[1:] {
		if digests[name] != digests[found[0]] {
			return "", ErrAmbiguousAgentForSelection
		}
	}
	return found[0], nil
}

// lineupAgent is the role's lineup agent, or else its escalated
// counterpart, when candidates holds it. A lineup the tree cannot resolve
// prefers nothing; admission refuses on it separately.
func (t Tree) lineupAgent(role domain.RoleName, candidates map[string]domain.Digest) (string, bool) {
	lineup, err := t.ResolveLineup()
	if err != nil {
		return "", false
	}
	line, ok := lineup.Line(role)
	if !ok {
		return "", false
	}
	if _, ok := candidates[line.AgentName]; ok {
		return line.AgentName, true
	}
	if counterpart, ok := lineup.EscalatedCounterpart(role, line.AgentName); ok {
		if _, ok := candidates[counterpart.AgentName]; ok {
			return counterpart.AgentName, true
		}
	}
	return "", false
}

// LineAgent is the tree agent a task line runs: the name a v1 line holds, or
// the agent a v2 line's selection maps to for its role (AgentForSelection).
// A cleared line runs no agent and returns "". A v1 line reads nothing from
// the tree, so its name may be one the tree lacks; admission refuses that
// separately.
func (t Tree) LineAgent(line domain.TaskLine) (string, error) {
	if line.Cleared {
		return "", nil
	}
	if line.Encoding == domain.TaskLineEncodingV1 {
		return line.Agent, nil
	}
	if line.Selection == nil {
		return "", fmt.Errorf("task line %s selects nothing: %w", line.ID, domain.ErrInvalidTaskLineSelection)
	}
	return t.AgentForSelection(line.Role, *line.Selection)
}

// LineRuns reports whether the line runs the named agent: a v1 line that
// names it, or a v2 line whose agent (LineAgent) is it or shares its agent
// digest. A record or policy that names an agent holds a v2 line this way,
// since a line cannot tell apart names for one definition. The error is
// LineAgent's: a line that maps to no agent runs none.
func (t Tree) LineRuns(line domain.TaskLine, name string) (bool, error) {
	agent, err := t.LineAgent(line)
	switch {
	case err != nil:
		return false, err
	case line.Cleared:
		return false, nil
	case agent == name:
		return true, nil
	case line.Encoding == domain.TaskLineEncodingV1:
		return false, nil
	}
	want, err := t.AgentDigest(agent)
	if err != nil {
		return false, err
	}
	got, err := t.AgentDigest(name)
	return err == nil && got == want, nil
}

// LineSelection is the selection a line for role records to run the named
// agent: its SelectionOfAgent, held to map back to the agent's definition
// for that role (LineRuns). A writer that chose an agent by name writes
// through it, so it never stores a line that would run another definition;
// a selection the lineup settles for another definition, or that two
// definitions share, refuses as ambiguous.
func (t Tree) LineSelection(role domain.RoleName, name string) (domain.TaskLineSelection, error) {
	selection, err := t.SelectionOfAgent(name)
	if err != nil {
		return domain.TaskLineSelection{}, err
	}
	runs, err := t.LineRuns(domain.TaskLine{
		Encoding: domain.TaskLineEncodingV2, Role: role, Selection: &selection,
	}, name)
	if err != nil {
		return domain.TaskLineSelection{}, fmt.Errorf("agent %s: %w", name, err)
	}
	if !runs {
		return domain.TaskLineSelection{}, fmt.Errorf("agent %s: %w", name, ErrAmbiguousAgentForSelection)
	}
	return selection, nil
}
