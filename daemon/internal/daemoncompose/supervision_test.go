package daemoncompose

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
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
