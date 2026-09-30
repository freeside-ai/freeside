package ward

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// stallWatch reports a running writer's stall heartbeat to its handoff's
// Stall hook (plan §5.12). The heartbeat is the last provider response byte
// the daemon-side proxy forwarded, never anything the agent reports, and the
// watch only reads it: the writer wait's poll count, deadline, and result
// are the same with or without a watch.
//
// Each poll compares the heartbeat to the interval and, when the answer
// differs from what the hook last accepted, starts one hook call off the
// wait's path. While a call is in flight later polls report nothing, so
// reports stay ordered; a failed call is logged and retried by a later poll.
type stallWatch struct {
	hook     func(context.Context, bool) error
	interval time.Duration
	started  time.Time
	now      func() time.Time
	lastByte func() time.Time
	// base carries the handoff's values without its cancellation: the
	// concluding call runs after a hard-budget expiry has already ended the
	// wait's own context.
	base    context.Context
	timeout time.Duration
	logger  *slog.Logger
	runID   string

	calls    sync.WaitGroup
	mu       sync.Mutex
	reported bool
	inFlight bool
}

// newStallWatch starts a watch at the writer's start, or returns nil when
// the handoff asked for none. A nil watch's methods do nothing.
func newStallWatch(ctx context.Context, cfg Config, hs HandoffSpec, lastByte func() time.Time) *stallWatch {
	if hs.Stall == nil {
		return nil
	}
	return &stallWatch{
		hook:     hs.Stall,
		interval: cfg.StallInterval,
		started:  cfg.Now(),
		now:      cfg.Now,
		lastByte: lastByte,
		base:     context.WithoutCancel(ctx),
		timeout:  cfg.TeardownTimeout,
		logger:   cfg.Logger,
		runID:    hs.RunID,
	}
}

// poll runs once per writer-wait observation of a container still running.
func (w *stallWatch) poll() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inFlight {
		return
	}
	heartbeat := w.started
	if last := w.lastByte(); last.After(heartbeat) {
		heartbeat = last
	}
	stalled := w.now().Sub(heartbeat) >= w.interval
	if stalled == w.reported {
		return
	}
	w.inFlight = true
	w.calls.Go(func() {
		accepted := w.report(stalled)
		w.mu.Lock()
		defer w.mu.Unlock()
		if accepted {
			w.reported = stalled
		}
		w.inFlight = false
	})
}

// finish runs once the writer wait has returned, whatever it returned. It
// waits out an in-flight call so a late stall report cannot land after the
// conclusion, then concludes a stall still reported.
func (w *stallWatch) finish() {
	if w == nil {
		return
	}
	w.calls.Wait()
	if w.reported {
		// A failure here leaves the notice open until the daemon's startup
		// sweep concludes it; the handoff result is unaffected either way.
		w.report(false)
	}
}

func (w *stallWatch) report(stalled bool) bool {
	ctx, cancel := context.WithTimeout(w.base, w.timeout)
	defer cancel()
	if err := w.hook(ctx, stalled); err != nil {
		if w.logger != nil {
			w.logger.Warn("ward stall report failed", "run", w.runID, "stalled", stalled, "error", err.Error())
		}
		return false
	}
	return true
}
