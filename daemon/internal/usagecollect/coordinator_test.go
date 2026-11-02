package usagecollect

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// TestScheduledPassCollectsEveryPool covers the doctor's trigger: one
// collection per pool through its default collector, each a client-visible
// write that advances the sync revision. Collection changes no enrollment's
// generation, and a pool whose collector has no registered harness collector
// gets no record.
func TestScheduledPassCollectsEveryPool(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		generations := f.generations()
		before, err := f.st.ServerState(context.Background())
		if err != nil {
			t.Fatal(err)
		}

		if err := f.coord.CollectAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()

		asked := []domain.ClientEnrollmentID{}
		for _, request := range f.collector.requests() {
			asked = append(asked, request.Enrollment.ID)
		}
		slices.Sort(asked)
		if want := []domain.ClientEnrollmentID{enrollFirst, enrollOther}; !slices.Equal(asked, want) {
			t.Fatalf("asked %v, want %v", asked, want)
		}
		for _, pool := range []domain.UsagePoolID{sharedPool, otherPool} {
			record := f.record(pool)
			if record.LastAttempt == nil || record.LastAttempt.Status != domain.CollectionAttemptSucceeded ||
				!slices.Equal(windowNames(record), []string{"five_hour"}) {
				t.Fatalf("%s record = %+v", pool, record)
			}
		}
		if f.hasRecord(claudePool) {
			t.Fatal("a pool with no harness collector gained a record")
		}
		after, err := f.st.ServerState(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if after.Revision != before.Revision+2 {
			t.Fatalf("revision %d -> %d, want one bump per collection write", before.Revision, after.Revision)
		}
		if got := f.generations(); !maps.Equal(got, generations) {
			t.Fatalf("generations changed: %v -> %v", generations, got)
		}
	})
}

// TestRateLimitEventCollects covers a run's rate-limit event: one collection
// for its pool alone.
func TestRateLimitEventCollects(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.coord.RateLimitEvent(otherPool)
		synctest.Wait()
		requests := f.collector.requests()
		if len(requests) != 1 || requests[0].Enrollment.ID != enrollOther || requests[0].Generation != 1 {
			t.Fatalf("requests = %+v", requests)
		}
		if got := windowNames(f.record(otherPool)); !slices.Equal(got, []string{"five_hour"}) {
			t.Fatalf("windows = %v", got)
		}
	})
}

// TestRefreshJoinsTheCollectionInFlight covers a manual refresh and the join:
// two refreshes and an event during one collection make one collector call,
// and both refreshes return its record.
func TestRefreshJoinsTheCollectionInFlight(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		gate := f.collector.block()
		reports := make([]Report, 2)
		var wg sync.WaitGroup
		for i := range reports {
			wg.Go(func() { reports[i] = f.refresh(sharedPool) })
		}
		synctest.Wait()
		f.coord.RateLimitEvent(sharedPool)
		close(gate)
		wg.Wait()
		synctest.Wait()

		if calls := len(f.collector.requests()); calls != 1 {
			t.Fatalf("collector called %d times, want 1", calls)
		}
		for _, report := range reports {
			if report.Outcome != OutcomeCollected || report.Observation == nil ||
				len(report.Windows) != 1 || report.Windows[0].Freshness != FreshnessFresh {
				t.Fatalf("report = %+v", report)
			}
		}
	})
}

// TestRefreshWithoutAHarnessCollectorFails covers a pool whose collector's
// harness client has no collector: the refresh says so and writes nothing.
func TestRefreshWithoutAHarnessCollectorFails(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		if _, err := f.coord.Refresh(context.Background(), claudePool); !errors.Is(err, ErrNoCollector) {
			t.Fatalf("refresh error = %v, want ErrNoCollector", err)
		}
		if f.hasRecord(claudePool) {
			t.Fatal("refresh without a collector wrote a record")
		}
	})
}

