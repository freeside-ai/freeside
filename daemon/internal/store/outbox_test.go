package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestQueueRejectsEmptyIdentity: an empty idempotency key would collapse
// unrelated actions onto one row, and an empty kind is unroutable; both are
// rejected before touching the table.
func TestQueueRejectsEmptyIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	cases := []struct {
		name string
		key  string
		kind string
	}{
		{"empty key", "", "AgentInvocationRequested"},
		{"empty kind", "cmd-1", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				if _, _, err := tx.EnqueueOutbox(ctx, tc.key, tc.kind, nil); err == nil {
					t.Error("EnqueueOutbox accepted an empty identity, want error")
				}
				if _, _, err := tx.RecordInbox(ctx, tc.key, tc.kind, nil); err == nil {
					t.Error("RecordInbox accepted an empty identity, want error")
				}
				return nil
			})
			if err != nil {
				t.Fatalf("WriteInternal: %v", err)
			}
		})
	}
}

// TestQueueIdempotency is acceptance fixture 4: a duplicate insert under the
// same idempotency key returns the original row and creates no second row,
// for the outbox and the inbox alike.
func TestQueueIdempotency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	cases := []struct {
		name   string
		record func(tx *store.InternalTx, key, kind string, payload []byte) (store.QueueEntry, bool, error)
	}{
		{"outbox", func(tx *store.InternalTx, key, kind string, payload []byte) (store.QueueEntry, bool, error) {
			return tx.EnqueueOutbox(ctx, key, kind, payload)
		}},
		{"inbox", func(tx *store.InternalTx, key, kind string, payload []byte) (store.QueueEntry, bool, error) {
			return tx.RecordInbox(ctx, key, kind, payload)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, store.Options{})

			var first store.QueueEntry
			err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				var inserted bool
				var err error
				first, inserted, err = tc.record(tx, "cmd-1", "AgentInvocationRequested", []byte(`{"n":1}`))
				if err != nil {
					return err
				}
				if !inserted {
					t.Error("first insert reported inserted=false")
				}
				return nil
			})
			if err != nil {
				t.Fatalf("first insert: %v", err)
			}

			// The retry carries a different payload; the original row must
			// win, unchanged.
			var second store.QueueEntry
			err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				var inserted bool
				var err error
				second, inserted, err = tc.record(tx, "cmd-1", "AgentInvocationRequested", []byte(`{"n":2}`))
				if err != nil {
					return err
				}
				if inserted {
					t.Error("duplicate insert reported inserted=true")
				}
				return nil
			})
			if err != nil {
				t.Fatalf("duplicate insert: %v", err)
			}
			if second.ID != first.ID {
				t.Fatalf("duplicate returned row %d, want original %d", second.ID, first.ID)
			}
			if !bytes.Equal(second.Payload, first.Payload) {
				t.Fatalf("duplicate returned payload %s, want original %s", second.Payload, first.Payload)
			}
			if !second.CreatedAt.Equal(first.CreatedAt) {
				t.Fatalf("duplicate returned created_at %v, want original %v", second.CreatedAt, first.CreatedAt)
			}
			if second.Status != "pending" {
				t.Fatalf("status = %q, want pending", second.Status)
			}

			// A distinct key still inserts: the dedup is per key, not global.
			err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				third, inserted, err := tc.record(tx, "cmd-2", "AgentInvocationRequested", nil)
				if err != nil {
					return err
				}
				if !inserted {
					t.Error("distinct key reported inserted=false")
				}
				if third.ID == first.ID {
					t.Errorf("distinct key returned row %d, want a new row", third.ID)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("distinct key insert: %v", err)
			}
		})
	}
}

