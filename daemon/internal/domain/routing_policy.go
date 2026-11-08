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

// RoutingPolicyRole is one role's enrollments: the router's candidates for
// the role, whose pools it ranks by the drain-first account, then the
// soonest known weekly reset, then this order (plan §5.4, revision 107). A
// router-sourced line runs the role's lineup model and effort on the chosen
// enrollment (#2593).
//
// Agents is the form a revision took before enrollments (#2736): agent names
// the router read each agent's enrollment through. A stored revision is
// never rewritten, so a role decoded from one may still hold it; a role
// holds exactly one of the two, and only Enrollments may start a new
// revision (NewRoutingPolicy).
type RoutingPolicyRole struct {
	Role        RoleName             `json:"role"`
	Enrollments []ClientEnrollmentID `json:"enrollments,omitempty"`
	Agents      []string             `json:"agents,omitempty"`
}

// AgentsForm reports whether the role holds the pre-enrollment agents form.
func (r RoutingPolicyRole) AgentsForm() bool { return len(r.Agents) > 0 }

// CanonicalRoutingPolicyRoles validates the roles' shape and returns a copy
// in TaskLineRoles order, so one policy has one form whatever order it was
// typed in. Whether each enrollment exists, and serves the role's lineup
// model and effort, is the setter's question, asked against the store and
// the tree it holds.
//
// As in ValidateTaskLineChoices, the error names the role by its position, and
// by name once it is known to be eligible, and an entry by its position, and
// never carries the text that failed a check: a credential pasted into either
// must not come back.
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
		if err := validateRoutingPolicyOrder(role); err != nil {
			return nil, fmt.Errorf("routing policy role %d (%s): %w", i+1, role.Role, err)
		}
		byRole[role.Role] = role
	}
	canonical := make([]RoutingPolicyRole, 0, len(roles))
	for _, name := range TaskLineRoles {
		if role, ok := byRole[name]; ok {
			canonical = append(canonical, RoutingPolicyRole{
				Role: name, Enrollments: slices.Clone(role.Enrollments), Agents: slices.Clone(role.Agents),
			})
		}
	}
	return canonical, nil
}

// validateRoutingPolicyOrder checks one role's order: exactly one form, no
// entry malformed or repeated. An entry is named by its position only.
func validateRoutingPolicyOrder(role RoutingPolicyRole) error {
	switch {
	case len(role.Enrollments) == 0 && len(role.Agents) == 0:
		return ErrRoutingPolicyOrderEmpty
	case len(role.Enrollments) > 0 && len(role.Agents) > 0:
		return ErrRoutingPolicyFormMixed
	}
	for j, enrollment := range role.Enrollments {
		if enrollment == "" || !ValidLaunchOptionValue(string(enrollment)) {
			return fmt.Errorf("enrollment %d: %w", j+1, ErrInvalidRoutingPolicyEnrollment)
		}
		if slices.Contains(role.Enrollments[:j], enrollment) {
			return fmt.Errorf("enrollment %d: %w", j+1, ErrRoutingPolicyEnrollmentRepeated)
		}
	}
	for j, agent := range role.Agents {
		if !validAgentName(agent) {
			return fmt.Errorf("agent %d: %w", j+1, ErrInvalidAgentName)
		}
		if slices.Contains(role.Agents[:j], agent) {
			return fmt.Errorf("agent %d: %w", j+1, ErrRoutingPolicyAgentRepeated)
		}
	}
	return nil
}

func sameRoutingPolicyRoles(a, b []RoutingPolicyRole) bool {
	return slices.EqualFunc(a, b, func(x, y RoutingPolicyRole) bool {
		return x.Role == y.Role && slices.Equal(x.Enrollments, y.Enrollments) && slices.Equal(x.Agents, y.Agents)
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
// task runs on, and so which enrollment takes it when the operator chose none
// for the role. It orders enrollments the store holds, each running the role's
// lineup model and effort; the orders are the router's candidates and its
// fallback order. A role absent from it keeps lineup behaviour. The router ranks the candidates'
// pools drain-first account, then soonest known weekly reset, then the
// order. A change is a new revision and a revision is never edited, so the
// revision a task line cites (TaskLineProvenance.PolicyRevision) stays the
// rule that chose its enrollment.
type RoutingPolicy struct {
	// Revision counts the deployment's policies from 1.
	Revision int `json:"revision"`
	// Roles holds one order per role, in TaskLineRoles order.
	Roles  []RoutingPolicyRole `json:"roles"`
	Source RoutingPolicySource `json:"source"`
	SetBy  string              `json:"set_by"`
	SetAt  time.Time           `json:"set_at"`
	// LineupRevision is the agent tree's revision (agenttree.Revision) the
	// roles were validated against: its lineup gave each role's model and
	// effort. A later tree may change them, so the router reads the lineup
	// it holds at admission.
	LineupRevision Digest `json:"lineup_revision"`
	// DrainFirstAccount is the usage pool that takes every new task ahead of
	// reset order, when the deciding role's order holds an enrollment on it;
	// nil when the revision names none. The setter accepted it as the
	// effective pool of an ordered enrollment's identity; an identity's pool
	// can move, so the router still resolves pools at admission.
	DrainFirstAccount *UsagePoolID `json:"drain_first_account"`
}

// NewRoutingPolicy builds a validated revision in canonical form: the roles
// are in TaskLineRoles order and detached from the input, and the timestamp is
// normalized to UTC. A new revision orders enrollments only; the agents form
// is one a stored revision may hold, never one a change may write.
func NewRoutingPolicy(in RoutingPolicyInput, revision int, setAt time.Time) (RoutingPolicy, error) {
	roles, err := CanonicalRoutingPolicyRoles(in.Roles)
	if err != nil {
		return RoutingPolicy{}, err
	}
	if i := slices.IndexFunc(roles, RoutingPolicyRole.AgentsForm); i >= 0 {
		return RoutingPolicy{}, fmt.Errorf("routing policy role %s: %w", roles[i].Role, ErrRoutingPolicyAgentsForm)
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
// on every decode, so it accepts a role in either form.
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
