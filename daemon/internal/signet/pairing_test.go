package signet_test

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// pairingCodeAlphabet mirrors the service's Crockford base32 alphabet; the
// duplication is deliberate, pinning the displayed-code contract from
// outside the package.
const pairingCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// TestMintPairingCodeDisplaysPlaintextPersistsDigest: the mint returns the
// one-time plaintext for the daemon host to display and persists only the
// keyed digest; minting is daemon-internal bookkeeping, so it must not bump
// the server revision (no client cache invalidates because a code exists).
func TestMintPairingCodeDisplaysPlaintextPersistsDigest(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	before := f.revision(t)

	plaintext, code, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	if len(plaintext) != 8 {
		t.Errorf("plaintext length = %d, want 8", len(plaintext))
	}
	for _, c := range plaintext {
		if !strings.ContainsRune(pairingCodeAlphabet, c) {
			t.Errorf("plaintext %q contains %q outside the unambiguous alphabet", plaintext, c)
		}
	}
	if !strings.HasPrefix(string(code.CodeHash), "sha256:") || strings.Contains(string(code.CodeHash), plaintext) {
		t.Errorf("code hash %q must be a digest carrying no plaintext", code.CodeHash)
	}
	if got := code.ExpiresAt.Sub(code.CreatedAt); got != 10*time.Minute {
		t.Errorf("code TTL = %v, want 10m", got)
	}
	var stored domain.PairingCode
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetPairingCode(ctx, code.CodeHash)
		return err
	}); err != nil {
		t.Fatalf("GetPairingCode: %v", err)
	}
	if stored.ConsumedAt != nil || stored.DeviceID != nil {
		t.Errorf("minted code is already consumed: %+v", stored)
	}
	if after := f.revision(t); after != before {
		t.Errorf("minting moved the server revision %d -> %d", before, after)
	}
}

// TestPairGrantsDeviceAndOneTimeToken: a successful redemption creates the
// active device, returns the token in its documented format, and stores only
// the token's digest as the verifier; the grant's snapshot carries the
// pairing transaction's own store-stamped metadata.
func TestPairGrantsDeviceAndOneTimeToken(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	plaintext, _, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	grant, err := f.service.Pair(ctx, plaintext, "Ben's iPad")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	parts := strings.Split(grant.DeviceToken, ".")
	if len(parts) != 3 || parts[0] != "fsd1" {
		t.Fatalf("token %q, want fsd1.<device_id_b64>.<secret>", grant.DeviceToken)
	}
	rawID, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("token device_id segment: %v", err)
	}
	device := grant.Device.Device
	if domain.DeviceID(rawID) != device.ID {
		t.Errorf("token embeds device %q, grant names %q", rawID, device.ID)
	}
	if device.Status != domain.DeviceActive || device.DisplayName != "Ben's iPad" || device.RevokedAt != nil {
		t.Errorf("granted device = %+v, want an active device with the requested label", device)
	}
	if grant.Device.EntityVersion != 1 {
		t.Errorf("granted entity_version = %d, want 1", grant.Device.EntityVersion)
	}
	if grant.Device.AsOfRevision != f.revision(t) {
		t.Errorf("granted as_of_revision = %d, want the pairing transaction's revision %d",
			grant.Device.AsOfRevision, f.revision(t))
	}

	// The daemon stores the token's sha256 digest, never the token: the
	// verifier row must be a digest and must not contain either plaintext.
	var credential domain.DeviceCredential
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		credential, err = tx.GetDeviceCredential(ctx, device.ID)
		return err
	}); err != nil {
		t.Fatalf("GetDeviceCredential: %v", err)
	}
	if credential.Kind != domain.CredentialHash {
		t.Errorf("credential kind = %q, want %q", credential.Kind, domain.CredentialHash)
	}
	if !strings.HasPrefix(credential.Credential, "sha256:") ||
		strings.Contains(credential.Credential, parts[2]) {
		t.Errorf("stored credential %q must be a digest carrying no token material", credential.Credential)
	}
}

// TestPairRejectsExpiredConsumedUnknownCodes is §5.14 test 13 plus the
// unknown-code case: none of the three may create a device, none may move
// the server revision, and all three surface as the same undifferentiated
// rejection.
func TestPairRejectsExpiredConsumedUnknownCodes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	expiredCode, _, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("mint expired-case code: %v", err)
	}
	*f.now = f.now.Add(10 * time.Minute) // exactly the TTL boundary: already expired

	consumedCode, _, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("mint consumed-case code: %v", err)
	}
	if _, err := f.service.Pair(ctx, consumedCode, "First device"); err != nil {
		t.Fatalf("consume code: %v", err)
	}

	before := f.revision(t)
	for name, code := range map[string]string{
		"expired":  expiredCode,
		"consumed": consumedCode,
		"unknown":  "AAAAAAAA",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.service.Pair(ctx, code, "Intruder"); !errors.Is(err, signet.ErrPairingRejected) {
				t.Fatalf("Pair error = %v, want ErrPairingRejected", err)
			}
		})
	}
	if after := f.revision(t); after != before {
		t.Errorf("rejected pairings moved the server revision %d -> %d", before, after)
	}
}

