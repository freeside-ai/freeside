package wardstore

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// DefaultCredentialReadHold bounds one credential read's hold and the
// observer under it. It is short for the integrity probe's reason: the hold
// delays a refresh or enrollment on that identity, and a crashed read leaves
// it until it expires.
const DefaultCredentialReadHold = 2 * time.Minute

// The causes a credential read refuses for. Each is a fixed sentence: a
// refusal names what the store's records said, never what the store held.
var (
	// ErrCredentialReadUnsupported: only a Claude Code setup-token store
	// holds a token this read knows how to take out.
	ErrCredentialReadUnsupported = errors.New(
		"credential read: the enrollment is not a Claude Code setup-token enrollment")
	// ErrCredentialReadMutationLeaseLive: the identity's store may be
	// changing, so nothing reads it.
	ErrCredentialReadMutationLeaseLive = errors.New(
		"credential read: the identity's mutation lease is live")
	// ErrCredentialReadGenerationChanged: the enrollment's current
	// generation is not the one the caller resolved.
	ErrCredentialReadGenerationChanged = errors.New(
		"credential read: the enrollment's current generation is not the one asked for")
	// ErrCredentialReadHoldEnded: the hold was no longer live when the read
	// finished, so the store may have changed under it.
	ErrCredentialReadHoldEnded = errors.New(
		"credential read: the read hold ended before the read finished")
	// ErrCredentialReadDigestMismatch: the store is not the one the
	// generation recorded.
	ErrCredentialReadDigestMismatch = errors.New(
		"credential read: the store matches neither digest the generation could have recorded")
)

var errCredentialReadNilDependency = errors.New("credential read: nil dependency")

// CredentialReader takes the stored setup token out of a Claude Code
// enrollment's credential volume, for the judgment calls that authenticate
// with it (issue #1426). It returns a token only for the generation the
// caller resolved and only from a store that generation recorded; every
// other outcome is an error and no token. It records nothing: a store that
// fails the digest check is the integrity probe's to mark.
//
// A caller names the enrollment and the generation it resolved, and nothing
// else. The client and method that make a store readable, the identity the
// hold lands on, and the volume are each read from the store's own records.
type CredentialReader struct {
	Store *store.Store
	// ReadSetupToken reads a setup-token volume; production binds
	// ward.ReadSetupToken to the daemon's runtime.
	ReadSetupToken func(ctx context.Context, volume string) (ward.SetupTokenRead, error)
	// Now returns UTC instants. The hold window is judged on this clock.
	Now func() time.Time
	// HoldDuration bounds the read hold and the read under it; zero means
	// DefaultCredentialReadHold.
	HoldDuration time.Duration
}

// Read returns the token of the enrollment's generation numbered ordinal,
// read under the identity's shared read hold. It never takes the mutation
// lease: a live one is a refusal.
func (r CredentialReader) Read(
	ctx context.Context, id domain.ClientEnrollmentID, ordinal int,
) (ward.SetupToken, error) {
	if r.Now == nil {
		return ward.SetupToken{}, errCredentialReadNilDependency
	}
	enrollment, err := r.enrollment(ctx, id)
	if err != nil {
		return ward.SetupToken{}, err
	}
	holdFor := r.HoldDuration
	if holdFor == 0 {
		holdFor = DefaultCredentialReadHold
	}
	if holdFor < 0 {
		return ward.SetupToken{}, errors.New("credential read: negative hold duration")
	}
	holder, err := newCredentialReadHolder()
	if err != nil {
		return ward.SetupToken{}, err
	}
	// The deadline starts before the hold does, so on the daemon's wall
	// clock it ends no later than the hold: a read that outlives the window
	// is cancelled instead of reading a store nothing holds.
	readCtx, cancel := context.WithTimeout(ctx, holdFor)
	defer cancel()
	acquiredAt := r.Now()
	var hold domain.AuthStoreReadHold
	err = r.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		hold, err = tx.AcquireAuthStoreReadHold(
			ctx, enrollment.AuthIdentityID, holder, acquiredAt, acquiredAt.Add(holdFor))
		return err
	})
	if errors.Is(err, store.ErrLeaseHeld) {
		return ward.SetupToken{}, ErrCredentialReadMutationLeaseLive
	}
	if err != nil {
		return ward.SetupToken{}, fmt.Errorf("credential read: %w", err)
	}
	defer r.releaseHold(ctx, hold)

	token, err := r.read(readCtx, enrollment.ID, ordinal)
	if err != nil {
		return ward.SetupToken{}, err
	}
	// One transaction decides whether the read counts: the hold must still
	// be live and the generation must be the one read, or no token leaves.
	err = r.Store.WriteInternal(readCtx, func(tx *store.InternalTx) error {
		held, err := tx.GetAuthStoreReadHold(readCtx, enrollment.AuthIdentityID, holder)
		if err != nil {
			return err
		}
		if !held.HeldAt(r.Now().UTC()) {
			return ErrCredentialReadHoldEnded
		}
		current, err := tx.CurrentEnrollmentGeneration(readCtx, enrollment.ID)
		if err != nil {
			return err
		}
		if current.Ordinal != ordinal {
			return ErrCredentialReadGenerationChanged
		}
		return nil
	})
	if err != nil {
		return ward.SetupToken{}, wrapCredentialRead(err)
	}
	return token, nil
}

