package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type stallCalls struct {
	mu    sync.Mutex
	calls []bool
	err   error
}

func (c *stallCalls) hook(_ context.Context, stalled bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, stalled)
	return c.err
}

func (c *stallCalls) got() []bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.calls)
}

// The watch reports each change of the heartbeat's answer once: a stall
// after the interval, recovery on a fresh provider byte, a second stall
// after another quiet stretch, and a conclusion when the wait ends.
func TestStallWatchReportsTransitionsOnce(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	now, lastByte := start, time.Time{}
	var rec stallCalls
	cfg := testConfig().withDefaults()
	cfg.StallInterval = 5 * time.Minute
	cfg.Now = func() time.Time { return now }
	hs := testHandoffSpec()
	hs.Stall = rec.hook
	w := newStallWatch(context.Background(), cfg, hs, func() time.Time { return lastByte })

	pollAt := func(offset time.Duration) {
		t.Helper()
		now = start.Add(offset)
		w.poll()
		w.calls.Wait()
	}
	pollAt(time.Minute)
	pollAt(5*time.Minute - time.Nanosecond)
	if got := rec.got(); len(got) != 0 {
		t.Fatalf("reports before the interval = %v", got)
	}
	pollAt(5 * time.Minute)
	pollAt(6 * time.Minute)
	lastByte = start.Add(7 * time.Minute)
	pollAt(7 * time.Minute)
	pollAt(8 * time.Minute)
	pollAt(12 * time.Minute)
	w.finish()

	if got, want := rec.got(), []bool{true, false, true, false}; !slices.Equal(got, want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
}

// A failed report is retried on the next poll, and a failed stall never
// reported is not concluded.
func TestStallWatchRetriesFailedReport(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	now := start
	rec := stallCalls{err: errors.New("store unavailable")}
	cfg := testConfig().withDefaults()
	cfg.StallInterval = time.Minute
	cfg.Now = func() time.Time { return now }
	hs := testHandoffSpec()
	hs.Stall = rec.hook
	w := newStallWatch(context.Background(), cfg, hs, func() time.Time { return time.Time{} })

	for _, offset := range []time.Duration{time.Minute, 2 * time.Minute} {
		now = start.Add(offset)
		w.poll()
		w.calls.Wait()
	}
	w.finish()
	if got, want := rec.got(), []bool{true, true}; !slices.Equal(got, want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
}

// A slow report blocks neither the poll that started it nor later polls,
// and finish waits for it before concluding.
func TestStallWatchSlowReportStaysOffThePollPath(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	now := start
	release := make(chan struct{})
	var rec stallCalls
	cfg := testConfig().withDefaults()
	cfg.StallInterval = time.Minute
	cfg.Now = func() time.Time { return now }
	hs := testHandoffSpec()
	hs.Stall = func(ctx context.Context, stalled bool) error {
		if stalled {
			<-release
		}
		return rec.hook(ctx, stalled)
	}
	w := newStallWatch(context.Background(), cfg, hs, func() time.Time { return time.Time{} })

	now = start.Add(time.Minute)
	w.poll()
	now = start.Add(2 * time.Minute)
	w.poll()
	close(release)
	w.finish()
	if got, want := rec.got(), []bool{true, false}; !slices.Equal(got, want) {
		t.Fatalf("reports = %v, want %v", got, want)
	}
}

// The heartbeat never resets, extends, or delays the writer's hard budget:
// with the watch on and a stall reported, a writer that never stops fails
// with the same error after the same number of polls as without a watch,
// and a failing report changes nothing either.
func TestHandoffStallWatchLeavesWriterBudget(t *testing.T) {
	names := namesFor(testHandoffSpec().RunID)
	type outcome struct {
		err      string
		inspects int
		sleeps   int
		reports  []bool
	}
	run := func(t *testing.T, hook *stallCalls) outcome {
		t.Helper()
		fx := newHandoffFixture(t)
		fx.rt.runningInspects[names.Agent] = math.MaxInt - 1
		clock := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
		var clockMu sync.Mutex
		sleeps := 0
		fx.cfg.StallInterval = 3 * fx.cfg.PollInterval
		fx.cfg.Now = func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return clock
		}
		fx.cfg.Sleep = func(_ context.Context, d time.Duration) error {
			clockMu.Lock()
			defer clockMu.Unlock()
			clock = clock.Add(d)
			sleeps++
			return nil
		}
		hs := testHandoffSpec()
		if hook != nil {
			hs.Stall = hook.hook
		}
		_, err := fx.backend(t).Handoff(context.Background(), hs)
		wantCheckFailure(t, err, CheckWriterTermination)
		fx.assertReaped(t)
		o := outcome{err: err.Error(), sleeps: sleeps}
		fx.rt.mu.Lock()
		for _, c := range fx.rt.calls {
			if c == "inspect "+names.Agent {
				o.inspects++
			}
		}
		fx.rt.mu.Unlock()
		if hook != nil {
			o.reports = hook.got()
		}
		return o
	}

	baseline := run(t, nil)
	if baseline.inspects == 0 || !strings.Contains(baseline.err, "never observed") {
		t.Fatalf("baseline = %+v, want a poll-budget writer termination", baseline)
	}
	for _, tc := range []struct {
		name string
		hook *stallCalls
		want []bool
	}{
		{"accepted", &stallCalls{}, []bool{true, false}},
		{"failing", &stallCalls{err: errors.New("store unavailable")}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := run(t, tc.hook)
			if got.err != baseline.err || got.inspects != baseline.inspects || got.sleeps != baseline.sleeps {
				t.Fatalf("with watch = %+v, without = %+v", got, baseline)
			}
			if tc.want != nil && !slices.Equal(got.reports, tc.want) {
				t.Fatalf("reports = %v, want %v", got.reports, tc.want)
			}
			if tc.want == nil && (len(got.reports) == 0 || slices.Contains(got.reports, false)) {
				t.Fatalf("failing reports = %v, want retried stall reports and no conclusion", got.reports)
			}
		})
	}
}

