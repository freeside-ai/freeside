package signet

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// catalogFixtureRevision is the lineup revision routingPolicyFixture records,
// so the bootstrap golden's policy and catalog name one tree.
const catalogFixtureRevision domain.Digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// catalogFixtureTree is a small tree with computed fragment digests, as a
// parsed tree carries them: three agents authored out of name order on one
// route and adapter, an offer with a lineage group and one without, and
// lineup lines for the implementer and reviewer only, so the specifier and
// remediator stay unbound.
func catalogFixtureTree(t *testing.T) agenttree.Tree {
	t.Helper()
	route := domain.RouteFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		ServiceOperator: "anthropic", Protocol: "anthropic_messages",
		InferenceAuthorities: []string{"api.anthropic.com"},
		BillingMode:          "subscription", FallbackPolicy: "fail_closed",
		TermsBasisDate: "2026-08-22",
	}
	route.Digest = fixtureDigest(t)(route.ComputeDigest())
	adapter := domain.AdapterFragment{
		EncodingVersion: domain.AgentFragmentEncodingVersion,
		AdapterBuild:    "claude_code_v1@build-3", HarnessBuild: "claude-code 2.1.0",
		ClientKind: domain.HarnessClientClaudeCode, Vendor: domain.AgentVendorClaude,
		LaunchCapabilities: domain.NewLaunchCapabilitySet(
			domain.LaunchCapReadTools, domain.LaunchCapInstructionDelivery,
			domain.LaunchCapStructuredOutput, domain.LaunchCapRouteStoreContract,
		),
		SendableEfforts: []domain.EffortLevel{domain.EffortHigh, domain.EffortMax},
	}
	adapter.Digest = fixtureDigest(t)(adapter.ComputeDigest())
	offer := func(model, lineage string) domain.OfferFragment {
		fragment := domain.OfferFragment{
			EncodingVersion: domain.AgentFragmentEncodingVersion,
			RouteModelID:    model, LineageGroup: lineage,
			IdentityStability: domain.IdentityPinned,
			AllowedEfforts:    []domain.EffortLevel{domain.EffortHigh, domain.EffortMax},
			PricingRevision:   "2026-08",
			NotAfter:          time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		fragment.Digest = fixtureDigest(t)(fragment.ComputeDigest())
		return fragment
	}
	agent := func(name, enrollment, offerName string, effort domain.EffortLevel) domain.AgentSource {
		return domain.AgentSource{
			Name: name, Enrollment: enrollment, Route: "anthropic-oauth",
			Adapter: "claude_code_v1", Offer: offerName, Effort: effort,
		}
	}
	tree := agenttree.Tree{
		Agents: []domain.AgentSource{
			agent("claude-c", "anthropic-b/claude_code", "preview", domain.EffortHarnessDefault),
			agent("claude-a", "anthropic-a/claude_code", "opus", domain.EffortHigh),
			agent("claude-b", "anthropic-b/claude_code", "opus", domain.EffortMax),
		},
		Routes:   []agenttree.Route{{Name: "anthropic-oauth", Fragment: route}},
		Adapters: []agenttree.Adapter{{Name: "claude_code_v1", Fragment: adapter}},
		Offers: []agenttree.Offer{
			{Route: "anthropic-oauth", Name: "opus", Fragment: offer("claude-opus-5-5", "anthropic")},
			{Route: "anthropic-oauth", Name: "preview", Fragment: offer("claude-preview", "")},
		},
	}
	line := func(role domain.RoleName, agentName string) agenttree.LineupLine {
		return agenttree.LineupLine{Key: string(role), Selection: domain.LineupSelection{
			AgentName: agentName, AgentDigest: fixtureDigest(t)(tree.AgentDigest(agentName)),
			PromptName: string(role), PromptDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		}}
	}
	// Authored reviewer first: the catalog lists the lineup in TaskLineRole
	// order, not the file's.
	tree.Lineup = []agenttree.LineupLine{
		line(domain.RoleReviewer, "claude-c"),
		line(domain.RoleImplementer, "claude-a"),
	}
	return tree
}

// agentCatalogFixture is catalogFixtureTree's catalog at the fixture revision.
func agentCatalogFixture(t *testing.T) *AgentCatalog {
	t.Helper()
	catalog, err := AgentCatalogOf(catalogFixtureTree(t), catalogFixtureRevision)
	if err != nil {
		t.Fatal(err)
	}
	return &catalog
}

// fixtureDigest fails the test on a digest the fixture could not compute.
func fixtureDigest(t *testing.T) func(domain.Digest, error) domain.Digest {
	t.Helper()
	return func(digest domain.Digest, err error) domain.Digest {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
}

// TestAgentCatalogOf pins the projection's ordering and nulls: agents by name
// whatever the tree's order, the lineup in TaskLineRole order with unbound
// roles absent, and a null lineage group for an offer that declares none.
func TestAgentCatalogOf(t *testing.T) {
	t.Parallel()
	got := agentCatalogFixture(t)
	anthropic := "anthropic"
	want := &AgentCatalog{
		LineupRevision: catalogFixtureRevision,
		Agents: []AgentCatalogEntry{
			{
				Name: "claude-a", EnrollmentID: "anthropic-a/claude_code", Model: "claude-opus-5-5",
				Effort: domain.EffortHigh, LineageGroup: &anthropic,
			},
			{
				Name: "claude-b", EnrollmentID: "anthropic-b/claude_code", Model: "claude-opus-5-5",
				Effort: domain.EffortMax, LineageGroup: &anthropic,
			},
			{
				Name: "claude-c", EnrollmentID: "anthropic-b/claude_code", Model: "claude-preview",
				Effort: domain.EffortHarnessDefault, LineageGroup: nil,
			},
		},
		Lineup: []AgentCatalogLineupLine{
			{Role: domain.RoleImplementer, Agent: "claude-a"},
			{Role: domain.RoleReviewer, Agent: "claude-c"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentCatalogOf =\n%+v\nwant\n%+v", got, want)
	}
}

// TestAgentCatalogOfEmptyLineup renders empty lists, never null, for a tree
// with no agents and no lineup lines.
func TestAgentCatalogOfEmptyLineup(t *testing.T) {
	t.Parallel()
	catalog, err := AgentCatalogOf(agenttree.Tree{}, catalogFixtureRevision)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Agents == nil || catalog.Lineup == nil || len(catalog.Agents)+len(catalog.Lineup) != 0 {
		t.Fatalf("catalog = %+v, want empty non-nil lists", catalog)
	}
}

// TestAgentCatalogOfRefusesMissingOffer fails on an agent whose offer is under
// another route: the lookup is by the agent's own route, as admission's is.
func TestAgentCatalogOfRefusesMissingOffer(t *testing.T) {
	t.Parallel()
	tree := catalogFixtureTree(t)
	tree.Offers[0].Route = "other-route"
	if _, err := AgentCatalogOf(tree, catalogFixtureRevision); err == nil ||
		!strings.Contains(err.Error(), "anthropic-oauth/opus") {
		t.Fatalf("AgentCatalogOf error = %v, want the missing offer named", err)
	}
}
