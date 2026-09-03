package signet_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// itemActions is the fixture item's offered set.
var itemActions = []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss}

// registerAndDerive registers a capability contract for deviceID covering
// actions and returns the derived action surface.
func registerAndDerive(t *testing.T, f fixture, deviceID domain.DeviceID, actions []domain.Action) domain.DecisionActionSurface {
	t.Helper()
	ctx := context.Background()
	if _, err := f.service.RegisterClientCapability(ctx, deviceID, actions); err != nil {
		t.Fatalf("RegisterClientCapability: %v", err)
	}
	surface, err := f.service.DeriveActionSurface(ctx, deviceID, f.item.ID)
	if err != nil {
		t.Fatalf("DeriveActionSurface: %v", err)
	}
	return surface
}

func TestDeriveActionSurfaceIntersectsContract(t *testing.T) {
	f := newFixture(t)
	// Contract omits stop, so the surface is the item's set minus stop.
	surface := registerAndDerive(t, f, f.device.ID, []domain.Action{domain.ActionOpenPR, domain.ActionDismiss})
	want := []domain.Action{domain.ActionDismiss, domain.ActionOpenPR}
	if len(surface.Actions) != len(want) {
		t.Fatalf("surface actions = %v, want %v", surface.Actions, want)
	}
	for i := range want {
		if surface.Actions[i] != want[i] {
			t.Fatalf("surface actions = %v, want %v", surface.Actions, want)
		}
	}
	if surface.DeviceID != f.device.ID || surface.ItemID != f.item.ID {
		t.Fatalf("surface bindings = %q/%q", surface.DeviceID, surface.ItemID)
	}
}

func TestRegisterCapabilityIdempotentNoExtraBump(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if _, err := f.service.RegisterClientCapability(ctx, f.device.ID, itemActions); err != nil {
		t.Fatalf("register 1: %v", err)
	}
	afterFirst := f.revision(t)
	if _, err := f.service.RegisterClientCapability(ctx, f.device.ID, itemActions); err != nil {
		t.Fatalf("register 2: %v", err)
	}
	if got := f.revision(t); got != afterFirst {
		t.Fatalf("re-registering the same contract bumped revision %d -> %d", afterFirst, got)
	}
}

func TestDeriveActionSurfaceRequiresCapability(t *testing.T) {
	f := newFixture(t)
	_, err := f.service.DeriveActionSurface(context.Background(), f.device.ID, f.item.ID)
	if !errors.Is(err, signet.ErrCapabilityNotRegistered) {
		t.Fatalf("DeriveActionSurface without contract = %v, want ErrCapabilityNotRegistered", err)
	}
}

func TestSubmitStampsDecisionEvidence(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	surface := registerAndDerive(t, f, f.device.ID, itemActions)
	cmd := f.command("cmd-1", domain.ActionOpenPR)
	cmd.Payload.DecisionActionSurfaceDigest = &surface.Digest
	if _, err := f.service.Submit(ctx, cmd); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	var stored domain.Command
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetCommand(ctx, "cmd-1")
		return err
	}); err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if stored.DecisionEvidence == nil || stored.DecisionEvidence.ActionSurfaceDigest != surface.Digest {
		t.Fatalf("stamped evidence = %+v, want surface digest %q", stored.DecisionEvidence, surface.Digest)
	}
}

func TestSubmitRejectsForeignActionSurface(t *testing.T) {
	f := newFixture(t)
	f.seedDevice(t, "device-2")
	foreign := registerAndDerive(t, f, "device-2", itemActions)
	// device-1 also registers, so the mismatch is the surface's device, not a
	// missing contract.
	if _, err := f.service.RegisterClientCapability(context.Background(), f.device.ID, itemActions); err != nil {
		t.Fatalf("register device-1: %v", err)
	}
	cmd := f.command("cmd-1", domain.ActionOpenPR)
	cmd.Payload.DecisionActionSurfaceDigest = &foreign.Digest
	if _, err := f.service.Submit(context.Background(), cmd); !errors.Is(err, signet.ErrActionSurfaceMismatch) {
		t.Fatalf("Submit with foreign surface = %v, want ErrActionSurfaceMismatch", err)
	}
}

