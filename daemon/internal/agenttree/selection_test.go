package agenttree_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The bridge's selection is the one admission resolves for the agent, so a
// v2 line selects exactly what its agent would run.
func TestSelectionOfAgentMatchesResolution(t *testing.T) {
	tree := fixtureTree(t)
	enrollment, identity := fixtureRecords()
	for _, source := range tree.Agents {
		resolved := must(tree.ResolveAgent(source.Name, enrollment, identity))
		got := must(tree.SelectionOfAgent(source.Name))
		want := domain.TaskLineSelection{
			EnrollmentID: resolved.Selection.EnrollmentID,
			ModelID:      resolved.Selection.ModelID,
			Effort:       resolved.Selection.Effort,
		}
		if got != want {
			t.Fatalf("agent %s: bridge selection %+v, admission resolves %+v", source.Name, got, want)
		}
	}
	if _, err := tree.SelectionOfAgent("ghost"); !errors.Is(err, agenttree.ErrUnknownAgent) {
		t.Fatalf("unknown agent = %v", err)
	}
}

func TestAgentForSelection(t *testing.T) {
	tree := fixtureTree(t)
	selection := must(tree.SelectionOfAgent(agentName))
	for _, role := range []domain.RoleName{domain.RoleReviewer, domain.RoleImplementer} {
		if got := must(tree.AgentForSelection(role, selection)); got != agentName {
			t.Fatalf("%s agent = %q, want %q", role, got, agentName)
		}
	}
	other := selection
	other.Effort = "high"
	if _, err := tree.AgentForSelection(domain.RoleReviewer, other); !errors.Is(err, agenttree.ErrNoAgentForSelection) {
		t.Fatalf("another effort = %v", err)
	}
}

// addTwin adds an agent named name that is the fixture agent under another
// name: one definition, so one agent digest.
func addTwin(tree *agenttree.Tree, name string) {
	twin := tree.Agents[0]
	twin.Name = name
	tree.Agents = append(tree.Agents, twin)
}

// addRepriced adds an agent named name with the fixture agent's selection
// on an offer of another pricing revision: another definition.
func addRepriced(t *testing.T, tree *agenttree.Tree, name string) {
	t.Helper()
	offer := tree.Offers[0]
	offer.Name = name + "-offer"
	offer.Fragment.PricingRevision = "2026-09"
	offer.Fragment.Digest = must(offer.Fragment.ComputeDigest())
	tree.Offers = append(tree.Offers, offer)
	agent := tree.Agents[0]
	agent.Name, agent.Offer = name, offer.Name
	tree.Agents = append(tree.Agents, agent)
}

// addCallAgent adds an agent named name with the fixture agent's selection
// on a call adapter, which gives the harness no tools: the wardless launch
// shape.
func addCallAgent(t *testing.T, tree *agenttree.Tree, name string) {
	t.Helper()
	adapter := tree.Adapters[0]
	adapter.Name = "codex_call_v1"
	adapter.Fragment.AdapterBuild = "codex_call_v1@build-7"
	adapter.Fragment.LaunchCapabilities = domain.NewLaunchCapabilitySet(
		domain.LaunchCapInstructionDelivery, domain.LaunchCapStructuredOutput,
	)
	adapter.Fragment.Digest = must(adapter.Fragment.ComputeDigest())
	tree.Adapters = append(tree.Adapters, adapter)
	agent := tree.Agents[0]
	agent.Name, agent.Adapter = name, adapter.Name
	tree.Agents = append(tree.Agents, agent)
}

// One definition under two names is one agent, and its first name stands
// for it for every role.
func TestAgentForSelectionTwins(t *testing.T) {
	tree := fixtureTree(t)
	selection := must(tree.SelectionOfAgent(agentName))
	addTwin(&tree, "a-sol-twin")
	for _, role := range []domain.RoleName{domain.RoleReviewer, domain.RoleImplementer} {
		if got := must(tree.AgentForSelection(role, selection)); got != "a-sol-twin" {
			t.Fatalf("%s agent = %q, want the first name", role, got)
		}
	}
}

// Two definitions with one selection and one launch shape refuse, never by
// tree order.
func TestAgentForSelectionSplitDefinitions(t *testing.T) {
	tree := fixtureTree(t)
	selection := must(tree.SelectionOfAgent(agentName))
	addRepriced(t, &tree, "a-sol-repriced")
	for _, role := range []domain.RoleName{domain.RoleReviewer, domain.RoleImplementer} {
		if _, err := tree.AgentForSelection(role, selection); !errors.Is(err, agenttree.ErrAmbiguousAgentForSelection) {
			t.Fatalf("%s agent of two definitions = %v", role, err)
		}
	}
	// An agent the tree cannot close reduces to nothing, so it is skipped.
	tree.Agents[len(tree.Agents)-1].Offer = "missing"
	if got := must(tree.AgentForSelection(domain.RoleImplementer, selection)); got != agentName {
		t.Fatalf("agent past a broken twin = %q", got)
	}
}