// TestCollectsOnceAfterAWindowReset covers the reset trigger: one collection
// after the next reset, never a second for the same reset, and a restarted
// coordinator rearms from the stored record.
func TestCollectsOnceAfterAWindowReset(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(resetting)
		f.refresh(sharedPool)
		f.collector.set(reports("five_hour"))

		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 1 {
			t.Fatalf("collected %d times before the reset, want 1", calls)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collected %d times after the reset, want 2", calls)
		}
		time.Sleep(24 * time.Hour)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collected %d times a day later, want 2", calls)
		}

		// A restart arms the timer from what the store holds.
		f.collector.set(resetting)
		f.refresh(sharedPool)
		f.collector.set(reports("five_hour"))
		f.stop()
		f.start()
		time.Sleep(time.Hour + time.Second)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 4 {
			t.Fatalf("collected %d times after the restarted reset, want 4", calls)
		}
	})
}

// TestFailedRefreshKeepsReadingsAndBacksOff covers a failure: the readings and
// their times stay, the failure is the refresh status, and every trigger waits
// out a doubling backoff, or a longer throttle, until a success clears it.
func TestFailedRefreshKeepsReadingsAndBacksOff(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		collected := f.refresh(sharedPool).Observation
		f.tick()
		f.collector.set(func(ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			return ward.UsageCollection{}, errors.New("provider unavailable")
		})

		failed := f.refresh(sharedPool)
		if failed.Outcome != OutcomeFailed {
			t.Fatalf("outcome = %s", failed.Outcome)
		}
		record := f.record(sharedPool)
		if !slices.EqualFunc(record.Windows, collected.Windows, func(left, right domain.WindowReading) bool {
			return left.Window == right.Window && left.ObservedAt.Equal(right.ObservedAt)
		}) {
			t.Fatalf("windows %+v, want the kept %+v", record.Windows, collected.Windows)
		}
		if record.LastAttempt.Status != domain.CollectionAttemptFailed || record.LastAttempt.Detail != "provider unavailable" {
			t.Fatalf("last attempt = %+v", record.LastAttempt)
		}
		if record.Enrollments[0].Answer != domain.AvailabilityFailed {
			t.Fatalf("availability = %+v", record.Enrollments)
		}

		failedAt := time.Now()
		held := f.refresh(sharedPool)
		f.coord.RateLimitEvent(sharedPool)
		synctest.Wait()
		if held.Outcome != OutcomeBackingOff || !held.BackoffUntil.Equal(failedAt.Add(DefaultBackoffBase)) {
			t.Fatalf("report during backoff = %+v", held)
		}
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collector called %d times during backoff, want 2", calls)
		}

		time.Sleep(DefaultBackoffBase)
		if again := f.refresh(sharedPool); again.Outcome != OutcomeFailed {
			t.Fatalf("outcome after backoff = %s", again.Outcome)
		}
		if held := f.refresh(sharedPool); !held.BackoffUntil.Equal(time.Now().Add(2 * DefaultBackoffBase)) {
			t.Fatalf("second backoff until %v, want doubled", held.BackoffUntil)
		}

		time.Sleep(2 * DefaultBackoffBase)
		f.collector.set(func(ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			return ward.UsageCollection{}, &ward.UsageThrottleError{RetryAfter: 3 * time.Hour, Detail: "slow down"}
		})
		f.refresh(sharedPool)
		if held := f.refresh(sharedPool); !held.BackoffUntil.Equal(time.Now().Add(3 * time.Hour)) {
			t.Fatalf("throttled backoff until %v, want the provider's delay", held.BackoffUntil)
		}

		time.Sleep(3 * time.Hour)
		f.collector.set(reports("five_hour"))
		if ok := f.refresh(sharedPool); ok.Outcome != OutcomeCollected {
			t.Fatalf("outcome after throttle = %s", ok.Outcome)
		}
		f.tick()
		if ok := f.refresh(sharedPool); ok.Outcome != OutcomeCollected {
			t.Fatalf("a success left a backoff: %+v", ok)
		}
	})
}

// TestFullReportReplacesTheWindowSet covers a full response dropping a window
// it no longer reports.
func TestFullReportReplacesTheWindowSet(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(reports("five_hour", "seven_day"))
		f.refresh(sharedPool)
		f.tick()
		f.collector.set(reports("seven_day"))
		f.refresh(sharedPool)
		if got := windowNames(f.record(sharedPool)); !slices.Equal(got, []string{"seven_day"}) {
			t.Fatalf("windows = %v, want the reported set", got)
		}
	})
}

