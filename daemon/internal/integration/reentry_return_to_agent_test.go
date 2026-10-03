package integration_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestReenteredReadyItemOffersNoReturnToAgent: a re-entered cycle imported
// nothing, so there is no candidate to hand back. Its ready item does not
// offer the action, the command is rejected with a typed error before any
// fetch or replay, and the item keeps the readiness the cycle re-earned.
func TestReenteredReadyItemOffersNoReturnToAgent(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
	superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
	})
	if !started {
		t.Fatal("a base advance started no re-entry")
	}
	p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("re-entry result = %#v, %v", result, err)
	}
	reentered := p.assertReentered(t, superseded, 2, advanced, p.replay.HeadSHA)
	ready, err := p.attention.GetAttentionItem(p.ctx, reentered.ID)
	if err != nil {
		t.Fatal(err)
	}
	const deviceID domain.DeviceID = "reentry-device"
	if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(p.ctx, domain.Device{
			ID: deviceID, DisplayName: "Re-entry device", Status: domain.DeviceActive, PairedAt: p.now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(ready.Item.RequestedDecision, domain.ActionReturnToAgent) {
		t.Fatalf("re-entered ready item offers return to agent: %v", ready.Item.RequestedDecision)
	}
	const commandID = "return-reentered"
	fetches, merges := p.transport.fetchCount(), p.transport.mergeCount()
	if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
		CommandID: commandID, DeviceID: deviceID, ExpectedEntityVersion: ready.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: ready.Item.ID, ItemVersion: ready.Item.ItemVersion,
			PRHeadSHA: ready.Item.PRHeadSHA, ArtifactDigests: ready.Item.ArtifactDigests,
			Action: domain.ActionReturnToAgent, Message: "Rework the change.",
		},
	}); !errors.Is(err, store.ErrActionNotOffered) {
		t.Fatalf("return to agent on a re-entered ready item = %v, want ErrActionNotOffered", err)
	}
	for range 2 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatalf("a rejected return to agent stopped the lane: %v", err)
		}
	}
	if p.transport.fetchCount() != fetches || p.transport.mergeCount() != merges {
		t.Fatal("a rejected return to agent fetched or merged")
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		kept, err := tx.GetAttentionItem(p.ctx, ready.Item.ID)
		if err != nil {
			return err
		}
		if kept.Status != domain.StatusOpen || kept.ItemVersion != ready.Item.ItemVersion {
			t.Errorf("re-entered ready item = status %s version %d, want open at version %d",
				kept.Status, kept.ItemVersion, ready.Item.ItemVersion)
		}
		if _, err := tx.GetAttentionItem(
			p.ctx, domain.ItemID("operator-feedback-undeliverable-"+commandID),
		); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a rejected return to agent raised a notice: %v", err)
		}
		if _, err := tx.GetAgentInvocation(
			p.ctx, domain.InvocationID("inv-operator-feedback-"+commandID),
		); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("a rejected return to agent recorded an invocation: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
