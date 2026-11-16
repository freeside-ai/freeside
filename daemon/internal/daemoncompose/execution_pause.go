package daemoncompose

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemonlock"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The execution pause hands the App authority from an executing prod to a
// deputy run without stopping prod's service. The operator requests it; prod
// starts no new work (store.RequireNewWorkOpen), waits until restart
// readiness counts no running work, records the pause released, and restarts.
// A prod that starts with the pause released serves driverless and never
// takes the App authority writer lock, so the deputy can. Resume clears the
// pause and restarts prod, which then runs its gated order again.
//
// The pause lives in the instance settings' execution_pause section, so it
// survives a restart: a prod restarted while the pause is requested keeps
// waiting, and one restarted after the release stays driverless.
//
// A released pause nobody uses keeps prod from executing with nothing to show
// for it, so once no rig lease manifest has existed for an hour the waiter
// raises an advisory card naming resume.

const (
	executionPauseItemPrefix = "system-health-execution-pause-"
	executionPauseCode       = "execution_pause"
	// The unused-pause card's prefix is not an extension of the pause card's,
	// so the pause card's prefix match never finds it.
	unusedExecutionPauseItemPrefix = "system-health-unused-execution-pause-"
	unusedExecutionPauseCode       = "execution_pause_unused"
	// executionPauseUnusedAfter is how long a released pause may go with no
	// rig lease manifest before its card is raised.
	executionPauseUnusedAfter = time.Hour
	// executionPauseSetBy records the daemon's own release of a pause.
	executionPauseSetBy = "daemon:execution-pause"
	// executionPauseInterval is how often the waiter reads restart
	// readiness while a pause is requested, and the section while none is.
	executionPauseInterval = 5 * time.Second
)

// ReadExecutionPause returns the pause in force, or nil.
func ReadExecutionPause(ctx context.Context, st *store.Store) (*domain.ExecutionPauseSettings, error) {
	var pause *domain.ExecutionPauseSettings
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pause, err = tx.ExecutionPause(ctx)
		return err
	})
	return pause, err
}

// RequestExecutionPause puts a pause in force and raises its card, in one
// write. A pause already in force, requested or released, is kept as it is,
// card and all, so a repeated request changes nothing.
//
// The card is advisory, not blocking. A blocking system_health card closes
// the unattended operating gate, which stops dispatch of an attempt admitted
// before the pause as well as new ones; that attempt's slot would then keep
// readiness above zero for good. New work is closed by the pause itself, and
// once prod restarts driverless the execution_paused card blocks.
func RequestExecutionPause(ctx context.Context, st *store.Store, setBy string, at time.Time) error {
	return st.Write(ctx, func(tx *store.WriteTx) error {
		pause, err := tx.ExecutionPause(ctx)
		if err != nil || pause != nil {
			return err
		}
		if err := appendExecutionPause(ctx, tx, domain.ExecutionPauseRequested, setBy, at); err != nil {
			return err
		}
		open, err := openPauseCards(ctx, &tx.ReadTx, executionPauseItemPrefix)
		if err != nil || len(open) != 0 {
			return err
		}
		return putPauseCard(ctx, tx, executionPauseItemPrefix, executionPauseCode,
			"Agent execution is pausing: an operator ran freesided execution pause. This daemon "+
				"starts no new work and waits for its running work to finish, then restarts without "+
				"execution so a deputy run can use the App authority. Run freesided execution resume "+
				"to cancel the pause, or to restart this daemon with execution once the deputy run ends.", at)
	})
}

// ResumeExecution clears the pause and concludes its cards, the pause card
// and any unused-pause card, in one write, and reports whether a pause was in
// force. It writes nothing when neither is.
// Restarting a running daemon is the caller's: offline, the next start reads
// the cleared section.
func ResumeExecution(ctx context.Context, st *store.Store, setBy string, at time.Time) (bool, error) {
	at = at.UTC()
	var cleared bool
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		pause, err := tx.ExecutionPause(ctx)
		if err != nil {
			return err
		}
		if pause != nil {
			if err := appendExecutionPause(ctx, tx, "", setBy, at); err != nil {
				return err
			}
			cleared = true
		}
		for _, prefix := range []string{executionPauseItemPrefix, unusedExecutionPauseItemPrefix} {
			if err := concludePauseCards(ctx, tx, prefix, at,
				"Resolved when the operator resumed execution."); err != nil {
				return err
			}
		}
		return nil
	})
	return cleared, err
}

// concludePauseCards concludes the open system_health cards whose IDs start
// with prefix.
func concludePauseCards(ctx context.Context, tx *store.WriteTx, prefix string, at time.Time, reason string) error {
	open, err := openPauseCards(ctx, &tx.ReadTx, prefix)
	if err != nil {
		return err
	}
	for _, item := range open {
		item, err := item.ConcludeByDaemon(domain.ConcludedByDaemon, at, reason, nil)
		if err != nil {
			return err
		}
		item.ItemVersion++
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

// releaseExecutionPause records a requested pause as released and reports
// whether it did. It re-reads the section in the write, so a resume that
// committed after the waiter's read is never overwritten.
func releaseExecutionPause(ctx context.Context, st *store.Store, at time.Time) (bool, error) {
	var released bool
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		pause, err := tx.ExecutionPause(ctx)
		if err != nil || pause == nil || pause.State != domain.ExecutionPauseRequested {
			return err
		}
		released = true
		return appendExecutionPause(ctx, tx, domain.ExecutionPauseReleased, executionPauseSetBy, at)
	})
	return released, err
}

