package publish_test

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func fixtureReservation(t *testing.T) publish.Reservation {
	t.Helper()
	r, err := publish.NewReservation("inv-0001", "run-0001")
	if err != nil {
		t.Fatalf("NewReservation: %v", err)
	}
	return r
}

// TestReservationGolden pins the encoded reservation payload. The row outlives
// any single daemon build, and a build that cannot decode a reservation an
// older build wrote would read the invocation as free.
func TestReservationGolden(t *testing.T) {
	t.Parallel()
	payload, err := fixtureReservation(t).Encode()
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "publication-reservation", append(payload, '\n'))
}

// TestReservationRoundTrip: Encode then DecodeReservation returns the same
// claim, so the ownership comparison is over the value that was committed.
func TestReservationRoundTrip(t *testing.T) {
	t.Parallel()
	want := fixtureReservation(t)
	payload, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := publish.DecodeReservation(payload)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Same(want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

// TestReservationRejectsIncompleteClaim: an empty run id would make every
// claim match every reservation, and an empty invocation id names no key.
func TestReservationRejectsIncompleteClaim(t *testing.T) {
	t.Parallel()
	if _, err := publish.NewReservation("", "run-0001"); err == nil {
		t.Error("NewReservation accepted an empty invocation id, want error")
	}
	if _, err := publish.NewReservation("inv-0001", ""); err == nil {
		t.Error("NewReservation accepted an empty run id, want error")
	}
}

// TestDecodeReservationFailsClosed: a payload this build cannot fully
// interpret must not decode. The alternative reading of an undecodable
// reservation is "the invocation is free", which is exactly the state the
// reservation exists to deny.
func TestDecodeReservationFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		payload string
	}{
		{"unknown field", `{"version":"freeside-publication-reservation/v1","invocation_id":"inv-0001","run_id":"run-0001","extra":1}`},
		{"trailing data", `{"version":"freeside-publication-reservation/v1","invocation_id":"inv-0001","run_id":"run-0001"} {}`},
		{"unknown version", `{"version":"freeside-publication-reservation/v2","invocation_id":"inv-0001","run_id":"run-0001"}`},
		{"empty run id", `{"version":"freeside-publication-reservation/v1","invocation_id":"inv-0001","run_id":""}`},
		{"empty invocation id", `{"version":"freeside-publication-reservation/v1","invocation_id":"","run_id":"run-0001"}`},
		{"not json", `nonsense`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := publish.DecodeReservation([]byte(tc.payload)); err == nil {
				t.Fatal("DecodeReservation accepted an uninterpretable payload, want error")
			}
		})
	}
}

// seedOutbox puts one row at the reservation's key so the gates can be driven
// against every state the key can be in.
func seedOutbox(t *testing.T, s *store.Store, key, kind string, payload []byte) {
	t.Helper()
	if err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(context.Background(), key, kind, payload)
		return err
	}); err != nil {
		t.Fatalf("seed %s: %v", kind, err)
	}
}

func reservationKey(t *testing.T, r publish.Reservation) string {
	t.Helper()
	key, err := r.Key()
	if err != nil {
		t.Fatalf("Key: %v", err)
	}
	return key
}

// TestReservationKeyIsThePublicationIntentKey is the design in one assertion:
// the reservation holds the intent's own key, which is what makes an unaware
// writer collide with it instead of racing past it.
func TestReservationKeyIsThePublicationIntentKey(t *testing.T) {
	t.Parallel()
	claim := fixtureReservation(t)
	want, err := publish.IntentKey(claim.InvocationID, publish.IntentKindPublication)
	if err != nil {
		t.Fatal(err)
	}
	if got := reservationKey(t, claim); got != want {
		t.Fatalf("Key() = %q, want the publication intent key %q", got, want)
	}
}

// TestCheckInvocationAvailableAdmitsFreeAndOwnedInvocations: admission may
// commit to an untouched invocation, and re-admitting the same request finds
// its own reservation and converges rather than refusing itself.
func TestCheckInvocationAvailableAdmitsFreeAndOwnedInvocations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	claim := fixtureReservation(t)

	s := newTestStore(t)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		return publish.CheckInvocationAvailable(ctx, tx, claim)
	}); err != nil {
		t.Fatalf("free invocation: %v", err)
	}

	payload, err := claim.Encode()
	if err != nil {
		t.Fatal(err)
	}
	seedOutbox(t, s, reservationKey(t, claim), publish.IntentKindReservation, payload)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		return publish.CheckInvocationAvailable(ctx, tx, claim)
	}); err != nil {
		t.Fatalf("own reservation: %v", err)
	}
}

