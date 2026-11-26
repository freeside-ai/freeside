package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// errProjectImageBuilderBusy is a publication rebuild that found every build
// slot held. The lane does not wait: the task stays queued, and its next
// pass finds the image a holder built if that one serves its commit.
var errProjectImageBuilderBusy = errors.New("the project-image builder is busy with other builds")

// projectImageBuildGate lets at most a limit of project-image builds run at
// once per daemon (plan §5.7, Project-image upkeep); a limit below one means
// one. Upkeep and the publication lane share it through their one
// ProjectImageRebuild. A slot bounds a build's whole run; the host's one
// builder VM is a separate turn the builder takes (projectimage), and one
// repository's builds still take turns on its pointer. A waiter takes a
// slot in the order it asked, oldest first; a publication rebuild never
// waits, so a build in progress never holds the lane. The zero value is an
// open gate.
type projectImageBuildGate struct {
	mu   sync.Mutex
	held int
	// waiters are the turns of blocked acquirers, oldest first. Release
	// hands a slot to the head by closing its channel, so a waiter never
	// races a newcomer for it.
	waiters []chan struct{}
}

// free reports whether a slot is free for a newcomer under limit: one is
// unheld and nobody waits for one. The caller holds g.mu.
func (g *projectImageBuildGate) free(limit int) bool {
	return g.held < max(limit, 1) && len(g.waiters) == 0
}

// tryAcquire takes a slot only when one is free and nobody waits for one.
func (g *projectImageBuildGate) tryAcquire(limit int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.free(limit) {
		return false
	}
	g.held++
	return true
}

// acquire waits for a slot behind every earlier waiter, or until ctx ends.
func (g *projectImageBuildGate) acquire(ctx context.Context, limit int) error {
	g.mu.Lock()
	if g.free(limit) {
		g.held++
		g.mu.Unlock()
		return nil
	}
	turn := make(chan struct{})
	g.waiters = append(g.waiters, turn)
	g.mu.Unlock()
	select {
	case <-turn:
		return nil
	case <-ctx.Done():
	}
	g.mu.Lock()
	if index := slices.Index(g.waiters, turn); index >= 0 {
		g.waiters = slices.Delete(g.waiters, index, index+1)
		g.mu.Unlock()
		return context.Cause(ctx)
	}
	g.mu.Unlock()
	// A slot was handed over as ctx ended; pass it on.
	g.release()
	return context.Cause(ctx)
}

// release hands the caller's slot to the oldest waiter, or frees it.
func (g *projectImageBuildGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.waiters) == 0 {
		g.held--
		return
	}
	next := g.waiters[0]
	g.waiters = g.waiters[1:]
	close(next)
}
