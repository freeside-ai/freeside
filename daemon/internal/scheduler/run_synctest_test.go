package scheduler_test

import (
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
)

// TestRunTickerCadence pins Scheduler.Run's real time.NewTicker cadence: one
// immediate pass, then exactly one pass per interval, and a clean nil return
// on ctx cancel. Run is the scheduler's only real-stdlib-time behavior (the
// occurrence-due logic is an injected clock, already deterministic), so it is
// the daemon's first testing/synctest home per the timer-dependent-tests
// convention (AGENTS.md, daemon/README.md). The bubble's fake clock advances
// only when every goroutine is durably blocked, so the ticker fires
// deterministically with no real sleep and no wall-clock flakiness.
func TestRunTickerCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := openStore(t)

		// clock() is called exactly once per pass while no schedule is armed:
		// its only other caller fires when a schedule is consumed, which never
		// happens here. So it counts passes without touching schedule state.
		var passes atomic.Int64
		countingClock := func() time.Time {
			passes.Add(1)
			return time.Now().UTC()
		}
		s, err := scheduler.New(st, domain.ModeAttendedDev, countingClock,
			map[domain.ScheduleKind]scheduler.Registration{
				domain.SchedulePRChecksDeadline: {Handle: func(
					context.Context, domain.ScheduleEvent, domain.Schedule,
				) (scheduler.Consumption, error) {
					t.Error("no schedule armed; handler must not fire")
					return scheduler.Consumption{}, nil
				}},
			})
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx, time.Second) }()

		// Immediate pass at bubble start, then one pass per tick. Let five
		// ticks elapse in fake time, then wait for Run to settle back into its
		// select before reading the count.
		time.Sleep(5*time.Second + 500*time.Millisecond)
		synctest.Wait()
		if got := passes.Load(); got != 6 {
			t.Fatalf("passes after immediate + 5 ticks = %d, want 6", got)
		}

		cancel()
		if err := <-done; err != nil {
			t.Fatalf("Run returned %v, want nil on cancel", err)
		}
	})
}
