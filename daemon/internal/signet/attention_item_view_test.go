package signet_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// recordCardOpened records a card_opened event from device for the fixture
// item bound to digest.
func recordCardOpened(
	t *testing.T, f fixture, device domain.DeviceID, eventID string, digest domain.Digest,
) domain.ComprehensionEvent {
	t.Helper()
	return recordCardOpenedOn(t, f.service, f.item.ID, device, eventID, digest, *f.now)
}

// recordCardOpenedOn records a card_opened event for any item through service.
func recordCardOpenedOn(
	t *testing.T, service *signet.Service, itemID domain.ItemID, device domain.DeviceID,
	eventID string, digest domain.Digest, at time.Time,
) domain.ComprehensionEvent {
	t.Helper()
	event, err := service.RecordComprehensionEvent(context.Background(), domain.ComprehensionEventInput{
		DeviceID: device, EventID: eventID, ItemID: itemID,
		Kind: domain.ComprehensionCardOpened, ItemDecisionSurfaceDigest: digest,
		OccurredAt: at, Sequence: 1,
	})
	if err != nil {
		t.Fatalf("RecordComprehensionEvent %s: %v", eventID, err)
	}
	return event
}

// servedItem returns the fixture item as GetAttentionItem serves it.
func servedItem(t *testing.T, f fixture) signet.AttentionItemSnapshot {
	t.Helper()
	snapshot, err := f.service.GetAttentionItem(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("GetAttentionItem: %v", err)
	}
	return snapshot
}

// TestRecordCardOpenedRecordsFirstView covers the viewed mark's write and read
// paths (issue #2930): the first card_opened for the item's current surface
// moves the revision once and every read serves its received_at, at that
// revision, with entity_version unchanged; a second device opening the same
// surface moves nothing and keeps the first time; the stored item body never
// carries the value.
func TestRecordCardOpenedRecordsFirstView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	f.seedDevice(t, "device-2")
	digest := servedItem(t, f).Item.DecisionSurface.Digest

	before := servedItem(t, f)
	if before.Item.SurfaceFirstOpenedAt != nil {
		t.Fatalf("unopened item serves surface_first_opened_at %v, want nil", before.Item.SurfaceFirstOpenedAt)
	}
	revision := f.revision(t)
	first := recordCardOpened(t, f, f.device.ID, "event-1", digest)
	if got := f.revision(t); got != revision+1 {
		t.Fatalf("first card_opened moved revision %d -> %d, want one bump", revision, got)
	}
	viewed := servedItem(t, f)
	if viewed.Item.SurfaceFirstOpenedAt == nil || !viewed.Item.SurfaceFirstOpenedAt.Equal(first.ReceivedAt) {
		t.Fatalf("served surface_first_opened_at = %v, want %v", viewed.Item.SurfaceFirstOpenedAt, first.ReceivedAt)
	}
	if viewed.AsOfRevision != revision+1 || viewed.EntityVersion != before.EntityVersion {
		t.Fatalf("viewed snapshot at revision %d entity %d, want revision %d entity %d",
			viewed.AsOfRevision, viewed.EntityVersion, revision+1, before.EntityVersion)
	}

	listed, err := f.service.ListAttentionItems(ctx)
	if err != nil {
		t.Fatalf("ListAttentionItems: %v", err)
	}
	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for name, snapshots := range map[string][]signet.AttentionItemSnapshot{
		"list": listed, "bootstrap": bootstrap.AttentionItems,
	} {
		if len(snapshots) != 1 || snapshots[0].Item.SurfaceFirstOpenedAt == nil ||
			!snapshots[0].Item.SurfaceFirstOpenedAt.Equal(first.ReceivedAt) {
			t.Fatalf("%s serves %+v, want the item viewed at %v", name, snapshots, first.ReceivedAt)
		}
	}

	*f.now = f.now.Add(time.Hour)
	recordCardOpened(t, f, "device-2", "event-2", digest)
	if got := f.revision(t); got != revision+1 {
		t.Fatalf("repeat card_opened moved revision %d -> %d, want unchanged", revision+1, got)
	}
	if again := servedItem(t, f); !again.Item.SurfaceFirstOpenedAt.Equal(first.ReceivedAt) {
		t.Fatalf("repeat open moved surface_first_opened_at to %v, want %v", again.Item.SurfaceFirstOpenedAt, first.ReceivedAt)
	}

	stored, _ := f.itemSnapshot(t)
	if stored.SurfaceFirstOpenedAt != nil {
		t.Fatalf("stored item body carries surface_first_opened_at %v, want nil", stored.SurfaceFirstOpenedAt)
	}
}