func TestSubmitRejectsUnknownActionSurface(t *testing.T) {
	f := newFixture(t)
	if _, err := f.service.RegisterClientCapability(context.Background(), f.device.ID, itemActions); err != nil {
		t.Fatalf("register: %v", err)
	}
	unknown := domain.Digest("sha256:" + strings.Repeat("a", 64))
	cmd := f.command("cmd-1", domain.ActionOpenPR)
	cmd.Payload.DecisionActionSurfaceDigest = &unknown
	if _, err := f.service.Submit(context.Background(), cmd); !errors.Is(err, signet.ErrActionSurfaceMismatch) {
		t.Fatalf("Submit with unknown surface = %v, want ErrActionSurfaceMismatch", err)
	}
}

func TestSubmitRejectsActionOutsideSurface(t *testing.T) {
	f := newFixture(t)
	// Contract omits stop, so the surface does not offer it; the item still
	// does, so only the surface check rejects the stop command.
	surface := registerAndDerive(t, f, f.device.ID, []domain.Action{domain.ActionOpenPR, domain.ActionDismiss})
	cmd := f.command("cmd-1", domain.ActionStop)
	cmd.Payload.DecisionActionSurfaceDigest = &surface.Digest
	if _, err := f.service.Submit(context.Background(), cmd); !errors.Is(err, signet.ErrActionSurfaceMismatch) {
		t.Fatalf("Submit stop outside surface = %v, want ErrActionSurfaceMismatch", err)
	}
}

func TestSubmitRejectsStaleCapabilityDigest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	surface := registerAndDerive(t, f, f.device.ID, itemActions)
	// Re-register with a different action set, changing the capability digest.
	if _, err := f.service.RegisterClientCapability(ctx, f.device.ID,
		[]domain.Action{domain.ActionOpenPR, domain.ActionDismiss}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	cmd := f.command("cmd-1", domain.ActionOpenPR)
	cmd.Payload.DecisionActionSurfaceDigest = &surface.Digest
	if _, err := f.service.Submit(ctx, cmd); !errors.Is(err, signet.ErrActionSurfaceMismatch) {
		t.Fatalf("Submit with stale capability digest = %v, want ErrActionSurfaceMismatch", err)
	}
}

func TestSubmitReplayIgnoresDifferentDigest(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	surface := registerAndDerive(t, f, f.device.ID, itemActions)
	cmd := f.command("cmd-1", domain.ActionOpenPR)
	cmd.Payload.DecisionActionSurfaceDigest = &surface.Digest
	if _, err := f.service.Submit(ctx, cmd); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Replay the same command id with a different (unknown) digest: idempotency
	// by command id returns the original result and never re-stamps.
	other := domain.Digest("sha256:" + strings.Repeat("b", 64))
	replay := cmd
	replay.Payload.DecisionActionSurfaceDigest = &other
	if _, err := f.service.Submit(ctx, replay); err != nil {
		t.Fatalf("replay Submit = %v, want the original result", err)
	}
	var stored domain.Command
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetCommand(ctx, "cmd-1")
		return err
	}); err != nil {
		t.Fatalf("GetCommand: %v", err)
	}
	if stored.DecisionEvidence == nil || stored.DecisionEvidence.ActionSurfaceDigest != surface.Digest {
		t.Fatalf("replay changed stamped evidence to %+v, want %q", stored.DecisionEvidence, surface.Digest)
	}
}

func TestRecordComprehensionEventNoRevisionBump(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	surface := registerAndDerive(t, f, f.device.ID, itemActions)
	before := f.revision(t)
	event, err := f.service.RecordComprehensionEvent(ctx, domain.ComprehensionEventInput{
		DeviceID: f.device.ID, EventID: "event-1", ItemID: f.item.ID,
		Kind: domain.ComprehensionCardOpened, ItemDecisionSurfaceDigest: surface.ItemDecisionSurfaceDigest,
		OccurredAt: *f.now, Sequence: 1,
	})
	if err != nil {
		t.Fatalf("RecordComprehensionEvent: %v", err)
	}
	if event.ReceivedAt.IsZero() {
		t.Fatalf("event received_at not stamped")
	}
	if got := f.revision(t); got != before {
		t.Fatalf("recording an event bumped revision %d -> %d", before, got)
	}
}

