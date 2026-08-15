package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The engine writes the run-observation projection (issue #394): milestones
// in the transactions that commit workflow facts, plus paced hold and
// liveness observations from its reconcile passes. Everything here is
// projection: no engine, recovery, or publication decision reads it back.

// observationRefreshInterval paces the per-pass observation upserts: an
// unchanged observation is rewritten at most this often, so the 100 ms
// reconcile cadence does not turn into constant WAL churn while a run
// executes. It is deliberately far inside the reader-side
// domain.DefaultObservationFreshnessWindow, so a paced write is never
// mistaken for an observation gap.
const observationRefreshInterval = 10 * time.Second

// observationStamp is the last written observation state for one key, used
// only for pacing (never as authority).
type observationStamp struct {
	state string
	at    time.Time
}

// observationPace decides whether a changed-or-stale write is due and
// remembers what was written. It is engine-process state: a restart forgets
// it, which is correct, because the first pass after a restart should
// re-observe everything.
type observationPace struct {
	mu    sync.Mutex
	stamp map[string]observationStamp
}

// observationPaceSweepSize bounds the pacing map for the daemon lifetime: a
// concluded run stops being observed, so its stamp only goes stale, and the
// sweep drops stale stamps once the map is large enough to matter. Dropping
// a live stamp is harmless — the next pass rewrites it (one extra upsert).
const observationPaceSweepSize = 256

func (p *observationPace) due(key, state string, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stamp == nil {
		p.stamp = make(map[string]observationStamp)
	}
	// A negative age means the clock stepped back past the stamp; suppressing
	// until wall time caught up would leave a future-dated observation
	// standing (which readers derive as a gap), so a rolled-back clock is
	// immediately due and the stamp is replaced.
	last, ok := p.stamp[key]
	if ok && last.state == state {
		if age := now.Sub(last.at); age >= 0 && age < observationRefreshInterval {
			return false
		}
	}
	if len(p.stamp) >= observationPaceSweepSize {
		for staleKey, stamp := range p.stamp {
			if age := now.Sub(stamp.at); age >= observationRefreshInterval || age < 0 {
				delete(p.stamp, staleKey)
			}
		}
	}
	p.stamp[key] = observationStamp{state: state, at: now}
	return true
}

// forget drops one stamp so a write that failed after due() said yes is not
// suppressed on the retrying pass.
func (p *observationPace) forget(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.stamp, key)
}

// observedStatus maps the driver vocabulary onto the observation mirror.
// Behaviour dispatch, so no default: a new driver status must declare its
// observed form; the trailing return rejects the invalid zero value.
func observedStatus(s exec.Status) (domain.ObservedInvocationStatus, bool) {
	switch s {
	case exec.StatusPending:
		return domain.ObservedStatusPending, true
	case exec.StatusRunning:
		return domain.ObservedStatusRunning, true
	case exec.StatusCompleted:
		return domain.ObservedStatusCompleted, true
	case exec.StatusFailed:
		return domain.ObservedStatusFailed, true
	case exec.StatusCanceled:
		return domain.ObservedStatusCanceled, true
	case exec.StatusGone:
		return domain.ObservedStatusGone, true
	}
	return "", false
}

