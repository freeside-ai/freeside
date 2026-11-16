package daemoncompose

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

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

const (
	executionPauseItemPrefix = "system-health-execution-pause-"
	executionPauseCode       = "execution_pause"
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
		open, err := openExecutionPauseCards(ctx, tx)
		if err != nil || len(open) != 0 {
			return err
		}
		return putExecutionPauseCard(ctx, tx, at)
	})
}

// ResumeExecution clears the pause and concludes its card, in one write, and
// reports whether a pause was in force. It writes nothing when neither is.
// Restarting a running daemon is the caller's: offline, the next start reads
// the cleared section.
func ResumeExecution(ctx context.Context, st *store.Store, setBy string, at time.Time) (bool, error) {
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
		open, err := openExecutionPauseCards(ctx, tx)
		if err != nil {
			return err
		}
		for _, item := range open {
			item, err := item.ConcludeByDaemon(domain.ConcludedByDaemon, at,
				"Resolved when the operator resumed execution.", nil)
			if err != nil {
				return err
			}
			item.ItemVersion++
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	})
	return cleared, err
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

func openExecutionPauseCards(ctx context.Context, tx *store.WriteTx) ([]domain.AttentionItem, error) {
	items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return nil, err
	}
	var open []domain.AttentionItem
	for _, item := range items {
		if strings.HasPrefix(string(item.ID), executionPauseItemPrefix) {
			open = append(open, item)
		}
	}
	return open, nil
}

func putExecutionPauseCard(ctx context.Context, tx *store.WriteTx, at time.Time) error {
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
		ID:        domain.ItemID(fmt.Sprintf("%s%d", executionPauseItemPrefix, serverState.Revision+1)),
		ProjectID: "project-system", Subject: subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason: "Agent execution is pausing: an operator ran freesided execution pause. This daemon " +
			"starts no new work and waits for its running work to finish, then restarts without " +
			"execution so a deputy run can use the App authority. Run freesided execution resume " +
			"to cancel the pause, or to restart this daemon with execution once the deputy run ends.",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: executionPauseCode, Impairs: domain.ImpairedCapabilityUnattendedAdmission,
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
// online and one interrupted by a restart are both served.
type executionPauseWaiter struct {
	store     *store.Store
	readiness func(context.Context) (signet.RestartReadinessResponse, error)
	restart   func()
	now       func() time.Time
	logger    *slog.Logger
}

// run returns only when ctx ends or a read or write fails; the caller
// supervises it, and a failure is a durable stop.
func (w executionPauseWaiter) run(ctx context.Context) error {
	ticker := time.NewTicker(executionPauseInterval)
	defer ticker.Stop()
	for {
		released, err := w.step(ctx)
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

func (w executionPauseWaiter) step(ctx context.Context) (bool, error) {
	pause, err := ReadExecutionPause(ctx, w.store)
	if err != nil || pause == nil || pause.State != domain.ExecutionPauseRequested {
		return false, err
	}
	readiness, err := w.readiness(ctx)
	if err != nil || len(readiness.Blockers) != 0 {
		return false, err
	}
	return releaseExecutionPause(ctx, w.store, w.now().UTC())
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
