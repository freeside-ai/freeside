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
