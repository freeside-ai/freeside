package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// errRoutingPolicyUnchanged rolls the write back when the current revision
// already holds the requested rule, so a repeated request records nothing
// and moves no revision.
var errRoutingPolicyUnchanged = errors.New("the current routing policy already holds the requested rule")

// SetRoutingPolicyRequest is one operator change to the routing policy: the
// whole new policy, since a role left out keeps lineup behaviour and a nil
// DrainFirstAccount names none.
type SetRoutingPolicyRequest struct {
	Roles             []domain.RoutingPolicyRole
	DrainFirstAccount *domain.UsagePoolID
	Source            domain.RoutingPolicySource
	SetBy             string
}

// SetRoutingPolicyResult reports a change. Recorded is false when the current
// revision already held the rule. Policy is the current revision after the
// request.
type SetRoutingPolicyResult struct {
	Recorded bool
	Policy   domain.RoutingPolicy
}

// SetRoutingPolicy records a new routing policy revision, unless the current
// one already holds the orders and drain-first account. Every ordered role
// must be one the tree's lineup binds, every enrollment one the store holds
// on an enabled identity whose recorded catalog, when it has one, serves the
// role's lineup model and effort, and the account the usage pool of at least
// one ordered enrollment's identity; a refusal writes nothing. The store and
// tree are checked on a repeat as well, so a repeat is never reported as held
// by an enrollment that no longer qualifies. A repeat keeps the current
// revision's lineup_revision: the orders and account are the rule, and the
// tree is only what they were first validated against.
func SetRoutingPolicy(
	ctx context.Context, st *store.Store, tree agenttree.Tree, lineupRevision domain.Digest,
	req SetRoutingPolicyRequest, now time.Time,
) (SetRoutingPolicyResult, error) {
	// A malformed request opens no transaction.
	if _, err := routingPolicyRoles(tree, req); err != nil {
		return SetRoutingPolicyResult{}, err
	}
	var result SetRoutingPolicyResult
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		result, err = SetRoutingPolicyTx(ctx, tx, tree, lineupRevision, req, now)
		if err == nil && !result.Recorded {
			return errRoutingPolicyUnchanged
		}
		return err
	})
	if errors.Is(err, errRoutingPolicyUnchanged) {
		return result, nil
	}
	if err != nil {
		return SetRoutingPolicyResult{}, err
	}
	return result, nil
}

// SetRoutingPolicyTx is SetRoutingPolicy inside the caller's transaction, so
// a command can commit its receipt with the revision. A request for the rule
// the current revision holds returns Recorded false and a nil error, leaving
// the caller to decide whether its transaction commits.
func SetRoutingPolicyTx(
	ctx context.Context, tx *store.WriteTx, tree agenttree.Tree, lineupRevision domain.Digest,
	req SetRoutingPolicyRequest, now time.Time,
) (SetRoutingPolicyResult, error) {
	roles, err := routingPolicyRoles(tree, req)
	if err != nil {
		return SetRoutingPolicyResult{}, err
	}
	pools, err := checkRoutingPolicyEnrollments(ctx, &tx.ReadTx, tree, roles)
	if err != nil {
		return SetRoutingPolicyResult{}, err
	}
	if req.DrainFirstAccount != nil && !slices.Contains(pools, *req.DrainFirstAccount) {
		// The error never repeats the account, which is request text.
		return SetRoutingPolicyResult{}, fmt.Errorf("routing policy drain_first_account: %w",
			domain.ErrRoutingPolicyDrainFirstUnknown)
	}
	current, err := tx.CurrentRoutingPolicy(ctx)
	switch {
	case err == nil:
		if current.SameRule(roles, req.DrainFirstAccount) {
			return SetRoutingPolicyResult{Policy: current}, nil
		}
	case !errors.Is(err, store.ErrNotFound):
		return SetRoutingPolicyResult{}, err
	}
	policy, err := tx.AppendRoutingPolicy(ctx, domain.RoutingPolicyInput{
		Roles: roles, Source: req.Source, SetBy: req.SetBy, LineupRevision: lineupRevision,
		DrainFirstAccount: req.DrainFirstAccount,
	}, now)
	if err != nil {
		return SetRoutingPolicyResult{}, err
	}
	return SetRoutingPolicyResult{Recorded: true, Policy: policy}, nil
}

// routingPolicyRoles returns the request's roles in canonical form, refusing
// a malformed shape, an order in the agents form (domain.NewRoutingPolicy),
// or a role the tree's lineup leaves unbound, which has no model and effort
// to route on.
func routingPolicyRoles(tree agenttree.Tree, req SetRoutingPolicyRequest) ([]domain.RoutingPolicyRole, error) {
	roles, err := domain.CanonicalRoutingPolicyRoles(req.Roles)
	if err != nil {
		return nil, err
	}
	for _, role := range roles {
		if role.AgentsForm() {
			return nil, fmt.Errorf("routing policy role %s: %w", role.Role, domain.ErrRoutingPolicyAgentsForm)
		}
		if _, err := lineupLine(tree, role.Role); err != nil {
			return nil, fmt.Errorf("routing policy role %s: %w", role.Role, domain.ErrRoutingPolicyRoleUnbound)
		}
	}
	return roles, nil
}

// checkRoutingPolicyEnrollments refuses an ordered enrollment the store
// lacks, one whose identity is disabled, and one that cannot run the role's
// lineup model and effort (domain.SelectionServed), as the store holds them
// in this transaction. An enrollment with no recorded catalog passes on a
// native effort: admission resolves it under the bridge
// (domain.ResolveAgentSelection). It returns the ordered enrollments'
// effective usage pools, which a drain-first account must be one of. A
// refusal names the role and the entry's position, never the enrollment,
// which is request text.
func checkRoutingPolicyEnrollments(
	ctx context.Context, tx *store.ReadTx, tree agenttree.Tree, roles []domain.RoutingPolicyRole,
) ([]domain.UsagePoolID, error) {
	var pools []domain.UsagePoolID
	for _, role := range roles {
		line, err := lineupLine(tree, role.Role)
		if err != nil {
			return nil, fmt.Errorf("routing policy role %s: %w", role.Role, domain.ErrRoutingPolicyRoleUnbound)
		}
		for i, id := range role.Enrollments {
			refuse := func(sentinel error) error {
				return fmt.Errorf("routing policy role %s enrollment %d: %w", role.Role, i+1, sentinel)
			}
			enrollment, err := tx.GetClientEnrollment(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return nil, refuse(domain.ErrRoutingPolicyEnrollmentUnknown)
			}
			if err != nil {
				return nil, err
			}
			identity, err := tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID)
			if errors.Is(err, store.ErrNotFound) {
				return nil, refuse(domain.ErrRoutingPolicyEnrollmentUnknown)
			}
			if err != nil {
				return nil, err
			}
			if !identity.Enabled {
				return nil, refuse(domain.ErrRoutingPolicyEnrollmentDisabled)
			}
			catalog, err := readCatalog(ctx, tx, enrollment.ID)
			if err != nil {
				return nil, err
			}
			if domain.SelectionServed(enrollment.HarnessClient, catalog, line.Names.Model, line.Names.Effort) != nil {
				return nil, refuse(domain.ErrRoutingPolicyEnrollmentUnserved)
			}
			if pool := identity.EffectiveUsagePool(); !slices.Contains(pools, pool) {
				pools = append(pools, pool)
			}
		}
	}
	return pools, nil
}
