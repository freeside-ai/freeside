package signet_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
)

// wakeCounter is a FollowUpFiler that counts its wakes.
type wakeCounter struct{ wakes int }

func (w *wakeCounter) Wake() { w.wakes++ }

func filingDecision(f filingFixture, id string, action domain.Action) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: id, DeviceID: f.device.ID, ExpectedEntityVersion: 1,
		Payload: signet.DecisionPayload{
			ItemID: f.item.ID, Action: action, ItemVersion: f.item.ItemVersion,
			ArtifactDigests: f.item.ArtifactDigests,
		},
	}
}

// TestApprovingAFilingWakesTheFiler pins the approve wake: one wake when a
// new approve of a follow-up filing commits, and none for its replay.
func TestApprovingAFilingWakesTheFiler(t *testing.T) {
	f := newFilingFixture(t)
	ctx := context.Background()
	filer := &wakeCounter{}
	// The hook is read at each approval, so a filer bound after the service
	// is built is still the one woken.
	var bound signet.FollowUpFiler
	service := signet.NewService(f.store,
		signet.WithClock(func() time.Time { return *f.now }),
		signet.WithFollowUpFiler(func() signet.FollowUpFiler { return bound }))
	bound = filer

	approve := filingDecision(f, "approve-filing", domain.ActionApprove)
	if _, err := service.Submit(ctx, approve); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if filer.wakes != 1 {
		t.Fatalf("wakes after the approve = %d, want 1", filer.wakes)
	}
	if _, err := service.Submit(ctx, approve); err != nil {
		t.Fatalf("approve replay: %v", err)
	}
	if filer.wakes != 1 {
		t.Fatalf("wakes after the replay = %d, want 1", filer.wakes)
	}
}

// TestOnlyACommittedFilingApproveWakesTheFiler proves the wake is tied to the
// filing approval itself: a decline, a refused approve, and a closure approve
// wake nothing.
func TestOnlyACommittedFilingApproveWakesTheFiler(t *testing.T) {
	ctx := context.Background()
	t.Run("decline", func(t *testing.T) {
		f := newFilingFixture(t)
		filer := &wakeCounter{}
		service := signet.NewService(f.store,
			signet.WithClock(func() time.Time { return *f.now }),
			signet.WithFollowUpFiler(func() signet.FollowUpFiler { return filer }))
		if _, err := service.Submit(ctx, filingDecision(f, "decline-filing", domain.ActionDecline)); err != nil {
			t.Fatalf("decline: %v", err)
		}
		if filer.wakes != 0 {
			t.Fatalf("wakes after a decline = %d, want 0", filer.wakes)
		}
	})
	t.Run("refused approve", func(t *testing.T) {
		f := newFilingFixture(t)
		filer := &wakeCounter{}
		service := signet.NewService(f.store,
			signet.WithClock(func() time.Time { return *f.now }),
			signet.WithFollowUpFiler(func() signet.FollowUpFiler { return filer }))
		stale := filingDecision(f, "approve-stale", domain.ActionApprove)
		stale.ExpectedEntityVersion = 2
		if _, err := service.Submit(ctx, stale); err == nil {
			t.Fatal("an approve against a stale entity version was accepted")
		}
		if filer.wakes != 0 {
			t.Fatalf("wakes after a refused approve = %d, want 0", filer.wakes)
		}
	})
	t.Run("closure approve", func(t *testing.T) {
		f := newClosureFixture(t, true, domain.ClosureFlagOriginProposeSite)
		item := f.open(t, f.merge)
		filer := &wakeCounter{}
		service := signet.NewService(f.store,
			signet.WithClock(func() time.Time { return *f.now }),
			signet.WithFollowUpFiler(func() signet.FollowUpFiler { return filer }))
		if _, err := service.Submit(ctx, f.decision(item, "approve-closure", domain.ActionApprove)); err != nil {
			t.Fatalf("approve: %v", err)
		}
		if filer.wakes != 0 {
			t.Fatalf("wakes after a closure approve = %d, want 0", filer.wakes)
		}
	})
}

// TestApprovingAFilingWithoutAFilerIsRecorded proves the wake is optional: no
// option, a nil func, and a func that returns no filer all leave the approval
// recorded.
func TestApprovingAFilingWithoutAFilerIsRecorded(t *testing.T) {
	ctx := context.Background()
	hooks := map[string][]signet.Option{
		"no option":    nil,
		"nil func":     {signet.WithFollowUpFiler(nil)},
		"no filer yet": {signet.WithFollowUpFiler(func() signet.FollowUpFiler { return nil })},
	}
	for name, opts := range hooks {
		t.Run(name, func(t *testing.T) {
			f := newFilingFixture(t)
			service := signet.NewService(f.store,
				append(opts, signet.WithClock(func() time.Time { return *f.now }))...)
			if _, err := service.Submit(ctx, filingDecision(f, "approve-filing", domain.ActionApprove)); err != nil {
				t.Fatalf("approve: %v", err)
			}
			if decided, _ := f.itemSnapshotFor(t, f.item.ID); decided.Status != domain.StatusResolved {
				t.Fatalf("item status after the approve = %q, want resolved", decided.Status)
			}
		})
	}
}
