package daemoncompose

import (
	"errors"
	"fmt"
	"net/http"
	osexec "os/exec"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestScheduledExternalFailureThresholdRecordsTheRun(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 10, 1, 2, 3, 0, time.UTC)
	tracker := newScheduledFailureTracker(func() time.Time {
		observed := now
		now = now.Add(time.Minute)
		return observed
	}, nil)

	for attempt := 1; attempt <= scheduledFailureThreshold; attempt++ {
		consumption, err := tracker.consumption(domain.ScheduleJanitor,
			&publish.APIError{Status: http.StatusServiceUnavailable, RequestPath: "/installations"})
		if attempt < scheduledFailureThreshold {
			if err != nil || consumption.Outcome != domain.OutcomeObserveFailed {
				t.Fatalf("attempt %d = outcome %s, err %v; want observe_failed", attempt, consumption.Outcome, err)
			}
			continue
		}
		var run *scheduledFailureRunError
		if !errors.As(err, &run) || len(run.failures) != scheduledFailureThreshold {
			t.Fatalf("attempt %d error = %#v, want %d-failure run", attempt, err, scheduledFailureThreshold)
		}
		for _, want := range []string{
			"2026-08-10T01:02:03Z",
			"2026-08-10T01:03:03Z",
			"2026-08-10T01:04:03Z",
			"status 503",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("threshold error %q does not record %q", err, want)
			}
		}
	}
}

// A doctor pass that failed only on a re-proof whose retry failed on a
// runtime command is transient: it records observe_failed and retries at the
// next fire.
func TestScheduledRuntimeCommandReproveFailureIsTransient(t *testing.T) {
	t.Parallel()
	tracker := newScheduledFailureTracker(time.Now, nil)
	failure := fmt.Errorf("scheduled doctor pass: %w", errors.Join(fmt.Errorf("re-prove image %s: %w", "image",
		&reproveRuntimeFailure{err: fmt.Errorf("container inspect: %w", &osexec.ExitError{})})))
	consumption, err := tracker.consumption(domain.ScheduleDoctor, failure)
	if err != nil || consumption.Outcome != domain.OutcomeObserveFailed {
		t.Fatalf("consumption = outcome %s, err %v; want observe_failed", consumption.Outcome, err)
	}
}

func TestScheduledFailureClassificationFailsClosed(t *testing.T) {
	t.Parallel()
	for name, failure := range map[string]error{
		"unclassified": errors.New("operational source unreadable"),
		"revoked authority": &publish.APIError{
			Status: http.StatusUnauthorized, RequestPath: "/installations",
		},
		"mixed transient and revoked": fmt.Errorf("wrapped pass: %w", errors.Join(
			&publish.APIError{Status: http.StatusServiceUnavailable, RequestPath: "/installations"},
			&publish.APIError{Status: http.StatusUnauthorized, RequestPath: "/installations"},
		)),
		"conformance regression caused by an exit": fmt.Errorf("scheduled doctor pass: %w",
			fmt.Errorf("re-prove image %s: %w", "image", &ward.ConformanceFailure{
				Backend: ward.BackendName, Check: ward.CheckCredentialContainment,
				Reason: "credential seed did not stop", Err: &osexec.ExitError{},
			})),
		"runtime-command re-prove joined with an unclassified failure": fmt.Errorf("scheduled doctor pass: %w",
			errors.Join(
				fmt.Errorf("re-prove image %s: %w", "image", &reproveRuntimeFailure{err: &osexec.ExitError{}}),
				errors.New("doctor check unreadable"),
			)),
	} {
		t.Run(name, func(t *testing.T) {
			tracker := newScheduledFailureTracker(time.Now, nil)
			_, err := tracker.consumption(domain.ScheduleDoctor, failure)
			var run *scheduledFailureRunError
			if !errors.As(err, &run) || len(run.failures) != 1 {
				t.Fatalf("error = %#v, want immediate one-failure durable stop", err)
			}
		})
	}
}
