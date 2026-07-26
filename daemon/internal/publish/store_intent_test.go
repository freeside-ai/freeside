package publish_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// reserveFor commits the reservation a run holds on the fixture invocation and
// returns the key it occupies.
func reserveFor(t *testing.T, s *store.Store, claim publish.Reservation) string {
	t.Helper()
	ctx := context.Background()
	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return publish.ClaimInvocation(ctx, tx, claim)
	}); err != nil {
		t.Fatalf("ClaimInvocation: %v", err)
	}
	return reservationKey(t, claim)
}

func outboxRow(t *testing.T, s *store.Store, key string) store.QueueEntry {
	t.Helper()
	ctx := context.Background()
	var entry store.QueueEntry
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, key)
		return err
	}); err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return entry
}

// TestStoreLedgerRefusesReservedInvocationWithoutClaim is the enforcement the
// reservation exists for: a writer that does not hold the reservation cannot
// commit an intent under the invocation, and it does not have to know
// reservations exist to fail — it collides with the key it tried to take.
func TestStoreLedgerRefusesReservedInvocationWithoutClaim(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := fixtureReservation(t)
	key := reserveFor(t, s, owner)
	before := outboxRow(t, s, key)

	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	payload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}

	other, err := publish.NewReservation(owner.InvocationID, "run-other")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		claim *publish.Reservation
	}{
		{"no claim at all", nil},
		{"another run's claim", &other},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ledger.Record(ctx, key, publish.IntentKindPublication, payload, tc.claim)
			if !errors.Is(err, publish.ErrInvocationReserved) {
				t.Fatalf("Record error = %v, want ErrInvocationReserved", err)
			}
			after := outboxRow(t, s, key)
			if after.ID != before.ID || after.Kind != publish.IntentKindReservation ||
				!bytes.Equal(after.Payload, before.Payload) {
				t.Fatalf("refused write still moved the row: %+v", after)
			}
		})
	}
}

// TestStoreLedgerSettlesItsOwnReservation: the owner's intent replaces the
// reservation on the same row, so nothing ever released the key, and the
// settled intent is pending — the recovery scan must still find it.
func TestStoreLedgerSettlesItsOwnReservation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := fixtureReservation(t)
	key := reserveFor(t, s, owner)
	reserved := outboxRow(t, s, key)

	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	payload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}

	prior, recorded, err := ledger.Record(ctx, key, publish.IntentKindPublication, payload, &owner)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if !recorded {
		t.Error("recorded = false; settling a reservation commits the intent")
	}
	if !bytes.Equal(prior, payload) {
		t.Errorf("prior = %q, want the committed intent %q", prior, payload)
	}

	settled := outboxRow(t, s, key)
	if settled.ID != reserved.ID {
		t.Errorf("settled row id = %d, want the reservation's row %d", settled.ID, reserved.ID)
	}
	if settled.Kind != publish.IntentKindPublication || !bytes.Equal(settled.Payload, payload) {
		t.Errorf("settled row = kind %q payload %q, want the publication intent", settled.Kind, settled.Payload)
	}
	if settled.Dispatched() || settled.Quarantined() {
		t.Errorf("settled row status = %q, want pending so recovery drains it", settled.Status)
	}

	var pending []store.QueueEntry
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, publish.IntentKindPublication)
		return err
	}); err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 || pending[0].IdempotencyKey != key {
		t.Fatalf("pending publication intents = %+v, want the settled row", pending)
	}
}

// TestStoreLedgerConvergesAfterSettling: a retry of the same invocation, and a
// restart that re-runs it, must converge on the one committed intent rather
// than reading its own settled row as somebody else's.
func TestStoreLedgerConvergesAfterSettling(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := fixtureReservation(t)
	key := reserveFor(t, s, owner)
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	payload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Record(ctx, key, publish.IntentKindPublication, payload, &owner); err != nil {
		t.Fatalf("first Record: %v", err)
	}

	prior, recorded, err := ledger.Record(ctx, key, publish.IntentKindPublication, payload, &owner)
	if err != nil {
		t.Fatalf("retry Record: %v", err)
	}
	if recorded {
		t.Error("retry recorded = true, want convergence on the committed intent")
	}
	if !bytes.Equal(prior, payload) {
		t.Errorf("retry prior = %q, want the committed intent", prior)
	}

	// A settled invocation is no longer reserved, so a foreign writer meets the
	// ordinary intent-convergence refusal rather than the reservation one.
	foreign := fixtureIntent()
	foreign.SourceHeadSHA = "0f4a19e0d1c2b3a4958677889900aabbccddeeff"
	foreignPayload, err := foreign.Encode()
	if err != nil {
		t.Fatal(err)
	}
	prior, recorded, err = ledger.Record(ctx, key, publish.IntentKindPublication, foreignPayload, nil)
	if err != nil {
		t.Fatalf("foreign Record after settling: %v", err)
	}
	if recorded || !bytes.Equal(prior, payload) {
		t.Fatalf("foreign write took the settled key: recorded=%v prior=%q", recorded, prior)
	}
}