// A ward agent and a call agent on one selection never compete: a ward role
// runs the agent whose adapter gives read tools, a wardless role the one
// whose adapter gives none, and an invalid role neither.
func TestAgentForSelectionFollowsLaunchShape(t *testing.T) {
	tree := fixtureTree(t)
	selection := must(tree.SelectionOfAgent(agentName))
	addCallAgent(t, &tree, "a-sol-call")
	for _, tc := range []struct {
		role domain.RoleName
		want string
	}{
		{domain.RoleReviewer, agentName},
		{domain.RoleImplementer, agentName},
		{domain.RoleTaskNamer, "a-sol-call"},
		{domain.RoleFindingAdjudicator, "a-sol-call"},
	} {
		if got := must(tree.AgentForSelection(tc.role, selection)); got != tc.want {
			t.Errorf("%s agent = %q, want %q", tc.role, got, tc.want)
		}
	}
	if _, err := tree.AgentForSelection("critic", selection); !errors.Is(err, agenttree.ErrNoAgentForSelection) {
		t.Fatalf("agent for an invalid role = %v", err)
	}
}

func TestLineAgent(t *testing.T) {
	tree := fixtureTree(t)
	selection := must(tree.SelectionOfAgent(agentName))
	v1 := domain.TaskLine{Encoding: domain.TaskLineEncodingV1, Agent: "not-in-tree"}
	if got := must(tree.LineAgent(v1)); got != "not-in-tree" {
		t.Fatalf("v1 line agent = %q", got)
	}
	v2 := domain.TaskLine{Encoding: domain.TaskLineEncodingV2, Role: domain.RoleReviewer, Selection: &selection}
	if got := must(tree.LineAgent(v2)); got != agentName {
		t.Fatalf("v2 line agent = %q", got)
	}
	cleared := domain.TaskLine{Encoding: domain.TaskLineEncodingV2, Cleared: true}
	if got := must(tree.LineAgent(cleared)); got != "" {
		t.Fatalf("cleared line agent = %q", got)
	}
	if _, err := (agenttree.Tree{}).LineAgent(v2); !errors.Is(err, agenttree.ErrNoAgentForSelection) {
		t.Fatalf("v2 line against an empty tree = %v", err)
	}
}

// A line runs a name when it names it (v1) or its agent shares the name's
// definition (v2), so a record or policy naming a twin holds the line.
func TestLineRuns(t *testing.T) {
	tree := fixtureTree(t)
	selection := must(tree.SelectionOfAgent(agentName))
	addTwin(&tree, "z-sol-twin")
	addCallAgent(t, &tree, "z-sol-call")
	v1 := domain.TaskLine{Encoding: domain.TaskLineEncodingV1, Role: domain.RoleImplementer, Agent: agentName}
	v2 := domain.TaskLine{Encoding: domain.TaskLineEncodingV2, Role: domain.RoleReviewer, Selection: &selection}
	cleared := domain.TaskLine{Encoding: domain.TaskLineEncodingV2, Role: domain.RoleReviewer, Cleared: true}
	for _, tc := range []struct {
		line domain.TaskLine
		name string
		want bool
	}{
		{v1, agentName, true},
		{v1, "z-sol-twin", false},
		{v2, agentName, true},
		{v2, "z-sol-twin", true},
		{v2, "z-sol-call", false},
		{v2, "ghost", false},
		{cleared, agentName, false},
	} {
		if got := must(tree.LineRuns(tc.line, tc.name)); got != tc.want {
			t.Errorf("%s line runs %s = %t, want %t", tc.line.Encoding, tc.name, got, tc.want)
		}
	}
	addRepriced(t, &tree, "z-sol-repriced")
	if _, err := tree.LineRuns(v2, agentName); !errors.Is(err, agenttree.ErrAmbiguousAgentForSelection) {
		t.Fatalf("a line two definitions share = %v", err)
	}
}

// A writer that chose an agent by name records a selection only when a line
// holding it runs that agent.
func TestLineSelection(t *testing.T) {
	tree := fixtureTree(t)
	selection := must(tree.SelectionOfAgent(agentName))
	addTwin(&tree, "a-sol-twin")
	if got := must(tree.LineSelection(domain.RoleImplementer, agentName)); got != selection {
		t.Fatalf("selection of a twin = %+v, want %+v", got, selection)
	}
	addCallAgent(t, &tree, "z-sol-call")
	if got := must(tree.LineSelection(domain.RoleTaskNamer, "z-sol-call")); got != selection {
		t.Fatalf("selection of the call agent for a wardless role = %+v", got)
	}
	if _, err := tree.LineSelection(domain.RoleReviewer, "z-sol-call"); !errors.Is(err, agenttree.ErrAmbiguousAgentForSelection) {
		t.Fatalf("selection of the call agent for a ward role = %v, want ambiguous", err)
	}
	addRepriced(t, &tree, "z-sol-repriced")
	if _, err := tree.LineSelection(domain.RoleReviewer, agentName); !errors.Is(err, agenttree.ErrAmbiguousAgentForSelection) {
		t.Fatalf("selection two definitions share = %v, want ambiguous", err)
	}
	if _, err := tree.LineSelection(domain.RoleReviewer, "ghost"); !errors.Is(err, agenttree.ErrUnknownAgent) {
		t.Fatalf("unknown agent = %v", err)
	}
}