// TestListPendingOutbox: the recovery scan (§5.14 test 5) returns only
// pending intents of the requested kind, in insertion order.
func TestListPendingOutbox(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, row := range []struct{ key, kind string }{
			{"inv-1", "agent_invocation_requested"},
			{"inv-2", "agent_invocation_requested"},
			{"pub-1", "publication_requested"},
		} {
			if _, _, err := tx.EnqueueOutbox(ctx, row.key, row.kind, nil); err != nil {
				return err
			}
		}
		if _, _, err := tx.RecordInbox(ctx, "inbox-key", "kind", []byte("result")); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	assertPending := func(t *testing.T, want ...string) {
		t.Helper()
		err := s.Read(ctx, func(tx *store.ReadTx) error {
			entries, err := tx.ListPendingOutbox(ctx, "agent_invocation_requested")
			if err != nil {
				return err
			}
			var got []string
			for _, e := range entries {
				got = append(got, e.IdempotencyKey)
			}
			if len(got) != len(want) {
				t.Fatalf("pending keys = %v, want %v", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("pending keys = %v, want %v", got, want)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	// Only the requested kind, in insertion order; the other kind's row is
	// not swept into a foreign dispatcher's scan.
	assertPending(t, "inv-1", "inv-2")

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.ListPendingOutbox(ctx, ""); err == nil {
			t.Error("ListPendingOutbox accepted an empty kind, want error")
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}

	// Marking removes a row from the pending scan; marking again (or marking
	// an unknown key) is an idempotent no-op, never an error: a re-dispatch
	// after a crashed mark must converge, not fail.
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.MarkOutboxDispatched(ctx, "inv-1"); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, "inv-1"); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, "inv-missing"); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, ""); err == nil {
			t.Error("MarkOutboxDispatched accepted an empty key, want error")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	assertPending(t, "inv-2")
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		dispatched, err := tx.ListDispatchedOutbox(ctx, "agent_invocation_requested")
		if err != nil {
			return err
		}
		if len(dispatched) != 1 || dispatched[0].IdempotencyKey != "inv-1" {
			t.Fatalf("dispatched entries = %+v, want inv-1", dispatched)
		}
		if _, err := tx.ListDispatchedOutbox(ctx, ""); err == nil {
			t.Fatal("ListDispatchedOutbox accepted an empty kind")
		}
		entry, err := tx.GetOutbox(ctx, "inv-1")
		if err != nil {
			return err
		}
		if entry.IdempotencyKey != "inv-1" || !entry.Dispatched() {
			t.Fatalf("dispatched entry = %+v", entry)
		}
		if _, err := tx.GetOutbox(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing outbox error = %v, want ErrNotFound", err)
		}
		if _, err := tx.GetOutbox(ctx, ""); err == nil {
			t.Fatal("GetOutbox accepted an empty key")
		}
		inbox, err := tx.GetInbox(ctx, "inbox-key")
		if err != nil {
			return err
		}
		if inbox.IdempotencyKey != "inbox-key" || inbox.Kind != "kind" {
			t.Fatalf("inbox entry = %+v", inbox)
		}
		if _, err := tx.GetInbox(ctx, "missing"); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("missing inbox error = %v, want ErrNotFound", err)
		}
		if _, err := tx.GetInbox(ctx, ""); err == nil {
			t.Fatal("GetInbox accepted an empty key")
		}
		return nil
	}); err != nil {
		t.Fatalf("get dispatched outbox: %v", err)
	}
}

// TestMarkOutboxDispatchedInvisibleToSync: dispatch bookkeeping rides
// WriteInternal, so it must not bump the client-visible revision (§5.14: a
// revision change invalidates client caches; re-dispatching on recovery must
// not).
func TestMarkOutboxDispatchedInvisibleToSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, "inv-1", "agent_invocation_requested", nil)
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("server state: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, "inv-1")
	})
	if err != nil {
		t.Fatalf("mark dispatched: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("server state: %v", err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("revision moved %d -> %d; dispatch bookkeeping must be invisible to sync", before.Revision, after.Revision)
	}
}

// TestPromoteOutboxRefinesPendingRowInPlace: a placeholder settles into its
// final kind and payload without the row ever being absent, so the identity a
// caller occupied at insert survives the promotion — same id, same created_at,
// still pending and therefore still visible to the recovery scan.
func TestPromoteOutboxRefinesPendingRowInPlace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	placeholder := []byte(`{"reserved":true}`)
	final := []byte(`{"intent":true}`)

	var before store.QueueEntry
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		before, _, err = tx.EnqueueOutbox(ctx, "key-1", "reservation", placeholder)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var after store.QueueEntry
	var promoted bool
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		after, promoted, err = tx.PromoteOutbox(ctx, "key-1", "reservation", "intent", placeholder, final)
		return err
	}); err != nil {
		t.Fatalf("PromoteOutbox: %v", err)
	}
	if !promoted {
		t.Error("promoted = false, want true")
	}
	if after.ID != before.ID || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Errorf("promotion moved the row: id %d -> %d, created_at %v -> %v",
			before.ID, after.ID, before.CreatedAt, after.CreatedAt)
	}
	if after.Kind != "intent" || !bytes.Equal(after.Payload, final) {
		t.Errorf("promoted row = kind %q payload %q, want kind \"intent\" payload %q",
			after.Kind, after.Payload, final)
	}
	if after.Dispatched() || after.Quarantined() {
		t.Errorf("promoted row status = %q, want pending so recovery still scans it", after.Status)
	}

	var pending []store.QueueEntry
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, "intent")
		return err
	}); err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].IdempotencyKey != "key-1" {
		t.Fatalf("pending under the final kind = %+v, want the promoted row", pending)
	}
}

