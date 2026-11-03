package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var (
	routingPolicyTree = agenttree.Tree{Agents: []domain.AgentSource{
		{Name: "claude-a"}, {Name: "claude-b"}, {Name: "codex"},
	}}
	routingPolicyTreeRevision = domain.Digest("sha256:" + strings.Repeat("a", 64))
	routingPolicyAt           = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
)

func routingPolicyRequest(agents ...string) SetRoutingPolicyRequest {
	return SetRoutingPolicyRequest{
		Roles:  []domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Agents: agents}},
		Source: domain.RoutingPolicySourceCLISet, SetBy: "cli:operator",
	}
}

func setRoutingPolicy(t *testing.T, st *store.Store, revision domain.Digest, req SetRoutingPolicyRequest) SetRoutingPolicyResult {
	t.Helper()
	result, err := SetRoutingPolicy(context.Background(), st, routingPolicyTree, revision, req, routingPolicyAt)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func routingPolicyState(t *testing.T, st *store.Store) ([]domain.RoutingPolicy, int64) {
	t.Helper()
	ctx := context.Background()
	var (
		revisions []domain.RoutingPolicy
		server    int64
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if revisions, err = tx.RoutingPolicyRevisions(ctx); err != nil {
			return err
		}
		state, err := tx.ServerState(ctx)
		server = state.Revision
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return revisions, server
}

func TestSetRoutingPolicyRefusesAnAgentTheTreeLacks(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "routing.db"), store.Options{})
	_, err := SetRoutingPolicy(context.Background(), st, routingPolicyTree, routingPolicyTreeRevision,
		routingPolicyRequest("claude-a", "not-in-tree"), routingPolicyAt)
	if !errors.Is(err, domain.ErrRoutingPolicyAgentUnknown) {
		t.Fatalf("set = %v, want %v", err, domain.ErrRoutingPolicyAgentUnknown)
	}
	if !strings.Contains(err.Error(), string(domain.RoleImplementer)) || !strings.Contains(err.Error(), "not-in-tree") {
		t.Fatalf("refusal names neither the role nor the agent: %v", err)
	}
	if revisions, _ := routingPolicyState(t, st); len(revisions) != 0 {
		t.Fatalf("revisions after a refusal = %+v", revisions)
	}
}

func TestSetRoutingPolicyRefusesAShapeBeforeTheStore(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "routing.db"), store.Options{})
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := SetRoutingPolicy(context.Background(), st, routingPolicyTree, routingPolicyTreeRevision,
		routingPolicyRequest("claude-a", "claude-a"), routingPolicyAt)
	if !errors.Is(err, domain.ErrRoutingPolicyAgentRepeated) {
		t.Fatalf("set = %v, want %v", err, domain.ErrRoutingPolicyAgentRepeated)
	}
}

func TestSetRoutingPolicyRecordsAChangedOrderOnly(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "routing.db"), store.Options{})

	first := setRoutingPolicy(t, st, routingPolicyTreeRevision, routingPolicyRequest("claude-a", "claude-b"))
	if !first.Recorded || first.Policy.Revision != 1 || first.Policy.LineupRevision != routingPolicyTreeRevision {
		t.Fatalf("first set = %+v", first)
	}
	_, server := routingPolicyState(t, st)

	// The same orders against a newer tree record nothing and move no
	// revision, and the result is still the current one.
	newer := domain.Digest("sha256:" + strings.Repeat("b", 64))
	repeat := setRoutingPolicy(t, st, newer, routingPolicyRequest("claude-a", "claude-b"))
	if repeat.Recorded || repeat.Policy.Revision != 1 || repeat.Policy.LineupRevision != routingPolicyTreeRevision {
		t.Fatalf("repeat = %+v", repeat)
	}
	if revisions, after := routingPolicyState(t, st); len(revisions) != 1 || after != server {
		t.Fatalf("repeat wrote: %d revisions, server revision %d to %d", len(revisions), server, after)
	}

	changed := setRoutingPolicy(t, st, newer, routingPolicyRequest("claude-b", "claude-a"))
	if !changed.Recorded || changed.Policy.Revision != 2 || changed.Policy.LineupRevision != newer {
		t.Fatalf("changed = %+v", changed)
	}
	if revisions, after := routingPolicyState(t, st); len(revisions) != 2 || after != server+1 {
		t.Fatalf("change: %d revisions, server revision %d to %d", len(revisions), server, after)
	}
}

// TestSetRoutingPolicyChecksATreeThatDroppedTheAgent: a repeat is still
// checked, so it cannot report as current a policy this tree refuses.
func TestSetRoutingPolicyChecksATreeThatDroppedTheAgent(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "routing.db"), store.Options{})
	setRoutingPolicy(t, st, routingPolicyTreeRevision, routingPolicyRequest("claude-a", "codex"))
	smaller := agenttree.Tree{Agents: []domain.AgentSource{{Name: "claude-a"}}}
	_, err := SetRoutingPolicy(context.Background(), st, smaller, routingPolicyTreeRevision,
		routingPolicyRequest("claude-a", "codex"), routingPolicyAt)
	if !errors.Is(err, domain.ErrRoutingPolicyAgentUnknown) {
		t.Fatalf("set = %v, want %v", err, domain.ErrRoutingPolicyAgentUnknown)
	}
}

