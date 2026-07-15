package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// pairDevice commits the whole pairing flow the attention service will run:
// one Write consuming the code, creating the device, and recording its
// credential atomically (the device is synchronized, so the transaction bumps
// the revision; the bookkeeping rides the same commit through embedding).
func pairDevice(t *testing.T, s *store.Store, f fixtures, consumedAt time.Time) {
	t.Helper()
	ctx := context.Background()
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.MintPairingCode(ctx, f.pairingCode); err != nil {
			return err
		}
		if err := tx.PutDevice(ctx, f.device); err != nil {
			return err
		}
		if err := tx.RecordDeviceCredential(ctx, f.credential); err != nil {
			return err
		}
		return tx.ConsumePairingCode(ctx, f.pairingCode.CodeHash, f.device.ID, consumedAt)
	})
	if err != nil {
		t.Fatalf("pairing write: %v", err)
	}
}

// TestPairingRoundTrip covers the internal bookkeeping round-trips: a minted
// then consumed pairing code and a recorded credential read back equal, with
// stable golden forms alongside the synchronized entities'.
func TestPairingRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	consumedAt := f.pairingCode.CreatedAt.Add(time.Minute)
	pairDevice(t, s, f, consumedAt)

	consumed := f.pairingCode
	consumed.ConsumedAt = &consumedAt
	deviceID := f.device.ID
	consumed.DeviceID = &deviceID

	cases := []struct {
		name string
		want any
		get  func(tx *store.ReadTx) (any, error)
	}{
		{"pairing_code", consumed, func(tx *store.ReadTx) (any, error) {
			return tx.GetPairingCode(ctx, f.pairingCode.CodeHash)
		}},
		{"device_credential", f.credential, func(tx *store.ReadTx) (any, error) {
			return tx.GetDeviceCredential(ctx, f.credential.DeviceID)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got any
			err := s.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				got, err = tc.get(tx)
				return err
			})
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			gotJSON := marshalIndent(t, got)
			wantJSON := marshalIndent(t, tc.want)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("round-trip mismatch:\ngot:  %s\nwant: %s", gotJSON, wantJSON)
			}
			golden.Assert(t, tc.name, gotJSON)
		})
	}
}

// TestPairingBookkeepingBumpsNoRevision: minting a code and recording a
// credential are daemon-internal bookkeeping, so a WriteInternal carrying
// them must not advance the client-visible revision (§5.14: clients cache
// nothing about pairing codes, so nothing invalidates).
func TestPairingBookkeepingBumpsNoRevision(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MintPairingCode(ctx, f.pairingCode)
	})
	if err != nil {
		t.Fatalf("WriteInternal: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("minting a pairing code bumped revision %d -> %d, want unchanged", before.Revision, after.Revision)
	}
}

// TestConsumePairingCodeSingleWinner is the structural half of §5.14 test 14:
// one consume wins, an identical replay converges, and any second consume
// (another device, or the same device at another instant) is an immutable
// conflict, so one code can never create two devices.
func TestConsumePairingCodeSingleWinner(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	consumedAt := f.pairingCode.CreatedAt.Add(time.Minute)
	pairDevice(t, s, f, consumedAt)

	second := domain.Device{
		ID: "device-2", DisplayName: "Ben's iPad",
		Status: domain.DeviceActive, PairedAt: f.device.PairedAt,
	}
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDevice(ctx, second); err != nil {
			return err
		}
		// Identical replay converges: a retried pairing commit is not a second
		// consumption.
		if err := tx.ConsumePairingCode(ctx, f.pairingCode.CodeHash, f.device.ID, consumedAt); err != nil {
			return err
		}
		if err := tx.ConsumePairingCode(ctx, f.pairingCode.CodeHash, second.ID, consumedAt.Add(time.Second)); !errors.Is(err, store.ErrImmutableConflict) {
			t.Errorf("consume by a second device = %v, want ErrImmutableConflict", err)
		}
		if err := tx.ConsumePairingCode(ctx, f.pairingCode.CodeHash, f.device.ID, consumedAt.Add(time.Second)); !errors.Is(err, store.ErrImmutableConflict) {
			t.Errorf("re-consume at a new instant = %v, want ErrImmutableConflict", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		code, err := tx.GetPairingCode(ctx, f.pairingCode.CodeHash)
		if err != nil {
			return err
		}
		if code.DeviceID == nil || *code.DeviceID != f.device.ID {
			t.Errorf("consumed code names device %v, want %q", code.DeviceID, f.device.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestConsumeUnknownPairingCode: consuming a code that was never minted is
// ErrNotFound, not a silent no-op.
func TestConsumeUnknownPairingCode(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ConsumePairingCode(ctx, "sha256:never-minted", "device-1", time.Now().UTC())
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("consume unknown code = %v, want ErrNotFound", err)
	}
}

// TestMintPairingCodeImmutable: minting is write-once with putImmutable
// convergence: an identical replay converges, a same-hash mint with a
// different window conflicts.
func TestMintPairingCodeImmutable(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.MintPairingCode(ctx, f.pairingCode); err != nil {
			return err
		}
		return tx.MintPairingCode(ctx, f.pairingCode)
	})
	if err != nil {
		t.Fatalf("identical re-mint did not converge: %v", err)
	}
	rewound := f.pairingCode
	rewound.ExpiresAt = rewound.ExpiresAt.Add(time.Hour)
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MintPairingCode(ctx, rewound)
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("re-mint with a widened window = %v, want ErrImmutableConflict", err)
	}
}

// TestMintRejectsPreConsumedCode: consumption is recorded exclusively by
// ConsumePairingCode, so a crafted mint carrying a consumption (which would
// fabricate a redemption and burn the device's one-code slot without the
// single-winner path) is rejected outright.
func TestMintRejectsPreConsumedCode(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	pairDevice(t, s, f, f.pairingCode.CreatedAt.Add(time.Minute))

	crafted := domain.PairingCode{
		CodeHash:  "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CreatedAt: f.pairingCode.CreatedAt, ExpiresAt: f.pairingCode.ExpiresAt,
	}
	crafted.ConsumedAt = ptrTime(f.pairingCode.CreatedAt.Add(time.Minute))
	deviceID := f.device.ID
	crafted.DeviceID = &deviceID
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MintPairingCode(ctx, crafted)
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("pre-consumed mint = %v, want ErrImmutableConflict", err)
	}
}

