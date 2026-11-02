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
// already holds the requested orders, so a repeated request records nothing
// and moves no revision.
var errRoutingPolicyUnchanged = errors.New("the current routing policy already holds the requested orders")

// SetRoutingPolicyRequest is one operator change to the routing policy: the
// whole new policy, since a role left out keeps lineup behaviour.
type SetRoutingPolicyRequest struct {
	Roles  []domain.RoutingPolicyRole
	Source domain.RoutingPolicySource
	SetBy  string
}

// SetRoutingPolicyResult reports a change. Recorded is false when the current
// revision already held the orders. Policy is the current revision after the
// request.
type SetRoutingPolicyResult struct {
	Recorded bool
	Policy   domain.RoutingPolicy
}

// SetRoutingPolicy records a new routing policy revision, unless the current
// one already holds the orders. Every agent must be one the committed tree
// carries; a refusal writes nothing. The tree is checked on a repeat as well,
// so a repeat is never reported as held by a tree that dropped the agent. A
// repeat keeps the current revision's lineup_revision: the orders are the
// rule, and the tree is only what they were first validated against.
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
// a command can commit its receipt with the revision. A request for the
// orders the current revision holds returns Recorded false and a nil error,
// leaving the caller to decide whether its transaction commits.
func SetRoutingPolicyTx(
	ctx context.Context, tx *store.WriteTx, tree agenttree.Tree, lineupRevision domain.Digest,
	req SetRoutingPolicyRequest, now time.Time,
) (SetRoutingPolicyResult, error) {
	roles, err := routingPolicyRoles(tree, req)
	if err != nil {
		return SetRoutingPolicyResult{}, err
	}
	current, err := tx.CurrentRoutingPolicy(ctx)
	switch {
	case err == nil:
		if current.SameOrder(roles) {
			return SetRoutingPolicyResult{Policy: current}, nil
		}
	case !errors.Is(err, store.ErrNotFound):
		return SetRoutingPolicyResult{}, err
	}
	policy, err := tx.AppendRoutingPolicy(ctx, domain.RoutingPolicyInput{
		Roles: roles, Source: req.Source, SetBy: req.SetBy, LineupRevision: lineupRevision,
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