// TestCheckInvocationAvailableRefusesOccupiedInvocations: every way the key can
// already be spoken for. A committed intent means somebody published under this
// invocation; the rest are rows this build must not interpret as free.
func TestCheckInvocationAvailableRefusesOccupiedInvocations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	claim := fixtureReservation(t)
	key := reservationKey(t, claim)

	foreign, err := publish.NewReservation(claim.InvocationID, "run-other")
	if err != nil {
		t.Fatal(err)
	}
	foreignPayload, err := foreign.Encode()
	if err != nil {
		t.Fatal(err)
	}
	intentPayload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	otherIntent := fixtureIntent()
	otherIntent.InvocationID = "inv-other"
	otherIntentPayload, err := otherIntent.Encode()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		kind    string
		payload []byte
		wantErr error
	}{
		{"another owner's reservation", publish.IntentKindReservation, foreignPayload, publish.ErrInvocationReserved},
		{"committed publication intent", publish.IntentKindPublication, intentPayload, domain.ErrParentKeyMismatch},
		{"intent naming another invocation", publish.IntentKindPublication, otherIntentPayload, domain.ErrParentKeyMismatch},
		{"foreign kind", "engine.something_else", []byte(`{}`), domain.ErrParentKeyMismatch},
		// A row this build cannot read is not the reservation or intent the key
		// should hold, so it carries the same mismatch class rather than
		// letting the caller read the invocation as free.
		{"undecodable reservation", publish.IntentKindReservation, []byte(`{"version":"v0"}`), domain.ErrParentKeyMismatch},
		{"undecodable intent", publish.IntentKindPublication, []byte(`{"foreign":true}`), domain.ErrParentKeyMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestStore(t)
			seedOutbox(t, s, key, tc.kind, tc.payload)
			err := s.Read(ctx, func(tx *store.ReadTx) error {
				return publish.CheckInvocationAvailable(ctx, tx, claim)
			})
			if err == nil {
				t.Fatal("CheckInvocationAvailable admitted an occupied invocation, want error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestClaimInvocationConverges: the claim is committed once and every later
// pass over the same request finds it. Reclaiming after the workflow already
// published finds its own promoted intent, which is what lets a replay of a
// finished task re-run admission instead of failing on its own work.
func TestClaimInvocationConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	claim := fixtureReservation(t)
	key := reservationKey(t, claim)
	s := newTestStore(t)

	for i := range 2 {
		if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return publish.ClaimInvocation(ctx, tx, claim)
		}); err != nil {
			t.Fatalf("ClaimInvocation pass %d: %v", i, err)
		}
	}
	var entry store.QueueEntry
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, key)
		return err
	}); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	if entry.Kind != publish.IntentKindReservation {
		t.Fatalf("reserved row kind = %q, want %q", entry.Kind, publish.IntentKindReservation)
	}
	held, err := publish.DecodeReservation(entry.Payload)
	if err != nil {
		t.Fatalf("decode reservation: %v", err)
	}
	if !held.Same(claim) {
		t.Fatalf("reserved row = %+v, want %+v", held, claim)
	}

	// A replay arriving after the reservation was promoted must converge on the
	// task's own intent rather than reading it as somebody else's.
	promoted := newTestStore(t)
	intentPayload, err := fixtureIntent().Encode()
	if err != nil {
		t.Fatal(err)
	}
	seedOutbox(t, promoted, key, publish.IntentKindPublication, intentPayload)
	if err := promoted.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return publish.ClaimInvocation(ctx, tx, claim)
	}); err != nil {
		t.Fatalf("ClaimInvocation over own intent: %v", err)
	}
}

// TestClaimInvocationRefusesAnotherOwner: two runs cannot hold one publication
// invocation, and the loser must find out before it commits anything.
func TestClaimInvocationRefusesAnotherOwner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := newTestStore(t)
	first := fixtureReservation(t)
	second, err := publish.NewReservation(first.InvocationID, "run-other")
	if err != nil {
		t.Fatal(err)
	}

	if err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return publish.ClaimInvocation(ctx, tx, first)
	}); err != nil {
		t.Fatalf("first ClaimInvocation: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return publish.ClaimInvocation(ctx, tx, second)
	})
	if !errors.Is(err, publish.ErrInvocationReserved) {
		t.Fatalf("second ClaimInvocation error = %v, want ErrInvocationReserved", err)
	}

	var entry store.QueueEntry
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, reservationKey(t, first))
		return err
	}); err != nil {
		t.Fatalf("read reservation: %v", err)
	}
	held, err := publish.DecodeReservation(entry.Payload)
	if err != nil {
		t.Fatalf("decode reservation: %v", err)
	}
	if !held.Same(first) {
		t.Fatalf("reservation = %+v, want the first claimant %+v", held, first)
	}
}
