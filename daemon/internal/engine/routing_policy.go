package engine

import (
	"context"
	"errors"
	"fmt"
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
// one already holds the orders and drain-first account. Every agent must be
// one the committed tree carries, and the account the usage pool of at least
// one of them; a refusal writes nothing. The tree is checked on a repeat as
// well, so a repeat is never reported as held by a tree that dropped the
// agent or moved off the pool. A repeat keeps the current revision's
// lineup_revision: the orders and account are the rule, and the tree is only
// what they were first validated against.
func SetRoutingPolicy(
	ctx context.Context, st *store.Store, tree agenttree.Tree, lineupRevision domain.Digest,
	req SetRoutingPolicyRequest, now time.Time,
) (SetRoutingPolicyResult, error) {
	// A refusal opens no transaction.
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
	if req.DrainFirstAccount != nil {
		if err := checkDrainFirstAccount(ctx, tx, tree, *req.DrainFirstAccount); err != nil {
			return SetRoutingPolicyResult{}, err
		}
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
// a malformed shape or an agent the tree doesn't carry.
func routingPolicyRoles(tree agenttree.Tree, req SetRoutingPolicyRequest) ([]domain.RoutingPolicyRole, error) {
	roles, err := domain.CanonicalRoutingPolicyRoles(req.Roles)
	if err != nil {
		return nil, err
	}
	for _, role := range roles {
		for _, agent := range role.Agents {
			if _, ok := tree.Agent(agent); !ok {
				return nil, fmt.Errorf("routing policy role %s agent %s: %w",
					role.Role, agent, domain.ErrRoutingPolicyAgentUnknown)
			}
		}
	}
	return roles, nil
}

// checkDrainFirstAccount refuses an account that is not the effective usage
// pool of any agent the tree carries, read through each agent's enrollment
// and that enrollment's identity as they stand in this transaction. An agent
// whose enrollment or identity the store lacks can't run, so it is skipped
// rather than refused: the orders check is tree membership only. Whether the
// account holds an agent for a given role is the router's skip rule (plan
// §5.4, tier 1), not the setter's. The error never repeats the account, which
// is request text.
func checkDrainFirstAccount(
	ctx context.Context, tx *store.WriteTx, tree agenttree.Tree, account domain.UsagePoolID,
) error {
	for _, agent := range tree.Agents {
		enrollment, err := tx.GetClientEnrollment(ctx, domain.ClientEnrollmentID(agent.Enrollment))
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		identity, err := tx.GetAuthIdentity(ctx, enrollment.AuthIdentityID)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		if identity.EffectiveUsagePool() == account {
			return nil
		}
	}
	return fmt.Errorf("routing policy drain_first_account: %w", domain.ErrRoutingPolicyDrainFirstUnknown)
}
