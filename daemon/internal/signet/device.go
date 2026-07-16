package signet

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrDeviceNotActive rejects a genuinely new command whose submitting device
// is revoked or unknown (§5.14 test 15). It is judged inside the accepting
// transaction, after idempotency: a retry of an already-committed command
// still returns its recorded result (test 16), because the replay branch
// never reaches this gate and never writes.
var ErrDeviceNotActive = errors.New("device is not active")

// gateActiveDevice enforces the active-device gate against the durable row,
// in the same transaction that would commit the caller's effect: an unknown
// device gates identically to a revoked one (non-enumeration), and any store
// failure passes through for the caller to wrap.
func gateActiveDevice(ctx context.Context, tx *store.WriteTx, id domain.DeviceID) error {
	device, err := tx.GetDevice(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("device %q: %w", id, ErrDeviceNotActive)
		}
		return err
	}
	if device.Status != domain.DeviceActive {
		return fmt.Errorf("device %q: %w", id, ErrDeviceNotActive)
	}
	return nil
}

// NewRequestAuthorizer verifies the paired-device bearer credential on every
// request (the real implementation the RequestAuthorizer typedef reserved for
// this unit): it parses `Authorization: Bearer fsd1.<device_id_b64>.<secret>`,
// routes by the embedded device ID, compares the whole token's sha256 digest
// against the stored verifier in constant time, and requires the device to be
// active. Every failure is the same bare denial; the middleware renders 401.
func NewRequestAuthorizer(st *store.Store) RequestAuthorizer {
	return func(r *http.Request) (domain.DeviceID, bool) {
		token, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			return "", false
		}
		deviceID, ok := parseDeviceToken(token)
		if !ok {
			return "", false
		}
		presented := credentialDigest(token)
		authorized := false
		err := st.Read(r.Context(), func(tx *store.ReadTx) error {
			credential, err := tx.GetDeviceCredential(r.Context(), deviceID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil
				}
				return err
			}
			device, err := tx.GetDevice(r.Context(), deviceID)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					return nil
				}
				return err
			}
			// Kind gates first: a public-key credential (a different verifier
			// scheme) never matches a digest comparison. The compare itself is
			// constant-time over the two digest strings.
			authorized = credential.Kind == domain.CredentialHash &&
				subtle.ConstantTimeCompare([]byte(credential.Credential), []byte(presented)) == 1 &&
				device.Status == domain.DeviceActive
			return nil
		})
		if err != nil || !authorized {
			return "", false
		}
		return deviceID, true
	}
}

// bearerToken extracts the credential from an Authorization header. The
// auth-scheme is case-insensitive and followed by one or more spaces
// (RFC 7235 §2.1 / RFC 6750 §2.1); a device token is token68 and so can
// never contain a space itself.
func bearerToken(header string) (string, bool) {
	scheme, credential, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	credential = strings.TrimLeft(credential, " ")
	if credential == "" {
		return "", false
	}
	return credential, true
}

// parseDeviceToken splits a presented bearer token per the deviceCredential
// scheme and recovers the routing device ID. The format is total: base64url
// is dot-free, so any well-formed token has exactly three segments; anything
// else is rejected before a single store read.
func parseDeviceToken(token string) (domain.DeviceID, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != deviceTokenPrefix || parts[1] == "" || parts[2] == "" {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return domain.DeviceID(raw), true
}

// errRevokeReplay abandons the Write of an idempotent re-revocation after
// the recorded snapshot has been captured: revocation is terminal, so the
// replay returns the same snapshot without bumping the server revision (the
// errReplay pattern from Submit).
var errRevokeReplay = errors.New("idempotent revocation replay: recorded snapshot captured")

// Revoke marks the target device revoked on behalf of caller and returns the
// post-revocation snapshot (§5.14; api/openapi.yaml POST
// /devices/{device_id}/revoke). The caller's active-device gate runs inside
// the revoking transaction, mirroring Submit: the HTTP authorizer's check is
// a read outside this Write, so without the in-tx re-check a device being
// revoked could race its own already-authorized request through and revoke
// the owner's remaining device. Self-revocation passes the gate (the caller
// is still active when it commits). Revocation stops future access only: the
// credential row stays as a record, the authorizer and the gates judge the
// device row's status. Terminal and idempotent: re-revoking returns the
// recorded snapshot unchanged.
func (s *Service) Revoke(ctx context.Context, caller, id domain.DeviceID) (DeviceSnapshot, error) {
	var out DeviceSnapshot
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := gateActiveDevice(ctx, tx, caller); err != nil {
			return fmt.Errorf("revoke device %q: caller %w", id, err)
		}
		device, snap, err := tx.GetDeviceSnapshot(ctx, id)
		if err != nil {
			return fmt.Errorf("revoke device %q: %w", id, err)
		}
		if device.Status == domain.DeviceRevoked {
			// RevokedAt is a recorded final outcome that never moves
			// (domain.ValidateDeviceTransition), so the replay re-reads rather
			// than re-writes.
			out = deviceSnapshot(device, snap)
			return errRevokeReplay
		}
		// Revocation is a security action and must not fail on a backwards
		// clock: clamp to PairedAt so the row keeps the domain's
		// revoked-after-paired invariant instead of persisting quietly
		// invalid or refusing to revoke (refute-pass finding, see the
		// decision note).
		now := s.now()
		if now.Before(device.PairedAt) {
			now = device.PairedAt
		}
		revoked := device
		revoked.Status = domain.DeviceRevoked
		revoked.RevokedAt = &now
		if err := tx.PutDevice(ctx, revoked); err != nil {
			return fmt.Errorf("revoke device %q: %w", id, err)
		}
		device, snap, err = tx.GetDeviceSnapshot(ctx, id)
		if err != nil {
			return fmt.Errorf("revoke device %q: %w", id, err)
		}
		out = deviceSnapshot(device, snap)
		return nil
	})
	if err != nil && !errors.Is(err, errRevokeReplay) {
		return DeviceSnapshot{}, err
	}
	return out, nil
}