// appendExecutionPause appends an execution_pause revision in state, or one
// that clears the section when state is empty.
func appendExecutionPause(
	ctx context.Context, tx *store.WriteTx, state domain.ExecutionPauseState, setBy string, at time.Time,
) error {
	var value json.RawMessage
	if state != "" {
		encoded, err := json.Marshal(domain.ExecutionPauseSettings{State: state})
		if err != nil {
			return err
		}
		value = encoded
	}
	_, err := tx.AppendInstanceSettingsRevision(ctx, domain.InstanceSettingsInput{
		Section: domain.InstanceSectionExecutionPause, Value: value, SetBy: setBy,
	}, at.UTC())
	return err
}

func openPauseCards(ctx context.Context, tx *store.ReadTx, prefix string) ([]domain.AttentionItem, error) {
	items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return nil, err
	}
	var open []domain.AttentionItem
	for _, item := range items {
		if strings.HasPrefix(string(item.ID), prefix) {
			open = append(open, item)
		}
	}
	return open, nil
}

// putPauseCard raises an advisory, acknowledge-only system_health card. Both
// pause cards are advisory: see RequestExecutionPause.
func putPauseCard(ctx context.Context, tx *store.WriteTx, prefix, code, reason string, at time.Time) error {
	serverState, err := tx.ServerState(ctx)
	if err != nil {
		return err
	}
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	names, err := tx.DisplayNamesFor(ctx, "project-system", subject)
	if err != nil {
		return err
	}
	at = at.UTC()
	posture := domain.HealthPostureAdvisory
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        domain.ItemID(fmt.Sprintf("%s%d", prefix, serverState.Revision+1)),
		ProjectID: "project-system", Subject: subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason:            reason,
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: code, Impairs: domain.ImpairedCapabilityUnattendedAdmission,
		},
		DisplayNames: names, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional,
		CreatedAt:         &at, Posture: &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}

// executionPauseWaiter releases a requested pause once nothing runs, then
// asks for the restart that brings prod back driverless. It runs for the
// daemon's life in every prod started with a driver, so a pause requested
// online and one interrupted by a restart are both served. In a prod that
// started with the pause released it watches for the pause going unused.
type executionPauseWaiter struct {
	store     *store.Store
	readiness func(context.Context) (signet.RestartReadinessResponse, error)
	restart   func()
	// leaseRoot is where a deputy run's rig lease manifest appears.
	leaseRoot string
	now       func() time.Time
	logger    *slog.Logger
}