// TestSurfaceRevisionResetsView: a new decision surface serves nil until its
// own card is opened, and an event bound to an older surface records a view
// for that surface that the item's projection ignores.
func TestSurfaceRevisionResetsView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	oldDigest := servedItem(t, f).Item.DecisionSurface.Digest
	recordCardOpened(t, f, f.device.ID, "event-1", oldDigest)

	advanced, _ := f.itemSnapshot(t)
	advanced.ItemVersion++
	advanced.PRHeadSHA = "deadbeef"
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, advanced)
	}); err != nil {
		t.Fatalf("advance item: %v", err)
	}
	revised := servedItem(t, f)
	newDigest := revised.Item.DecisionSurface.Digest
	if newDigest == oldDigest {
		t.Fatal("advancing pr_head_sha kept the decision surface digest")
	}
	if revised.Item.SurfaceFirstOpenedAt != nil {
		t.Fatalf("revised surface serves surface_first_opened_at %v, want nil", revised.Item.SurfaceFirstOpenedAt)
	}

	recordCardOpened(t, f, f.device.ID, "event-stale", oldDigest)
	if stale := servedItem(t, f); stale.Item.SurfaceFirstOpenedAt != nil {
		t.Fatalf("an open of the old surface marked the new one viewed at %v", stale.Item.SurfaceFirstOpenedAt)
	}

	opened := recordCardOpened(t, f, f.device.ID, "event-2", newDigest)
	if viewed := servedItem(t, f); viewed.Item.SurfaceFirstOpenedAt == nil ||
		!viewed.Item.SurfaceFirstOpenedAt.Equal(opened.ReceivedAt) {
		t.Fatalf("new surface serves %v, want %v", viewed.Item.SurfaceFirstOpenedAt, opened.ReceivedAt)
	}
}

// TestMalformedViewServesUnviewed: an unreadable view row is a display hint
// gone bad, so the item still serves, as unviewed, on every read path, and a
// later open of the same surface still records.
func TestMalformedViewServesUnviewed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	digest := servedItem(t, f).Item.DecisionSurface.Digest
	recordCardOpened(t, f, f.device.ID, "event-1", digest)

	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE attention_item_views SET first_opened_at = 'not a time'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if served := servedItem(t, f); served.Item.SurfaceFirstOpenedAt != nil {
		t.Fatalf("malformed view serves %v, want nil", served.Item.SurfaceFirstOpenedAt)
	}
	listed, err := f.service.ListAttentionItems(ctx)
	if err != nil || len(listed) != 1 || listed[0].Item.SurfaceFirstOpenedAt != nil {
		t.Fatalf("ListAttentionItems = %+v, %v; want the item unviewed", listed, err)
	}
	bootstrap, err := f.service.Bootstrap(ctx)
	if err != nil || len(bootstrap.AttentionItems) != 1 || bootstrap.AttentionItems[0].Item.SurfaceFirstOpenedAt != nil {
		t.Fatalf("Bootstrap = %+v, %v; want the item unviewed", bootstrap.AttentionItems, err)
	}
	revision := f.revision(t)
	recordCardOpened(t, f, f.device.ID, "event-2", digest)
	if got := f.revision(t); got != revision {
		t.Fatalf("open over a malformed view moved revision %d -> %d, want unchanged", revision, got)
	}
}

