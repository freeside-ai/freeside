package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// seedComprehensionDeps writes the run/item/device/command graph the
// comprehension rows reference, in referential order.
func seedComprehensionDeps(t *testing.T, ctx context.Context, s *store.Store, f fixtures) {
	t.Helper()
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, f.invocation); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, f.artifact); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, f.item); err != nil {
			return err
		}
		if err := tx.PutDevice(ctx, f.device); err != nil {
			return err
		}
		return tx.PutCommand(ctx, f.command)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// comprehensionFixtures builds the surface, contract, and events the tests
// share, all bound to the seeded item and device.
func comprehensionFixtures(t *testing.T, f fixtures, ts time.Time) (
	domain.ClientCapabilityContract, domain.DecisionActionSurface,
	domain.ComprehensionEvent, domain.ComprehensionEvent,
) {
	t.Helper()
	surface, err := domain.NewDecisionSurface(f.item)
	if err != nil {
		t.Fatalf("NewDecisionSurface: %v", err)
	}
	contract, err := domain.NewClientCapabilityContract(f.device.ID, []domain.Action{
		domain.ActionOpenPR, domain.ActionReturnToAgent, domain.ActionDismiss, domain.ActionApprove,
	})
	if err != nil {
		t.Fatalf("NewClientCapabilityContract: %v", err)
	}
	actionSurface, err := domain.DeriveDecisionActionSurface(f.device.ID, surface, contract)
	if err != nil {
		t.Fatalf("DeriveDecisionActionSurface: %v", err)
	}
	cardOpened, err := domain.NewComprehensionEvent(domain.ComprehensionEventInput{
		DeviceID: f.device.ID, EventID: "event-card-1", ItemID: f.item.ID,
		Kind: domain.ComprehensionCardOpened, ItemDecisionSurfaceDigest: surface.Digest,
		OccurredAt: ts, Sequence: 1,
	}, ts.Add(time.Second))
	if err != nil {
		t.Fatalf("card event: %v", err)
	}
	actionDigest := actionSurface.Digest
	actionTaken, err := domain.NewComprehensionEvent(domain.ComprehensionEventInput{
		DeviceID: f.device.ID, EventID: "event-action-1", ItemID: f.item.ID,
		Kind: domain.ComprehensionActionTaken, ItemDecisionSurfaceDigest: surface.Digest,
		DecisionActionSurfaceDigest: &actionDigest, CommandID: f.command.CommandID,
		OccurredAt: ts.Add(time.Minute), Sequence: 2,
	}, ts.Add(time.Minute+time.Second))
	if err != nil {
		t.Fatalf("action event: %v", err)
	}
	return contract, actionSurface, cardOpened, actionTaken
}