// run returns only when ctx ends or a read or write fails; the caller
// supervises it, and a failure is a durable stop.
func (w executionPauseWaiter) run(ctx context.Context) error {
	ticker := time.NewTicker(executionPauseInterval)
	defer ticker.Stop()
	leaseSeen := w.now()
	for {
		released, err := w.step(ctx, &leaseSeen)
		if err != nil {
			return err
		}
		if released {
			w.logger.Info("execution pause released; restarting without execution")
			w.restart()
			<-ctx.Done()
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// step releases a requested pause once readiness counts nothing, and keeps
// the unused-pause card in step with a released one. leaseSeen is when a rig
// lease manifest was last seen, or when the pause was last not released.
func (w executionPauseWaiter) step(ctx context.Context, leaseSeen *time.Time) (bool, error) {
	pause, err := ReadExecutionPause(ctx, w.store)
	if err != nil {
		return false, err
	}
	if err := w.watchUnused(ctx, pause, leaseSeen); err != nil {
		return false, err
	}
	if pause == nil || pause.State != domain.ExecutionPauseRequested {
		return false, nil
	}
	readiness, err := w.readiness(ctx)
	if err != nil || len(readiness.Blockers) != 0 {
		return false, err
	}
	return releaseExecutionPause(ctx, w.store, w.now().UTC())
}

// watchUnused raises the unused-pause card once a released pause has gone
// executionPauseUnusedAfter with no rig lease manifest, and concludes it when
// a manifest appears or the pause is no longer released. A manifest that
// can't be read is logged and leaves the card as it is: the card is advisory,
// and a lease root gone bad must not stop a driverless prod's service.
func (w executionPauseWaiter) watchUnused(
	ctx context.Context, pause *domain.ExecutionPauseSettings, leaseSeen *time.Time,
) error {
	now := w.now()
	if pause == nil || pause.State != domain.ExecutionPauseReleased {
		*leaseSeen = now
		return syncUnusedPauseCard(ctx, w.store, false, "Resolved when the pause cleared.", now)
	}
	present, err := daemonlock.RigManifestPresent(w.leaseRoot)
	if err != nil {
		w.logger.Warn("execution pause: cannot check for a rig lease", "lease_root", w.leaseRoot, "error", err)
		return nil
	}
	if present {
		*leaseSeen = now
		return syncUnusedPauseCard(ctx, w.store, false, "Resolved when a deputy run took a rig lease.", now)
	}
	return syncUnusedPauseCard(ctx, w.store, now.Sub(*leaseSeen) >= executionPauseUnusedAfter, "", now)
}

// syncUnusedPauseCard raises the unused-pause card when want and none is
// open, or concludes the open ones with reason when not want. It reads before
// it writes, so a tick that changes nothing takes no write. The write re-reads
// the pause and raises no card unless it is still released, so a resume that
// committed after the waiter's read never leaves a card behind.
func syncUnusedPauseCard(ctx context.Context, st *store.Store, want bool, reason string, at time.Time) error {
	var open []domain.AttentionItem
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		open, err = openPauseCards(ctx, tx, unusedExecutionPauseItemPrefix)
		return err
	}); err != nil {
		return err
	}
	if want == (len(open) != 0) {
		return nil
	}
	at = at.UTC()
	return st.Write(ctx, func(tx *store.WriteTx) error {
		if !want {
			return concludePauseCards(ctx, tx, unusedExecutionPauseItemPrefix, at, reason)
		}
		pause, err := tx.ExecutionPause(ctx)
		if err != nil || pause == nil || pause.State != domain.ExecutionPauseReleased {
			return err
		}
		open, err := openPauseCards(ctx, &tx.ReadTx, unusedExecutionPauseItemPrefix)
		if err != nil || len(open) != 0 {
			return err
		}
		return putPauseCard(ctx, tx, unusedExecutionPauseItemPrefix, unusedExecutionPauseCode,
			"Agent execution has been paused for over an hour with no deputy run holding a rig "+
				"lease. This daemon serves without execution until the pause clears. Run freesided "+
				"execution resume to restart it with execution.", at)
	})
}

// ExecutionPauseStatus is what the execution pause commands answer.
type ExecutionPauseStatus struct {
	// Pause is the pause in force, or nil when none is.
	Pause *domain.ExecutionPauseSettings `json:"pause"`
	// Readiness is the daemon's restart readiness: the running work a
	// requested pause still waits on.
	Readiness signet.RestartReadinessResponse `json:"readiness"`
	// Executes reports that the answering daemon runs a driver, and so may
	// hold the App authority writer lock. A released pause reported by a
	// daemon that executes is the one about to restart.
	Executes bool `json:"executes"`
	// Restarting reports that the daemon asked to be restarted.
	Restarting bool `json:"restarting"`
}

// ExecutionPauseControl is what the execution pause control route acts
// through in a prod started with a driver.
type ExecutionPauseControl struct {
	store     *store.Store
	readiness func(context.Context) (signet.RestartReadinessResponse, error)
	restart   func()
	now       func() time.Time
	// executes reports that this daemon's gated order let a driver run.
	executes bool
}

// Pause requests a pause, or keeps the one in force, and reports it.
func (c *ExecutionPauseControl) Pause(ctx context.Context, setBy string) (ExecutionPauseStatus, error) {
	if err := RequestExecutionPause(ctx, c.store, setBy, c.now().UTC()); err != nil {
		return ExecutionPauseStatus{}, fmt.Errorf("execution pause: %w", err)
	}
	return c.Status(ctx)
}

// Resume clears the pause, and restarts the daemon when it runs without a
// driver, even when no pause was in force: a prod that came up driverless
// beside a deputy names resume as the way to retry its gated order once the
// deputy ends. A daemon that executes is not restarted, so resume cancels a
// requested pause without interrupting the work the pause was waiting on.
func (c *ExecutionPauseControl) Resume(ctx context.Context, setBy string) (ExecutionPauseStatus, error) {
	if _, err := ResumeExecution(ctx, c.store, setBy, c.now().UTC()); err != nil {
		return ExecutionPauseStatus{}, fmt.Errorf("execution resume: %w", err)
	}
	status, err := c.Status(ctx)
	if err != nil {
		return ExecutionPauseStatus{}, err
	}
	if !c.executes {
		c.restart()
		status.Restarting = true
	}
	return status, nil
}

// Status reports the pause and restart readiness, and changes nothing.
func (c *ExecutionPauseControl) Status(ctx context.Context) (ExecutionPauseStatus, error) {
	pause, err := ReadExecutionPause(ctx, c.store)
	if err != nil {
		return ExecutionPauseStatus{}, fmt.Errorf("execution pause: %w", err)
	}
	readiness, err := c.readiness(ctx)
	if err != nil {
		return ExecutionPauseStatus{}, fmt.Errorf("execution pause: %w", err)
	}
	return ExecutionPauseStatus{Pause: pause, Readiness: readiness, Executes: c.executes}, nil
}

// nowOrWallClock is now, or the wall clock when the start was given none.
func nowOrWallClock(now func() time.Time) func() time.Time {
	if now != nil {
		return now
	}
	return time.Now
}