// dispatchHoldReason classifies a dispatch-path hold or refusal error onto
// the closed reason vocabulary. Only the code crosses into the observation;
// an error that classifies onto no code records no hold and keeps its
// ordinary loud failure path. Backup-protection sentinels are matched before
// the general admission-policy class because they are a distinct operator
// action.
func dispatchHoldReason(err error) (domain.RunHoldReason, bool) {
	switch {
	case errors.Is(err, domain.ErrUnattendedOperationStopped):
		return domain.HoldOperationStopped, true
	case errors.Is(err, domain.ErrBlockingSystemHealth):
		return domain.HoldBlockingSystemHealth, true
	case errors.Is(err, exec.ErrInputUnavailable):
		return domain.HoldInputUnavailable, true
	case errors.Is(err, domain.ErrIdentityParallelismExhausted):
		return domain.HoldIdentityParallelism, true
	case backendConformanceRefusal(err):
		return domain.HoldBackendNotConformant, true
	case errors.Is(err, domain.ErrBackupHealthUnavailable),
		errors.Is(err, domain.ErrCheckpointNotEncrypted),
		errors.Is(err, domain.ErrCheckpointNotCurrent),
		errors.Is(err, domain.ErrArtifactClosureIncomplete),
		errors.Is(err, domain.ErrRestoreTestStale),
		errors.Is(err, domain.ErrInvalidBackupHealthStatus):
		return domain.HoldBackupProtectionUnready, true
	case errors.Is(err, store.ErrRepositoryUntrusted):
		return domain.HoldRepositoryUntrusted, true
	case errors.Is(err, publish.ErrJanitorInactive):
		return domain.HoldProviderAuthorityUnavailable, true
	case errors.Is(err, exec.ErrCapabilityRefused),
		errors.Is(err, exec.ErrPreJobRefused),
		errors.Is(err, domain.ErrUnknownAdmissionFloor),
		errors.Is(err, domain.ErrCapabilityBelowFloor),
		errors.Is(err, domain.ErrCredentialModeNotApproved),
		errors.Is(err, domain.ErrWaiverNotConfigured),
		errors.Is(err, domain.ErrRepositoryIdentityMismatch),
		errors.Is(err, domain.ErrPathBoundaryMismatch),
		errors.Is(err, domain.ErrTrustProfileSuperseded),
		errors.Is(err, domain.ErrReviewConfigurationUnapproved):
		return domain.HoldAdmissionPolicyRefused, true
	}
	return "", false
}

// productionBlockReason maps a definitive publication block reason onto its
// closed code. The vocabulary is exactly the daemon's own definitive
// reasons; recovery already fails closed on any other string, so an
// unmatched reason here is the same contract violation.
func productionBlockReason(reason string) (domain.RunHoldReason, bool) {
	if mapped, ok := domain.DefinitivePublicationBlockReason(reason); ok {
		return mapped, true
	}
	if reason == productionBlockExternal {
		return domain.HoldExternalConflict, true
	}
	return "", false
}

// observeInvocation records one paced liveness observation from a driver
// inspection. The observation carries only the mirrored status and the live
// bit: never the driver's summary, transcript, or any other payload.
func (e *Engine) observeInvocation(
	ctx context.Context, runID domain.RunID,
	invocationID domain.InvocationID, inspection exec.Inspection,
) error {
	status, ok := observedStatus(inspection.Status)
	if !ok {
		// The caller validates the inspection on its own path; an
		// unclassifiable status records nothing here.
		return nil
	}
	now := time.Now().UTC()
	state := string(status)
	if inspection.Live {
		state += "+live"
	}
	key := "inv:" + string(invocationID)
	if !e.pace.due(key, state, now) {
		return nil
	}
	if err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordInvocationObservation(ctx, domain.InvocationObservation{
			InvocationID: invocationID, RunID: runID,
			Status: status, Live: inspection.Live, ObservedAt: now,
		})
	}); err != nil {
		e.pace.forget(key)
		return err
	}
	return nil
}

// observeRunHold records one paced hold observation for a run.
func (e *Engine) observeRunHold(
	ctx context.Context, runID domain.RunID,
	invocationID domain.InvocationID, reason domain.RunHoldReason,
) error {
	now := time.Now().UTC()
	key := "hold:" + string(runID)
	if !e.pace.due(key, string(reason), now) {
		return nil
	}
	if err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		return recordRunHold(ctx, tx, runID, invocationID, reason, now)
	}); err != nil {
		e.pace.forget(key)
		return err
	}
	return nil
}

// recordRunHold is the shared write: the publication workflow uses it
// directly because its retry window already paces it.
func recordRunHold(
	ctx context.Context, tx *store.WriteTx, runID domain.RunID,
	invocationID domain.InvocationID, reason domain.RunHoldReason, now time.Time,
) error {
	hold := domain.RunHoldObservation{
		RunID: runID, Reason: reason,
		FirstObservedAt: now, LastObservedAt: now,
	}
	if invocationID != "" {
		hold.InvocationID = &invocationID
	}
	return tx.RecordRunHold(ctx, hold)
}
