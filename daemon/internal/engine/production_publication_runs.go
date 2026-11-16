package engine

import (
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/keyedrun"
)

// laneRuns is the publication lane's per-run group: at most one goroutine
// per run, whether a task, an end-of-task proof, or another holder of the
// run (#3214). Beside keyedrun.Group it keeps two facts the lane needs:
//
//   - which runs are busy, so a pass can skip every run whose goroutine was
//     running when it read the pending tasks. That goroutine may finish
//     before the pass starts the run again, and the pass's read would then
//     hand a task it already finished to a new goroutine.
//   - whether the lane has shut down, so no run starts once Wait has begun:
//     an operator release arriving during shutdown would otherwise add to
//     the group while Wait waits.
//
// Its zero value is ready to use.
type laneRuns struct {
	group  keyedrun.Group[domain.RunID]
	mu     sync.Mutex
	busy   map[domain.RunID]bool
	closed bool
}

// Go starts fn as runID's goroutine and returns a channel closed when fn
// returns, or returns nil and starts nothing when runID is busy or the lane
// has shut down.
func (l *laneRuns) Go(runID domain.RunID, fn func()) <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	if l.busy == nil {
		l.busy = map[domain.RunID]bool{}
	}
	// The goroutine clears busy under mu, so it cannot clear it before this
	// call sets it. It clears it once fn has returned, a moment before
	// keyedrun frees the run: a snapshot can miss a run in that moment, but
	// only after the run's work is done, and Go still refuses the run.
	done := l.group.Go(runID, func() {
		defer func() {
			l.mu.Lock()
			delete(l.busy, runID)
			l.mu.Unlock()
		}()
		fn()
	})
	if done != nil {
		l.busy[runID] = true
	}
	return done
}

// Busy returns the runs whose goroutine is running now.
func (l *laneRuns) Busy() map[domain.RunID]bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	busy := make(map[domain.RunID]bool, len(l.busy))
	for runID := range l.busy {
		busy[runID] = true
	}
	return busy
}

// Wait refuses every later Go and blocks until every started goroutine has
// returned.
func (l *laneRuns) Wait() {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.group.Wait()
}
