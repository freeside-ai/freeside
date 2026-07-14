package store_test

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestWriteIncrementsRevisionOnce is acceptance fixture 5: one client-visible
// write transaction increments ServerState.revision exactly once, and the
// internal write path does not increment it at all.
func TestWriteIncrementsRevisionOnce(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if before.Revision != 0 {
		t.Fatalf("fresh revision = %d, want 0", before.Revision)
	}
	if before.SyncEpoch == "" {
		t.Fatal("fresh store has an empty sync_epoch, want Open to seed one")
	}

	// Multiple statements inside one Write are one client-visible
	// transaction: exactly one bump. (The entity round-trip test does the
	// same with multiple Puts.)
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		for range 2 {
			if _, err := tx.ServerState(ctx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if after.Revision != before.Revision+1 {
		t.Fatalf("revision after one Write = %d, want %d", after.Revision, before.Revision+1)
	}

	if err := s.WriteInternal(ctx, func(tx *store.WriteTx) error { return nil }); err != nil {
		t.Fatalf("WriteInternal: %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error { return nil }); err != nil {
		t.Fatalf("Read: %v", err)
	}
	final, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if final.Revision != after.Revision {
		t.Fatalf("revision after WriteInternal and Read = %d, want unchanged %d", final.Revision, after.Revision)
	}
}

// TestNewEpoch is the fixture-5 epoch path: a bump issues a fresh epoch and
// leaves the revision alone (the epoch change itself invalidates cursors).
func TestNewEpoch(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	after, err := s.NewEpoch(ctx)
	if err != nil {
		t.Fatalf("NewEpoch: %v", err)
	}
	if after.SyncEpoch == "" || after.SyncEpoch == before.SyncEpoch {
		t.Fatalf("NewEpoch produced %q from %q, want a fresh non-empty epoch", after.SyncEpoch, before.SyncEpoch)
	}
	if after.Revision != before.Revision {
		t.Fatalf("NewEpoch changed revision %d -> %d, want unchanged", before.Revision, after.Revision)
	}
}

// TestEpochSurvivesReopen: Open seeds an epoch once and never overwrites an
// established one.
func TestEpochSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)
	s, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	first, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	second, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if second.SyncEpoch != first.SyncEpoch {
		t.Fatalf("epoch changed across reopen %q -> %q, want stable", first.SyncEpoch, second.SyncEpoch)
	}
}
