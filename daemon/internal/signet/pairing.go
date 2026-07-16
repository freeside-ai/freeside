package signet

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Pairing (plan §5.14): the daemon mints a short-lived single-use code,
// displays it on the daemon host, and POST /pairing exchanges the typed
// plaintext for a new device and its bearer token. Only keyed digests
// persist: the code's HMAC-SHA256 under the daemon-held pairing key, and the
// sha256 of the issued token; the plaintexts appear once and are never
// retrievable again.

// Pairing codes are 8 characters from the Crockford base32 alphabet (no
// 0/O or 1/I/L confusion when read off a terminal and typed on a phone):
// 32^8 ≈ 1.1e12 (~40 bits), hopeless to guess online through the
// undifferentiated 403 within the 10-minute single-use window.
const (
	pairingCodeAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	pairingCodeLength   = 8
	pairingCodeTTL      = 10 * time.Minute
)

// deviceTokenPrefix versions the bearer-token format
// `fsd1.<device_id_b64>.<secret>` (api/openapi.yaml deviceCredential).
const deviceTokenPrefix = "fsd1"

// ErrPairingRejected is the single undifferentiated pairing rejection: an
// unknown, expired, or already-consumed code (and a daemon holding no pairing
// key, which can have no valid codes) all match it, so an unauthenticated
// caller cannot probe which (api/openapi.yaml POST /pairing 403).
var ErrPairingRejected = errors.New("pairing code is unknown, expired, or already consumed")

// PairingGrant is the successful pairing exchange, matching api/openapi.yaml:
// the token's only appearance, ever, alongside the new device's snapshot.
type PairingGrant struct {
	DeviceToken string         `json:"device_token"`
	Device      DeviceSnapshot `json:"device"`
}

// DeviceSnapshot is a Device with its store-stamped sync metadata, matching
// api/openapi.yaml.
type DeviceSnapshot struct {
	AsOfRevision  int64         `json:"as_of_revision"`
	EntityVersion int64         `json:"entity_version"`
	Device        domain.Device `json:"device"`
}

func deviceSnapshot(device domain.Device, snapshot store.Snapshot) DeviceSnapshot {
	return DeviceSnapshot{
		AsOfRevision: snapshot.AsOfRevision, EntityVersion: snapshot.EntityVersion, Device: device,
	}
}

// MintPairingCode mints one short-lived single-use pairing code and returns
// its plaintext for the daemon host to display or print (the composition's
// job; the service never logs it). Only the keyed digest persists, through
// WriteInternal: minting is daemon-internal bookkeeping and must not
// invalidate client caches.
func (s *Service) MintPairingCode(ctx context.Context) (string, domain.PairingCode, error) {
	if len(s.pairingKey) == 0 {
		return "", domain.PairingCode{}, errors.New("mint pairing code: no pairing key configured")
	}
	plaintext, err := s.randomPairingCode()
	if err != nil {
		return "", domain.PairingCode{}, fmt.Errorf("mint pairing code: %w", err)
	}
	now := s.now()
	code := domain.PairingCode{
		CodeHash:  s.pairingCodeHash(plaintext),
		CreatedAt: now,
		ExpiresAt: now.Add(pairingCodeTTL),
	}
	if err := code.Validate(); err != nil {
		return "", domain.PairingCode{}, fmt.Errorf("mint pairing code: %w", err)
	}
	err = s.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MintPairingCode(ctx, code)
	})
	if err != nil {
		return "", domain.PairingCode{}, err
	}
	return plaintext, code, nil
}

