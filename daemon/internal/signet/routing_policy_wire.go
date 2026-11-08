package signet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The routing policy orders enrollments (#2736), but the client wire still
// carries agent names until #2807 moves it, so signet translates both ways
// here against the committed tree's catalog. #2807 deletes this file.

// RoutingPolicyWire is a routing policy revision as the client wire carries
// it, matching api/openapi.yaml: each role's order in agent names
// (renderRoutingPolicy).
type RoutingPolicyWire struct {
	Revision          int                        `json:"revision"`
	Roles             []RoutingPolicyRoleWire    `json:"roles"`
	Source            domain.RoutingPolicySource `json:"source"`
	SetBy             string                     `json:"set_by"`
	SetAt             time.Time                  `json:"set_at"`
	LineupRevision    domain.Digest              `json:"lineup_revision"`
	DrainFirstAccount *domain.UsagePoolID        `json:"drain_first_account"`
}

// RoutingPolicyRoleWire is one role's order as the client wire carries it.
type RoutingPolicyRoleWire struct {
	Role   domain.RoleName `json:"role"`
	Agents []string        `json:"agents"`
}

// renderRoutingPolicy projects a revision for the client wire. Each ordered
// enrollment renders as the catalog agent whose selection is that enrollment
// on the role's lineup model and effort, which is what the router runs, or
// as the enrollment id when no agent has it (or the daemon holds no tree);
// an id does not match the API's agent-name pattern, and clients decode it
// as a plain string. A revision stored before #2736 renders its agent names
// as stored.
func renderRoutingPolicy(policy domain.RoutingPolicy, catalog *AgentCatalog) RoutingPolicyWire {
	roles := make([]RoutingPolicyRoleWire, 0, len(policy.Roles))
	for _, role := range policy.Roles {
		if role.AgentsForm() {
			roles = append(roles, RoutingPolicyRoleWire{Role: role.Role, Agents: slices.Clone(role.Agents)})
			continue
		}
		agents := make([]string, 0, len(role.Enrollments))
		for _, enrollment := range role.Enrollments {
			agents = append(agents, catalogAgentFor(catalog, role.Role, enrollment))
		}
		roles = append(roles, RoutingPolicyRoleWire{Role: role.Role, Agents: agents})
	}
	return RoutingPolicyWire{
		Revision: policy.Revision, Roles: roles, Source: policy.Source, SetBy: policy.SetBy, SetAt: policy.SetAt,
		LineupRevision: policy.LineupRevision, DrainFirstAccount: policy.DrainFirstAccount,
	}
}

// catalogAgentFor names the catalog agent that runs enrollment on role's
// lineup model and effort, or the enrollment id when none does.
func catalogAgentFor(catalog *AgentCatalog, role domain.RoleName, enrollment domain.ClientEnrollmentID) string {
	if catalog == nil {
		return string(enrollment)
	}
	line := slices.IndexFunc(catalog.Lineup, func(l AgentCatalogLineupLine) bool { return l.Role == role })
	if line < 0 {
		return string(enrollment)
	}
	want := catalog.Lineup[line].Selection
	want.EnrollmentID = enrollment
	agent := slices.IndexFunc(catalog.Agents, func(a AgentCatalogEntry) bool { return a.Selection == want })
	if agent < 0 {
		return string(enrollment)
	}
	return catalog.Agents[agent].Name
}

// routingPolicyRolesFromWire translates wire roles, whose orders name agents,
// to enrollment orders, for domain.CanonicalRoutingPolicyRoles to check. A
// name the store holds as an enrollment id is that enrollment, which accepts
// back the ids renderRoutingPolicy falls back to; otherwise it is the
// enrollment of the catalog agent of that name. Two names on one enrollment
// keep its first position, but one name twice is refused with
// domain.ErrRoutingPolicyAgentRepeated, as before #2736. A name that is
// neither is refused with domain.ErrRoutingPolicyAgentUnknown, by the
// role's and the name's positions, never by either text. With no committed
// tree there is no catalog and no setter either, so the roles pass in the
// agents form and the command fails closed where its hook is missing.
func routingPolicyRolesFromWire(
	ctx context.Context, tx *store.ReadTx, catalog *AgentCatalog, roles []RoutingPolicyRoleWire,
) ([]domain.RoutingPolicyRole, error) {
	translated := make([]domain.RoutingPolicyRole, 0, len(roles))
	for r, role := range roles {
		if catalog == nil {
			translated = append(translated, domain.RoutingPolicyRole{Role: role.Role, Agents: role.Agents})
			continue
		}
		var enrollments []domain.ClientEnrollmentID
		for i, name := range role.Agents {
			// Positions only: the role is request text until
			// domain.CanonicalRoutingPolicyRoles finds it eligible.
			refuse := func(err error) error {
				return fmt.Errorf("routing policy role %d agent %d: %w", r+1, i+1, err)
			}
			if slices.Contains(role.Agents[:i], name) {
				return nil, refuse(domain.ErrRoutingPolicyAgentRepeated)
			}
			enrollment, err := wireNameEnrollment(ctx, tx, catalog, name)
			if err != nil {
				return nil, refuse(err)
			}
			if !slices.Contains(enrollments, enrollment) {
				enrollments = append(enrollments, enrollment)
			}
		}
		translated = append(translated, domain.RoutingPolicyRole{Role: role.Role, Enrollments: enrollments})
	}
	return translated, nil
}

func wireNameEnrollment(
	ctx context.Context, tx *store.ReadTx, catalog *AgentCatalog, name string,
) (domain.ClientEnrollmentID, error) {
	switch _, err := tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(name)); {
	case err == nil:
		return domain.ClientEnrollmentID(name), nil
	case !errors.Is(err, store.ErrNotFound):
		return "", err
	}
	agent := slices.IndexFunc(catalog.Agents, func(a AgentCatalogEntry) bool { return a.Name == name })
	if agent < 0 {
		return "", domain.ErrRoutingPolicyAgentUnknown
	}
	return catalog.Agents[agent].Selection.EnrollmentID, nil
}
