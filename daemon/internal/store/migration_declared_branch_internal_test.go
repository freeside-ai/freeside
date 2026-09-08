package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestDeclaredBranchMigrationPreservesV2AndRequiresV3(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0068_")
	intent := publicationrecord.Intent{
		FormatVersion: publicationrecord.IntentFormatHistory,
		Identity:      domain.Digest("sha256:" + strings.Repeat("a", 64)),
		InvocationID:  "v2", Repo: "owner/repo", BaseRef: "main", SourceHeadSHA: "head",
		AuthorizationID: domain.Digest("sha256:" + strings.Repeat("b", 64)),
	}
	payload, err := intent.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, enqueueOutboxSQL, "v2", readyPublicationIntentKind, payload, 2, contentaddr.Sum(payload), "2026-09-08T02:00:00Z"); err != nil {
		t.Fatal(err)
	}
	s := &Store{db: db}
	read := func() QueueEntry {
		t.Helper()
		var entry QueueEntry
		if err := s.Read(ctx, func(tx *ReadTx) error { var err error; entry, err = tx.GetOutbox(ctx, "v2"); return err }); err != nil {
			t.Fatal(err)
		}
		return entry
	}
	before := read()
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if after := read(); !reflect.DeepEqual(before, after) {
		t.Fatalf("migration rewrote old row: before %+v, after %+v", before, after)
	}
	if _, err := db.ExecContext(ctx, enqueueOutboxSQL, "new-v2", readyPublicationIntentKind, payload, 2, contentaddr.Sum(payload), "2026-09-08T02:00:00Z"); err == nil {
		t.Fatal("new legacy row accepted")
	}
	intent.FormatVersion = publicationrecord.IntentFormatCurrent
	intent.Branch = "feat/new-task"
	intent.InvocationID = "v3"
	payload, err = intent.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		entry, inserted, err := tx.EnqueueOutbox(ctx, "v3", readyPublicationIntentKind, payload)
		if err == nil && (!inserted || entry.PayloadVersion != 3) {
			t.Fatalf("new row = %+v, inserted %t", entry, inserted)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, "UPDATE outbox SET payload_version=2 WHERE idempotency_key='v3'"); err == nil {
		t.Fatal("v3 downgrade accepted")
	}
}
