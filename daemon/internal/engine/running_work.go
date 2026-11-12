package engine

import (
	"context"
	"maps"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// RunningWork counts the work a restart would cut short that exists only in
// this process: verification jobs, image builds, probes, and publication
// passes (issue #3104). The restart readiness read adds these counts to the
// kinds the store records. Share one value per daemon. A nil *RunningWork
// counts nothing, for compositions and tests that serve no readiness read.
type RunningWork struct {
	mu     sync.Mutex
	counts map[domain.RestartBlockerKind]int
}

// NewRunningWork returns an empty counter.
func NewRunningWork() *RunningWork {
	return &RunningWork{counts: make(map[domain.RestartBlockerKind]int)}
}

// Begin counts one unit of kind as running until the returned end is called.
// end is safe to call more than once; only the first call counts.
func (r *RunningWork) Begin(kind domain.RestartBlockerKind) (end func()) {
	if r == nil {
		return func() {}
	}
	r.mu.Lock()
	r.counts[kind]++
	r.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			r.counts[kind]--
			if r.counts[kind] == 0 {
				delete(r.counts, kind)
			}
			r.mu.Unlock()
		})
	}
}

// Counts returns a copy of the running count of each kind.
func (r *RunningWork) Counts() map[domain.RestartBlockerKind]int {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.counts)
}

// BeginUnlessDraining starts one unit of in-process work of kind: it counts
// the work as running, then refuses with store.ErrUpdateDrainInForce while a
// drain is in force. The count comes first so a readiness read that lands
// between the check and the launch still sees the work; a refusal, or a
// failed read, ends the count before returning.
func (r *RunningWork) BeginUnlessDraining(
	ctx context.Context, st *store.Store, kind domain.RestartBlockerKind,
) (end func(), err error) {
	end = r.Begin(kind)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireNoUpdateDrain(ctx)
	}); err != nil {
		end()
		return nil, err
	}
	return end, nil
}
