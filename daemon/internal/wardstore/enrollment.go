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

// ClaudeEnrollment backs ward's Claude setup-token enrollment port. Its
// records are daemon-internal (identity, enrollment, lease, generation), so
// every write runs on an internal transaction with no revision bump.
type ClaudeEnrollment struct {
	store *store.Store
}

// Begin binds or records the identity, records the enrollment, and takes the
// lease bound to its first generation, all in one transaction, so no fence
// ever authors a store for an enrollment that was not recorded.
func (a *ClaudeEnrollment) Begin(
	ctx context.Context,
	identity domain.AuthIdentity,
	bootstrap ward.EnrollmentBootstrap,
	holder domain.InvocationID,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	var lease domain.AuthStoreMutationLease
	err := a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := recordEnrollingIdentity(ctx, tx, identity, now); err != nil {
			return err
		}
		if err := recordEnrollmentBootstrap(ctx, tx, bootstrap, now); err != nil {
			return err
		}
		binding := bootstrap.Binding
		var err error
		lease, err = tx.AcquireAuthStoreMutationLeaseBound(ctx, identity.ID, holder, &binding, now, expiresAt)
		if err != nil {
			return err
		}
		if !lease.AcquiredAt.Equal(now) || !lease.ExpiresAt.Equal(expiresAt) {
			return errors.New("begin Claude enrollment: acquisition converged on an existing lease window")
		}
		return nil
	})
	return lease, err
}

// AppendGeneration records the authored store under the live bound lease.
func (a *ClaudeEnrollment) AppendGeneration(
	ctx context.Context, generation domain.EnrollmentGeneration, now time.Time,
) (domain.EnrollmentGeneration, error) {
	var stamped domain.EnrollmentGeneration
	err := a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		stamped, err = tx.AppendEnrollmentGeneration(ctx, generation, now)
		return err
	})
	return stamped, err
}

// Adoption backs ward's adoption port: the first-enrollment writes, plus the
// read that finds an enrollment an identity already holds.
type Adoption struct {
	*ClaudeEnrollment
}

// Enrolled returns the identity's enrollment for the client and its current
// generation; found is false when none holds a generation yet.
func (a *Adoption) Enrolled(
	ctx context.Context, identity domain.AuthIdentityID, client domain.HarnessClientKind,
) (domain.ClientEnrollment, domain.EnrollmentGeneration, bool, error) {
	var (
		enrollment domain.ClientEnrollment
		generation domain.EnrollmentGeneration
		found      bool
	)
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		enrollments, err := tx.ListClientEnrollments(ctx, identity)
		if err != nil {
			return err
		}
		for _, candidate := range enrollments {
			if candidate.HarnessClient != client {
				continue
			}
			current, err := tx.CurrentEnrollmentGeneration(ctx, candidate.ID)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			if found {
				return fmt.Errorf("auth identity %s holds more than one %s enrollment", identity, client)
			}
			enrollment, generation, found = candidate, current, true
		}
		return nil
	})
	if err != nil {
		return domain.ClientEnrollment{}, domain.EnrollmentGeneration{}, false, err
	}
	return enrollment, generation, found, nil
}
