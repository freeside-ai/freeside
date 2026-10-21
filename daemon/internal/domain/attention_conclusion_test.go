package domain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var conclusionAt = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func dismissDecision() domain.ConclusionDecision {
	return domain.ConclusionDecision{
		CommandID: "cmd-1", Action: domain.ActionDismiss, DeviceID: "dev-1", DeviceDisplayName: "Test Mac",
	}
}

// closedItem is the spec_approval fixture at version 2 with status, decided at
// conclusionAt when decided is set.
func closedItem(t *testing.T, status domain.ItemStatus, decided bool) domain.AttentionItem {
	t.Helper()
	in := validItemInput(domain.AttentionSpecApproval)
	in.Status = status
	in.ItemVersion = 2
	item := mustItem(t, in)
	if decided {
		var err error
		if item, err = item.WithDecidedAt(conclusionAt); err != nil {
			t.Fatalf("WithDecidedAt: %v", err)
		}
	}
	return item
}

// TestConclusionCauseAdmitsStatus pins which closed statuses each cause may
// carry: a decision closes to its action's outcome, and each daemon cause to
// exactly one status.
func TestConclusionCauseAdmitsStatus(t *testing.T) {
	admitted := map[domain.ConclusionCause][]domain.ItemStatus{
		domain.ConcludedByDecision:     {domain.StatusResolved, domain.StatusDismissed, domain.StatusSuperseded},
		domain.ConcludedByExpiry:       {domain.StatusExpired},
		domain.ConcludedBySupersession: {domain.StatusSuperseded},
		domain.ConcludedByDaemon:       {domain.StatusResolved},
	}
	for _, cause := range domain.AllConclusionCauses {
		for _, status := range domain.AllItemStatuses {
			if status == domain.StatusOpen {
				continue
			}
			t.Run(string(cause)+" "+string(status), func(t *testing.T) {
				decided := cause == domain.ConcludedByDecision
				var conclusion domain.Conclusion
				var err error
				if decided {
					conclusion, err = domain.NewDecisionConclusion(conclusionAt, dismissDecision())
				} else {
					conclusion, err = domain.NewDaemonConclusion(cause, conclusionAt, "Closed by the daemon.", nil)
				}
				if err != nil {
					t.Fatalf("conclusion: %v", err)
				}
				_, err = closedItem(t, status, decided).WithConclusion(conclusion)
				want := false
				for _, s := range admitted[cause] {
					want = want || s == status
				}
				if want && err != nil {
					t.Fatalf("WithConclusion = %v, want accepted", err)
				}
				if !want && !errors.Is(err, domain.ErrConclusionStatusMismatch) {
					t.Fatalf("WithConclusion = %v, want ErrConclusionStatusMismatch", err)
				}
			})
		}
	}
}

func TestConclusionShape(t *testing.T) {
	successor := domain.ItemID("item-2")
	empty := domain.ItemID("")
	routes := []domain.ConcludedRoute{
		{FindingID: "finding-1", Route: domain.RouteRemediate},
		{FindingID: "finding-2", Route: domain.RouteDecline},
	}
	adjudicate := func(routes []domain.ConcludedRoute) domain.ConclusionDecision {
		d := dismissDecision()
		d.Action = domain.ActionAdjudicateFindings
		d.FindingRoutes = routes
		return d
	}
	t.Run("accepted", func(t *testing.T) {
		if _, err := domain.NewDecisionConclusion(conclusionAt, adjudicate(routes)); err != nil {
			t.Fatalf("adjudication = %v", err)
		}
		if _, err := domain.NewDaemonConclusion(domain.ConcludedBySupersession, conclusionAt, "Replaced.", &successor); err != nil {
			t.Fatalf("supersession = %v", err)
		}
	})
	rejected := []struct {
		name  string
		build func() error
	}{
		{"unknown cause", func() error {
			_, err := domain.NewDaemonConclusion("", conclusionAt, "Closed.", nil)
			return err
		}},
		{"daemon close without a reason", func() error {
			_, err := domain.NewDaemonConclusion(domain.ConcludedByExpiry, conclusionAt, "  ", nil)
			return err
		}},
		{"daemon cause given as decision", func() error {
			_, err := domain.NewDaemonConclusion(domain.ConcludedByDecision, conclusionAt, "Closed.", nil)
			return err
		}},
		{"empty successor", func() error {
			_, err := domain.NewDaemonConclusion(domain.ConcludedBySupersession, conclusionAt, "Replaced.", &empty)
			return err
		}},
		{"decision without a device name", func() error {
			d := dismissDecision()
			d.DeviceDisplayName = ""
			_, err := domain.NewDecisionConclusion(conclusionAt, d)
			return err
		}},
		{"routes on an action that carries none", func() error {
			d := dismissDecision()
			d.FindingRoutes = routes
			_, err := domain.NewDecisionConclusion(conclusionAt, d)
			return err
		}},
		{"empty routes", func() error {
			_, err := domain.NewDecisionConclusion(conclusionAt, adjudicate([]domain.ConcludedRoute{}))
			return err
		}},
		{"unsorted routes", func() error {
			_, err := domain.NewDecisionConclusion(conclusionAt, adjudicate([]domain.ConcludedRoute{routes[1], routes[0]}))
			return err
		}},
		{"duplicate route", func() error {
			_, err := domain.NewDecisionConclusion(conclusionAt, adjudicate([]domain.ConcludedRoute{routes[0], routes[0]}))
			return err
		}},
		{"non-UTC instant", func() error {
			_, err := domain.NewDaemonConclusion(domain.ConcludedByExpiry,
				conclusionAt.In(time.FixedZone("x", 3600)), "Closed.", nil)
			return err
		}},
	}
	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.build(); err == nil {
				t.Fatal("accepted, want refused")
			}
		})
	}
}