// The hook is a runtime observer: the journaled spec bytes and digest are
// the same with and without it.
func TestHandoffSpecStallHookLeavesJournalDigest(t *testing.T) {
	without := testHandoffSpec()
	with := testHandoffSpec()
	with.Stall = func(context.Context, bool) error { return nil }
	a, err := json.Marshal(without)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(with)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("Stall hook changed the marshalled spec")
	}
	da, err := specDigest(without)
	if err != nil {
		t.Fatal(err)
	}
	db, err := specDigest(with)
	if err != nil {
		t.Fatal(err)
	}
	if da != db {
		t.Fatalf("spec digest %s with hook, %s without", db, da)
	}
}

// Real socket accepts prevent synctest's clock from advancing. This listener
// keeps the production proxy lifecycle but blocks durably inside the bubble.
type stallTestListener struct {
	closed chan struct{}
	once   sync.Once
}

func (l *stallTestListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *stallTestListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*stallTestListener) Addr() net.Addr { return &net.TCPAddr{Port: 12345} }

func newStallHandoffFixture(t *testing.T) *handoffFixture {
	t.Helper()
	fx := newHandoffFixture(t)
	fx.cfg.listenEgressProxy = func() (net.Listener, error) {
		return &stallTestListener{closed: make(chan struct{})}, nil
	}
	fx.cfg.Sleep = sleepContext
	fx.cfg.Now = time.Now
	fx.cfg.PollInterval = time.Second
	fx.cfg.WriterStopTimeout = 3 * time.Second
	fx.cfg.ExporterTimeout = 3 * time.Second
	fx.cfg.StallInterval = time.Second
	return fx
}

