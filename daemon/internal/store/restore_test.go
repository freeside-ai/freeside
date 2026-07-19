package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestCheckpointRestoreRotatesEpochAndRollsBackData drives the real §5.14 /
// §5.10 restore: checkpoint a state, advance past it, then restore. The
// checkpoint's data (and revision) come back, post-checkpoint rows are gone,
// and the sync_epoch is fresh in the same operation. The item->conversation
// foreign key exercises the deferred-FK copy (alphabetical order inserts
// attention_items before conversations).
func TestCheckpointRestoreRotatesEpochAndRollsBackData(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})

	convID := domain.ConversationID("conv-1")
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, domain.Conversation{ID: convID, Status: domain.ConversationIdle}); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, newItem(t, "item-1", &convID, 1))
	}); err != nil {
		t.Fatalf("seed checkpoint state: %v", err)
	}

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState before checkpoint: %v", err)
	}
	beforeVersion := itemVersion(t, s, "item-1")

	checkpoint := filepath.Join(t.TempDir(), "checkpoint.db")
	if err := s.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Advance past the checkpoint: bump item-1 (entity_version climbs) and add
	// item-2, each a client-visible write that moves the revision.
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, newItem(t, "item-1", &convID, 2))
	}); err != nil {
		t.Fatalf("advance item-1: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, newItem(t, "item-2", &convID, 1))
	}); err != nil {
		t.Fatalf("add item-2: %v", err)
	}
	advanced, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState after advance: %v", err)
	}
	if advanced.Revision <= before.Revision {
		t.Fatalf("advance did not move revision %d -> %d", before.Revision, advanced.Revision)
	}

	restored, err := s.Restore(ctx, checkpoint)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// Epoch rotated to a fresh value, atomically with the data copy.
	if restored.SyncEpoch == "" || restored.SyncEpoch == before.SyncEpoch {
		t.Fatalf("restore epoch = %q (before %q), want a fresh non-empty epoch", restored.SyncEpoch, before.SyncEpoch)
	}
	// Revision rolled back to the checkpoint under the new epoch.
	if restored.Revision != before.Revision {
		t.Fatalf("restore revision = %d, want checkpoint revision %d", restored.Revision, before.Revision)
	}
	// The heartbeat surface reports the same restored state.
	current, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState after restore: %v", err)
	}
	if current != restored {
		t.Fatalf("ServerState after restore = %+v, want %+v", current, restored)
	}
	// item-1 regressed to its checkpoint version; item-2 (post-checkpoint) is gone.
	if got := itemVersion(t, s, "item-1"); got != beforeVersion {
		t.Fatalf("item-1 entity_version after restore = %d, want checkpoint version %d", got, beforeVersion)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, getErr := tx.GetAttentionItem(ctx, "item-2")
		if getErr == nil {
			t.Fatal("item-2 survived restore, want it dropped as post-checkpoint state")
		}
		return nil
	}); err != nil {
		t.Fatalf("Read item-2: %v", err)
	}
}

// TestRestoreLeavesTheConnectionClean guards the connection-state cleanup: a
// restore toggles foreign_keys and attaches the checkpoint on the store's
// single pooled connection, and must leave neither leaked. After a restore,
// foreign_keys is back on (a leak would silently disable the store's
// fail-closed FK posture) and a second checkpoint/restore succeeds (a leaked
// attachment would fail the next ATTACH).
func TestRestoreLeavesTheConnectionClean(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, newItem(t, "item-1", nil, 1))
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	first := filepath.Join(t.TempDir(), "first.db")
	if err := s.Checkpoint(ctx, first); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, err := s.Restore(ctx, first); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	pragmas, err := s.Pragmas(ctx)
	if err != nil {
		t.Fatalf("Pragmas: %v", err)
	}
	if !pragmas.ForeignKeys {
		t.Fatal("restore left foreign_keys disabled on the pooled connection")
	}

	// A second checkpoint/restore proves the first restore detached its
	// checkpoint: a leaked attachment would fail this ATTACH.
	second := filepath.Join(t.TempDir(), "second.db")
	if err := s.Checkpoint(ctx, second); err != nil {
		t.Fatalf("second Checkpoint: %v", err)
	}
	if _, err := s.Restore(ctx, second); err != nil {
		t.Fatalf("second Restore: %v", err)
	}
}

// TestCheckpointFileIsOwnerOnly: a checkpoint carries device credentials and
// pairing rows and is a portable artifact, so it must be owner-only on disk
// rather than rely on its parent directory's mode (VACUUM INTO otherwise
// honours the umask, e.g. 0644).
func TestCheckpointFileIsOwnerOnly(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	if err := s.Checkpoint(ctx, path); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat checkpoint: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("checkpoint file mode = %04o, want 0600", perm)
	}
}

// itemVersion reads the persisted §5.14 entity_version for an item.
func itemVersion(t *testing.T, s *store.Store, id domain.ItemID) int64 {
	t.Helper()
	var version int64
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		_, snap, err := tx.GetAttentionItemSnapshot(context.Background(), id)
		version = snap.EntityVersion
		return err
	}); err != nil {
		t.Fatalf("read %q version: %v", id, err)
	}
	return version
}

// newItem builds a minimal valid open attention item; itemVersion controls the
// domain ItemVersion so an advance is a real transition.
func newItem(t *testing.T, id domain.ItemID, conv *domain.ConversationID, itemVersion int) domain.AttentionItem {
	t.Helper()
	runID := domain.RunID("run-1")
	expires := time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss},
		PRHeadSHA:         "cafebabe", ItemVersion: itemVersion,
		InterruptionClass: domain.InterruptionPlannedGate,
		ExpiresWhen:       &expires, Status: domain.StatusOpen,
		ConversationID: conv,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem(%q): %v", id, err)
	}
	return item
}
