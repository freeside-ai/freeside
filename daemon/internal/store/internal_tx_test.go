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
// a future Put, or one of the named synchronized Record* writers, ever lands
// on the internal surface.
func TestInternalTxCannotWriteSynchronizedState(t *testing.T) {
	t.Parallel()
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

	// Record* methods sit on both handles by design (RecordInbox,
	// RecordDeviceCredential, and many more are intentional internal
	// writers), so no prefix rule can find the synchronized ones. These three
	// wrote synchronized state through *InternalTx without a bump until #2487
	// moved them to *WriteTx; they are named here instead. A new synchronized
	// Record* writer is guarded only once it is added to this list. Requiring
	// each name on *WriteTx keeps a rename from leaving the list stale.
	for _, name := range []string{
		"RecordUsagePool", "RecordAuthIdentity", "RecordClientEnrollment",
	} {
		if _, ok := writeTx.MethodByName(name); !ok {
			t.Errorf("*store.WriteTx lacks %s; update this list to the synchronized writer's current name", name)
		}
		if _, ok := internalTx.MethodByName(name); ok {
			t.Errorf("*store.InternalTx exposes %s: an internal (non-revision-bumping) transaction can write synchronized state (#2487 regression)", name)
		}
	}

	// The inbox/outbox queue and pairing-bookkeeping methods are the
	// intentional internal-write use cases and must stay reachable from both
	// handles (the pairing Write reaches them through WriteTx's embedding).
	for _, name := range []string{
		"EnqueueOutbox", "RecordInbox",
		"MintPairingCode", "ConsumePairingCode", "RecordDeviceCredential",
	} {
		if _, ok := internalTx.MethodByName(name); !ok {
			t.Errorf("*store.InternalTx lost %s; the inbox/outbox internal-write use case is broken", name)
		}
		if _, ok := writeTx.MethodByName(name); !ok {
			t.Errorf("*store.WriteTx lost %s; committing a queue entry alongside a client-visible decision is broken", name)
		}
	}
	if _, ok := internalTx.MethodByName("RecordDispatchedOutbox"); ok {
		t.Error("*store.InternalTx exposes RecordDispatchedOutbox; completion facts could bypass the revision bump")
	}
}