// TestComprehensionRoundTrip persists the capability contract, action surface,
// events, and a defect, and reads them back through the observation surface.
func TestComprehensionRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedComprehensionDeps(t, ctx, s, f)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	contract, actionSurface, cardOpened, actionTaken := comprehensionFixtures(t, f, ts)

	defect := domain.ComprehensionDefect{
		ItemID: f.item.ID, ClaimDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		RecordedAt: ts.Add(3 * time.Hour), Reason: "the readiness summary overstated the passing checks",
	}

	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDeviceCapabilityContract(ctx, contract, ts); err != nil {
			return err
		}
		if _, err := tx.PutDecisionActionSurface(ctx, actionSurface, ts); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("write contract/surface: %v", err)
	}
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if _, err := tx.RecordComprehensionEvent(ctx, cardOpened); err != nil {
			return err
		}
		if _, err := tx.RecordComprehensionEvent(ctx, actionTaken); err != nil {
			return err
		}
		return tx.RecordComprehensionDefect(ctx, defect)
	}); err != nil {
		t.Fatalf("record events/defect: %v", err)
	}

	// Read back through the observation surface and golden the persisted shapes.
	var (
		gotContract domain.ClientCapabilityContract
		gotSurfaces []domain.DecisionActionSurface
		gotEvents   []domain.ComprehensionEvent
		gotDefects  []domain.ComprehensionDefect
		gotDecided  []store.DecidedCommand
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		gotContract, err = tx.GetDeviceCapabilityContract(ctx, f.device.ID)
		return err
	}); err != nil {
		t.Fatalf("get contract: %v", err)
	}
	if err := s.ReadComprehension(ctx, func(tx *store.ComprehensionReadTx) error {
		var err error
		if gotSurfaces, err = tx.ListDecisionActionSurfaces(ctx); err != nil {
			return err
		}
		if gotEvents, err = tx.ListComprehensionEvents(ctx); err != nil {
			return err
		}
		if gotDefects, err = tx.ListComprehensionDefects(ctx); err != nil {
			return err
		}
		gotDecided, err = tx.ListDecidedCommands(ctx)
		return err
	}); err != nil {
		t.Fatalf("read comprehension: %v", err)
	}

	if string(marshalIndent(t, gotContract)) != string(marshalIndent(t, contract)) {
		t.Fatalf("contract round-trip mismatch:\ngot:  %s\nwant: %s",
			marshalIndent(t, gotContract), marshalIndent(t, contract))
	}
	golden.Assert(t, "client_capability_contract", marshalIndent(t, gotContract))
	if len(gotSurfaces) != 1 {
		t.Fatalf("surfaces = %d, want 1", len(gotSurfaces))
	}
	golden.Assert(t, "decision_action_surface", marshalIndent(t, gotSurfaces[0]))
	if len(gotEvents) != 2 {
		t.Fatalf("events = %d, want 2", len(gotEvents))
	}
	golden.Assert(t, "comprehension_event_card_opened", marshalIndent(t, gotEvents[0]))
	golden.Assert(t, "comprehension_event_action_taken", marshalIndent(t, gotEvents[1]))
	if len(gotDefects) != 1 {
		t.Fatalf("defects = %d, want 1", len(gotDefects))
	}
	golden.Assert(t, "comprehension_defect", marshalIndent(t, gotDefects[0]))
	if len(gotDecided) != 1 || gotDecided[0].Command.CommandID != f.command.CommandID {
		t.Fatalf("decided commands = %+v, want the seeded command", gotDecided)
	}
	if gotDecided[0].ItemType != f.item.Type {
		t.Fatalf("decided command item type = %q, want %q", gotDecided[0].ItemType, f.item.Type)
	}
}

// TestRecordComprehensionEventDoesNotAdvanceRevision is acceptance: recording
// an event never bumps the sync revision, and a replay returns the recorded
// row unchanged.
func TestRecordComprehensionEventDoesNotAdvanceRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedComprehensionDeps(t, ctx, s, f)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, _, cardOpened, _ := comprehensionFixtures(t, f, ts)

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	var recorded domain.ComprehensionEvent
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		recorded, err = tx.RecordComprehensionEvent(ctx, cardOpened)
		return err
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("recording an event bumped revision %d -> %d, want unchanged", before.Revision, after.Revision)
	}
	if recorded.ReceivedAt != cardOpened.ReceivedAt {
		t.Fatalf("recorded received_at = %v, want %v", recorded.ReceivedAt, cardOpened.ReceivedAt)
	}

	// Replay with a conflicting body under the same idempotency key: the stored
	// row is returned unchanged (the client event_id is the idempotency key).
	replay := cardOpened
	replay.Sequence = 99
	var replayed domain.ComprehensionEvent
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		replayed, err = tx.RecordComprehensionEvent(ctx, replay)
		return err
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if replayed.Sequence != cardOpened.Sequence {
		t.Fatalf("replay returned sequence %d, want the recorded %d", replayed.Sequence, cardOpened.Sequence)
	}
}

// TestPutDecisionActionSurfaceIdempotent: a second put of the same
// content-addressed surface returns the existing row rather than erroring.
func TestPutDecisionActionSurfaceIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedComprehensionDeps(t, ctx, s, f)
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	_, actionSurface, _, _ := comprehensionFixtures(t, f, ts)

	for i := range 2 {
		if err := s.Write(ctx, func(tx *store.WriteTx) error {
			got, err := tx.PutDecisionActionSurface(ctx, actionSurface, ts)
			if err != nil {
				return err
			}
			if got.Digest != actionSurface.Digest {
				t.Fatalf("put %d returned digest %q, want %q", i, got.Digest, actionSurface.Digest)
			}
			return nil
		}); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
}
