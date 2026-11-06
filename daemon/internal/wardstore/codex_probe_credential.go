package wardstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// codexProbeCredentialReadHold bounds the read hold a Codex probe credential
// read takes. The read is one local file, so the window is short, and the
// probe that uses the credential runs after the hold ends.
const codexProbeCredentialReadHold = 30 * time.Second

// ErrCodexProbeCredentialUnsupported: only a Codex CLI OAuth enrollment's
// store holds the auth.json a Codex probe runs under.
var ErrCodexProbeCredentialUnsupported = errors.New(
	"codex probe credential read: the enrollment is not a Codex CLI OAuth enrollment")

// CodexProbeCredentialReader reads the access-only credential a Codex model
// discovery probe runs under (issue #2595). It re-checks, on every read, the
// gates that make the store this enrollment's to read: the enrollment is a
// Codex CLI OAuth one, its identity's mutation lease is not live, the store
// is its current generation's, resolved under the read hold, and the store
// sits under InputRoot, which ward.ReviewSnapshotProbeCredential enforces
// along with the refresh-token strip and the ten-minute expiry floor. A read
// that outlives its hold is refused. It records nothing.
type CodexProbeCredentialReader struct {
	Store *store.Store
	// InputRoot is the private root the daemon's review reads Codex auth
	// stores under (-review-input-root).
	InputRoot string
	// Now returns UTC instants. The hold window is judged on this clock.
	Now func() time.Time
}

// Read returns the probe credential for the enrollment's current
// generation, read under the identity's shared read hold. It never takes
// the mutation lease: a live one is a refusal.
func (r CodexProbeCredentialReader) Read(
	ctx context.Context, id domain.ClientEnrollmentID,
) (ward.LineupProbeCredential, error) {
	if r.Store == nil || r.Now == nil || r.InputRoot == "" {
		return ward.LineupProbeCredential{}, errCredentialReadNilDependency
	}
	var enrollment domain.ClientEnrollment
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		enrollment, err = tx.GetClientEnrollment(ctx, id)
		return err
	}); err != nil {
		return ward.LineupProbeCredential{}, fmt.Errorf("codex probe credential read: %w", err)
	}
	if enrollment.HarnessClient != domain.HarnessClientCodexCLI || enrollment.AuthMethod != domain.AuthMethodOAuth {
		return ward.LineupProbeCredential{}, ErrCodexProbeCredentialUnsupported
	}
	holder, err := newCredentialReadHolder()
	if err != nil {
		return ward.LineupProbeCredential{}, err
	}
	acquiredAt := r.Now()
	var (
		hold       domain.AuthStoreReadHold
		generation domain.EnrollmentGeneration
	)
	// The hold and the generation are read together, so the store read is
	// the one current while nothing may mutate it.
	err = r.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		hold, err = tx.AcquireAuthStoreReadHold(
			ctx, enrollment.AuthIdentityID, holder, acquiredAt, acquiredAt.Add(codexProbeCredentialReadHold))
		if err != nil {
			return err
		}
		generation, err = tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
		return err
	})
	if errors.Is(err, store.ErrLeaseHeld) {
		return ward.LineupProbeCredential{}, ErrCredentialReadMutationLeaseLive
	}
	if err != nil {
		return ward.LineupProbeCredential{}, fmt.Errorf("codex probe credential read: %w", err)
	}
	defer CredentialReader{Store: r.Store, Now: r.Now}.releaseHold(ctx, hold)

	credential, err := ward.ReviewSnapshotProbeCredential(
		domain.HarnessClientCodexCLI, ward.CodexAuthSubscription, r.InputRoot, generation.AuthStoreVolume, r.Now())
	if err != nil {
		return ward.LineupProbeCredential{}, fmt.Errorf("codex probe credential read: %w", err)
	}
	if !hold.HeldAt(r.Now()) {
		return ward.LineupProbeCredential{}, ErrCredentialReadHoldEnded
	}
	return credential, nil
}
