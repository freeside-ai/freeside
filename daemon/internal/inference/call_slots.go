package inference

import (
	"context"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// callSlotKey is one site on one usage pool: the unit the pool's call limit
// counts (domain.UsagePool.MaxCallsPerSite).
type callSlotKey struct {
	pool domain.UsagePoolID
	site string
}

// callSlots counts the judgment calls running at each site on each pool and
// queues the calls waiting for a place, first come first served. Its mutex
// guards only the counts and queues and is never held while a call waits or
// runs: a waiter blocks on its own channel, so a waiting call holds nothing
// another task needs.
type callSlots struct {
	mu    sync.Mutex
	state map[callSlotKey]*callSlotState
}

type callSlotState struct {
	running int
	// limit is the pool's call limit as the latest arriving call read it. A
	// raised limit admits waiters when the next call arrives or ends; nothing
	// wakes them at the moment of the edit.
	limit   int
	waiters []chan struct{}
}

func newCallSlots() *callSlots {
	return &callSlots{state: map[callSlotKey]*callSlotState{}}
}

// acquire takes a place at key under limit, waiting behind earlier callers
// until one is free or ctx ends. waited reports whether the caller queued, so
// it knows the admission it read before the wait may be stale. On a ctx
// error the caller holds no place.
func (s *callSlots) acquire(ctx context.Context, key callSlotKey, limit int) (waited bool, err error) {
	s.mu.Lock()
	state := s.state[key]
	if state == nil {
		state = &callSlotState{}
		s.state[key] = state
	}
	state.limit = limit
	s.promoteLocked(state)
	if len(state.waiters) == 0 && state.running < state.limit {
		state.running++
		s.mu.Unlock()
		return false, nil
	}
	granted := make(chan struct{})
	state.waiters = append(state.waiters, granted)
	s.mu.Unlock()

	select {
	case <-granted:
		return true, nil
	case <-ctx.Done():
	}
	s.mu.Lock()
	for index, waiter := range state.waiters {
		if waiter == granted {
			state.waiters = append(state.waiters[:index], state.waiters[index+1:]...)
			s.forgetLocked(key, state)
			s.mu.Unlock()
			return true, ctx.Err()
		}
	}
	s.mu.Unlock()
	// The place was granted as ctx ended; hand it to the next waiter.
	s.release(key)
	return true, ctx.Err()
}

// release gives back a place acquire granted and admits the next waiters the
// limit allows.
func (s *callSlots) release(key callSlotKey) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state[key]
	state.running--
	s.promoteLocked(state)
	s.forgetLocked(key, state)
}

func (s *callSlots) promoteLocked(state *callSlotState) {
	for len(state.waiters) > 0 && state.running < state.limit {
		close(state.waiters[0])
		state.waiters = state.waiters[1:]
		state.running++
	}
}

func (s *callSlots) forgetLocked(key callSlotKey, state *callSlotState) {
	if state.running == 0 && len(state.waiters) == 0 {
		delete(s.state, key)
	}
}
