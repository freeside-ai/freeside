package usagecollect

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func failing(ward.UsageCollectionRequest) (ward.UsageCollection, error) {
	return ward.UsageCollection{}, errors.New("no writer turn has reported usage")
}

// laterReports answers like reports a minute later, so a collection that
// follows a failed one back to back writes after it: the bubble's clock would
// otherwise stamp both at one instant, and the store refuses the second.
func laterReports(windows ...string) func(ward.UsageCollectionRequest) (ward.UsageCollection, error) {
	return func(request ward.UsageCollectionRequest) (ward.UsageCollection, error) {
		time.Sleep(time.Minute)
		return reports(windows...)(request)
	}
}

func (f *fixture) captureArrived(pool domain.UsagePoolID, enrollment domain.ClientEnrollmentID, generation int) {
	f.t.Helper()
	if err := f.coord.CaptureArrived(context.Background(), pool, enrollment, generation); err != nil {
		f.t.Fatalf("capture arrived: %v", err)
	}
	synctest.Wait()
}

// TestCaptureArrivedCollectsDuringBackoff covers the capture trigger on a pool
// whose empty-memory failures put it in backoff: it collects at once, and the
// collected answer clears the backoff.
func TestCaptureArrivedCollectsDuringBackoff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(failing)
		if report := f.refresh(sharedPool); report.Outcome != OutcomeFailed {
			t.Fatalf("outcome = %s, want failed", report.Outcome)
		}
		if report := f.refresh(sharedPool); report.Outcome != OutcomeBackingOff {
			t.Fatalf("outcome = %s, want backing off", report.Outcome)
		}

		f.collector.set(reports("seven_day"))
		f.tick()
		f.captureArrived(sharedPool, enrollFirst, 1)
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collector called %d times, want the capture's collection", calls)
		}
		record := f.record(sharedPool)
		if got := windowNames(record); !slices.Equal(got, []string{"seven_day"}) ||
			record.LastAttempt.Status != domain.CollectionAttemptSucceeded {
			t.Fatalf("record = %+v, want the captured window", record)
		}
		f.tick()
		if report := f.refresh(sharedPool); report.Outcome != OutcomeCollected {
			t.Fatalf("outcome after the capture = %s, want the backoff cleared", report.Outcome)
		}
	})
}

// TestCaptureArrivedFromAnotherEnrollmentStartsNothing covers a capture that
// can't reach the record: from an enrollment that is not the pool's collector,
// under a replaced generation, or for a pool with no enrollment. None starts a
// collection or touches a backoff.
func TestCaptureArrivedFromAnotherEnrollmentStartsNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(failing)
		f.refresh(sharedPool)

		f.collector.set(reports("seven_day"))
		f.captureArrived(sharedPool, enrollSecond, 1)
		f.appendGeneration(enrollFirst)
		f.captureArrived(sharedPool, enrollFirst, 1)
		f.captureArrived("pool-unknown", enrollFirst, 2)
		if calls := len(f.collector.requests()); calls != 1 {
			t.Fatalf("collector called %d times, want none after the first", calls)
		}
		if report := f.refresh(sharedPool); report.Outcome != OutcomeBackingOff {
			t.Fatalf("outcome = %s, want the backoff kept", report.Outcome)
		}

		f.tick()
		f.captureArrived(sharedPool, enrollFirst, 2)
		if requests := f.collector.requests(); len(requests) != 2 || requests[1].Generation != 2 {
			t.Fatalf("requests = %+v, want one collection under generation 2", requests)
		}
	})
}

// TestCaptureDuringAFailingCollectionCollectsAgain covers a capture that lands
// while a collection that read the empty memory is in flight: that one fails
// into backoff, and the collection after it still starts.
func TestCaptureDuringAFailingCollectionCollectsAgain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(failing)
		gate := f.collector.block()
		f.coord.RequestRefresh(sharedPool)
		synctest.Wait()

		f.collector.set(laterReports("seven_day"))
		f.captureArrived(sharedPool, enrollFirst, 1)
		f.collector.mu.Lock()
		f.collector.gate = nil
		f.collector.mu.Unlock()
		close(gate)
		synctest.Sleep(2 * time.Minute)

		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collector called %d times, want one more after the failed one", calls)
		}
		record := f.record(sharedPool)
		if record.LastAttempt.Status != domain.CollectionAttemptSucceeded || len(record.Windows) != 1 {
			t.Fatalf("record = %+v, want the capture collected", record)
		}
		time.Sleep(time.Minute)
		if report := f.refresh(sharedPool); report.Outcome != OutcomeCollected {
			t.Fatalf("outcome = %s, want no backoff left", report.Outcome)
		}
	})
}
