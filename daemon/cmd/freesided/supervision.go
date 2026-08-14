package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// version is set by release or operator builds with -ldflags -X. Local builds
// fall back to the module version embedded by the Go toolchain.
var version string

func buildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "devel"
}

const readinessFileName = "readiness.json"

func publishReadiness(stateDir string, ready readiness) error {
	if stateDir == "" {
		return nil
	}
	body, err := json.Marshal(ready)
	if err != nil {
		return fmt.Errorf("encode readiness file: %w", err)
	}
	body = append(body, '\n')
	path := filepath.Join(stateDir, readinessFileName)
	if err := atomicfile.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("publish readiness file %q: %w", path, err)
	}
	return nil
}

type componentKind string

const (
	componentHTTP                  componentKind = "http"
	componentWorkflow              componentKind = "workflow"
	componentLocalBackups          componentKind = "local_backups"
	componentScheduler             componentKind = "scheduler"
	componentProductionPublication componentKind = "production_publication"
	componentActiveResource        componentKind = "active_resource"
	componentLabelIntake           componentKind = "label_intake"
	componentPanic                 componentKind = "panic"
)

// AllComponentKinds is the complete exit-classification registration set.
var AllComponentKinds = []componentKind{
	componentHTTP,
	componentWorkflow,
	componentLocalBackups,
	componentScheduler,
	componentProductionPublication,
	componentActiveResource,
	componentLabelIntake,
	componentPanic,
}

func (k componentKind) valid() bool {
	switch k {
	case componentHTTP,
		componentWorkflow,
		componentLocalBackups,
		componentScheduler,
		componentProductionPublication,
		componentActiveResource,
		componentLabelIntake,
		componentPanic:
		return true
	default:
		return false
	}
}

type exitDisposition string

const (
	exitDurableStop exitDisposition = "durable_stop"
	exitRestartSafe exitDisposition = "restart_safe"
	exitInvoluntary exitDisposition = "involuntary"
)

// AllExitDispositions is the complete process-exit classification set.
var AllExitDispositions = []exitDisposition{
	exitDurableStop,
	exitRestartSafe,
	exitInvoluntary,
}

func (d exitDisposition) valid() bool {
	switch d {
	case exitDurableStop, exitRestartSafe, exitInvoluntary:
		return true
	default:
		return false
	}
}

func classifyComponentExit(kind componentKind) exitDisposition {
	switch kind {
	case componentWorkflow,
		componentLocalBackups,
		componentScheduler,
		componentProductionPublication,
		componentActiveResource,
		componentLabelIntake:
		return exitDurableStop
	case componentHTTP:
		return exitRestartSafe
	case componentPanic:
		return exitInvoluntary
	}
	return exitInvoluntary
}

func (d *daemon) componentExited(
	lifetimeCtx, workerCtx context.Context,
	kind componentKind,
	err error,
) {
	if lifetimeCtx.Err() != nil || workerCtx.Err() != nil {
		return
	}
	if err == nil {
		err = errors.New("component stopped unexpectedly")
	}
	cause := fmt.Errorf("%s: %w", kind, err)
	switch classifyComponentExit(kind) {
	case exitDurableStop:
		d.durableStop(lifetimeCtx, cause)
	case exitRestartSafe:
		d.errs <- cause
	case exitInvoluntary:
		panic(cause)
	}
}

const durableStopItemPrefix = "system-health-daemon-stop-"