// TestConflictReplacementCarriesView: the item a 409 hands back agrees with
// what sync serves for it.
func TestConflictReplacementCarriesView(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	opened := recordCardOpened(t, f, f.device.ID, "event-1", servedItem(t, f).Item.DecisionSurface.Digest)

	// A non-surface change moves entity_version and keeps the digest, so the
	// command below is stale against an item whose surface is viewed.
	advanced, _ := f.itemSnapshot(t)
	advanced.ItemVersion++
	advanced.Reason = "canonical state changed"
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, advanced)
	}); err != nil {
		t.Fatalf("advance item: %v", err)
	}
	_, err := f.service.Submit(ctx, f.command("cmd-stale", domain.ActionStop))
	var stale *signet.StaleVersionError
	if !errors.As(err, &stale) {
		t.Fatalf("Submit = %v, want a StaleVersionError", err)
	}
	if stale.Replacement.SurfaceFirstOpenedAt == nil || !stale.Replacement.SurfaceFirstOpenedAt.Equal(opened.ReceivedAt) {
		t.Fatalf("replacement surface_first_opened_at = %v, want %v",
			stale.Replacement.SurfaceFirstOpenedAt, opened.ReceivedAt)
	}
	if served := servedItem(t, f); stale.Snapshot != (store.Snapshot{AsOfRevision: served.AsOfRevision, EntityVersion: served.EntityVersion}) {
		t.Fatalf("replacement snapshot = %+v, want the served %d/%d", stale.Snapshot, served.AsOfRevision, served.EntityVersion)
	}
}

// TestProposalFactsMatchViewedItem covers the card's facts check after a view
// (issue #2930): the decision card enables a proposal's actions only when the
// facts' as_of_revision equals the served item's, so once a first open raises
// the item's revision the task- and effect-proposal facts must carry it too.
func TestProposalFactsMatchViewedItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	task := newProposalDecisionFixture(t)
	unviewed, err := task.service.GetAttentionItem(ctx, task.item.ID)
	if err != nil {
		t.Fatalf("GetAttentionItem(task proposal): %v", err)
	}
	recordCardOpenedOn(t, task.service, task.item.ID, task.device.ID, "task-open",
		unviewed.Item.DecisionSurface.Digest, *task.now)
	served, err := task.service.GetAttentionItem(ctx, task.item.ID)
	if err != nil {
		t.Fatalf("GetAttentionItem(task proposal): %v", err)
	}
	if served.Item.SurfaceFirstOpenedAt == nil {
		t.Fatal("task proposal served unviewed after its first open")
	}
	taskFacts, err := task.service.GetTaskProposalFacts(ctx, task.item.ID)
	if err != nil {
		t.Fatalf("GetTaskProposalFacts: %v", err)
	}
	if taskFacts.AsOfRevision != served.AsOfRevision || taskFacts.EntityVersion != served.EntityVersion {
		t.Fatalf("task facts at (%d, %d), served item at (%d, %d)",
			taskFacts.AsOfRevision, taskFacts.EntityVersion, served.AsOfRevision, served.EntityVersion)
	}

	effect := newClosureFixture(t, true, domain.ClosureFlagOriginDaemonFallback)
	notice := effect.openNotice(t, effect.merge)
	unviewedNotice, err := effect.service.GetAttentionItem(ctx, notice.ID)
	if err != nil {
		t.Fatalf("GetAttentionItem(effect proposal): %v", err)
	}
	recordCardOpenedOn(t, effect.service, notice.ID, effect.device.ID, "effect-open",
		unviewedNotice.Item.DecisionSurface.Digest, *effect.now)
	servedNotice, err := effect.service.GetAttentionItem(ctx, notice.ID)
	if err != nil {
		t.Fatalf("GetAttentionItem(effect proposal): %v", err)
	}
	if servedNotice.Item.SurfaceFirstOpenedAt == nil {
		t.Fatal("effect proposal served unviewed after its first open")
	}
	effectFacts, err := effect.service.GetEffectProposalFacts(ctx, notice.ID)
	if err != nil {
		t.Fatalf("GetEffectProposalFacts: %v", err)
	}
	if effectFacts.AsOfRevision != servedNotice.AsOfRevision ||
		effectFacts.EntityVersion != servedNotice.EntityVersion {
		t.Fatalf("effect facts at (%d, %d), served item at (%d, %d)",
			effectFacts.AsOfRevision, effectFacts.EntityVersion,
			servedNotice.AsOfRevision, servedNotice.EntityVersion)
	}
}
