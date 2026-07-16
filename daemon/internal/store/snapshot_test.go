package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// seedItem writes the fixture conversation and item so command tests start
// from the same referential state TestCommandIdempotentAndStale uses.
func seedItem(t *testing.T, s *store.Store, f fixtures) {
	t.Helper()
	err := s.Write(context.Background(), func(tx *store.WriteTx) error {
		if err := tx.PutConversation(context.Background(), f.conversation); err != nil {
			return err
		}
		return tx.PutAttentionItem(context.Background(), f.item)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestAttentionItemSnapshotMetadata: the snapshot read returns the item with
// the store-stamped metadata the command-acceptance boundary checks (#91
// acceptance 1): entity_version counts Puts from 1, and as_of_revision is the
// revision the writing transaction committed as.
func TestAttentionItemSnapshotMetadata(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedItem(t, s, f)

	afterSeed, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	var (
		got  domain.AttentionItem
		snap store.Snapshot
	)
	read := func() {
		t.Helper()
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, snap, err = tx.GetAttentionItemSnapshot(ctx, f.item.ID)
			return err
		}); err != nil {
			t.Fatalf("GetAttentionItemSnapshot: %v", err)
		}
	}

	read()
	if string(marshalIndent(t, got)) != string(marshalIndent(t, f.item)) {
		t.Errorf("snapshot item differs from the put item:\ngot:  %s\nwant: %s",
			marshalIndent(t, got), marshalIndent(t, f.item))
	}
	if snap.EntityVersion != 1 {
		t.Errorf("entity_version after first put = %d, want 1", snap.EntityVersion)
	}
	if snap.AsOfRevision != afterSeed.Revision {
		t.Errorf("as_of_revision = %d, want the seeding transaction's revision %d",
			snap.AsOfRevision, afterSeed.Revision)
	}

	advanced := f.item
	advanced.ItemVersion = 2
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, advanced) }); err != nil {
		t.Fatalf("advance item: %v", err)
	}
	afterAdvance, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	read()
	if snap.EntityVersion != 2 {
		t.Errorf("entity_version after second put = %d, want 2", snap.EntityVersion)
	}
	if snap.AsOfRevision != afterAdvance.Revision {
		t.Errorf("as_of_revision after advance = %d, want %d", snap.AsOfRevision, afterAdvance.Revision)
	}
}

// TestItemSnapshotDistinguishesEntityVersionFromBindings (#91 acceptance 4):
// the domain binding fields cannot stand in for the store's version counter.
// An item born at item_version 2 sits at entity_version 1, so a command whose
// bindings match the live item exactly still diverges from the counter an
// expected_entity_version equal to the item_version would claim.
func TestItemSnapshotDistinguishesEntityVersionFromBindings(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)

	born := f.item
	born.ItemVersion = 2 // first persisted write: entity_version starts at 1 regardless
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, born)
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "cmd-ev", DeviceID: "device-1", ItemID: born.ID,
		ItemVersion: born.ItemVersion, PRHeadSHA: born.PRHeadSHA,
		ArtifactDigests: born.ArtifactDigests, Action: domain.ActionOpenPR,
	})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}

	var (
		got  domain.AttentionItem
		snap store.Snapshot
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, snap, err = tx.GetAttentionItemSnapshot(ctx, born.ID)
		return err
	}); err != nil {
		t.Fatalf("GetAttentionItemSnapshot: %v", err)
	}
	if !command.BindsSameAs(got) {
		t.Fatalf("command does not bind the live item; the divergence case needs matching bindings")
	}
	if snap.EntityVersion != 1 || got.ItemVersion != 2 {
		t.Fatalf("entity_version = %d, item_version = %d; want the counters to diverge as 1 vs 2",
			snap.EntityVersion, got.ItemVersion)
	}
}

// TestCommandSnapshotOriginalRevision (#91 acceptance 2): the command row is
// write-once, so its as_of_revision is the original committed result. Inside
// the accepting transaction the read already returns the revision that
// transaction commits as (the same-tx path Submit's fresh accept uses), and
// later client-visible writes leave the recorded value untouched.
func TestCommandSnapshotOriginalRevision(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedItem(t, s, f)

	var inTx store.Snapshot
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutCommand(ctx, f.command); err != nil {
			return err
		}
		var err error
		_, inTx, err = tx.GetCommandSnapshot(ctx, f.command.CommandID)
		return err
	})
	if err != nil {
		t.Fatalf("accept command: %v", err)
	}
	committed, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if inTx.AsOfRevision != committed.Revision {
		t.Fatalf("in-tx as_of_revision = %d, want the accepting transaction's committed revision %d",
			inTx.AsOfRevision, committed.Revision)
	}

	// An unrelated client-visible write advances the server revision; the
	// command's recorded result must not move with it.
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDevice(ctx, f.device) }); err != nil {
		t.Fatalf("unrelated write: %v", err)
	}
	bumped, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if bumped.Revision <= committed.Revision {
		t.Fatalf("revision did not advance: %d then %d", committed.Revision, bumped.Revision)
	}

	var (
		got  domain.Command
		snap store.Snapshot
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, snap, err = tx.GetCommandSnapshot(ctx, f.command.CommandID)
		return err
	}); err != nil {
		t.Fatalf("GetCommandSnapshot: %v", err)
	}
	if string(marshalIndent(t, got)) != string(marshalIndent(t, f.command)) {
		t.Errorf("retried read changed the record:\ngot:  %s\nwant: %s",
			marshalIndent(t, got), marshalIndent(t, f.command))
	}
	if snap.AsOfRevision != committed.Revision {
		t.Errorf("as_of_revision after later writes = %d, want the original %d",
			snap.AsOfRevision, committed.Revision)
	}
	if snap.EntityVersion != 1 {
		t.Errorf("command entity_version = %d, want the write-once 1", snap.EntityVersion)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if after.Revision != bumped.Revision {
		t.Errorf("a read moved the revision: %d then %d", bumped.Revision, after.Revision)
	}
}

