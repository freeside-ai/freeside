package store_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestInternalTxCannotWriteSynchronizedState proves issue #38's invariant
// structurally: the client-visible WriteTx exposes every synchronized Put
// method, and the non-bumping InternalTx exposes none of them, so a
// WriteInternal callback cannot commit synchronized state without a revision
// bump. A synchronized Put through the internal path does not even compile;
// this test guards the type boundary that makes that true and fails loudly if
// a future Put ever lands on the internal surface.
func TestInternalTxCannotWriteSynchronizedState(t *testing.T) {
	writeTx := reflect.TypeOf(&store.WriteTx{})
	internalTx := reflect.TypeOf(&store.InternalTx{})

	// Discover the synchronized-write surface by convention (the Put* prefix)
	// rather than a hardcoded list, so a newly added Put is guarded without a
	// test edit.
	var puts []string
	for i := range writeTx.NumMethod() {
		if name := writeTx.Method(i).Name; strings.HasPrefix(name, "Put") {
			puts = append(puts, name)
		}
	}
	if len(puts) == 0 {
		t.Fatal("no Put* methods found on *store.WriteTx; the reflection surface changed")
	}
	for _, name := range puts {
		if _, ok := internalTx.MethodByName(name); ok {
			t.Errorf("*store.InternalTx exposes %s: an internal (non-revision-bumping) transaction can write synchronized state (#38 regression)", name)
		}
	}

	// The inbox/outbox queue methods are the intentional internal-write use
	// case and must stay reachable from both handles.
	for _, name := range []string{"EnqueueOutbox", "RecordInbox"} {
		if _, ok := internalTx.MethodByName(name); !ok {
			t.Errorf("*store.InternalTx lost %s; the inbox/outbox internal-write use case is broken", name)
		}
		if _, ok := writeTx.MethodByName(name); !ok {
			t.Errorf("*store.WriteTx lost %s; committing a queue entry alongside a client-visible decision is broken", name)
		}
	}
}
