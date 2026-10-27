package engine

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// errProjectImageBuilderBusy is a publication rebuild that found the builder
// held. The lane does not wait: the task stays queued, and its next pass
// finds the image the holder built if that one serves its commit.
var errProjectImageBuilderBusy = errors.New("the project-image builder is busy with another build")

// projectImageBuildGate lets one project-image build run at a time per
// daemon (plan §5.7, Project-image upkeep). Upkeep and the publication lane
// share it through their one ProjectImageRebuild. A waiter takes the gate in
// the order it asked, oldest first; a publication rebuild never waits, so a
// build in progress never holds the lane. The zero value is an open gate.
type projectImageBuildGate struct {
	mu   sync.Mutex
	held bool
	// waiters are the turns of blocked acquirers, oldest first. Release
	// hands the gate to the head by closing its channel, so a waiter never
	// races a newcomer for it.
	waiters []chan struct{}
}

// tryAcquire takes the gate only when nobody holds it or waits for it.
func (g *projectImageBuildGate) tryAcquire() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held || len(g.waiters) > 0 {
		return false
	}
	g.held = true
	return true
}

// acquire waits for the gate behind every earlier waiter, or until ctx ends.
func (g *projectImageBuildGate) acquire(ctx context.Context) error {
	g.mu.Lock()
	if !g.held && len(g.waiters) == 0 {
		g.held = true
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
	// The gate was handed over as ctx ended; pass it on.
	g.release()
	return context.Cause(ctx)
}

// release hands the gate to the oldest waiter, or opens it.
func (g *projectImageBuildGate) release() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.waiters) == 0 {
		g.held = false
		return
	}
	next := g.waiters[0]
	g.waiters = g.waiters[1:]
	close(next)
}