// TestDeviceSnapshotMetadata (#106 acceptance 1-3): the device snapshot read
// returns the store-stamped metadata the pairing and revocation responses
// render. Devices are mutable, so a revocation write advances entity_version
// to 2, unlike the write-once command rows; GetDevice reads through the same
// path, so its behaviour is pinned by the same assertions.
func TestDeviceSnapshotMetadata(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)

	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDevice(ctx, f.device) }); err != nil {
		t.Fatalf("pair device: %v", err)
	}
	afterPair, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	var (
		got  domain.Device
		snap store.Snapshot
	)
	read := func() {
		t.Helper()
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, snap, err = tx.GetDeviceSnapshot(ctx, f.device.ID)
			return err
		}); err != nil {
			t.Fatalf("GetDeviceSnapshot: %v", err)
		}
	}

	read()
	if string(marshalIndent(t, got)) != string(marshalIndent(t, f.device)) {
		t.Errorf("snapshot device differs from the put device:\ngot:  %s\nwant: %s",
			marshalIndent(t, got), marshalIndent(t, f.device))
	}
	if snap.EntityVersion != 1 {
		t.Errorf("entity_version after pairing = %d, want 1", snap.EntityVersion)
	}
	if snap.AsOfRevision != afterPair.Revision {
		t.Errorf("as_of_revision = %d, want the pairing transaction's revision %d",
			snap.AsOfRevision, afterPair.Revision)
	}

	revokedAt := f.device.PairedAt.Add(time.Minute)
	revoked := f.device
	revoked.Status = domain.DeviceRevoked
	revoked.RevokedAt = &revokedAt
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDevice(ctx, revoked) }); err != nil {
		t.Fatalf("revoke device: %v", err)
	}
	afterRevoke, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	read()
	if snap.EntityVersion != 2 {
		t.Errorf("entity_version after revocation = %d, want 2", snap.EntityVersion)
	}
	if snap.AsOfRevision != afterRevoke.Revision {
		t.Errorf("as_of_revision after revocation = %d, want %d", snap.AsOfRevision, afterRevoke.Revision)
	}

	err = s.Read(ctx, func(tx *store.ReadTx) error {
		_, _, err := tx.GetDeviceSnapshot(ctx, "device-unknown")
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown device error = %v, want ErrNotFound", err)
	}
}

// errAbandonReplay stands in for the sentinel a command-acceptance service
// returns to abandon a Write whose command turned out to be a replay.
var errAbandonReplay = errors.New("abandon replay")

// TestCommandSnapshotReplayInsideWrite: the exact retry path the acceptance
// boundary runs. A byte-identical PutCommand replay inside a later Write is a
// no-op, so an in-tx GetCommandSnapshot still returns the original committed
// revision, not the retrying transaction's; and abandoning that Write with an
// error leaves the server revision unmoved, which is how a service returns
// the original result without burning a revision on the replay.
func TestCommandSnapshotReplayInsideWrite(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedItem(t, s, f)

	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutCommand(ctx, f.command) }); err != nil {
		t.Fatalf("accept command: %v", err)
	}
	committed, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}

	var replay store.Snapshot
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutCommand(ctx, f.command); err != nil {
			return err
		}
		var err error
		_, replay, err = tx.GetCommandSnapshot(ctx, f.command.CommandID)
		if err != nil {
			return err
		}
		return errAbandonReplay
	})
	if !errors.Is(err, errAbandonReplay) {
		t.Fatalf("abandoned Write error = %v, want errAbandonReplay", err)
	}
	if replay.AsOfRevision != committed.Revision {
		t.Errorf("replay read as_of_revision = %d, want the original %d", replay.AsOfRevision, committed.Revision)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if after.Revision != committed.Revision {
		t.Errorf("abandoned replay moved the revision: %d then %d", committed.Revision, after.Revision)
	}
}