// seedRoutingPolicyPools gives claude-a an enrollment on pool-a and claude-b
// one on pool-b, and returns the tree naming them; codex keeps no enrollment.
func seedRoutingPolicyPools(t *testing.T, st *store.Store) agenttree.Tree {
	t.Helper()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, seed := range []struct{ agent, pool string }{{"claude-a", "pool-a"}, {"claude-b", "pool-b"}} {
			identity := domain.AuthIdentity{
				ID: domain.AuthIdentityID("auth-" + seed.agent), Provider: "openai",
				AccountBinding: "acct-" + seed.agent, UsagePool: seed.pool,
				AuthStoreMutationLease: true, Enabled: true,
			}
			if err := storetest.RecordIdentityOnPool(ctx, tx, identity, 1, routingPolicyAt.Add(-time.Hour)); err != nil {
				return err
			}
			if err := tx.RecordClientEnrollment(ctx, domain.ClientEnrollment{
				ID: domain.ClientEnrollmentID("enrollment-" + seed.agent), AuthIdentityID: identity.ID,
				HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
				AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
				RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
				AccountBinding: identity.AccountBinding,
			}, routingPolicyAt.Add(-time.Hour)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return agenttree.Tree{Agents: []domain.AgentSource{
		{Name: "claude-a", Enrollment: "enrollment-claude-a"},
		{Name: "claude-b", Enrollment: "enrollment-claude-b"},
		{Name: "codex", Enrollment: "enrollment-codex"},
	}}
}

// TestSetRoutingPolicyRecordsADrainFirstAccount: the account is part of the
// rule, so naming, repeating, and clearing it behave like a change of order.
func TestSetRoutingPolicyRecordsADrainFirstAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "routing.db"), store.Options{})
	tree := seedRoutingPolicyPools(t, st)
	set := func(account *domain.UsagePoolID) SetRoutingPolicyResult {
		t.Helper()
		req := routingPolicyRequest("claude-a", "claude-b")
		req.DrainFirstAccount = account
		result, err := SetRoutingPolicy(ctx, st, tree, routingPolicyTreeRevision, req, routingPolicyAt)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}

	set(nil)
	// The account is claude-b's pool, though claude-b is second in the order.
	named := set(new(domain.UsagePoolID("pool-b")))
	if !named.Recorded || named.Policy.Revision != 2 || named.Policy.DrainFirstAccount == nil ||
		*named.Policy.DrainFirstAccount != "pool-b" {
		t.Fatalf("named = %+v", named)
	}
	_, server := routingPolicyState(t, st)
	if repeat := set(new(domain.UsagePoolID("pool-b"))); repeat.Recorded || repeat.Policy.Revision != 2 {
		t.Fatalf("repeat = %+v", repeat)
	}
	if revisions, after := routingPolicyState(t, st); len(revisions) != 2 || after != server {
		t.Fatalf("repeat wrote: %d revisions, server revision %d to %d", len(revisions), server, after)
	}
	cleared := set(nil)
	if !cleared.Recorded || cleared.Policy.Revision != 3 || cleared.Policy.DrainFirstAccount != nil {
		t.Fatalf("cleared = %+v", cleared)
	}
}

// TestSetRoutingPolicyRefusesAnUnknownDrainFirstAccount: an account no tree
// agent's identity meters is refused and writes nothing, whether it names no
// pool or a pool only an agent outside the tree is on.
func TestSetRoutingPolicyRefusesAnUnknownDrainFirstAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "routing.db"), store.Options{})
	tree := seedRoutingPolicyPools(t, st)
	onlyA := agenttree.Tree{Agents: tree.Agents[:1]}
	for name, tc := range map[string]struct {
		tree    agenttree.Tree
		account domain.UsagePoolID
	}{
		"no such pool":        {tree, "SECRET-TOKEN"},
		"empty":               {tree, ""},
		"agent left the tree": {onlyA, "pool-b"},
	} {
		req := routingPolicyRequest("claude-a")
		req.DrainFirstAccount = &tc.account
		_, err := SetRoutingPolicy(ctx, st, tc.tree, routingPolicyTreeRevision, req, routingPolicyAt)
		if !errors.Is(err, domain.ErrRoutingPolicyDrainFirstUnknown) {
			t.Errorf("%s: set = %v, want %v", name, err, domain.ErrRoutingPolicyDrainFirstUnknown)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET-TOKEN") {
			t.Errorf("%s: the error repeats the refused text: %v", name, err)
		}
	}
	if revisions, _ := routingPolicyState(t, st); len(revisions) != 0 {
		t.Fatalf("a refusal wrote %d revisions", len(revisions))
	}
}
