package store_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestQueueRejectsEmptyIdentity: an empty idempotency key would collapse
// unrelated actions onto one row, and an empty kind is unroutable; both are
// rejected before touching the table.
func TestQueueRejectsEmptyIdentity(t *testing.T) {
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
}

// TestMarkOutboxDispatchedInvisibleToSync: dispatch bookkeeping rides
// WriteInternal, so it must not bump the client-visible revision (§5.14: a
// revision change invalidates client caches; re-dispatching on recovery must
// not).
func TestMarkOutboxDispatchedInvisibleToSync(t *testing.T) {
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