func (d *daemon) durableStop(ctx context.Context, cause error) {
	d.logger.Error("daemon entered a durable stop", "error", cause)
	for {
		if err := d.fileDurableStop(ctx, cause); err == nil {
			d.logger.Error("daemon durable stop recorded", "error", cause)
			return
		} else {
			d.logger.Error("record daemon durable stop", "error", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (d *daemon) fileDurableStop(ctx context.Context, cause error) error {
	return d.store.Write(ctx, func(tx *store.WriteTx) error {
		open, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range open {
			if strings.HasPrefix(string(item.ID), durableStopItemPrefix) {
				next := item
				next.Reason = durableStopReason(cause)
				if item.Offers(domain.ActionResumeUnattended) || next.Reason != item.Reason {
					next.ItemVersion++
					next.RequestedDecision = withoutAction(next.RequestedDecision, domain.ActionResumeUnattended)
					return tx.PutAttentionItem(ctx, next)
				}
				return nil
			}
		}
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		posture := domain.HealthPostureBlocking
		createdAt := time.Now().UTC()
		if d.now != nil {
			createdAt = d.now().UTC()
		}
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID:        domain.ItemID(fmt.Sprintf("%s%d", durableStopItemPrefix, state.Revision+1)),
			ProjectID: domain.ProjectID("project-system"),
			Subject:   domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
			Type:      domain.AttentionSystemHealth,
			Priority:  domain.PriorityHigh,
			Reason:    durableStopReason(cause),
			RequestedDecision: []domain.Action{
				domain.ActionAcknowledge,
				domain.ActionRunDoctor,
			},
			ItemVersion:       1,
			InterruptionClass: domain.InterruptionExceptional,
			CreatedAt:         &createdAt,
			Posture:           &posture,
			Status:            domain.StatusOpen,
		}, nil)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	})
}

func durableStopReason(cause error) string {
	return fmt.Sprintf(
		"The daemon stopped unattended operation after a health failure requiring a durable stop: %v. "+
			"Unattended admission remains closed until this item is resolved; a restart does not reopen it.",
		cause,
	)
}

// enableDurableStopRecovery offers the explicit operator recovery only after
// a fresh daemon start. A same-process recurrence strips the offer again,
// because the failed loop has returned and is not safe to resume in place.
func (d *daemon) enableDurableStopRecovery(ctx context.Context) error {
	return d.store.Write(ctx, func(tx *store.WriteTx) error {
		open, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range open {
			if !strings.HasPrefix(string(item.ID), durableStopItemPrefix) ||
				item.Offers(domain.ActionResumeUnattended) {
				continue
			}
			next := item
			next.ItemVersion++
			next.RequestedDecision = append(next.RequestedDecision, domain.ActionResumeUnattended)
			if err := tx.PutAttentionItem(ctx, next); err != nil {
				return err
			}
		}
		return nil
	})
}

func withoutAction(actions []domain.Action, removed domain.Action) []domain.Action {
	next := make([]domain.Action, 0, len(actions))
	for _, action := range actions {
		if action != removed {
			next = append(next, action)
		}
	}
	return next
}

const scheduledFailureThreshold = 3

type scheduledFailure struct {
	at    time.Time
	cause string
}

type scheduledFailureRunError struct {
	kind     domain.ScheduleKind
	failures []scheduledFailure
}

func (e *scheduledFailureRunError) Error() string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s failed %d consecutive times", e.kind, len(e.failures))
	for _, failure := range e.failures {
		fmt.Fprintf(&out, "; %s: %s", failure.at.Format(time.RFC3339Nano), failure.cause)
	}
	return out.String()
}

type scheduledFailureTracker struct {
	mu       sync.Mutex
	now      func() time.Time
	logger   *slog.Logger
	failures map[domain.ScheduleKind][]scheduledFailure
}

func newScheduledFailureTracker(now func() time.Time, logger *slog.Logger) *scheduledFailureTracker {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &scheduledFailureTracker{
		now:      now,
		logger:   logger.With("subsystem", "supervision"),
		failures: make(map[domain.ScheduleKind][]scheduledFailure),
	}
}

func (t *scheduledFailureTracker) consumption(
	kind domain.ScheduleKind,
	err error,
) (scheduler.Consumption, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err == nil {
		delete(t.failures, kind)
		return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
	}
	failure := scheduledFailure{at: t.now().UTC(), cause: err.Error()}
	if !transientExternalFailure(err) {
		return scheduler.Consumption{}, &scheduledFailureRunError{
			kind:     kind,
			failures: []scheduledFailure{failure},
		}
	}
	t.failures[kind] = append(t.failures[kind], failure)
	run := t.failures[kind]
	t.logger.Warn("scheduled external health pass failed",
		"schedule_kind", string(kind),
		"consecutive_failures", len(run),
		"threshold", scheduledFailureThreshold,
		"error", err,
	)
	if len(run) < scheduledFailureThreshold {
		return scheduler.Consumption{Outcome: domain.OutcomeObserveFailed}, nil
	}
	return scheduler.Consumption{}, &scheduledFailureRunError{
		kind:     kind,
		failures: append([]scheduledFailure(nil), run...),
	}
}

func transientExternalFailure(err error) bool {
	if many, ok := err.(interface{ Unwrap() []error }); ok {
		causes := many.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !transientExternalFailure(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		if cause := wrapped.Unwrap(); cause != nil {
			return transientExternalFailure(cause)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return true
	}
	var apiError *publish.APIError
	if errors.As(err, &apiError) {
		return apiError.Status == http.StatusForbidden ||
			apiError.Status == http.StatusRequestTimeout ||
			apiError.Status == http.StatusTooManyRequests ||
			apiError.Status >= http.StatusInternalServerError
	}
	return false
}

func janitorRegistrationFailures(faults []publish.JanitorRegistrationFault) error {
	causes := make([]error, 0, len(faults))
	for _, fault := range faults {
		causes = append(causes, fmt.Errorf("registration %d: %w", fault.RegistrationID, fault.Err))
	}
	return errors.Join(causes...)
}
