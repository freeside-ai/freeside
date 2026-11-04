package store_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestRevisionChangedClosesOnEveryCursorMove pins which commits wake the
// revision long-poll: each path that moves the sync revision or epoch closes
// the channel a waiter took before it, and a non-bumping WriteInternal does
// not, so a client's own bookkeeping cannot wake its wait.
func TestRevisionChangedClosesOnEveryCursorMove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openRawStore(t, store.Options{})
	checkpoint := filepath.Join(t.TempDir(), "checkpoint.db")
	if err := s.Checkpoint(ctx, checkpoint); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	internal := s.RevisionChanged()
	if err := s.WriteInternal(ctx, func(*store.InternalTx) error { return nil }); err != nil {
		t.Fatalf("WriteInternal: %v", err)
	}
	select {
	case <-internal:
		t.Fatal("WriteInternal woke a revision waiter")
	default:
	}

	for name, move := range map[string]func() error{
		"Write": func() error {
			return s.Write(ctx, func(*store.WriteTx) error { return nil })
		},
		"NewEpoch": func() error { _, err := s.NewEpoch(ctx); return err },
		"Restore":  func() error { _, err := s.Restore(ctx, checkpoint); return err },
	} {
		changed := s.RevisionChanged()
		if err := move(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		select {
		case <-changed:
		default:
			t.Fatalf("%s did not wake a revision waiter", name)
		}
	}
}