// ReadUnheld is Read without the read hold, for a caller that already
// excludes every store mutation another way: preflight holds the daemon lock,
// which each command that enrolls, adopts, or renews takes first, and opens
// the store read-only, where a hold cannot be recorded. It keeps the
// generation and digest checks.
func (r CredentialReader) ReadUnheld(
	ctx context.Context, id domain.ClientEnrollmentID, ordinal int,
) (ward.SetupToken, error) {
	enrollment, err := r.enrollment(ctx, id)
	if err != nil {
		return ward.SetupToken{}, err
	}
	token, err := r.read(ctx, enrollment.ID, ordinal)
	if err != nil {
		return ward.SetupToken{}, err
	}
	if _, err := r.currentGeneration(ctx, enrollment.ID, ordinal); err != nil {
		return ward.SetupToken{}, err
	}
	return token, nil
}

// enrollment loads the stored enrollment and refuses any but a Claude Code
// setup-token one: only that store holds a token this read can take out.
func (r CredentialReader) enrollment(
	ctx context.Context, id domain.ClientEnrollmentID,
) (domain.ClientEnrollment, error) {
	if r.Store == nil || r.ReadSetupToken == nil {
		return domain.ClientEnrollment{}, errCredentialReadNilDependency
	}
	var enrollment domain.ClientEnrollment
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		enrollment, err = tx.GetClientEnrollment(ctx, id)
		return err
	}); err != nil {
		return domain.ClientEnrollment{}, fmt.Errorf("credential read: %w", err)
	}
	if enrollment.HarnessClient != domain.HarnessClientClaudeCode ||
		enrollment.AuthMethod != domain.AuthMethodSetupToken {
		return domain.ClientEnrollment{}, ErrCredentialReadUnsupported
	}
	return enrollment, nil
}

// read takes the token out of the volume the enrollment's current generation
// names, once that generation is the one asked for, and returns it only when
// the store is the one that generation recorded.
func (r CredentialReader) read(
	ctx context.Context, enrollment domain.ClientEnrollmentID, ordinal int,
) (ward.SetupToken, error) {
	generation, err := r.currentGeneration(ctx, enrollment, ordinal)
	if err != nil {
		return ward.SetupToken{}, err
	}
	seen, err := r.ReadSetupToken(ctx, generation.AuthStoreVolume)
	if err != nil {
		return ward.SetupToken{}, fmt.Errorf("credential read: %w", err)
	}
	// auth add records the token's hash and auth adopt the volume's tree
	// digest, so the recorded store matches either.
	if generation.StoreManifestDigest != seen.TokenDigest && generation.StoreManifestDigest != seen.TreeDigest {
		return ward.SetupToken{}, ErrCredentialReadDigestMismatch
	}
	return seen.Token, nil
}

func (r CredentialReader) currentGeneration(
	ctx context.Context, enrollment domain.ClientEnrollmentID, ordinal int,
) (domain.EnrollmentGeneration, error) {
	var generation domain.EnrollmentGeneration
	if err := r.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		generation, err = tx.CurrentEnrollmentGeneration(ctx, enrollment)
		return err
	}); err != nil {
		return domain.EnrollmentGeneration{}, fmt.Errorf("credential read: %w", err)
	}
	if generation.Ordinal != ordinal {
		return domain.EnrollmentGeneration{}, ErrCredentialReadGenerationChanged
	}
	return generation, nil
}

// releaseHold ends the hold on a context that survives the read's
// cancellation. Its error is dropped on purpose, for the integrity probe's
// reason: a hold that is not released expires on its own, and one that
// already expired refuses the release.
func (r CredentialReader) releaseHold(ctx context.Context, hold domain.AuthStoreReadHold) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	_ = r.Store.WriteInternal(releaseCtx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreReadHold(
			releaseCtx, hold.AuthIdentityID, hold.Holder, hold.AcquiredAt, r.Now())
	})
}

// wrapCredentialRead leaves this file's own refusals as they are and names
// the read on a store error.
func wrapCredentialRead(err error) error {
	if errors.Is(err, ErrCredentialReadHoldEnded) || errors.Is(err, ErrCredentialReadGenerationChanged) {
		return err
	}
	return fmt.Errorf("credential read: %w", err)
}

// newCredentialReadHolder mints a holder unique to one read: the read hold
// is keyed by holder, so two reads sharing one would share a window.
func newCredentialReadHolder() (domain.InvocationID, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", errors.New("credential read: mint the read hold's holder")
	}
	return domain.InvocationID("judgment-credential-read-" + hex.EncodeToString(token[:])), nil
}
