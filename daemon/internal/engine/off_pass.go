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

// startChecks runs an invocation's first start-authority check outside the
// reconcile pass, one run per invocation, and keeps its answer for the pass
// that next reaches the invocation, so one repository's hung GitHub call
// never holds up another project's dispatch (#3224). The answer only decides
// when Start is called: Start runs the same check again, so a passed check
// grants nothing on its own.
//
// An answer lives only in memory. A restart forgets it and the next pass
// checks again.
type startChecks struct {
	runs keyedrun.Group[domain.InvocationID]
	mu   sync.Mutex
	done map[domain.InvocationID]error
}

// take returns id's finished check and forgets it, with done true and the
// check's error. With no finished check it starts check unless one for id is
// running, and returns done false. A check whose context ended keeps no
// answer, so the next pass checks again rather than reading a cancellation as
// a refusal.
func (c *startChecks) take(
	ctx context.Context, id domain.InvocationID, check func(context.Context) error,
) (done bool, err error) {
	// Holding mu across the lookup and the start means a run for id either
	// stored its answer before the lookup or is still running, so a finished
	// check is never started again.
	c.mu.Lock()
	defer c.mu.Unlock()
	if err, ok := c.done[id]; ok {
		delete(c.done, id)
		return true, err
	}
	c.runs.Go(id, func() {
		err := check(ctx)
		if ctx.Err() != nil {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.done == nil {
			c.done = map[domain.InvocationID]error{}
		}
		c.done[id] = err
	})
	return false, nil
}

// errStartAuthorityPending holds an invocation's start while its
// start-authority check runs outside the pass. The next pass that reaches
// the invocation finds the check's answer.
var errStartAuthorityPending = errors.New("start authority check in progress")

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
