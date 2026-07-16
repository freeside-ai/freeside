package signet_test

import (
	"context"
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

// TestNewEpochForcesFreshBootstrap is §5.14 test 8's server half: a restore
// changes the epoch without making an old revision cursor meaningful in the
// new world. The next heartbeat exposes the mismatch and the canonical
// bootstrap carries the new epoch.
func TestNewEpochForcesFreshBootstrap(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	before, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision before restore: %v", err)
	}
	if _, err := f.store.NewEpoch(ctx); err != nil {
		t.Fatalf("NewEpoch: %v", err)
	}
	after, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision after restore: %v", err)
	}
	if after.SyncEpoch == before.SyncEpoch {
		t.Fatalf("epoch stayed %q across restore", before.SyncEpoch)
	}
	if after.Revision != before.Revision {
		t.Errorf("restore moved revision %d -> %d; epoch change is the invalidation", before.Revision, after.Revision)
	}
	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if bootstrap.SyncEpoch != after.SyncEpoch || bootstrap.Revision != after.Revision {
		t.Errorf("bootstrap state = %q/%d, heartbeat = %q/%d",
			bootstrap.SyncEpoch, bootstrap.Revision, after.SyncEpoch, after.Revision)
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
