package domain

import (
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// RoutingPolicySource is the path that set a routing policy revision (plan
// §5.4, routing policy). Every member is an authenticated operator action.
type RoutingPolicySource string

const (
	// RoutingPolicySourceCLISet is `freesided routing set` on the daemon host.
	RoutingPolicySourceCLISet RoutingPolicySource = "cli_routing_set"
	// RoutingPolicySourceClientSet is a paired client's set_routing_policy
	// command; SetBy names the device.
	RoutingPolicySourceClientSet RoutingPolicySource = "client_routing_set"
)

// AllRoutingPolicySources lists every valid RoutingPolicySource; it is the
// single registration point.
var AllRoutingPolicySources = []RoutingPolicySource{RoutingPolicySourceCLISet, RoutingPolicySourceClientSet}

func (s RoutingPolicySource) valid() bool {
	switch s {
	case RoutingPolicySourceCLISet, RoutingPolicySourceClientSet:
		return true
	default:
		return false
	}
}

// RoutingPolicyRole is one role's agents: the router's candidates for the
// role, whose pools it ranks by the drain-first account, then the soonest
// known weekly reset, then this order (plan §5.4, revision 107). It stores
// agent names only; the router reads each agent's pool through its
// enrollment.
type RoutingPolicyRole struct {
	Role   RoleName `json:"role"`
	Agents []string `json:"agents"`
}

// CanonicalRoutingPolicyRoles validates the roles' shape and returns a copy
// in TaskLineRoles order, so one policy has one form whatever order it was
// typed in. Whether each agent resolves in the committed tree is the setter's
// question, asked against the tree it holds.
//
// As in ValidateTaskLineChoices, the error names the role by its position, and
// by name once it is known to be eligible, and never carries the text that
// failed a check: a credential pasted into either must not come back.
func CanonicalRoutingPolicyRoles(roles []RoutingPolicyRole) ([]RoutingPolicyRole, error) {
	if len(roles) == 0 {
		return nil, ErrRoutingPolicyNoRoles
	}
	byRole := make(map[RoleName]RoutingPolicyRole, len(roles))
	for i, role := range roles {
		if !role.Role.TaskLineEligible() {
			return nil, fmt.Errorf("routing policy role %d: %w", i+1, ErrTaskLineRoleIneligible)
		}
		if _, seen := byRole[role.Role]; seen {
			return nil, fmt.Errorf("routing policy role %d (%s): %w", i+1, role.Role, ErrRoutingPolicyRoleRepeated)
		}
		if len(role.Agents) == 0 {
			return nil, fmt.Errorf("routing policy role %d (%s): %w", i+1, role.Role, ErrRoutingPolicyOrderEmpty)
		}
		for j, agent := range role.Agents {
			if !validAgentName(agent) {
				return nil, fmt.Errorf("routing policy role %d (%s) agent %d: %w", i+1, role.Role, j+1, ErrInvalidAgentName)
			}
			if slices.Contains(role.Agents[:j], agent) {
				return nil, fmt.Errorf("routing policy role %d (%s) agent %d: %w",
					i+1, role.Role, j+1, ErrRoutingPolicyAgentRepeated)
			}
		}
		byRole[role.Role] = role
	}
	canonical := make([]RoutingPolicyRole, 0, len(roles))
	for _, name := range TaskLineRoles {
		if role, ok := byRole[name]; ok {
			canonical = append(canonical, RoutingPolicyRole{Role: name, Agents: slices.Clone(role.Agents)})
		}
	}
	return canonical, nil
}

func sameRoutingPolicyRoles(a, b []RoutingPolicyRole) bool {
	return slices.EqualFunc(a, b, func(x, y RoutingPolicyRole) bool {
		return x.Role == y.Role && slices.Equal(x.Agents, y.Agents)
	})
}

// RoutingPolicyInput carries the caller-supplied fields of a RoutingPolicy.
// It has no Revision: the store assigns it inside the write transaction, so no
// input path can choose it.
type RoutingPolicyInput struct {
	Roles  []RoutingPolicyRole
	Source RoutingPolicySource
	// SetBy is the operator identity that set the revision.
	SetBy string
	// LineupRevision is the agent tree's revision the roles were validated
	// against.
	LineupRevision Digest
	// DrainFirstAccount is the usage pool that takes every new task ahead of
	// reset order; nil when none.
	DrainFirstAccount *UsagePoolID
}

// RoutingPolicy is one revision of the deployment's routing policy (plan §5.4,
// revisions 99, 103, and 107): the standing rule for which usage pool a new
// task runs on, and so which agent takes it when the operator chose none for
// the role. It orders agents the committed tree already carries and never adds
// one; the orders are the router's candidates and its fallback order. A role
// absent from it keeps lineup behaviour. The router ranks the candidates'
// pools drain-first account, then soonest known weekly reset, then the
// order. A change is a new revision and a revision is never edited, so the
// revision a task line cites (TaskLineProvenance.PolicyRevision) stays the
// rule that chose its agent.
type RoutingPolicy struct {
	// Revision counts the deployment's policies from 1.
	Revision int `json:"revision"`
	// Roles holds one order per role, in TaskLineRoles order.
	Roles  []RoutingPolicyRole `json:"roles"`
	Source RoutingPolicySource `json:"source"`
	SetBy  string              `json:"set_by"`
	SetAt  time.Time           `json:"set_at"`
	// LineupRevision is the agent tree's revision (agenttree.Revision) the
	// roles were validated against. A later tree may drop an agent, so the
	// router must still resolve each one at admission.
	LineupRevision Digest `json:"lineup_revision"`
	// DrainFirstAccount is the usage pool that takes every new task ahead of
	// reset order, when the deciding role's order holds an agent on it; nil
	// when the revision names none. The setter accepted it as the effective
	// pool of an agent of the tree at LineupRevision; a later tree may move
	// that agent, so the router still resolves pools at admission.
	DrainFirstAccount *UsagePoolID `json:"drain_first_account"`
}

// NewRoutingPolicy builds a validated revision in canonical form: the roles
// are in TaskLineRoles order and detached from the input, and the timestamp is
// normalized to UTC.
func NewRoutingPolicy(in RoutingPolicyInput, revision int, setAt time.Time) (RoutingPolicy, error) {
	roles, err := CanonicalRoutingPolicyRoles(in.Roles)
	if err != nil {
		return RoutingPolicy{}, err
	}
	policy := RoutingPolicy{
		Revision: revision, Roles: roles, Source: in.Source, SetBy: in.SetBy,
		SetAt: setAt.UTC(), LineupRevision: in.LineupRevision,
		DrainFirstAccount: clonePtr(in.DrainFirstAccount),
	}
	if err := policy.Validate(); err != nil {
		return RoutingPolicy{}, err
	}
	return policy, nil
}

// SameRule reports whether the revision already holds these roles, which
// must be canonical, and this drain-first account. Who set a revision, and
// against which tree, is not part of the rule.
func (p RoutingPolicy) SameRule(roles []RoutingPolicyRole, drainFirst *UsagePoolID) bool {
	sameAccount := (p.DrainFirstAccount == nil) == (drainFirst == nil) &&
		(drainFirst == nil || *p.DrainFirstAccount == *drainFirst)
	return sameAccount && sameRoutingPolicyRoles(p.Roles, roles)
}

// Validate reports whether the revision is well-formed. The store re-runs it
// on every decode.
func (p RoutingPolicy) Validate() error {
	if p.Revision < 1 {
		return fmt.Errorf("routing policy revision %d: %w", p.Revision, ErrNonPositive)
	}
	canonical, err := CanonicalRoutingPolicyRoles(p.Roles)
	if err != nil {
		return fmt.Errorf("routing policy revision %d: %w", p.Revision, err)
	}
	if !sameRoutingPolicyRoles(canonical, p.Roles) {
		return fmt.Errorf("routing policy revision %d: %w", p.Revision, ErrRoutingPolicyNotCanonical)
	}
	if !p.Source.valid() {
		return fmt.Errorf("routing policy revision %d source %q: %w", p.Revision, p.Source, ErrInvalidRoutingPolicySource)
	}
	if p.SetBy == "" {
		return fmt.Errorf("routing policy revision %d set_by: %w", p.Revision, ErrEmptyField)
	}
	if p.SetAt.IsZero() {
		return fmt.Errorf("routing policy revision %d set_at: %w", p.Revision, ErrMissingTimestamp)
	}
	if p.SetAt.Location() != time.UTC {
		return fmt.Errorf("routing policy revision %d set_at: %w", p.Revision, ErrTimestampNotUTC)
	}
	if !contentaddr.Valid(string(p.LineupRevision)) {
		return fmt.Errorf("routing policy revision %d lineup_revision %q: %w", p.Revision, p.LineupRevision, ErrInvalidDigest)
	}
	if p.DrainFirstAccount != nil && *p.DrainFirstAccount == "" {
		return fmt.Errorf("routing policy revision %d drain_first_account: %w", p.Revision, ErrEmptyID)
	}
	return nil
}
