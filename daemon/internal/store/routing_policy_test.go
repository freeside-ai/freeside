package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var routingPolicySetAt = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

func routingPolicyInput(agents ...string) domain.RoutingPolicyInput {
	return domain.RoutingPolicyInput{
		Roles:  []domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Agents: agents}},
		Source: domain.RoutingPolicySourceCLISet, SetBy: "cli:operator",
		LineupRevision: domain.Digest("sha256:" + strings.Repeat("a", 64)),
	}
}

func appendRoutingPolicy(t *testing.T, s *store.Store, in domain.RoutingPolicyInput, at time.Time) domain.RoutingPolicy {
	t.Helper()
	ctx := context.Background()
	var policy domain.RoutingPolicy
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		policy, err = tx.AppendRoutingPolicy(ctx, in, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return policy
}

func readRoutingPolicies(t *testing.T, s *store.Store) (domain.RoutingPolicy, []domain.RoutingPolicy, error, error) {
	t.Helper()
	ctx := context.Background()
	var (
		current          domain.RoutingPolicy
		revisions        []domain.RoutingPolicy
		currentErr, list error
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		current, currentErr = tx.CurrentRoutingPolicy(ctx)
		revisions, list = tx.RoutingPolicyRevisions(ctx)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return current, revisions, currentErr, list
}

func TestRoutingPolicyAbsentUntilSet(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "routing.db"), store.Options{})
	_, revisions, currentErr, listErr := readRoutingPolicies(t, s)
	if !errors.Is(currentErr, store.ErrNotFound) {
		t.Fatalf("current = %v, want %v", currentErr, store.ErrNotFound)
	}
	if listErr != nil || revisions == nil || len(revisions) != 0 {
		t.Fatalf("revisions = %#v, %v, want an empty listing", revisions, listErr)
	}
}

func TestRoutingPolicyRevisionsSupersede(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "routing.db"), store.Options{})
	first := appendRoutingPolicy(t, s, routingPolicyInput("claude-a", "claude-b"), routingPolicySetAt)
	drained := routingPolicyInput("claude-b", "claude-a")
	drained.DrainFirstAccount = new(domain.UsagePoolID("pool-a"))
	second := appendRoutingPolicy(t, s, drained, routingPolicySetAt.Add(time.Hour))
	third := appendRoutingPolicy(t, s, routingPolicyInput("claude-b", "claude-a"), routingPolicySetAt.Add(2*time.Hour))
	if first.Revision != 1 || second.Revision != 2 || third.Revision != 3 {
		t.Fatalf("revisions = %d, %d, %d, want 1, 2, 3", first.Revision, second.Revision, third.Revision)
	}
	current, revisions, currentErr, listErr := readRoutingPolicies(t, s)
	if currentErr != nil || listErr != nil {
		t.Fatalf("read: %v, %v", currentErr, listErr)
	}
	if !reflect.DeepEqual(current, third) {
		t.Fatalf("current = %+v, want %+v", current, third)
	}
	if !reflect.DeepEqual(revisions, []domain.RoutingPolicy{first, second, third}) {
		t.Fatalf("revisions = %+v", revisions)
	}
	if revisions[1].DrainFirstAccount == nil || *revisions[1].DrainFirstAccount != "pool-a" ||
		revisions[2].DrainFirstAccount != nil {
		t.Fatalf("accounts = %v, %v, want pool-a then none", revisions[1].DrainFirstAccount, revisions[2].DrainFirstAccount)
	}
}

func TestAppendRoutingPolicyRefusesAnInvalidPolicy(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "routing.db"), store.Options{})
	ctx := context.Background()
	in := routingPolicyInput()
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.AppendRoutingPolicy(ctx, in, routingPolicySetAt)
		return err
	}); !errors.Is(err, domain.ErrRoutingPolicyOrderEmpty) {
		t.Fatalf("append = %v, want %v", err, domain.ErrRoutingPolicyOrderEmpty)
	}
	if _, _, currentErr, _ := readRoutingPolicies(t, s); !errors.Is(currentErr, store.ErrNotFound) {
		t.Fatalf("current after a refusal = %v", currentErr)
	}
}