// TestPartialUpdateTouchesOneWindow covers a partial update: it replaces the
// window it names and leaves the rest. One from another enrollment, or before
// a success, is dropped for a full collection.
func TestPartialUpdateTouchesOneWindow(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		request := ward.UsageCollectionRequest{Enrollment: domain.ClientEnrollment{ID: enrollFirst}, Generation: 1}

		if applied, err := f.coord.ApplyPartial(context.Background(), sharedPool, reading(request, "five_hour")); err != nil || applied {
			t.Fatalf("partial before any collection = %v, %v", applied, err)
		}
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 1 {
			t.Fatalf("a dropped partial started %d collections, want 1", calls)
		}

		f.tick()
		f.collector.set(reports("five_hour", "seven_day"))
		before := f.refresh(sharedPool).Observation
		f.tick()
		update := reading(request, "seven_day")
		update.Status = domain.WindowStatusExhausted
		applied, err := f.coord.ApplyPartial(context.Background(), sharedPool, update)
		if err != nil || !applied {
			t.Fatalf("partial = %v, %v", applied, err)
		}
		record := f.record(sharedPool)
		if !reflect.DeepEqual(record.Windows[0], before.Windows[0]) || record.Windows[1].Status != domain.WindowStatusExhausted {
			t.Fatalf("windows = %+v", record.Windows)
		}

		stranger := reading(ward.UsageCollectionRequest{Enrollment: domain.ClientEnrollment{ID: enrollSecond}, Generation: 1}, "five_hour")
		if applied, err := f.coord.ApplyPartial(context.Background(), sharedPool, stranger); err != nil || applied {
			t.Fatalf("partial from another enrollment = %v, %v", applied, err)
		}
	})
}

// TestReplacedGenerationDiscardsTheReading covers a reading taken under a
// generation replaced before it is written: a full report and a partial
// update are both discarded, and the record is unchanged.
func TestReplacedGenerationDiscardsTheReading(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.refresh(sharedPool)
		before := f.record(sharedPool)
		f.tick()

		f.collector.set(func(request ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			f.appendGeneration(enrollFirst)
			return reports("five_hour", "seven_day")(request)
		})
		if report := f.refresh(sharedPool); report.Outcome != OutcomeDiscarded {
			t.Fatalf("outcome = %s, want discarded", report.Outcome)
		}
		if got := f.record(sharedPool); !reflect.DeepEqual(got, before) {
			t.Fatalf("record changed: %+v", got)
		}

		f.tick()
		stale := reading(ward.UsageCollectionRequest{Enrollment: domain.ClientEnrollment{ID: enrollFirst}, Generation: 1}, "five_hour")
		if applied, err := f.coord.ApplyPartial(context.Background(), sharedPool, stale); err != nil || applied {
			t.Fatalf("partial under a replaced generation = %v, %v", applied, err)
		}
	})
}

// TestSetCollectorReassigns covers reassignment: the old collector's readings
// go, the availability answers stay, the new collector collects, and an
// enrollment on another pool is refused.
func TestSetCollectorReassigns(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.refresh(sharedPool)
		f.tick()

		if _, err := f.coord.SetCollector(context.Background(), sharedPool, enrollOther); !errors.Is(err, store.ErrEnrollmentNotOnPool) {
			t.Fatalf("reassign off the pool = %v, want ErrEnrollmentNotOnPool", err)
		}
		gate := f.collector.block()
		result, err := f.coord.SetCollector(context.Background(), sharedPool, enrollSecond)
		if err != nil {
			t.Fatal(err)
		}
		if result != (Reassignment{Pool: sharedPool, PreviousCollector: enrollFirst, Collector: enrollSecond, Recorded: true}) {
			t.Fatalf("result = %+v", result)
		}
		reassignedRecord := f.record(sharedPool)
		if reassignedRecord.Collector != enrollSecond || len(reassignedRecord.Windows) != 0 ||
			reassignedRecord.LastAttempt != nil || len(reassignedRecord.Enrollments) != 1 {
			t.Fatalf("record after reassignment = %+v", reassignedRecord)
		}
		f.tick()
		close(gate)
		synctest.Wait()

		requests := f.collector.requests()
		if last := requests[len(requests)-1]; last.Enrollment.ID != enrollSecond {
			t.Fatalf("collected through %s, want the new collector", last.Enrollment.ID)
		}
		record := f.record(sharedPool)
		if record.Collector != enrollSecond || !slices.Equal(windowNames(record), []string{"five_hour"}) ||
			len(record.Enrollments) != 2 {
			t.Fatalf("record after the new collector's collection = %+v", record)
		}

		f.tick()
		if again, err := f.coord.SetCollector(context.Background(), sharedPool, enrollSecond); err != nil || again.Recorded {
			t.Fatalf("an unchanged collector = %+v, %v", again, err)
		}
	})
}

