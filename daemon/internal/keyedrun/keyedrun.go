// Package keyedrun runs work for each key in its own goroutine, at most one
// run per key at a time, so one key's slow network call never delays
// another key's work (#3215). The daemon's GitHub-facing passes key their
// items by project: a project names one repository, so a stalled repository
// holds up only its own items.
package keyedrun

import (
	"errors"
	"sync"
)

// Group tracks the runs it started. Its zero value is ready to use. A key
// whose previous run has not returned is skipped, not queued: the caller's
// next pass picks its work up again, so a stalled key never accumulates
// waiting runs.
type Group[K comparable] struct {
	mu      sync.Mutex
	running map[K]bool
	runs    sync.WaitGroup
}

// Go starts fn for key in its own goroutine and returns a channel closed when
// fn returns, or returns nil and starts nothing when key's previous run has
// not returned.
func (g *Group[K]) Go(key K, fn func()) <-chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.running[key] {
		return nil
	}
	if g.running == nil {
		g.running = map[K]bool{}
	}
	g.running[key] = true
	done := make(chan struct{})
	g.runs.Go(func() {
		defer func() {
			g.mu.Lock()
			delete(g.running, key)
			g.mu.Unlock()
			close(done)
		}()
		fn()
	})
	return done
}

// Running reports whether key's run is in progress.
func (g *Group[K]) Running(key K) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.running[key]
}

// Wait blocks until every run Go started has returned. Shutdown calls it so
// no run outlives its owner.
func (g *Group[K]) Wait() { g.runs.Wait() }

// waitAll blocks until every channel Go returned has closed; nil channels,
// for skipped keys, are ignored. A pass uses it to wait for its own runs and
// no others.
func waitAll(done []<-chan struct{}) {
	for _, ch := range done {
		if ch != nil {
			<-ch
		}
	}
}

// Part is one key's share of a pass's items.
type Part[K comparable, V any] struct {
	Key   K
	Items []V
}

// Partition groups items by key, each key's items in their given order, and
// returns the parts in the order each key first appears, so a pass starts
// its keys in the order it read them.
func Partition[K comparable, V any](items []V, key func(V) K) []Part[K, V] {
	var parts []Part[K, V]
	index := map[K]int{}
	for _, item := range items {
		k := key(item)
		i, seen := index[k]
		if !seen {
			i = len(parts)
			index[k] = i
			parts = append(parts, Part[K, V]{Key: k})
		}
		parts[i].Items = append(parts[i].Items, item)
	}
	return parts
}

// Run starts fn on each part's items under its key in g, skipping a key whose
// previous run has not returned, waits for the runs it started, and joins
// their errors.
func Run[K comparable, V any](g *Group[K], parts []Part[K, V], fn func([]V) error) error {
	var (
		mu   sync.Mutex
		errs []error
		done = make([]<-chan struct{}, 0, len(parts))
	)
	for _, part := range parts {
		done = append(done, g.Go(part.Key, func() {
			if err := fn(part.Items); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}))
	}
	waitAll(done)
	return errors.Join(errs...)
}