func TestRecordActionTakenRequiresBackingCommand(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	surface := registerAndDerive(t, f, f.device.ID, itemActions)
	cmd := f.command("cmd-1", domain.ActionOpenPR)
	cmd.Payload.DecisionActionSurfaceDigest = &surface.Digest
	if _, err := f.service.Submit(ctx, cmd); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	digest := surface.Digest
	base := domain.ComprehensionEventInput{
		DeviceID: f.device.ID, ItemID: f.item.ID, Kind: domain.ComprehensionActionTaken,
		ItemDecisionSurfaceDigest:   surface.ItemDecisionSurfaceDigest,
		DecisionActionSurfaceDigest: &digest, OccurredAt: *f.now, Sequence: 1,
	}
	// A backed event referencing the accepted command is recorded.
	backed := base
	backed.EventID, backed.CommandID = "event-ok", "cmd-1"
	if _, err := f.service.RecordComprehensionEvent(ctx, backed); err != nil {
		t.Fatalf("backed action_taken: %v", err)
	}
	// An event referencing an unknown command is rejected.
	unbacked := base
	unbacked.EventID, unbacked.CommandID = "event-bad", "cmd-missing"
	if _, err := f.service.RecordComprehensionEvent(ctx, unbacked); !errors.Is(err, signet.ErrComprehensionEventUnbacked) {
		t.Fatalf("unbacked action_taken = %v, want ErrComprehensionEventUnbacked", err)
	}
}

func TestRecordComprehensionEventReplay(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	surface := registerAndDerive(t, f, f.device.ID, itemActions)
	first, err := f.service.RecordComprehensionEvent(ctx, domain.ComprehensionEventInput{
		DeviceID: f.device.ID, EventID: "event-1", ItemID: f.item.ID,
		Kind: domain.ComprehensionCardOpened, ItemDecisionSurfaceDigest: surface.ItemDecisionSurfaceDigest,
		OccurredAt: *f.now, Sequence: 1,
	})
	if err != nil {
		t.Fatalf("record 1: %v", err)
	}
	// Replay the same (device, event) key with a different sequence: the
	// recorded row is returned unchanged.
	replay, err := f.service.RecordComprehensionEvent(ctx, domain.ComprehensionEventInput{
		DeviceID: f.device.ID, EventID: "event-1", ItemID: f.item.ID,
		Kind: domain.ComprehensionCardOpened, ItemDecisionSurfaceDigest: surface.ItemDecisionSurfaceDigest,
		OccurredAt: f.now.Add(time.Hour), Sequence: 99,
	})
	if err != nil {
		t.Fatalf("record replay: %v", err)
	}
	if replay.Sequence != first.Sequence {
		t.Fatalf("replay returned sequence %d, want the recorded %d", replay.Sequence, first.Sequence)
	}
}

func TestRecordComprehensionEventInactiveDevice(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	surface := registerAndDerive(t, f, f.device.ID, itemActions)
	// Revoke the device.
	revokedAt := f.now.Add(time.Minute)
	revoked := f.device
	revoked.Status = domain.DeviceRevoked
	revoked.RevokedAt = &revokedAt
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDevice(ctx, revoked) }); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_, err := f.service.RecordComprehensionEvent(ctx, domain.ComprehensionEventInput{
		DeviceID: f.device.ID, EventID: "event-1", ItemID: f.item.ID,
		Kind: domain.ComprehensionCardOpened, ItemDecisionSurfaceDigest: surface.ItemDecisionSurfaceDigest,
		OccurredAt: *f.now, Sequence: 1,
	})
	if !errors.Is(err, signet.ErrDeviceNotActive) {
		t.Fatalf("record from revoked device = %v, want ErrDeviceNotActive", err)
	}
}
