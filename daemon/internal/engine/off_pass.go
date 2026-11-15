package engine

import (
	"context"
	"errors"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/keyedrun"
)

// researchFetches runs a specification attempt's research fetches outside
// the reconcile pass, one run per invocation, and keeps their artifacts for
// the pass that next asks for them, so one task's slow fetch never holds up
// another task's dispatch (#3215). The pass that takes a result re-checks, as
// it always has, the state the fetch was made against before it writes
// anything.
//
// A result lives only in memory. A restart forgets it and the next pass
// fetches again, which recovers what the earlier fetch registered.
type researchFetches struct {
	runs keyedrun.Group[domain.InvocationID]
	mu   sync.Mutex
	done map[domain.InvocationID]fetchedResearch
}

type fetchedResearch struct {
	ids []domain.ArtifactID
	err error
}

// take returns id's finished fetch and forgets it, with ok true. With no
// finished fetch it starts fetch unless one for id is running, and returns
// ok false. A fetch whose context ended keeps no result, so the next pass
// fetches again rather than reading a cancellation as the fetch's answer.
func (r *researchFetches) take(
	ctx context.Context, id domain.InvocationID, fetch func(context.Context) ([]domain.ArtifactID, error),
) (ids []domain.ArtifactID, ok bool, err error) {
	// Holding mu across the check and the start means a run for id either
	// stored its result before the check or is still running, so a finished
	// fetch is never started again.
	r.mu.Lock()
	defer r.mu.Unlock()
	result, ok := r.done[id]
	delete(r.done, id)
	if ok {
		return result.ids, true, result.err
	}
	r.runs.Go(id, func() {
		ids, err := fetch(ctx)
		if ctx.Err() != nil {
			return
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.done == nil {
			r.done = map[domain.InvocationID]fetchedResearch{}
		}
		r.done[id] = fetchedResearch{ids: ids, err: err}
	})
	return nil, false, nil
}

// offPassOutcomes carries what work done outside a pass accepted, and any
// error that would have stopped the pass had the work run inside it, to the
// next pass, which reports the count and returns the errors (#3215). A
// correctness error so still stops the engine loop rather than vanishing into
// a log.
type offPassOutcomes struct {
	mu       sync.Mutex
	accepted int
	errs     []error
}

func (o *offPassOutcomes) record(accepted bool, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if accepted {
		o.accepted++
	}
	if err != nil {
		o.errs = append(o.errs, err)
	}
}

// collect returns and forgets what was recorded since the last collect.
func (o *offPassOutcomes) collect() (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	accepted, err := o.accepted, errors.Join(o.errs...)
	o.accepted, o.errs = 0, nil
	return accepted, err
}
