package signet

import (
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// catalogFixtureRevision is the lineup revision routingPolicyFixture records,
// so the bootstrap golden's policy and catalog name one tree.
const catalogFixtureRevision domain.Digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// catalogFixtureTree is a small tree with computed fragment digests, as a
// parsed tree carries them: one route and adapter, and lineup lines for the
// implementer and reviewer only, authored reviewer first, so the specifier
// and remediator stay unbound.
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
	tree := agenttree.Tree{
		Routes:   []agenttree.Route{{Name: "anthropic-oauth", Fragment: route}},
		Adapters: []agenttree.Adapter{{Name: "claude_code_v1", Fragment: adapter}},
	}
	line := func(role domain.RoleName, enrollment, model string, effort domain.EffortLevel) agenttree.LineupLine {
		return agenttree.LineupLine{Key: string(role), Selection: domain.LineupSelection{
			Names:      domain.AgentSelectionNames{Enrollment: enrollment, Model: model, Effort: string(effort)},
			PromptName: string(role), PromptDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		}}
	}
	// Authored reviewer first: the catalog lists the lineup in TaskLineRole
	// order, not the file's.
	tree.Lineup = []agenttree.LineupLine{
		line(domain.RoleReviewer, "anthropic-b/claude_code", "claude-preview", domain.EffortHarnessDefault),
		line(domain.RoleImplementer, "anthropic-a/claude_code", "claude-opus-5-5", domain.EffortHigh),
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

// TestAgentCatalogOf pins the projection's ordering: the lineup in
// TaskLineRole order with unbound roles absent.
func TestAgentCatalogOf(t *testing.T) {
	t.Parallel()
	got := agentCatalogFixture(t)
	revision := catalogFixtureRevision
	selection := func(enrollment domain.ClientEnrollmentID, model, effort string) domain.TaskLineSelection {
		return domain.TaskLineSelection{EnrollmentID: enrollment, ModelID: model, Effort: effort}
	}
	want := &AgentCatalog{
		LineupRevision: &revision,
		Lineup: []AgentCatalogLineupLine{
			{Role: domain.RoleImplementer, Selection: selection("anthropic-a/claude_code", "claude-opus-5-5", "high")},
			{Role: domain.RoleReviewer, Selection: selection(
				"anthropic-b/claude_code", "claude-preview", string(domain.EffortHarnessDefault))},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AgentCatalogOf =\n%+v\nwant\n%+v", got, want)
	}
}

// TestAgentCatalogOfEmptyLineup renders an empty list, never null, for a tree
// with no lineup lines.
func TestAgentCatalogOfEmptyLineup(t *testing.T) {
	t.Parallel()
	catalog, err := AgentCatalogOf(agenttree.Tree{}, catalogFixtureRevision)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Lineup == nil || len(catalog.Lineup) != 0 {
		t.Fatalf("catalog = %+v, want an empty non-nil list", catalog)
	}
}
