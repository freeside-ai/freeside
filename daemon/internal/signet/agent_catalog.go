package signet

import (
	"fmt"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// AgentCatalog is the committed agent tree's agents and lineup, matching
// api/openapi.yaml. The daemon admits only agents the tree carries, so a
// client offers only these. LineupRevision is the tree's digest; it changes
// only when the daemon restarts on a changed tree, which moves no sync
// revision, so the heartbeat carries it too.
type AgentCatalog struct {
	LineupRevision domain.Digest            `json:"lineup_revision"`
	Agents         []AgentCatalogEntry      `json:"agents"`
	Lineup         []AgentCatalogLineupLine `json:"lineup"`
}

// AgentCatalogEntry is one tree agent with its route offer's facts. Model and
// LineageGroup carry InvocationAdmissionFacts' names because they are the
// same facts; Effort is the source's setting, not an admission's request.
type AgentCatalogEntry struct {
	Name         string                    `json:"name"`
	EnrollmentID domain.ClientEnrollmentID `json:"enrollment_id"`
	Model        string                    `json:"model"`
	Effort       domain.EffortLevel        `json:"effort"`
	// LineageGroup is nil when the offer declares none.
	LineageGroup *string `json:"lineage_group"`
}

// AgentCatalogLineupLine is the agent the lineup binds to one task-line role.
type AgentCatalogLineupLine struct {
	Role  domain.RoleName `json:"role"`
	Agent string          `json:"agent"`
}

// AgentCatalogOf projects a parsed tree at its revision. Agents come in name
// order and the lineup in domain.TaskLineRoles order, with no entry for a
// role the lineup leaves unbound. A parsed tree always resolves, so an error
// means the tree was not one Parse accepted.
func AgentCatalogOf(tree agenttree.Tree, revision domain.Digest) (AgentCatalog, error) {
	catalog := AgentCatalog{
		LineupRevision: revision,
		Agents:         make([]AgentCatalogEntry, 0, len(tree.Agents)),
		Lineup:         make([]AgentCatalogLineupLine, 0, len(domain.TaskLineRoles)),
	}
	for _, agent := range tree.Agents {
		// The offer is looked up under the agent's own route, as the tree's
		// own closure does: an offer of the same name under another route is
		// another offer.
		offer := slices.IndexFunc(tree.Offers, func(o agenttree.Offer) bool {
			return o.Route == agent.Route && o.Name == agent.Offer
		})
		if offer < 0 {
			return AgentCatalog{}, fmt.Errorf("agent catalog: agent %s names offer %s/%s, which the tree lacks",
				agent.Name, agent.Route, agent.Offer)
		}
		fragment := tree.Offers[offer].Fragment
		entry := AgentCatalogEntry{
			Name:         agent.Name,
			EnrollmentID: domain.ClientEnrollmentID(agent.Enrollment),
			Model:        fragment.RouteModelID,
			Effort:       agent.Effort,
		}
		if fragment.LineageGroup != "" {
			group := fragment.LineageGroup
			entry.LineageGroup = &group
		}
		catalog.Agents = append(catalog.Agents, entry)
	}
	slices.SortFunc(catalog.Agents, func(a, b AgentCatalogEntry) int { return strings.Compare(a.Name, b.Name) })

	lineup, err := tree.ResolveLineup()
	if err != nil {
		return AgentCatalog{}, fmt.Errorf("agent catalog: %w", err)
	}
	for _, role := range domain.TaskLineRoles {
		if selection, bound := lineup.Line(role); bound {
			catalog.Lineup = append(catalog.Lineup, AgentCatalogLineupLine{Role: role, Agent: selection.AgentName})
		}
	}
	return catalog, nil
}

// agentCatalog is the composed catalog, or nil when the daemon holds no
// committed tree.
func (s *Service) agentCatalog() *AgentCatalog {
	if s.agentCatalogFunc == nil {
		return nil
	}
	return s.agentCatalogFunc()
}

// lineupRevision is the heartbeat's view of the catalog: its revision, or nil
// with no committed tree.
func lineupRevision(catalog *AgentCatalog) *domain.Digest {
	if catalog == nil {
		return nil
	}
	revision := catalog.LineupRevision
	return &revision
}
