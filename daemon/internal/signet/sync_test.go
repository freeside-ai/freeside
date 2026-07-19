package signet_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestBootstrapReconstructsInboxAfterMissedNotifications is §5.14 test 3:
// bootstrap needs no notification cursor or event history; the canonical
// store snapshot alone reconstructs every current inbox item.
func TestBootstrapReconstructsInboxAfterMissedNotifications(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	second := f.item
	second.ID = "item-2"
	second.Reason = "a second decision arrived while the client was offline"
	if err := f.service.PutItem(ctx, second); err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if bootstrap.SyncEpoch == "" || bootstrap.Revision != f.revision(t) {
		t.Errorf("bootstrap state = %q/%d, want current non-empty epoch/revision",
			bootstrap.SyncEpoch, bootstrap.Revision)
	}
	if len(bootstrap.AttentionItems) != 2 ||
		bootstrap.AttentionItems[0].Item.ID != "item-1" ||
		bootstrap.AttentionItems[1].Item.ID != "item-2" {
		t.Fatalf("bootstrap items = %+v, want item-1 and item-2 in canonical order", bootstrap.AttentionItems)
	}
	if bootstrap.AttentionDeliveries == nil || bootstrap.Runs == nil || bootstrap.Conversations == nil {
		t.Fatal("empty bootstrap collections must encode as [] rather than null")
	}
	for _, item := range bootstrap.AttentionItems {
		if item.EntityVersion < 1 || item.AsOfRevision < 1 || item.AsOfRevision > bootstrap.Revision {
			t.Errorf("item %q metadata = v%d/r%d outside bootstrap revision %d",
				item.Item.ID, item.EntityVersion, item.AsOfRevision, bootstrap.Revision)
		}
	}
}

// TestBootstrapProjectsOneTransactionalSnapshot covers #66 acceptance 4 at
// the service boundary. The store's permanent concurrent-write test proves
// isolation; this pins that signet reads ServerState and all four collections
// through that one callback and preserves every row's stamped metadata.
func TestBootstrapProjectsOneTransactionalSnapshot(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedSyncResources(t, f)

	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if len(bootstrap.AttentionItems) != 1 || len(bootstrap.AttentionDeliveries) != 1 ||
		len(bootstrap.Runs) != 1 || len(bootstrap.Conversations) != 1 {
		t.Fatalf("bootstrap collection sizes = %d/%d/%d/%d, want 1/1/1/1",
			len(bootstrap.AttentionItems), len(bootstrap.AttentionDeliveries),
			len(bootstrap.Runs), len(bootstrap.Conversations))
	}
	for name, snapshot := range map[string]struct {
		entityVersion int64
		asOfRevision  int64
	}{
		"item":         {bootstrap.AttentionItems[0].EntityVersion, bootstrap.AttentionItems[0].AsOfRevision},
		"delivery":     {bootstrap.AttentionDeliveries[0].EntityVersion, bootstrap.AttentionDeliveries[0].AsOfRevision},
		"run":          {bootstrap.Runs[0].EntityVersion, bootstrap.Runs[0].AsOfRevision},
		"conversation": {bootstrap.Conversations[0].EntityVersion, bootstrap.Conversations[0].AsOfRevision},
	} {
		if snapshot.entityVersion < 1 || snapshot.asOfRevision < 1 || snapshot.asOfRevision > bootstrap.Revision {
			t.Errorf("%s metadata = v%d/r%d outside bootstrap revision %d",
				name, snapshot.entityVersion, snapshot.asOfRevision, bootstrap.Revision)
		}
	}
}

// TestRestoreForcesFreshBootstrap is §5.14 test 8's server half, driven by the
// real checkpoint/restore path (#165) rather than a bare epoch hook: a restore
// atomically rolls the database back to the checkpoint and rotates the sync
// epoch. The rollback regresses the item version and the revision below what a
// client cached from the advanced world; the epoch change is what invalidates
// that client's cursor even though its cached revision is now the higher one.
// The eviction itself is client-side (SyncCoordinator/DecisionModel, #162);
// this pins that the server exposes a fresh epoch over regressed state through
// the real restore, not through store.NewEpoch.
func TestRestoreForcesFreshBootstrap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	// Checkpoint the seeded state (item-1 at version 1), then advance past it.
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.db")
	if err := f.store.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	atCheckpoint, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision at checkpoint: %v", err)
	}

	advanced := f.item
	advanced.ItemVersion = 2
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, advanced)
	}); err != nil {
		t.Fatalf("advance item: %v", err)
	}
	// The state a client caches from the advanced world.
	cached, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision after advance: %v", err)
	}
	if cached.Revision <= atCheckpoint.Revision {
		t.Fatalf("advance did not move revision %d -> %d", atCheckpoint.Revision, cached.Revision)
	}

	// Restore: data rolls back and the epoch rotates in one operation.
	if _, err := f.store.Restore(ctx, checkpoint); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	after, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision after restore: %v", err)
	}
	// The epoch the cached client holds is now stale: it must discard and
	// bootstrap regardless of its (higher) cached revision.
	if after.SyncEpoch == cached.SyncEpoch {
		t.Fatalf("epoch stayed %q across restore; a cached cursor would never evict", cached.SyncEpoch)
	}
	// Revision legitimately regressed to the checkpoint under the new epoch;
	// revisions compare only within an epoch, so the lower value is unambiguous.
	if after.Revision != atCheckpoint.Revision {
		t.Fatalf("restore revision = %d, want checkpoint revision %d", after.Revision, atCheckpoint.Revision)
	}
	if after.Revision >= cached.Revision {
		t.Fatalf("restore revision %d did not regress below the cached %d", after.Revision, cached.Revision)
	}

	// The canonical bootstrap carries the new epoch and the regressed item.
	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if bootstrap.SyncEpoch != after.SyncEpoch || bootstrap.Revision != after.Revision {
		t.Errorf("bootstrap state = %q/%d, heartbeat = %q/%d",
			bootstrap.SyncEpoch, bootstrap.Revision, after.SyncEpoch, after.Revision)
	}
	if len(bootstrap.AttentionItems) != 1 ||
		bootstrap.AttentionItems[0].Item.ItemVersion != 1 ||
		bootstrap.AttentionItems[0].EntityVersion != 1 {
		t.Fatalf("bootstrap item = %+v, want item-1 regressed to version 1 / entity_version 1", bootstrap.AttentionItems)
	}
}

func seedSyncResources(t *testing.T, f fixture) {
	t.Helper()
	ctx := context.Background()
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	run := domain.Run{
		ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		Stages: []domain.Stage{{ID: "stage-1", RunID: "run-1", Name: "implementation"}},
	}
	conversation := domain.Conversation{ID: "conv-1", Status: domain.ConversationIdle}
	delivery := domain.AttentionDelivery{
		ItemID: f.item.ID, DeviceID: "device-1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: ts, Status: domain.DeliverySubmitted,
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutConversation(ctx, conversation); err != nil {
			return err
		}
		return tx.PutAttentionDelivery(ctx, delivery)
	}); err != nil {
		t.Fatalf("seed sync resources: %v", err)
	}
}