// TestPromoteOutboxRefusesRowThatMoved: the guard is the whole point. A row
// whose kind, payload, or status is not exactly what the caller matched is a
// row somebody else settled, so the promotion must affect nothing and report
// that it did not promote rather than overwriting the other writer's decision.
func TestPromoteOutboxRefusesRowThatMoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	placeholder := []byte(`{"reserved":true}`)
	final := []byte(`{"intent":true}`)
	cases := []struct {
		name          string
		seedKind      string
		seedPayload   []byte
		dispatch      bool
		fromKind      string
		expectPayload []byte
	}{
		{"wrong from kind", "other", placeholder, false, "reservation", placeholder},
		{"changed payload", "reservation", []byte(`{"reserved":false}`), false, "reservation", placeholder},
		{"already dispatched", "reservation", placeholder, true, "reservation", placeholder},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, store.Options{})
			if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				if _, _, err := tx.EnqueueOutbox(ctx, "key-1", tc.seedKind, tc.seedPayload); err != nil {
					return err
				}
				if tc.dispatch {
					return tx.MarkOutboxDispatched(ctx, "key-1")
				}
				return nil
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}

			var entry store.QueueEntry
			var promoted bool
			if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				var err error
				entry, promoted, err = tx.PromoteOutbox(
					ctx, "key-1", tc.fromKind, "intent", tc.expectPayload, final)
				return err
			}); err != nil {
				t.Fatalf("PromoteOutbox: %v", err)
			}
			if promoted {
				t.Error("promoted = true, want false")
			}
			if entry.Kind != tc.seedKind || !bytes.Equal(entry.Payload, tc.seedPayload) {
				t.Errorf("row = kind %q payload %q, want it untouched (kind %q payload %q)",
					entry.Kind, entry.Payload, tc.seedKind, tc.seedPayload)
			}
		})
	}
}

// TestPromoteOutboxSecondAttemptReportsNotPromoted: a retried promotion is
// ordinary (a caller re-entering after a crash), so the second attempt reports
// promoted false and hands back the settled row instead of erroring — the
// caller compares it against what it wanted and converges.
func TestPromoteOutboxSecondAttemptReportsNotPromoted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	placeholder := []byte(`{"reserved":true}`)
	final := []byte(`{"intent":true}`)

	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if _, _, err := tx.EnqueueOutbox(ctx, "key-1", "reservation", placeholder); err != nil {
			return err
		}
		_, _, err := tx.PromoteOutbox(ctx, "key-1", "reservation", "intent", placeholder, final)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var entry store.QueueEntry
	var promoted bool
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		entry, promoted, err = tx.PromoteOutbox(ctx, "key-1", "reservation", "intent", placeholder, final)
		return err
	}); err != nil {
		t.Fatalf("second PromoteOutbox: %v", err)
	}
	if promoted {
		t.Error("second promoted = true, want false")
	}
	if entry.Kind != "intent" || !bytes.Equal(entry.Payload, final) {
		t.Errorf("row = kind %q payload %q, want the already-promoted intent", entry.Kind, entry.Payload)
	}
}

// TestPromoteOutboxRejectsUnusableRequest: an absent key names no row to
// refine, and an unchanged kind would let the guard match the row the caller
// just wrote, so a payload rewrite could pass as a fresh promotion.
func TestPromoteOutboxRejectsUnusableRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, "key-1", "reservation", []byte(`{}`))
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	cases := []struct {
		name     string
		key      string
		fromKind string
		toKind   string
		wantErr  error
	}{
		{"empty key", "", "reservation", "intent", nil},
		{"empty from kind", "key-1", "", "intent", nil},
		{"empty to kind", "key-1", "reservation", "", nil},
		{"unchanged kind", "key-1", "intent", "intent", nil},
		{"missing row", "key-2", "reservation", "intent", store.ErrNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				_, _, err := tx.PromoteOutbox(ctx, tc.key, tc.fromKind, tc.toKind, []byte(`{}`), []byte(`{}`))
				return err
			})
			if err == nil {
				t.Fatal("PromoteOutbox accepted an unusable request, want error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("PromoteOutbox error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
