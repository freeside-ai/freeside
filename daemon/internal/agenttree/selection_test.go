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

// One definition under two names is one agent: the role's lineup names it
// when the lineup holds one of the names, and the first name stands for it
// otherwise.
func TestAgentForSelectionTwins(t *testing.T) {
	tree := fixtureTree(t)
	selection := must(tree.SelectionOfAgent(agentName))
	addTwin(&tree, "a-sol-twin")
	if got := must(tree.AgentForSelection(domain.RoleReviewer, selection)); got != agentName {
		t.Fatalf("reviewer agent = %q, want the lineup's %q", got, agentName)
	}
	if got := must(tree.AgentForSelection(domain.RoleImplementer, selection)); got != "a-sol-twin" {
		t.Fatalf("implementer agent = %q, want the first name", got)
	}
}

// Two definitions with one selection: the role's lineup settles it, and
// with no lineup agent among them it refuses, never by tree order.
func TestAgentForSelectionSplitDefinitions(t *testing.T) {
	tree := fixtureTree(t)
	selection := must(tree.SelectionOfAgent(agentName))
	addRepriced(t, &tree, "a-sol-repriced")
	if got := must(tree.AgentForSelection(domain.RoleReviewer, selection)); got != agentName {
		t.Fatalf("reviewer agent = %q, want the lineup's %q", got, agentName)
	}
	if _, err := tree.AgentForSelection(domain.RoleImplementer, selection); !errors.Is(err, agenttree.ErrAmbiguousAgentForSelection) {
		t.Fatalf("implementer agent of two definitions = %v", err)
	}
	// An agent the tree cannot close reduces to nothing, so it is skipped.
	tree.Agents[len(tree.Agents)-1].Offer = "missing"
	if got := must(tree.AgentForSelection(domain.RoleImplementer, selection)); got != agentName {
		t.Fatalf("agent past a broken twin = %q", got)
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
	addTwin(&tree, "a-sol-twin")
	addRepriced(t, &tree, "z-sol-repriced")
	v1 := domain.TaskLine{Encoding: domain.TaskLineEncodingV1, Role: domain.RoleImplementer, Agent: agentName}
	v2 := domain.TaskLine{Encoding: domain.TaskLineEncodingV2, Role: domain.RoleReviewer, Selection: &selection}
	cleared := domain.TaskLine{Encoding: domain.TaskLineEncodingV2, Role: domain.RoleReviewer, Cleared: true}
	for _, tc := range []struct {
		line domain.TaskLine
		name string
		want bool
	}{
		{v1, agentName, true},
		{v1, "a-sol-twin", false},
		{v2, agentName, true},
		{v2, "a-sol-twin", true},
		{v2, "z-sol-repriced", false},
		{v2, "ghost", false},
		{cleared, agentName, false},
	} {
		if got := must(tree.LineRuns(tc.line, tc.name)); got != tc.want {
			t.Errorf("%s line runs %s = %t, want %t", tc.line.Encoding, tc.name, got, tc.want)
		}
	}
	implementer := v2
	implementer.Role = domain.RoleImplementer
	if _, err := tree.LineRuns(implementer, agentName); !errors.Is(err, agenttree.ErrAmbiguousAgentForSelection) {
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
	addRepriced(t, &tree, "z-sol-repriced")
	if got := must(tree.LineSelection(domain.RoleReviewer, agentName)); got != selection {
		t.Fatalf("selection the lineup settles = %+v", got)
	}
	for _, tc := range []struct {
		role domain.RoleName
		name string
	}{
		{domain.RoleReviewer, "z-sol-repriced"},
		{domain.RoleImplementer, agentName},
	} {
		if _, err := tree.LineSelection(tc.role, tc.name); !errors.Is(err, agenttree.ErrAmbiguousAgentForSelection) {
			t.Errorf("%s selection of %s = %v, want ambiguous", tc.role, tc.name, err)
		}
	}
	if _, err := tree.LineSelection(domain.RoleReviewer, "ghost"); !errors.Is(err, agenttree.ErrUnknownAgent) {
		t.Fatalf("unknown agent = %v", err)
	}
}