// TestConsumeSecondCodeSameDevice: a device consumes at most one code ever
// (the pairing_codes UNIQUE); the violation surfaces as the store's conflict
// error, not a raw constraint failure.
func TestConsumeSecondCodeSameDevice(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	pairDevice(t, s, f, f.pairingCode.CreatedAt.Add(time.Minute))

	second := domain.PairingCode{
		CodeHash:  "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		CreatedAt: f.pairingCode.CreatedAt, ExpiresAt: f.pairingCode.ExpiresAt,
	}
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.MintPairingCode(ctx, second); err != nil {
			return err
		}
		return tx.ConsumePairingCode(ctx, second.CodeHash, f.device.ID, f.pairingCode.CreatedAt.Add(2*time.Minute))
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("second code for the same device = %v, want ErrImmutableConflict", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

// TestDeviceCredentialWriteOnce: a device's credential is fixed at pairing.
// An identical replay converges; different material (or a different kind)
// under the same device conflicts, and no method can update it in place.
func TestDeviceCredentialWriteOnce(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	pairDevice(t, s, f, f.pairingCode.CreatedAt.Add(time.Minute))

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordDeviceCredential(ctx, f.credential); err != nil {
			return err
		}
		rotated := f.credential
		rotated.Credential = "sha256:11a349bb5374e2bae123bf9fd058156e9bc57e650f3a850c291eeecdc942da8d"
		if err := tx.RecordDeviceCredential(ctx, rotated); !errors.Is(err, store.ErrImmutableConflict) {
			t.Errorf("re-record with different material = %v, want ErrImmutableConflict", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestDeviceRevocationLifecycle covers the store mapping of the device
// transition rules: revoking an active device advances entity_version and the
// revision; reactivating a revoked device or moving its recorded revoked_at
// maps to ErrImmutableConflict (§5.14 test 16's shape: revocation is a
// recorded terminal outcome, never an erasure).
func TestDeviceRevocationLifecycle(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	f := newFixtures(t)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, f.device)
	}); err != nil {
		t.Fatalf("put device: %v", err)
	}

	revoked := f.device
	revoked.Status = domain.DeviceRevoked
	revokedAt := f.device.PairedAt.Add(time.Hour)
	revoked.RevokedAt = &revokedAt
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, revoked)
	}); err != nil {
		t.Fatalf("revoke device: %v", err)
	}

	err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, f.device) // back to active
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("reactivating a revoked device = %v, want ErrImmutableConflict", err)
	}

	moved := revoked
	movedAt := revokedAt.Add(time.Hour)
	moved.RevokedAt = &movedAt
	err = s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, moved)
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("moving a recorded revoked_at = %v, want ErrImmutableConflict", err)
	}

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetDevice(ctx, f.device.ID)
		if err != nil {
			return err
		}
		if got.Status != domain.DeviceRevoked {
			t.Errorf("device status = %q, want %q", got.Status, domain.DeviceRevoked)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
