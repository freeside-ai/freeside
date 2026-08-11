package publish_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestStoreLedgerRecordsIntent drives an intent through the production
// outbox path (issue #82): the intent lands on the store-owned outbox
// and Record reports it recorded.
func TestStoreLedgerRecordsIntent(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}

	prior, recorded, err := ledger.Record(context.Background(), key, publish.IntentKindPublication, payload, nil)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !recorded {
		t.Error("first Record recorded = false, want true")
	}
	if !bytes.Equal(prior, payload) {
		t.Errorf("first Record prior = %q, want the recorded payload", prior)
	}
}

// TestStoreLedgerConverges is the effectively-once contract at the store
// boundary: a second Record under the same key (a retried invocation)
// returns the original payload with recorded false and writes nothing
// new, so a retry converges on the one committed intent instead of
// minting a second.
func TestStoreLedgerConverges(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	original, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Record(context.Background(), key, publish.IntentKindPublication, original, nil); err != nil {
		t.Fatalf("first Record: %v", err)
	}

	// A retry under the same key with a different payload must not
	// overwrite: the port returns the committed intent, recorded false.
	prior, recorded, err := ledger.Record(context.Background(), key, publish.IntentKindPublication, []byte(`{"retry":true}`), nil)
	if err != nil {
		t.Fatalf("second Record: %v", err)
	}
	if recorded {
		t.Error("second Record recorded = true, want false (converged on the original)")
	}
	if !bytes.Equal(prior, original) {
		t.Errorf("second Record prior = %q, want the original intent", prior)
	}
}

func TestStoreLedgerAuthenticatesCommittedIntentFormat(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	s, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatal(err)
	}
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Record(ctx, key, publish.IntentKindPublication, payload, nil); err != nil {
		t.Fatal(err)
	}

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(
		ctx, "UPDATE outbox SET payload_version = 1 WHERE idempotency_key = ?", key,
	); err == nil || !strings.Contains(err.Error(), "cannot be downgraded") {
		_ = raw.Close()
		t.Fatalf("payload-version downgrade = %v, want schema refusal", err)
	}
	var changed []byte
	if err := raw.QueryRowContext(
		ctx,
		`UPDATE outbox
		 SET payload = CAST(json_set(payload, '$.format_version', 1) AS BLOB)
		 WHERE idempotency_key = ?
		 RETURNING payload`,
		key,
	).Scan(&changed); err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	_, digestErr := raw.ExecContext(
		ctx, "UPDATE outbox SET payload_digest = ? WHERE idempotency_key = ?",
		contentaddr.Sum(changed), key,
	)
	closeErr := raw.Close()
	if digestErr != nil || closeErr != nil {
		t.Fatal(errors.Join(digestErr, closeErr))
	}

	if _, _, err := ledger.Record(
		ctx, key, publish.IntentKindPublication, payload, nil,
	); err == nil || !strings.Contains(err.Error(), "disagrees with outbox format") {
		t.Fatalf("Record with payload-declared legacy row = %v, want format mismatch", err)
	}
}

// TestStoreLedgerRejectsForeignKind: the outbox is unique by key alone,
// so a row under the requested key with another kind is not a durable
// publication intent even when its payload bytes match. Record must fail
// closed instead of allowing an external effect with no recoverable intent.
func TestStoreLedgerRejectsForeignKind(t *testing.T) {
	t.Parallel()
	s := newTestStore(t)
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	key, err := publish.IntentKey("inv-0001", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Record(context.Background(), key, "foreign.kind", payload, nil); err != nil {
		t.Fatalf("seed foreign kind: %v", err)
	}
	if _, _, err := ledger.Record(context.Background(), key, publish.IntentKindPublication, payload, nil); err == nil {
		t.Fatal("Record accepted a foreign-kind row under the publication key")
	}
}

// TestNewStoreLedgerNilStore fails closed at construction: a nil store
// must error there, not at the first Record.
func TestNewStoreLedgerNilStore(t *testing.T) {
	t.Parallel()
	if _, err := publish.NewStoreLedger(nil); err == nil {
		t.Error("NewStoreLedger(nil) accepted, want error")
	}
}
