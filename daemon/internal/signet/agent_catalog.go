package signet

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// AgentCatalog is what a client can choose for a task line, matching
// api/openapi.yaml: every enrollment with the models the daemon discovered
// for it, read from the store, and the committed agent tree's lineup, read
// from the tree loaded at startup. The daemon admits a line's selection only
// on an enrollment it holds. LineupRevision is the tree's digest, nil with no committed tree; it
// changes only when the daemon restarts on a changed tree, which moves no
// sync revision, so the heartbeat carries it too. A new discovery record is
// store state and moves the sync revision.
type AgentCatalog struct {
	LineupRevision *domain.Digest           `json:"lineup_revision"`
	Enrollments    []AgentCatalogEnrollment `json:"enrollments"`
	Lineup         []AgentCatalogLineupLine `json:"lineup"`
}

// AgentCatalogEnrollment is one enrollment a line can select. Discovery is
// nil until the daemon records the enrollment's discovered catalog, so a
// client can tell "not discovered yet" from "offers nothing".
type AgentCatalogEnrollment struct {
	EnrollmentID  domain.ClientEnrollmentID `json:"enrollment_id"`
	HarnessClient domain.HarnessClientKind  `json:"harness_client"`
	Discovery     *AgentCatalogDiscovery    `json:"discovery"`
}

// AgentCatalogDiscovery is an enrollment's discovered catalog
// (domain.DiscoveredModelCatalog) less the enrollment it is keyed by.
type AgentCatalogDiscovery struct {
	HarnessBuild string                   `json:"harness_build"`
	ObservedAt   time.Time                `json:"observed_at"`
	Models       []domain.DiscoveredModel `json:"models"`
}

// AgentCatalogLineupLine is the selection the lineup binds to one task-line
// role.
type AgentCatalogLineupLine struct {
	Role      domain.RoleName          `json:"role"`
	Selection domain.TaskLineSelection `json:"selection"`
}

// AgentCatalogOf projects a parsed tree at its revision: the catalog's tree
// half, with no enrollments, which the bootstrap reads from the store
// (agentCatalogSnapshot). The lineup comes in domain.TaskLineRoles order, with no entry for a role the lineup leaves
// unbound. A parsed tree always resolves, so an error means the tree was not
// one Parse accepted.
func AgentCatalogOf(tree agenttree.Tree, revision domain.Digest) (AgentCatalog, error) {
	catalog := AgentCatalog{
		LineupRevision: &revision,
		Lineup:         make([]AgentCatalogLineupLine, 0, len(domain.TaskLineRoles)),
	}
	lineup, err := tree.ResolveLineup()
	if err != nil {
		return AgentCatalog{}, fmt.Errorf("agent catalog: %w", err)
	}
	for _, role := range domain.TaskLineRoles {
		bound, ok := lineup.Line(role)
		if !ok {
			continue
		}
		catalog.Lineup = append(catalog.Lineup, AgentCatalogLineupLine{Role: role, Selection: bound.TaskLineSelection()})
	}
	return catalog, nil
}

// agentCatalogSnapshot is the bootstrap's catalog: tree's lineup,
// or none with no committed tree (tree nil), beside every enrollment the
// store holds in enrollment id order, each with its discovered catalog when
// one is recorded. It is never nil, so a client with no tree still sees what
// it could select.
func agentCatalogSnapshot(ctx context.Context, tx *store.ReadTx, tree *AgentCatalog) (AgentCatalog, error) {
	catalog := AgentCatalog{Lineup: []AgentCatalogLineupLine{}}
	if tree != nil {
		catalog.LineupRevision, catalog.Lineup = tree.LineupRevision, tree.Lineup
	}
	identities, err := tx.ListAuthIdentities(ctx)
	if err != nil {
		return AgentCatalog{}, err
	}
	catalogs, err := tx.ListDiscoveredModelCatalogs(ctx)
	if err != nil {
		return AgentCatalog{}, err
	}
	discovered := make(map[domain.ClientEnrollmentID]domain.DiscoveredModelCatalog, len(catalogs))
	for _, record := range catalogs {
		discovered[record.EnrollmentID] = record
	}
	catalog.Enrollments = []AgentCatalogEnrollment{}
	for _, identity := range identities {
		enrollments, err := tx.ListClientEnrollments(ctx, identity.ID)
		if err != nil {
			return AgentCatalog{}, err
		}
		for _, enrollment := range enrollments {
			entry := AgentCatalogEnrollment{EnrollmentID: enrollment.ID, HarnessClient: enrollment.HarnessClient}
			if record, ok := discovered[enrollment.ID]; ok {
				entry.Discovery = &AgentCatalogDiscovery{
					HarnessBuild: record.HarnessBuild, ObservedAt: record.ObservedAt, Models: record.Models,
				}
			}
			catalog.Enrollments = append(catalog.Enrollments, entry)
		}
	}
	slices.SortFunc(catalog.Enrollments, func(a, b AgentCatalogEnrollment) int {
		return cmp.Compare(a.EnrollmentID, b.EnrollmentID)
	})
	return catalog, nil
}

// agentCatalog is the catalog's tree half, or nil when the daemon holds no
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
	return catalog.LineupRevision
}
