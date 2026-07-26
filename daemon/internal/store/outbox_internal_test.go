package store

import (
	"context"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestGetOutboxRejectsUnknownStatus(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := &Store{db: db}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, "intent-1", "publish", []byte(`{}`))
		return err
	}); err != nil {
		t.Fatalf("enqueue outbox: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE outbox SET status = 'foreign' WHERE idempotency_key = 'intent-1'`,
	); err != nil {
		t.Fatalf("corrupt status: %v", err)
	}
	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetOutbox(ctx, "intent-1")
		return err
	})
	if err == nil || !strings.Contains(err.Error(), `invalid status "foreign"`) {
		t.Fatalf("GetOutbox error = %v, want invalid status", err)
	}
	err = s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, "intent-1", "publish", []byte(`{}`))
		return err
	})
	if err == nil || !strings.Contains(err.Error(), `stored status "foreign" is invalid`) {
		t.Fatalf("EnqueueOutbox error = %v, want invalid status", err)
	}
}

// TestPromoteOutboxRefusesQuarantinedRow: quarantine preserves an intent whose
// authority can no longer be reconstructed (migration 0012). Promotion must not
// resurrect one by settling a new payload onto it, so the pending guard covers
// quarantined as well as dispatched.
func TestPromoteOutboxRefusesQuarantinedRow(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := &Store{db: db}
	placeholder := []byte(`{"reserved":true}`)
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, "intent-1", "reservation", placeholder)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE outbox SET status = 'quarantined' WHERE idempotency_key = 'intent-1'`,
	); err != nil {
		t.Fatalf("quarantine: %v", err)
	}

	var entry QueueEntry
	var promoted bool
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		var err error
		entry, promoted, err = tx.PromoteOutbox(
			ctx, "intent-1", "reservation", "intent", placeholder, []byte(`{"intent":true}`))
		return err
	}); err != nil {
		t.Fatalf("PromoteOutbox: %v", err)
	}
	if promoted {
		t.Error("promoted = true, want false")
	}
	if !entry.Quarantined() || entry.Kind != "reservation" {
		t.Errorf("row = kind %q status %q, want the quarantined reservation untouched",
			entry.Kind, entry.Status)
	}
}
