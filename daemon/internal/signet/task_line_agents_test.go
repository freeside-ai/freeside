package signet_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// fakeLineAgents is a TaskLineAgents over a fixed name-to-selection map. It
// has no lineup and gives every name its own definition, so two names
// sharing a selection make it ambiguous, as two tree agents of different
// definitions do.
type fakeLineAgents map[string]domain.TaskLineSelection

func (f fakeLineAgents) AgentDigest(name string) (domain.Digest, error) {
	if _, ok := f[name]; !ok {
		return "", agenttree.ErrUnknownAgent
	}
	return domain.Digest("definition-" + name), nil
}

func (f fakeLineAgents) SelectionOfAgent(name string) (domain.TaskLineSelection, error) {
	selection, ok := f[name]
	if !ok {
		return domain.TaskLineSelection{}, agenttree.ErrUnknownAgent
	}
	return selection, nil
}

func (f fakeLineAgents) AgentForSelection(_ domain.RoleName, selection domain.TaskLineSelection) (string, error) {
	var found []string
	for name, candidate := range f {
		if candidate == selection {
			found = append(found, name)
		}
	}
	switch len(found) {
	case 0:
		return "", agenttree.ErrNoAgentForSelection
	case 1:
		return found[0], nil
	default:
		return "", agenttree.ErrAmbiguousAgentForSelection
	}
}

// lineSelectionOf is the selection testLineAgents maps name to.
func lineSelectionOf(name string) domain.TaskLineSelection {
	return domain.TaskLineSelection{
		EnrollmentID: domain.ClientEnrollmentID("enrollment-" + name), ModelID: "model-" + name, Effort: "high",
	}
}

// testLineAgents names every agent the signet tests choose for a line.
var testLineAgents = func() fakeLineAgents {
	agents := fakeLineAgents{}
	for _, name := range []string{
		"codex-primary", "claude-primary", "claude-implementer", "claude-reviewer", "claude-specifier",
		"codex-implementer", "codex-reviewer", "claude-max-b", "codex-pro", "claude", "claude-b", "codex", "other",
	} {
		agents[name] = lineSelectionOf(name)
	}
	return agents
}()

// withTestLineAgents binds testLineAgents as the service's bridge.
func withTestLineAgents() signet.Option {
	return signet.WithTaskLineAgents(func() (signet.TaskLineAgents, error) { return testLineAgents, nil })
}

// selectionChoice is the recorded choice signet writes for a wire choice of
// role and name under testLineAgents.
func selectionChoice(role domain.RoleName, name string) domain.TaskLineChoice {
	selection := lineSelectionOf(name)
	return domain.TaskLineChoice{Role: role, Selection: &selection}
}

// wireLine is a v1 line as the wire carries it: the agent it names.
func wireLine(line domain.TaskLine) signet.TaskLine {
	return signet.TaskLine{
		ID: line.ID, TaskID: line.TaskID, Role: line.Role, Agent: line.Agent, Version: line.Version,
		PredecessorID: line.PredecessorID, Source: line.Source, SetBy: line.SetBy, SetAt: line.SetAt,
		Provenance: line.Provenance,
	}
}

// TestChangeTaskLineTranslatesAgentNames: a line change names an agent on the
// wire, records the selection the tree maps it to, and answers with the name
// again. A name the tree cannot select both ways is refused without repeating
// it, and with no tree bound the change fails closed.
func TestChangeTaskLineTranslatesAgentNames(t *testing.T) {
	svc, st, id := newOperatorService(t, signet.WithTaskLineChanger(engineTaskLineChanger), withTestLineAgents())
	result, err := svc.Submit(t.Context(), lineCommand("line-a", id, new("codex-primary")))
	if err != nil {
		t.Fatal(err)
	}
	line := result.Receipt.TaskLineChange.Line
	if line == nil || line.Encoding != domain.TaskLineEncodingV2 || line.Selection == nil ||
		*line.Selection != lineSelectionOf("codex-primary") {
		t.Fatalf("recorded line = %+v", line)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"agent":"codex-primary"`) || strings.Contains(string(body), "enrollment-") {
		t.Fatalf("receipt on the wire = %s, want the agent's name and no selection", body)
	}

	ambiguous := fakeLineAgents{"twin-a": lineSelectionOf("twin"), "twin-b": lineSelectionOf("twin")}
	refusals := []struct {
		name  string
		svc   *signet.Service
		agent string
		want  error
	}{
		{"an agent the tree lacks", svc, "nobody-known", signet.ErrTaskLineAgentUnknown},
		{"an agent sharing its selection", withService(t, st, ambiguous), "twin-a", signet.ErrTaskLineAgentUnknown},
		{"no tree bound", withService(t, st, nil), "codex-primary", signet.ErrOperatorSettingUnavailable},
	}
	for i, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			before := serverRevision(t, st)
			_, err := tc.svc.Submit(t.Context(), lineCommand("refused-"+strconv.Itoa(i), id, new(tc.agent)))
			if !errors.Is(err, tc.want) {
				t.Fatalf("change naming %s = %v, want %v", tc.agent, err, tc.want)
			}
			if strings.Contains(err.Error(), tc.agent) {
				t.Errorf("refusal %q repeats the client's name", err)
			}
			if serverRevision(t, st) != before {
				t.Fatal("a refused line change committed")
			}
		})
	}
}

// withService is a line-changing service over st whose bridge is agents, or
// unbound when agents is nil.
func withService(t *testing.T, st *store.Store, agents fakeLineAgents) *signet.Service {
	t.Helper()
	opts := []signet.Option{
		signet.WithClock(func() time.Time { return operatorAt }), signet.WithTaskLineChanger(engineTaskLineChanger),
	}
	if agents != nil {
		opts = append(opts, signet.WithTaskLineAgents(func() (signet.TaskLineAgents, error) { return agents, nil }))
	}
	return signet.NewService(st, opts...)
}