// TestPairSingleDevicePerCode is §5.14 test 14's redemption-policy half: one
// code yields exactly one device, the second attempt failing with the same
// undifferentiated rejection and no state change. The racing-writers half
// (two concurrent consumes, one winner) is pinned at the store layer by
// TestConsumePairingCodeSingleWinner; SQLite serializes writers, so the
// sequential second attempt exercises the same conditional-update path a
// raced loser hits.
func TestPairSingleDevicePerCode(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	plaintext, code, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	winner, err := f.service.Pair(ctx, plaintext, "Winner")
	if err != nil {
		t.Fatalf("first Pair: %v", err)
	}
	after := f.revision(t)
	if _, err := f.service.Pair(ctx, plaintext, "Loser"); !errors.Is(err, signet.ErrPairingRejected) {
		t.Fatalf("second Pair error = %v, want ErrPairingRejected", err)
	}
	if got := f.revision(t); got != after {
		t.Errorf("losing attempt moved the server revision %d -> %d", after, got)
	}
	var stored domain.PairingCode
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetPairingCode(ctx, code.CodeHash)
		return err
	}); err != nil {
		t.Fatalf("GetPairingCode: %v", err)
	}
	if stored.DeviceID == nil || *stored.DeviceID != winner.Device.Device.ID {
		t.Errorf("code consumption records %v, want the winner %q", stored.DeviceID, winner.Device.Device.ID)
	}
}

// TestPairNormalizesTypedCodes: the code is hand-typed off the daemon host,
// so Crockford base32's decoding tolerance applies: lowercase, the aliased
// glyphs the minted alphabet excludes (O for 0, I and L for 1), and grouping
// separators all redeem the same code.
func TestPairNormalizesTypedCodes(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)

	variant := func(canonical string) string {
		typed := strings.ToLower(canonical)
		typed = strings.ReplaceAll(typed, "0", "O")
		typed = strings.ReplaceAll(typed, "1", "l")
		return typed[:4] + "-" + typed[4:]
	}
	for i := 0; ; i++ {
		plaintext, _, err := f.service.MintPairingCode(ctx)
		if err != nil {
			t.Fatalf("MintPairingCode: %v", err)
		}
		typed := variant(plaintext)
		if _, err := f.service.Pair(ctx, typed, "Typed by hand"); err != nil {
			t.Fatalf("Pair(%q) for minted %q: %v", typed, plaintext, err)
		}
		// Keep drawing until a code exercised at least one aliased glyph, so
		// the run cannot pass on case-folding alone; 0/1 appear in ~2/5 of
		// 8-char draws, making the retry bound generous.
		if strings.ContainsAny(plaintext, "01") {
			break
		}
		if i > 100 {
			t.Fatal("no minted code contained an aliased glyph after 100 draws")
		}
	}
}

// TestPairClampsBackwardsClock: a clock rewound past the mint must not turn
// a valid, unexpired code into a rejected redemption (the domain validator
// pins consumed_at >= created_at); the redemption instant clamps to the
// code's created_at, the same rule Revoke applies to revoked_at.
func TestPairClampsBackwardsClock(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	plaintext, code, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	*f.now = code.CreatedAt.Add(-time.Hour)

	grant, err := f.service.Pair(ctx, plaintext, "Paired under skew")
	if err != nil {
		t.Fatalf("Pair under a backwards clock: %v", err)
	}
	if !grant.Device.Device.PairedAt.Equal(code.CreatedAt) {
		t.Errorf("paired_at = %v, want clamped to the code's created_at %v",
			grant.Device.Device.PairedAt, code.CreatedAt)
	}
	var stored domain.PairingCode
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetPairingCode(ctx, code.CodeHash)
		return err
	}); err != nil {
		t.Fatalf("GetPairingCode: %v", err)
	}
	if stored.ConsumedAt == nil || !stored.ConsumedAt.Equal(code.CreatedAt) {
		t.Errorf("consumed_at = %v, want clamped to created_at %v", stored.ConsumedAt, code.CreatedAt)
	}
}

// TestPairRejectsEmptyDisplayNameBeforeConsuming: a structurally invalid
// request must fail before the code is spent, so the client can retry with a
// fixed label.
func TestPairRejectsEmptyDisplayNameBeforeConsuming(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	plaintext, _, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}

	if _, err := f.service.Pair(ctx, plaintext, ""); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("Pair error = %v, want ErrEmptyField", err)
	}
	if _, err := f.service.Pair(ctx, plaintext, "Recovered"); err != nil {
		t.Fatalf("Pair after rejected label: %v", err)
	}
}
