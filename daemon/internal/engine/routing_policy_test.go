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
