package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agenttree"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var (
	routingPolicyTreeRevision = domain.Digest("sha256:" + strings.Repeat("a", 64))
	routingPolicyAt           = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
)

func routingPolicyRequest(enrollments ...domain.ClientEnrollmentID) SetRoutingPolicyRequest {
	return SetRoutingPolicyRequest{
		Roles:  []domain.RoutingPolicyRole{order(domain.RoleImplementer, enrollments...)},
		Source: domain.RoutingPolicySourceCLISet, SetBy: "cli:operator",
	}
}

// newRoutingPolicyFixture is the admission fixture with a second enrollment,
// on its own identity and pool, beside the main one.
func newRoutingPolicyFixture(t *testing.T) *agentAdmissionFixture {
	t.Helper()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	return f
}

func (f *agentAdmissionFixture) setPolicy(req SetRoutingPolicyRequest) (SetRoutingPolicyResult, error) {
	return SetRoutingPolicy(context.Background(), f.store, f.selection.Tree, routingPolicyTreeRevision, req, routingPolicyAt)
}

func setRoutingPolicy(t *testing.T, f *agentAdmissionFixture, req SetRoutingPolicyRequest) SetRoutingPolicyResult {
	t.Helper()
	result, err := f.setPolicy(req)
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

// Each refusal writes nothing and names the role and the entry's position,
// never the enrollment, which is request text.
func TestSetRoutingPolicyRefusesAnEnrollmentThatCannotRoute(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		change func(t *testing.T, f *agentAdmissionFixture)
		order  []domain.ClientEnrollmentID
		want   error
	}{
		"unknown": {
			order: []domain.ClientEnrollmentID{agentTestEnrollment, agentTestPastedName},
			want:  domain.ErrRoutingPolicyEnrollmentUnknown,
		},
		"disabled identity": {
			change: func(t *testing.T, f *agentAdmissionFixture) {
				identity := f.identity
				identity.Enabled = false
				if err := f.store.Write(context.Background(), func(tx *store.WriteTx) error {
					return tx.RecordAuthIdentity(context.Background(), identity, agentTestAt)
				}); err != nil {
					t.Fatal(err)
				}
			},
			order: []domain.ClientEnrollmentID{agentTestSecondEnrollment, agentTestEnrollment},
			want:  domain.ErrRoutingPolicyEnrollmentDisabled,
		},
		"catalog lacks the lineup model": {
			change: func(t *testing.T, f *agentAdmissionFixture) {
				f.recordCatalog(t, agentTestEnrollment, "claude-other-test", "high")
			},
			order: []domain.ClientEnrollmentID{agentTestSecondEnrollment, agentTestEnrollment},
			want:  domain.ErrRoutingPolicyEnrollmentUnserved,
		},
		"catalog lacks the lineup effort": {
			change: func(t *testing.T, f *agentAdmissionFixture) {
				f.recordCatalog(t, agentTestEnrollment, "claude-implementer-test", "low")
			},
			order: []domain.ClientEnrollmentID{agentTestSecondEnrollment, agentTestEnrollment},
			want:  domain.ErrRoutingPolicyEnrollmentUnserved,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newRoutingPolicyFixture(t)
			if tc.change != nil {
				tc.change(t, f)
			}
			_, err := f.setPolicy(routingPolicyRequest(tc.order...))
			if !errors.Is(err, tc.want) {
				t.Fatalf("set = %v, want %v", err, tc.want)
			}
			if !strings.Contains(err.Error(), "role implementer enrollment 2") {
				t.Fatalf("refusal names neither the role nor the position: %v", err)
			}
			if strings.Contains(err.Error(), string(tc.order[1])) {
				t.Fatalf("refusal repeats the enrollment: %v", err)
			}
			if revisions, _ := routingPolicyState(t, f.store); len(revisions) != 0 {
				t.Fatalf("revisions after a refusal = %+v", revisions)
			}
		})
	}
}