// TestAttentionItemConclusionAgreesWithItem covers the structural gate the
// store re-runs on every decode: a conclusion that disagrees with its item is
// refused, and a closed item without one (a row from before the field) passes.
func TestAttentionItemConclusionAgreesWithItem(t *testing.T) {
	if err := closedItem(t, domain.StatusResolved, true).Validate(); err != nil {
		t.Fatalf("legacy closed item without a conclusion = %v, want accepted", err)
	}
	decision, err := domain.NewDecisionConclusion(conclusionAt, dismissDecision())
	if err != nil {
		t.Fatal(err)
	}
	expiry, err := domain.NewDaemonConclusion(domain.ConcludedByExpiry, conclusionAt, "Expired.", nil)
	if err != nil {
		t.Fatal(err)
	}
	open := mustItem(t, validItemInput(domain.AttentionSpecApproval))
	open.Conclusion = &expiry
	if err := open.Validate(); !errors.Is(err, domain.ErrConclusionOnOpenItem) {
		t.Fatalf("open item with a conclusion = %v, want ErrConclusionOnOpenItem", err)
	}
	undecided := closedItem(t, domain.StatusDismissed, false)
	undecided.Conclusion = &decision
	if err := undecided.Validate(); !errors.Is(err, domain.ErrInvalidConclusion) {
		t.Fatalf("decision conclusion without decided_at = %v, want ErrInvalidConclusion", err)
	}
	moved := closedItem(t, domain.StatusDismissed, true)
	later := decision
	later.ConcludedAt = conclusionAt.Add(time.Second)
	moved.Conclusion = &later
	if err := moved.Validate(); !errors.Is(err, domain.ErrInvalidConclusion) {
		t.Fatalf("decision conclusion off decided_at = %v, want ErrInvalidConclusion", err)
	}
	decidedExpiry := closedItem(t, domain.StatusExpired, true)
	decidedExpiry.Conclusion = &expiry
	if err := decidedExpiry.Validate(); !errors.Is(err, domain.ErrInvalidConclusion) {
		t.Fatalf("daemon conclusion with decided_at = %v, want ErrInvalidConclusion", err)
	}
	self := domain.ItemID("item-1")
	selfSuccession, err := domain.NewDaemonConclusion(domain.ConcludedBySupersession, conclusionAt, "Replaced.", &self)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := closedItem(t, domain.StatusSuperseded, false).WithConclusion(selfSuccession); !errors.Is(err, domain.ErrInvalidConclusion) {
		t.Fatalf("self successor = %v, want ErrInvalidConclusion", err)
	}
	concludedOnce, err := closedItem(t, domain.StatusExpired, false).WithConclusion(expiry)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := concludedOnce.WithConclusion(expiry); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("second conclusion = %v, want ErrImmutableTransition", err)
	}
}

// TestValidateAttentionItemConclusionTransition covers the lifecycle rule: a
// close carries its conclusion, a recorded one never changes, and a legacy
// closed item is never backfilled.
func TestValidateAttentionItemConclusionTransition(t *testing.T) {
	open := mustItem(t, validItemInput(domain.AttentionSpecApproval))
	expiry, err := domain.NewDaemonConclusion(domain.ConcludedByExpiry, conclusionAt, "Expired.", nil)
	if err != nil {
		t.Fatal(err)
	}
	bare := closedItem(t, domain.StatusExpired, false)
	expired, err := bare.WithConclusion(expiry)
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.ValidateAttentionItemTransition(open, bare); !errors.Is(err, domain.ErrConclusionMissing) {
		t.Fatalf("close without a conclusion = %v, want ErrConclusionMissing", err)
	}
	if err := domain.ValidateAttentionItemTransition(open, expired); err != nil {
		t.Fatalf("close with its conclusion = %v, want accepted", err)
	}
	next := expired
	next.ItemVersion = 3
	if err := domain.ValidateAttentionItemTransition(expired, next); err != nil {
		t.Fatalf("advance keeping the conclusion = %v, want accepted", err)
	}
	reworded := next
	changed := expiry
	changed.Reason = "Expired for another reason."
	reworded.Conclusion = &changed
	if err := domain.ValidateAttentionItemTransition(expired, reworded); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("changed conclusion = %v, want ErrImmutableTransition", err)
	}
	erased := next
	erased.Conclusion = nil
	if err := domain.ValidateAttentionItemTransition(expired, erased); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("erased conclusion = %v, want ErrImmutableTransition", err)
	}
	backfilled := next
	if err := domain.ValidateAttentionItemTransition(bare, backfilled); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("backfilled conclusion = %v, want ErrImmutableTransition", err)
	}
}

// TestAttentionItemConclusionDecodesLegacyBody pins that a stored body from
// before the field, with no conclusion key, decodes to a nil conclusion and
// validates.
func TestAttentionItemConclusionDecodesLegacyBody(t *testing.T) {
	body, err := json.Marshal(closedItem(t, domain.StatusResolved, true))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "conclusion")
	legacy, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.AttentionItem
	if err := json.Unmarshal(legacy, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Conclusion != nil {
		t.Fatalf("legacy conclusion = %+v, want nil", decoded.Conclusion)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatalf("legacy body = %v, want valid", err)
	}
}