// Pair exchanges a pairing-code plaintext for a new active device and its
// bearer token (§5.14 tests 13-14). Expiry is judged at redemption against
// this service's clock; consumption, device creation, and credential
// recording commit in one Write, so the grant's snapshot is the pairing
// transaction's own revision and no partial pairing can persist. Every
// code-related rejection wraps ErrPairingRejected, undifferentiated.
func (s *Service) Pair(ctx context.Context, plaintext, displayName string) (PairingGrant, error) {
	if displayName == "" {
		return PairingGrant{}, fmt.Errorf("pair device: display_name: %w", domain.ErrEmptyField)
	}
	if len(s.pairingKey) == 0 {
		// No key means no code was ever mintable; indistinguishable from an
		// unknown code by design.
		return PairingGrant{}, fmt.Errorf("pair device: no pairing key: %w", ErrPairingRejected)
	}
	codeHash := s.pairingCodeHash(normalizePairingCode(plaintext))
	deviceID, token, credential, err := s.newDeviceIdentity()
	if err != nil {
		return PairingGrant{}, fmt.Errorf("pair device: %w", err)
	}

	var grant PairingGrant
	err = s.store.Write(ctx, func(tx *store.WriteTx) error {
		code, err := tx.GetPairingCode(ctx, codeHash)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("pair device: %w", ErrPairingRejected)
			}
			return err
		}
		now := s.now()
		if code.ConsumedAt != nil || !now.Before(code.ExpiresAt) {
			return fmt.Errorf("pair device: %w", ErrPairingRejected)
		}
		// A clock rewound past the mint must not turn a valid code into a
		// domain-invalid consumption (consumed_at before created_at fails the
		// pairing-code validator); clamp the redemption instant like Revoke
		// clamps revoked_at. The device's paired_at rides the same clamp, so
		// a later revocation stays ordered too.
		if now.Before(code.CreatedAt) {
			now = code.CreatedAt
		}
		// The device row precedes the consumption because the code's device_id
		// column references it; atomicity is the transaction's, so a lost
		// consumption race below rolls the device back out.
		if err := tx.PutDevice(ctx, domain.Device{
			ID: deviceID, DisplayName: displayName,
			Status: domain.DeviceActive, PairedAt: now,
		}); err != nil {
			return err
		}
		if err := tx.RecordDeviceCredential(ctx, domain.DeviceCredential{
			DeviceID: deviceID, Kind: domain.CredentialHash, Credential: credential,
		}); err != nil {
			return err
		}
		if err := tx.ConsumePairingCode(ctx, codeHash, deviceID, now); err != nil {
			// A concurrent winner or a device-slot conflict is a consumed code
			// from this caller's perspective; anything else is internal.
			if errors.Is(err, store.ErrImmutableConflict) {
				return fmt.Errorf("pair device: %w", ErrPairingRejected)
			}
			return err
		}
		device, snap, err := tx.GetDeviceSnapshot(ctx, deviceID)
		if err != nil {
			return err
		}
		grant = PairingGrant{DeviceToken: token, Device: deviceSnapshot(device, snap)}
		return nil
	})
	if err != nil {
		return PairingGrant{}, err
	}
	return grant, nil
}

// normalizePairingCode folds a hand-typed code onto the minted alphabet
// before hashing, per Crockford base32's decoding rules: the code is read
// off the daemon host and typed on a phone, so lowercase, the aliased
// glyphs the alphabet excludes (O for 0, I and L for 1), and grouping
// hyphens or spaces must not turn a valid code into a 403. Minted plaintexts
// are already canonical, so normalization is identity for them.
func normalizePairingCode(typed string) string {
	var code strings.Builder
	code.Grow(len(typed))
	for _, r := range strings.ToUpper(typed) {
		switch r {
		case '-', ' ':
			continue
		case 'O':
			r = '0'
		case 'I', 'L':
			r = '1'
		}
		code.WriteRune(r)
	}
	return code.String()
}

// pairingCodeHash derives a code's stored identity: the keyed digest that
// keeps a short displayed code from being offline-brute-forced out of a
// leaked store (domain.PairingCode.CodeHash).
func (s *Service) pairingCodeHash(plaintext string) domain.Digest {
	mac := hmac.New(sha256.New, s.pairingKey)
	mac.Write([]byte(plaintext))
	return domain.Digest("sha256:" + hex.EncodeToString(mac.Sum(nil)))
}

// randomPairingCode draws each character uniformly: the 32-character alphabet
// divides 256 evenly, so a byte modulo carries no bias.
func (s *Service) randomPairingCode() (string, error) {
	var raw [pairingCodeLength]byte
	if _, err := io.ReadFull(s.rand, raw[:]); err != nil {
		return "", fmt.Errorf("generate code: %w", err)
	}
	code := make([]byte, pairingCodeLength)
	for i, b := range raw {
		code[i] = pairingCodeAlphabet[int(b)%len(pairingCodeAlphabet)]
	}
	return string(code), nil
}

// newDeviceIdentity generates a pairing's device ID, its bearer token
// `fsd1.<device_id_b64>.<secret>` (256-bit secret, unpadded base64url
// segments per the deviceCredential scheme), and the credential the daemon
// stores: the sha256 digest of the whole token, never the token itself.
func (s *Service) newDeviceIdentity() (domain.DeviceID, string, string, error) {
	var raw [16]byte
	if _, err := io.ReadFull(s.rand, raw[:]); err != nil {
		return "", "", "", fmt.Errorf("generate device id: %w", err)
	}
	deviceID := domain.DeviceID(hex.EncodeToString(raw[:]))
	var secret [32]byte
	if _, err := io.ReadFull(s.rand, secret[:]); err != nil {
		return "", "", "", fmt.Errorf("generate token secret: %w", err)
	}
	token := deviceTokenPrefix +
		"." + base64.RawURLEncoding.EncodeToString([]byte(deviceID)) +
		"." + base64.RawURLEncoding.EncodeToString(secret[:])
	return deviceID, token, credentialDigest(token), nil
}

// credentialDigest is the stored verifier for an issued bearer token: the
// sha256 of the whole token string (api/openapi.yaml deviceCredential).
func credentialDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}
