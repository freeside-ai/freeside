package store_test

import (
	"context"
	"database/sql"
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
	second := appendRoutingPolicy(t, s, routingPolicyInput("claude-b", "claude-a"), routingPolicySetAt.Add(time.Hour))
	if first.Revision != 1 || second.Revision != 2 {
		t.Fatalf("revisions = %d, %d, want 1, 2", first.Revision, second.Revision)
	}
	current, revisions, currentErr, listErr := readRoutingPolicies(t, s)
	if currentErr != nil || listErr != nil {
		t.Fatalf("read: %v, %v", currentErr, listErr)
	}
	if !reflect.DeepEqual(current, second) {
		t.Fatalf("current = %+v, want %+v", current, second)
	}
	if !reflect.DeepEqual(revisions, []domain.RoutingPolicy{first, second}) {
		t.Fatalf("revisions = %+v", revisions)
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

// TestRoutingPolicyRowEditedInPlaceFailsTheRead: every column is cross-checked
// against the body, so an edit to either fails both reads.
func TestRoutingPolicyRowEditedInPlaceFailsTheRead(t *testing.T) {
	t.Parallel()
	for name, statement := range map[string]string{
		"edited body":     `UPDATE routing_policies SET body = replace(body, '"cli:operator"', '"cli:other"')`,
		"unparsable body": `UPDATE routing_policies SET body = 'not json'`,
		"edited revision": `UPDATE routing_policies SET revision = 9`,
		"edited source":   `UPDATE routing_policies SET source = 'other'`,
		"edited set_by":   `UPDATE routing_policies SET set_by = 'cli:other'`,
		"edited set_at":   `UPDATE routing_policies SET set_at = '2027-01-01T00:00:00Z'`,
		"edited lineup":   `UPDATE routing_policies SET lineup_revision = 'sha256:other'`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "routing.db")
			s := storetest.Open(t, path, store.Options{})
			appendRoutingPolicy(t, s, routingPolicyInput("claude-a"), routingPolicySetAt)
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			if _, err := db.ExecContext(t.Context(), statement); err != nil {
				t.Fatal(err)
			}
			if _, _, currentErr, listErr := readRoutingPolicies(t, s); currentErr == nil || listErr == nil ||
				errors.Is(currentErr, store.ErrNotFound) {
				t.Fatalf("reads = %v, %v, want both to fail", currentErr, listErr)
			}
		})
	}
}
