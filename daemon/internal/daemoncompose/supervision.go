package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
)

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
	if kind == domain.ScheduleJanitor && errors.Is(err, publish.ErrJanitorMintHeld) {
		return scheduler.Consumption{Outcome: domain.OutcomeObserveFailed}, nil
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
		if errors.Is(fault.Err, publish.ErrJanitorMintHeld) {
			continue
		}
		causes = append(causes, fmt.Errorf("registration %d: %w", fault.RegistrationID, fault.Err))
	}
	if len(causes) == 0 && len(faults) > 0 {
		return publish.ErrJanitorMintHeld
	}
	return errors.Join(causes...)
}