// TestStoreLedgerRefusesClaimForAnotherKey: a claim that matches the row it is
// presented against but names a different invocation would settle a
// reservation the caller never made.
func TestStoreLedgerRefusesClaimForAnotherKey(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := fixtureReservation(t)
	reserveFor(t, s, owner)

	elsewhere, err := publish.IntentKey("inv-elsewhere", publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := owner.Encode()
	if err != nil {
		t.Fatal(err)
	}
	seedOutbox(t, s, elsewhere, publish.IntentKindReservation, payload)

	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	intentPayload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ledger.Record(
		ctx, elsewhere, publish.IntentKindPublication, intentPayload, &owner,
	); err == nil {
		t.Fatal("Record settled a reservation at a key the claim does not name")
	}
}

// TestConcurrentWritersConvergeOnTheReservingRun runs the race the sequential
// tests only simulate: many writers reaching the same reserved invocation at
// once, one of them holding the reservation. The store serializes writers on a
// single connection, so what this proves is that serialization plus the
// reservation leaves exactly one committed intent, and that it is the owner's.
func TestConcurrentWritersConvergeOnTheReservingRun(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := fixtureReservation(t)
	key := reserveFor(t, s, owner)
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}
	ownerPayload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	foreignIntent := fixtureIntent()
	foreignIntent.SourceHeadSHA = "0f4a19e0d1c2b3a4958677889900aabbccddeeff"
	foreignPayload, err := foreignIntent.Encode()
	if err != nil {
		t.Fatal(err)
	}

	type attempt struct {
		prior    []byte
		recorded bool
		err      error
	}
	const foreigners = 7
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		ownerErr error
		foreign  []attempt
	)
	start := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, _, err := ledger.Record(ctx, key, publish.IntentKindPublication, ownerPayload, &owner)
		mu.Lock()
		ownerErr = err
		mu.Unlock()
	}()
	for i := range foreigners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			var got attempt
			claim, err := publish.NewReservation(owner.InvocationID, domain.RunID(fmt.Sprintf("run-foreign-%d", i)))
			got.err = err
			if err == nil {
				got.prior, got.recorded, got.err = ledger.Record(
					ctx, key, publish.IntentKindPublication, foreignPayload, &claim)
			}
			mu.Lock()
			foreign = append(foreign, got)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()

	if ownerErr != nil {
		t.Fatalf("owner Record: %v", ownerErr)
	}
	// A foreigner meets one of two outcomes depending on where it lands
	// relative to the owner: refused while the reservation still holds the key,
	// or converged on the owner's committed intent once it does not. What it
	// must never do is commit its own payload.
	for _, got := range foreign {
		switch {
		case got.err == nil:
			if got.recorded || !bytes.Equal(got.prior, ownerPayload) {
				t.Fatalf("foreign write took the key: recorded=%v prior=%q", got.recorded, got.prior)
			}
		case errors.Is(got.err, publish.ErrInvocationReserved):
		default:
			t.Fatalf("foreign Record error = %v, want ErrInvocationReserved or convergence", got.err)
		}
	}

	entry := outboxRow(t, s, key)
	if entry.Kind != publish.IntentKindPublication || !bytes.Equal(entry.Payload, ownerPayload) {
		t.Fatalf("row at %s = kind %q payload %q, want the owner's intent", key, entry.Kind, entry.Payload)
	}
	var pending []store.QueueEntry
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ListPendingOutbox(ctx, publish.IntentKindPublication)
		return err
	}); err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d publication intents committed, want exactly 1: %+v", len(pending), pending)
	}
}

// TestStoreLedgerRefusesSettlingAnIntentForAnotherInvocation: the payload that
// replaces the reservation must name the invocation whose key it takes. An
// intent settled under another invocation's key would fail the drain's own
// key-versus-payload check on every pass and could never drain, so the write
// side refuses it rather than committing a row nothing can ever finish.
func TestStoreLedgerRefusesSettlingAnIntentForAnotherInvocation(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	owner := fixtureReservation(t)
	key := reserveFor(t, s, owner)
	reserved := outboxRow(t, s, key)

	elsewhere := fixtureIntent()
	elsewhere.InvocationID = "inv-elsewhere"
	payload, err := elsewhere.Encode()
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := publish.NewStoreLedger(s)
	if err != nil {
		t.Fatalf("NewStoreLedger: %v", err)
	}

	_, _, err = ledger.Record(ctx, key, publish.IntentKindPublication, payload, &owner)
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("Record error = %v, want ErrParentKeyMismatch", err)
	}
	after := outboxRow(t, s, key)
	if after.ID != reserved.ID || after.Kind != publish.IntentKindReservation ||
		!bytes.Equal(after.Payload, reserved.Payload) {
		t.Fatalf("refused settlement moved the reservation: %+v", after)
	}
}