func TestHandoffStallDeadlineDoesNotDelayWriterStop(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		name := "budget"
		if cancelled {
			name = "cancellation"
		}
		t.Run(name, func(t *testing.T) {
			type outcome struct {
				stop, returned, hookReturned time.Duration
				err                          string
				calls                        []bool
			}
			run := func(withHook bool) (got outcome) {
				synctest.Test(t, func(t *testing.T) {
					start := time.Now()
					fx := newStallHandoffFixture(t)
					hs := testHandoffSpec()
					names := namesFor(hs.RunID)
					fx.rt.runningInspects[names.Agent] = math.MaxInt - 1
					fx.rt.onStop = func(id string) error {
						if id == names.Agent {
							got.stop = time.Since(start)
						}
						return nil
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if cancelled {
						go func() {
							time.Sleep(3500 * time.Millisecond)
							cancel()
						}()
					}
					if withHook {
						hs.Stall = func(ctx context.Context, stalled bool) error {
							got.calls = append(got.calls, stalled)
							<-ctx.Done()
							got.hookReturned = time.Since(start)
							return ctx.Err()
						}
					}
					_, err := fx.backend(t).Handoff(ctx, hs)
					wantCheckFailure(t, err, CheckWriterTermination)
					if cancelled && !strings.Contains(err.Error(), context.Canceled.Error()) {
						t.Fatalf("error = %v, want caller cancellation", err)
					}
					got.err = err.Error()
					got.returned = time.Since(start)
					fx.assertReaped(t)
				})
				return got
			}
			baseline, got := run(false), run(true)
			if baseline.stop == 0 || got.stop != baseline.stop {
				t.Fatalf("writer stop with hook = %s, without = %s", got.stop, baseline.stop)
			}
			if got.err != baseline.err {
				t.Fatalf("error with hook = %q, without = %q", got.err, baseline.err)
			}
			if !slices.Equal(got.calls, []bool{true}) {
				t.Fatalf("calls = %v, want one failed raise and no clear", got.calls)
			}
			if got.hookReturned <= got.stop || got.returned < got.hookReturned {
				t.Fatalf("notice was not joined after stop: %+v", got)
			}
		})
	}
}

func TestHandoffStallClearFollowsRaiseAfterWriterStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fx := newStallHandoffFixture(t)
		hs := testHandoffSpec()
		names := namesFor(hs.RunID)
		fx.rt.runningInspects[names.Agent] = math.MaxInt - 1
		var stopped, raised, clearStarted, cleared time.Time
		var calls []bool
		fx.rt.onStop = func(id string) error {
			if id == names.Agent {
				stopped = time.Now()
			}
			return nil
		}
		hs.Stall = func(_ context.Context, stalled bool) error {
			calls = append(calls, stalled)
			if stalled {
				time.Sleep(30 * time.Second)
				raised = time.Now()
			} else {
				clearStarted = time.Now()
				time.Sleep(30 * time.Second)
				cleared = time.Now()
			}
			return nil
		}
		_, err := fx.backend(t).Handoff(context.Background(), hs)
		wantCheckFailure(t, err, CheckWriterTermination)
		if !slices.Equal(calls, []bool{true, false}) {
			t.Fatalf("calls = %v, want raise then clear", calls)
		}
		if stopped.IsZero() || !stopped.Before(raised) || clearStarted.Before(raised) || !stopped.Before(clearStarted) {
			t.Fatalf("stop %s, raise returned %s, clear started %s", stopped, raised, clearStarted)
		}
		if cleared.IsZero() || time.Now().Before(cleared) {
			t.Fatal("handoff returned before the clear finished")
		}
		fx.assertReaped(t)
	})
}

func TestHandoffStallRecoveryStillSucceeds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fx := newStallHandoffFixture(t)
		fx.cfg.WriterStopTimeout = 10 * time.Second
		hs := testHandoffSpec()
		fx.rt.runningInspects[namesFor(hs.RunID).Agent] = 3
		var calls stallCalls
		hs.Stall = calls.hook
		fx.rt.onDeleteContainer = func(id string) (bool, error) {
			if id == namesFor(hs.RunID).Agent {
				if got := calls.got(); !slices.Equal(got, []bool{true, false}) {
					t.Errorf("calls at writer deletion = %v, want completed raise and clear", got)
				}
			}
			return false, nil
		}
		result, err := fx.backend(t).Handoff(context.Background(), hs)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(result.ExportDir) })
		if got := calls.got(); !slices.Equal(got, []bool{true, false}) {
			t.Fatalf("calls = %v, want raise then clear", got)
		}
		fx.assertReaped(t)
	})
}