// An enrollment whose catalog serves the lineup's model and effort is
// accepted, as is one with no catalog recorded.
func TestSetRoutingPolicyAcceptsAServedOrUncataloguedEnrollment(t *testing.T) {
	t.Parallel()
	f := newRoutingPolicyFixture(t)
	f.recordCatalog(t, agentTestEnrollment, "claude-implementer-test", "high")
	result := setRoutingPolicy(t, f, routingPolicyRequest(agentTestEnrollment, agentTestSecondEnrollment))
	if !result.Recorded || !slices.Equal(result.Policy.Roles[0].Enrollments,
		[]domain.ClientEnrollmentID{agentTestEnrollment, agentTestSecondEnrollment}) {
		t.Fatalf("set = %+v", result)
	}
}

// A role the lineup leaves unbound has no model and effort to route on, and
// a new revision is never written in the pre-unit agents form.
func TestSetRoutingPolicyRefusesARoleItCannotRoute(t *testing.T) {
	t.Parallel()
	f := newRoutingPolicyFixture(t)
	unbound := f.selection.Tree
	unbound.Lineup = slices.DeleteFunc(slices.Clone(unbound.Lineup), func(line agenttree.LineupLine) bool {
		return line.Key == string(domain.RoleImplementer)
	})
	_, err := SetRoutingPolicy(context.Background(), f.store, unbound, routingPolicyTreeRevision,
		routingPolicyRequest(agentTestEnrollment), routingPolicyAt)
	if !errors.Is(err, domain.ErrRoutingPolicyRoleUnbound) {
		t.Fatalf("unbound role: set = %v, want %v", err, domain.ErrRoutingPolicyRoleUnbound)
	}
	agents := SetRoutingPolicyRequest{
		Roles:  []domain.RoutingPolicyRole{{Role: domain.RoleImplementer, Agents: []string{agentTestWriterAgent}}},
		Source: domain.RoutingPolicySourceCLISet, SetBy: "cli:operator",
	}
	if _, err := f.setPolicy(agents); !errors.Is(err, domain.ErrRoutingPolicyAgentsForm) {
		t.Fatalf("agents form: set = %v, want %v", err, domain.ErrRoutingPolicyAgentsForm)
	}
	if revisions, _ := routingPolicyState(t, f.store); len(revisions) != 0 {
		t.Fatalf("revisions after a refusal = %+v", revisions)
	}
}

func TestSetRoutingPolicyRefusesAShapeBeforeTheStore(t *testing.T) {
	t.Parallel()
	f := newAgentAdmissionFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := f.setPolicy(routingPolicyRequest(agentTestEnrollment, agentTestEnrollment))
	if !errors.Is(err, domain.ErrRoutingPolicyEnrollmentRepeated) {
		t.Fatalf("set = %v, want %v", err, domain.ErrRoutingPolicyEnrollmentRepeated)
	}
}

func TestSetRoutingPolicyRecordsAChangedOrderOnly(t *testing.T) {
	t.Parallel()
	f := newRoutingPolicyFixture(t)
	ctx := context.Background()

	first := setRoutingPolicy(t, f, routingPolicyRequest(agentTestEnrollment, agentTestSecondEnrollment))
	if !first.Recorded || first.Policy.Revision != 1 || first.Policy.LineupRevision != routingPolicyTreeRevision {
		t.Fatalf("first set = %+v", first)
	}
	_, server := routingPolicyState(t, f.store)

	// The same orders against a newer tree record nothing and move no
	// revision, and the result is still the current one.
	newer := domain.Digest("sha256:" + strings.Repeat("b", 64))
	repeat, err := SetRoutingPolicy(ctx, f.store, f.selection.Tree, newer,
		routingPolicyRequest(agentTestEnrollment, agentTestSecondEnrollment), routingPolicyAt)
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Recorded || repeat.Policy.Revision != 1 || repeat.Policy.LineupRevision != routingPolicyTreeRevision {
		t.Fatalf("repeat = %+v", repeat)
	}
	if revisions, after := routingPolicyState(t, f.store); len(revisions) != 1 || after != server {
		t.Fatalf("repeat wrote: %d revisions, server revision %d to %d", len(revisions), server, after)
	}

	changed, err := SetRoutingPolicy(ctx, f.store, f.selection.Tree, newer,
		routingPolicyRequest(agentTestSecondEnrollment, agentTestEnrollment), routingPolicyAt)
	if err != nil {
		t.Fatal(err)
	}
	if !changed.Recorded || changed.Policy.Revision != 2 || changed.Policy.LineupRevision != newer {
		t.Fatalf("changed = %+v", changed)
	}
	if revisions, after := routingPolicyState(t, f.store); len(revisions) != 2 || after != server+1 {
		t.Fatalf("change: %d revisions, server revision %d to %d", len(revisions), server, after)
	}
}