// TestReassignmentDuringACollectionDiscardsIt covers a collector changed while
// the old one was asked: its answer is discarded and the new one collects.
func TestReassignmentDuringACollectionDiscardsIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(func(request ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			if request.Enrollment.ID == enrollSecond {
				// The new collector answers after the reassignment's write.
				time.Sleep(time.Second)
			}
			return reports("five_hour")(request)
		})
		gate := f.collector.block()
		done := make(chan Report, 1)
		go func() { done <- f.refresh(sharedPool) }()
		synctest.Wait()
		f.tick()
		if _, err := Reassign(context.Background(), f.st, sharedPool, enrollSecond, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		close(gate)
		if report := <-done; report.Outcome != OutcomeDiscarded {
			t.Fatalf("outcome = %s, want discarded", report.Outcome)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		requests := f.collector.requests()
		if len(requests) != 2 || requests[1].Enrollment.ID != enrollSecond {
			t.Fatalf("requests = %+v", requests)
		}
		if record := f.record(sharedPool); record.Collector != enrollSecond || len(record.Windows) != 1 ||
			record.Windows[0].EnrollmentID != enrollSecond {
			t.Fatalf("record = %+v", record)
		}
	})
}

func (f *fixture) generations() map[domain.ClientEnrollmentID]int {
	f.t.Helper()
	generations := map[domain.ClientEnrollmentID]int{}
	if err := f.st.Read(context.Background(), func(tx *store.ReadTx) error {
		for _, id := range []domain.ClientEnrollmentID{enrollFirst, enrollSecond, enrollOther, enrollClaude} {
			current, err := tx.CurrentEnrollmentGeneration(context.Background(), id)
			if err != nil {
				return err
			}
			generations[id] = current.Ordinal
		}
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
	return generations
}

// resetting answers supported with one window that resets in an hour.
func resetting(request ward.UsageCollectionRequest) (ward.UsageCollection, error) {
	window := reading(request, "five_hour")
	window.ResetsAt = new(time.Now().UTC().Add(time.Hour))
	return ward.UsageCollection{Availability: domain.AvailabilitySupported, Windows: []domain.WindowReading{window}}, nil
}

// TestLaterPartialSupersedesACollection covers a partial update committed
// during a collection's call: its later reading wins, and the collection is
// discarded rather than recorded as a failure with a backoff.
func TestLaterPartialSupersedesACollection(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.refresh(sharedPool)
		f.tick()
		f.collector.set(func(request ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			answer, err := reports("five_hour")(request)
			time.Sleep(time.Second)
			partial := reading(request, "five_hour")
			partial.Status = domain.WindowStatusExhausted
			if applied, err := f.coord.ApplyPartial(context.Background(), sharedPool, partial); err != nil || !applied {
				t.Errorf("partial during the call = %v, %v", applied, err)
			}
			// The full write then lands at a later instant than the partial.
			time.Sleep(time.Second)
			return answer, err
		})
		if report := f.refresh(sharedPool); report.Outcome != OutcomeDiscarded {
			t.Fatalf("outcome = %s, want discarded", report.Outcome)
		}
		record := f.record(sharedPool)
		if record.Windows[0].Status != domain.WindowStatusExhausted ||
			record.LastAttempt.Status != domain.CollectionAttemptSucceeded {
			t.Fatalf("record = %+v", record)
		}
		f.tick()
		f.collector.set(reports("five_hour"))
		if report := f.refresh(sharedPool); report.Outcome != OutcomeCollected {
			t.Fatalf("a discarded collection left a backoff: %+v", report)
		}
	})
}

