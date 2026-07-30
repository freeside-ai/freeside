package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// validateExecutionCandidate authenticates the frozen execution authority
// behind a real publication before its reservation is settled. The
// reservation supplies the run join (#308); the producing invocation supplies
// the attempt/admission/export join (#301). GetExecutionExportRecord
// deliberately avoids re-applying mutable current admission policy to
// terminal history while still re-validating every immutable binding.
func validateExecutionCandidate(
	ctx context.Context,
	tx *store.InternalTx,
	c Candidate,
	claim *Reservation,
	producingInvocationID domain.InvocationID,
	targetRepositoryID int64,
	authorizedBaseSHA string,
) error {
	if claim == nil || claim.RunID != c.RunID {
		return fmt.Errorf(
			"execution-bound publication has no matching run reservation: %w",
			ErrInvocationReserved,
		)
	}
	if producingInvocationID == "" {
		return fmt.Errorf(
			"execution-bound publication has an empty producing invocation: %w",
			ErrExecutionExportMissing,
		)
	}
	key, err := claim.Key()
	if err != nil {
		return err
	}
	if err := validateExecutionReservation(
		ctx, tx, key, claim, producingInvocationID,
	); err != nil {
		return err
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, producingInvocationID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf(
			"producing invocation %q has no execution admission: %w",
			producingInvocationID, ErrExecutionExportMissing,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"authenticate producing invocation %q admission: %w",
			producingInvocationID, err,
		)
	}
	if admission.OperatingMode != domain.ModeUnattended {
		return fmt.Errorf(
			"producing invocation %q ran under mode %q, not unattended: %w",
			producingInvocationID, admission.OperatingMode,
			ErrUnauthorizedPublication,
		)
	}
	if admission.RunID != claim.RunID {
		return fmt.Errorf(
			"producing invocation %q belongs to run %q, reservation belongs to %q: %w",
			producingInvocationID, admission.RunID, claim.RunID,
			domain.ErrParentKeyMismatch,
		)
	}
	if admission.Base.Repo != c.Repo ||
		admission.Base.RepositoryID != targetRepositoryID ||
		admission.Base.BaseRef != c.BaseRef ||
		admission.Base.BaseSHA != authorizedBaseSHA {
		return fmt.Errorf(
			"producing invocation %q ran against %s (%d) at %s=%s, candidate publishes to %s (%d) at %s=%s: %w",
			producingInvocationID,
			admission.Base.Repo, admission.Base.RepositoryID,
			admission.Base.BaseRef, admission.Base.BaseSHA,
			c.Repo, targetRepositoryID, c.BaseRef, authorizedBaseSHA,
			domain.ErrParentKeyMismatch,
		)
	}
	export, err := tx.GetExecutionExportRecord(ctx, producingInvocationID)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf(
			"producing invocation %q has no execution export: %w",
			producingInvocationID, ErrExecutionExportMissing,
		)
	}
	if err != nil {
		return fmt.Errorf(
			"authenticate producing invocation %q export: %w",
			producingInvocationID, err,
		)
	}
	if export.HeadSHA != c.HeadSHA {
		return fmt.Errorf(
			"candidate head %s, execution export %q records %s: %w",
			c.HeadSHA, producingInvocationID, export.HeadSHA,
			ErrExecutionExportHeadMismatch,
		)
	}
	return nil
}

// validateExecutionReservation requires the durable state that distinguishes
// an owner-settled execution intent from a caller-derived claim. A reservation
// row proves ownership before first settlement; an already-promoted retry must
// persist the same run and source invocation in its intent.
func validateExecutionReservation(
	ctx context.Context,
	tx *store.InternalTx,
	key string,
	claim *Reservation,
	producingInvocationID domain.InvocationID,
) error {
	if claim == nil {
		return fmt.Errorf(
			"execution-bound publication has no reservation claim: %w",
			ErrInvocationReserved,
		)
	}
	claimKey, err := claim.Key()
	if err != nil {
		return err
	}
	if claimKey != key {
		return fmt.Errorf(
			"execution publication reservation for %q presented at %q: %w",
			claimKey, key, domain.ErrParentKeyMismatch,
		)
	}
	state, entry, err := classifyInvocation(ctx, tx, *claim)
	if err != nil {
		return fmt.Errorf("authenticate execution publication reservation: %w", err)
	}
	return validateExecutionReservationState(
		state, entry, *claim, producingInvocationID,
	)
}

func validateExecutionReservationState(
	state invocationState,
	entry store.QueueEntry,
	claim Reservation,
	producingInvocationID domain.InvocationID,
) error {
	switch state {
	case invocationFree:
		return fmt.Errorf(
			"execution-bound publication invocation %q has no durable reservation: %w",
			claim.InvocationID, ErrInvocationReserved,
		)
	case invocationCommitted:
		intent, err := DecodeIntent(entry.Payload)
		if err != nil {
			return fmt.Errorf(
				"authenticate execution publication intent %q: %w",
				entry.IdempotencyKey, err,
			)
		}
		if intent.ProducingInvocationID != producingInvocationID ||
			intent.ReservationRunID != claim.RunID {
			return fmt.Errorf(
				"execution publication intent %q does not prove reservation by run %q: %w",
				entry.IdempotencyKey, claim.RunID, ErrInvocationReserved,
			)
		}
		return nil
	case invocationReserved:
		return nil
	}
	return unhandledInvocationState(state)
}