// TestSetRoutingPolicyChecksARepeat: a repeat is still checked, so it cannot
// report as current a policy whose enrollment no longer qualifies.
func TestSetRoutingPolicyChecksARepeat(t *testing.T) {
	t.Parallel()
	f := newRoutingPolicyFixture(t)
	setRoutingPolicy(t, f, routingPolicyRequest(agentTestSecondEnrollment, agentTestEnrollment))
	f.recordCatalog(t, agentTestEnrollment, "claude-other-test", "high")
	if _, err := f.setPolicy(routingPolicyRequest(agentTestSecondEnrollment, agentTestEnrollment)); !errors.Is(err, domain.ErrRoutingPolicyEnrollmentUnserved) {
		t.Fatalf("set = %v, want %v", err, domain.ErrRoutingPolicyEnrollmentUnserved)
	}
}

// TestSetRoutingPolicyRecordsADrainFirstAccount: the account is part of the
// rule, so naming, repeating, and clearing it behave like a change of order.
func TestSetRoutingPolicyRecordsADrainFirstAccount(t *testing.T) {
	t.Parallel()
	f := newRoutingPolicyFixture(t)
	set := func(account *domain.UsagePoolID) SetRoutingPolicyResult {
		t.Helper()
		req := routingPolicyRequest(agentTestEnrollment, agentTestSecondEnrollment)
		req.DrainFirstAccount = account
		return setRoutingPolicy(t, f, req)
	}

	set(nil)
	// The account is the second enrollment's pool, though it is second in
	// the order.
	named := set(new(routerSecondPool))
	if !named.Recorded || named.Policy.Revision != 2 || named.Policy.DrainFirstAccount == nil ||
		*named.Policy.DrainFirstAccount != routerSecondPool {
		t.Fatalf("named = %+v", named)
	}
	_, server := routingPolicyState(t, f.store)
	if repeat := set(new(routerSecondPool)); repeat.Recorded || repeat.Policy.Revision != 2 {
		t.Fatalf("repeat = %+v", repeat)
	}
	if revisions, after := routingPolicyState(t, f.store); len(revisions) != 2 || after != server {
		t.Fatalf("repeat wrote: %d revisions, server revision %d to %d", len(revisions), server, after)
	}
	cleared := set(nil)
	if !cleared.Recorded || cleared.Policy.Revision != 3 || cleared.Policy.DrainFirstAccount != nil {
		t.Fatalf("cleared = %+v", cleared)
	}
}

// TestSetRoutingPolicyRefusesAnUnknownDrainFirstAccount: an account no
// ordered enrollment's identity meters is refused and writes nothing,
// whether it names no pool or a pool only an unordered enrollment is on.
func TestSetRoutingPolicyRefusesAnUnknownDrainFirstAccount(t *testing.T) {
	t.Parallel()
	f := newRoutingPolicyFixture(t)
	for name, account := range map[string]domain.UsagePoolID{
		"no such pool":         "SECRET-TOKEN",
		"empty":                "",
		"unordered enrollment": routerSecondPool,
	} {
		req := routingPolicyRequest(agentTestEnrollment)
		req.DrainFirstAccount = &account
		_, err := f.setPolicy(req)
		if !errors.Is(err, domain.ErrRoutingPolicyDrainFirstUnknown) {
			t.Errorf("%s: set = %v, want %v", name, err, domain.ErrRoutingPolicyDrainFirstUnknown)
		}
		if err != nil && strings.Contains(err.Error(), "SECRET-TOKEN") {
			t.Errorf("%s: the error repeats the refused text: %v", name, err)
		}
	}
	if revisions, _ := routingPolicyState(t, f.store); len(revisions) != 0 {
		t.Fatalf("a refusal wrote %d revisions", len(revisions))
	}
}