// TestReplacedGenerationDiscardsAnAnswerWithoutReadings covers an answer that
// carries no new reading, which the store's reading gate never sees.
func TestReplacedGenerationDiscardsAnAnswerWithoutReadings(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.refresh(sharedPool)
		before := f.record(sharedPool)
		f.tick()
		f.collector.set(func(ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			f.appendGeneration(enrollFirst)
			return ward.UsageCollection{Availability: domain.AvailabilityUnsupported}, nil
		})
		if report := f.refresh(sharedPool); report.Outcome != OutcomeDiscarded {
			t.Fatalf("outcome = %s, want discarded", report.Outcome)
		}
		if got := f.record(sharedPool); !reflect.DeepEqual(got, before) {
			t.Fatalf("record changed: %+v", got)
		}
	})
}

// TestMisstampedReadingFailsTheAttempt covers a collector that stamps a
// reading with a generation it was not asked under: the stamp is refused as
// a failed attempt, not trusted or silently discarded.
func TestMisstampedReadingFailsTheAttempt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(func(request ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			request.Generation++
			return reports("five_hour")(request)
		})
		report := f.refresh(sharedPool)
		if report.Outcome != OutcomeFailed || !strings.Contains(report.Observation.LastAttempt.Detail, "stamped") ||
			len(report.Observation.Windows) != 0 {
			t.Fatalf("report = %+v", report)
		}
	})
}

// TestResetDuringACollectionCollectsAgain covers a reset that fires while a
// collection that read the pool before it is in flight.
func TestResetDuringACollectionCollectsAgain(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(resetting)
		f.refresh(sharedPool)
		f.collector.set(reports("five_hour"))
		time.Sleep(59 * time.Minute)
		gate := f.collector.block()
		f.coord.RateLimitEvent(sharedPool)
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		close(gate)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 3 {
			t.Fatalf("collected %d times, want one more after the joined reset", calls)
		}
	})
}

// TestResetDuringBackoffCollectsWhenItEnds covers a reset that fires during a
// backoff: it collects when the backoff ends.
func TestResetDuringBackoffCollectsWhenItEnds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(resetting)
		f.refresh(sharedPool)
		time.Sleep(50 * time.Minute)
		f.collector.set(func(ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			return ward.UsageCollection{}, &ward.UsageThrottleError{RetryAfter: 30 * time.Minute, Detail: "slow down"}
		})
		f.refresh(sharedPool)
		f.collector.set(reports("five_hour"))
		time.Sleep(25 * time.Minute)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collected %d times during the backoff, want 2", calls)
		}
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 3 {
			t.Fatalf("collected %d times after the backoff, want the reset's collection", calls)
		}
	})
}

// TestStartCollectsAResetMissedWhileStopped covers a reset that passed while
// the daemon was down: the next start collects at once.
func TestStartCollectsAResetMissedWhileStopped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(resetting)
		f.refresh(sharedPool)
		f.collector.set(reports("five_hour"))
		f.stop()
		time.Sleep(2 * time.Hour)
		f.start()
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collected %d times after the restart, want the missed reset's collection", calls)
		}
		f.stop()
		f.start()
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("a second restart collected again: %d", calls)
		}
	})
}

// TestFailedRerunAfterAResetCollectsWhenTheBackoffEnds covers a reset that
// joins a collection in flight which then fails: the reset is still owed, so
// it collects once the backoff ends.
func TestFailedRerunAfterAResetCollectsWhenTheBackoffEnds(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(resetting)
		f.refresh(sharedPool)
		time.Sleep(59 * time.Minute)
		f.collector.set(func(ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			return ward.UsageCollection{}, errors.New("provider unavailable")
		})
		gate := f.collector.block()
		f.coord.RateLimitEvent(sharedPool)
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		f.collector.set(reports("five_hour"))
		close(gate)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collected %d times during the backoff, want 2", calls)
		}
		time.Sleep(DefaultBackoffBase + time.Minute)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 3 {
			t.Fatalf("collected %d times after the backoff, want the owed reset's collection", calls)
		}
	})
}

