package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// admissionConfigurationChangedPrefix names the notice of an attempt the
// driver started under a backend configuration this daemon no longer yields
// (readmitStaleAdmission).
const admissionConfigurationChangedPrefix = "admission-configuration-changed-"

// runningConfigurationDigest is the backend configuration digest a fresh
// unattended admission of an attempt on image binds: the digest admitAttemptIn
// reads for the image. For the ward backend it is the function the start
// authority checks a recorded admission against
// (admissionauth.Authority.BackendConfigurationDigestFor), and it is fixed for
// the life of the process.
func (a *admitter) runningConfigurationDigest(image domain.ImageRef) domain.Digest {
	if bound, ok := a.backend.(exec.ImageBoundBackend); ok {
		digest, _, _ := bound.ConformanceFor(string(image))
		return digest
	}
	return a.backendConfigurationDigest
}

// readmitStaleAdmission gives a recorded, never-started unattended attempt a
// fresh admission when the daemon or its backend changed since the attempt
// was admitted (issue #3433). The start authority refuses an admission bound
// to a configuration the running process does not yield, and a recorded
// attempt replays only its stored admission, so without this the attempt
// would hold on every pass, for ever.
//
// It acts only when this engine admits unattended work and the stored
// admission is unattended and binds another configuration than the one this
// process yields for its image. Then the driver decides: an invocation it has
// no intent for never started, because the driver commits its intent before
// any runtime object exists, and only this engine's pass calls Start. That
// attempt is admitted again under the running configuration and the stored
// admission is superseded in one write (store.SupersedeUnstartedExecutionAdmission).
// A refused fresh admission returns its refusal, so the attempt holds as a new
// admission would, keeps its stored admission, and a later pass retries.
//
// An invocation the driver knows did start, and its writer may have run, so
// it is never admitted again: its stored admission still binds it, and the
// stage driver's own recovery owns it. It keeps the backend_not_conformant
// hold its start refusal would record, and raises one execution_failure
// notice on the run so the operator learns why it waits; later passes
// converge on the open notice. Starting the invocation releases it, and
// closing the task's workflow expires it. Any other Inspect error fails
// closed: nothing is superseded.
//
// reserved reports, with a refusal, whether the stored admission carries a
// dispatch reservation for the caller to release (Engine.refuseStart).
func (e *Engine) readmitStaleAdmission(
	ctx context.Context, binding invocationBinding, stage domain.Stage, invocationID domain.InvocationID,
) (reserved bool, err error) {
	if e.admission == nil || e.admission.environment.OperatingMode != domain.ModeUnattended {
		return false, nil
	}
	var (
		stale domain.ExecutionAdmission
		found bool
	)
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		// The authenticated record, not the current-policy read: whether to
		// supersede is a question about the record as it stands, and the
		// replay that follows re-gates whichever admission it finds.
		stale, err = tx.GetExecutionAdmissionRecord(ctx, invocationID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	}); err != nil {
		return false, fmt.Errorf("read admission of invocation %q: %w", invocationID, err)
	}
	// An attempt with no admission is recordAttempt's to refuse.
	if !found || stale.OperatingMode != domain.ModeUnattended {
		return false, nil
	}
	running := e.admission.runningConfigurationDigest(stale.ImageRef)
	if running == stale.BackendConfigurationDigest {
		return false, nil
	}
	reserved = stale.AuthIdentityID != nil
	if _, err := e.driver.Inspect(ctx, invocationID); err == nil {
		if err := recordProductionQuarantine(ctx, e.store, e.signet, admissionConfigurationChangedPrefix,
			binding.run.ID, binding.run.ProjectID, admissionConfigurationChangedReason(invocationID),
		); err != nil {
			return reserved, err
		}
		return reserved, fmt.Errorf("invocation %q started under backend configuration %s, this daemon yields %s for image %s: %w",
			invocationID, stale.BackendConfigurationDigest, running, stale.ImageRef,
			domain.ErrAdmissionConfigurationMismatch)
	} else if !errors.Is(err, exec.ErrUnknownInvocation) {
		return reserved, fmt.Errorf("inspect invocation %q before re-admission: %w", invocationID, err)
	}
	fresh, _, _, err := e.admitAttempt(ctx, binding, stage, invocationID)
	if err != nil {
		return reserved, err
	}
	return reserved, e.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := requireTaskExecutionOpen(ctx, &tx.ReadTx, binding.run.ID); err != nil {
			return err
		}
		if err := e.requireReviewConfigurationApproved(ctx, tx, fresh); err != nil {
			return err
		}
		run, err := tx.GetRun(ctx, binding.run.ID)
		if err != nil {
			return err
		}
		memory, err := e.executionMemory(ctx, &tx.ReadTx, run, fresh)
		if err != nil {
			return err
		}
		return tx.SupersedeUnstartedExecutionAdmission(ctx, stale, fresh, memory)
	})
}

// admissionConfigurationChangedReason is the notice's operator-facing reason.
func admissionConfigurationChangedReason(invocationID domain.InvocationID) string {
	return fmt.Sprintf("Invocation %s started before the daemon or its runner backend changed, "+
		"so this daemon cannot run or recover it, and it waits under backend_not_conformant. "+
		"Restarting the daemon on the configuration it started under lets it continue; "+
		"otherwise stop the task.", invocationID)
}
