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

// recordEnrollingIdentity records a new identity as declared (it must name a
// cost owner), or binds an
// existing one for its first enrollment. An existing identity must agree on
// the fixed bindings; its account binding and cost owner are set when empty
// and must match when set, so enrollment never rebinds an account or silently
// changes who pays, and it leaves no enrolled identity without a cost owner. Everything else keeps the stored value: enrollment is not
// the place to re-enable an identity or re-measure its limit. The store's
// own gates (the set-once transition, one identity per account binding) still
// run underneath on the write.
func recordEnrollingIdentity(
	ctx context.Context, tx *store.InternalTx, identity domain.AuthIdentity, now time.Time,
) error {
	stored, err := tx.GetAuthIdentity(ctx, identity.ID)
	if errors.Is(err, store.ErrNotFound) {
		// §5.4: every selection reads and records the cost owner, so an
		// identity is never born without one.
		if identity.CostOwner == "" {
			return fmt.Errorf("new auth identity %s needs a cost owner", identity.ID)
		}
		return tx.RecordAuthIdentity(ctx, identity, now)
	}
	if err != nil {
		return err
	}
	if !stored.SameFixedBindings(identity) {
		return fmt.Errorf("existing auth identity %s has incompatible fixed bindings: %w",
			identity.ID, domain.ErrImmutableTransition)
	}
	next := stored
	switch stored.AccountBinding {
	case identity.AccountBinding:
	case "":
		next.AccountBinding = identity.AccountBinding
	default:
		return fmt.Errorf("auth identity %s is bound to a different account: %w",
			identity.ID, domain.ErrAccountBindingMismatch)
	}
	if identity.CostOwner != "" && stored.CostOwner != identity.CostOwner {
		if stored.CostOwner != "" {
			return fmt.Errorf("auth identity %s has cost owner %q, not %q",
				identity.ID, stored.CostOwner, identity.CostOwner)
		}
		next.CostOwner = identity.CostOwner
	}
	// A flag-era identity may still have no cost owner. Its enrollment must
	// supply one: once the first generation lands, the next add refuses as an
	// existing enrollment, so an ownerless identity would stay ownerless.
	if next.CostOwner == "" {
		return fmt.Errorf("auth identity %s has no cost owner; name one to enroll it", identity.ID)
	}
	if next == stored {
		return nil
	}
	return tx.RecordAuthIdentity(ctx, next, now)
}

// recordEnrollmentBootstrap records the bootstrap's enrollment, or accepts an
// identical enrollment that holds no generation yet: a bootstrap that failed
// before its verified append leaves exactly that, and refusing it would strand
// the enrollment with no store and no way to add one. Any other existing
// enrollment under the id refuses with ward.ErrEnrollmentExists.
func recordEnrollmentBootstrap(
	ctx context.Context, tx *store.InternalTx, bootstrap ward.EnrollmentBootstrap, now time.Time,
) error {
	enrollment := bootstrap.Enrollment
	if bootstrap.Binding.EnrollmentID != enrollment.ID || bootstrap.Binding.Generation != 0 {
		return errors.New("enrollment bootstrap binding does not name its enrollment's first generation")
	}
	existing, err := tx.GetClientEnrollment(ctx, enrollment.ID)
	if errors.Is(err, store.ErrNotFound) {
		return tx.RecordClientEnrollment(ctx, enrollment, now)
	}
	if err != nil {
		return err
	}
	if existing != enrollment {
		return fmt.Errorf("client enrollment %s: %w", enrollment.ID, ward.ErrEnrollmentExists)
	}
	_, err = tx.CurrentEnrollmentGeneration(ctx, enrollment.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("client enrollment %s: %w", enrollment.ID, ward.ErrEnrollmentExists)
}
