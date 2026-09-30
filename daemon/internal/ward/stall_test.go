package ward

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
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