// TestDiscardedResetCollectionRetries covers a reset's collection discarded
// because its generation was replaced during the call: the reset is still
// owed, and the retry asks under the new generation.
func TestDiscardedResetCollectionRetries(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.collector.set(resetting)
		f.refresh(sharedPool)
		replaced := false
		f.collector.set(func(request ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			if !replaced {
				replaced = true
				f.appendGeneration(enrollFirst)
			}
			return reports("five_hour")(request)
		})
		time.Sleep(time.Hour + time.Minute)
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collected %d times at the reset, want 2", calls)
		}
		time.Sleep(DefaultBackoffBase)
		synctest.Wait()
		requests := f.collector.requests()
		if len(requests) != 3 || requests[2].Generation != 2 {
			t.Fatalf("requests = %+v, want a retry under generation 2", requests)
		}
		if got := f.record(sharedPool).Windows[0].Generation; got != 2 {
			t.Fatalf("reading generation = %d, want 2", got)
		}
	})
}

// TestPartialWindowDuringACollectionSurvivesIt covers a partial update that
// adds a window the full answer, taken before it, does not name.
func TestPartialWindowDuringACollectionSurvivesIt(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.refresh(sharedPool)
		f.tick()
		f.collector.set(func(request ward.UsageCollectionRequest) (ward.UsageCollection, error) {
			answer, err := reports("five_hour")(request)
			time.Sleep(time.Second)
			if applied, err := f.coord.ApplyPartial(context.Background(), sharedPool, reading(request, "weekly")); err != nil || !applied {
				t.Errorf("partial during the call = %v, %v", applied, err)
			}
			// The full write then lands at a later instant than the partial.
			time.Sleep(time.Second)
			return answer, err
		})
		if report := f.refresh(sharedPool); report.Outcome != OutcomeDiscarded {
			t.Fatalf("outcome = %s, want discarded", report.Outcome)
		}
		if got := windowNames(f.record(sharedPool)); !slices.Equal(got, []string{"five_hour", "weekly"}) {
			t.Fatalf("windows = %v, want the partial update's window kept", got)
		}
	})
}

// TestSettleAfterAReassignmentLeavesTheNewCollector covers a collection that
// settles after a reassignment: its failure is the old collector's, so it
// sets no backoff on the pool.
func TestSettleAfterAReassignmentLeavesTheNewCollector(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		old := f.refresh(sharedPool)
		f.tick()
		if _, err := f.coord.SetCollector(context.Background(), sharedPool, enrollSecond); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		f.coord.mu.Lock()
		f.coord.settle(sharedPool, &collection{}, Report{
			Pool: sharedPool, Outcome: OutcomeFailed, Observation: old.Observation,
		}, time.Hour)
		_, backingOff := f.coord.backoffs[sharedPool]
		f.coord.mu.Unlock()
		if backingOff {
			t.Fatal("the old collector's failure set a backoff on the reassigned pool")
		}
	})
}

// TestStartCollectsARefusalResetMissedWhileStopped covers a refusal mark
// whose reset passed while the daemon was down.
func TestStartCollectsARefusalResetMissedWhileStopped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		f.refresh(sharedPool)
		f.tick()
		now := time.Now().UTC()
		if err := f.st.Write(context.Background(), func(tx *store.WriteTx) error {
			return tx.RecordWindowRefusalMark(context.Background(), sharedPool, domain.RefusalMark{
				Window: "five_hour", InvocationID: "invocation-1", ResetsAt: new(now.Add(time.Hour)), SetAt: now,
			}, now)
		}); err != nil {
			t.Fatal(err)
		}
		f.stop()
		time.Sleep(2 * time.Hour)
		f.start()
		synctest.Wait()
		if calls := len(f.collector.requests()); calls != 2 {
			t.Fatalf("collected %d times after the restart, want the missed refusal reset's collection", calls)
		}
	})
}
