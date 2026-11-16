package engine

import "testing"

// reconcileSettled runs passes until no off-pass call holds a result a pass
// has yet to take, and returns the passes' summed counts. A test that
// expects a model call or research fetch to be answered uses it where one
// pass used to finish the call (#3215).
func reconcileSettled(t *testing.T, e *Engine) (ReconcileResult, error) {
	t.Helper()
	var sum ReconcileResult
	for range 10 {
		result, err := e.Reconcile(t.Context())
		sum.RunTransitions += result.RunTransitions
		sum.InvocationsStarted += result.InvocationsStarted
		sum.ResultsAccepted += result.ResultsAccepted
		sum.PublicationTasksCompleted += result.PublicationTasksCompleted
		sum.ReadyCleanItemsCreated += result.ReadyCleanItemsCreated
		sum.ReadyDegradedItemsCreated += result.ReadyDegradedItemsCreated
		sum.ReadyItemsCreated += result.ReadyItemsCreated
		sum.BlockedItemsCreated += result.BlockedItemsCreated
		if result.LastPRNumber != 0 {
			sum.LastPRNumber = result.LastPRNumber
		}
		if err != nil {
			return sum, err
		}
		e.waitOffPass()
		if !e.discussionOutcomes.holdsOutcome() && !e.research.holdsResult() {
			return sum, nil
		}
	}
	t.Fatal("off-pass calls kept producing results after 10 passes")
	return sum, nil
}

// holdsResult reports whether a finished fetch waits for a pass to take it.
func (r *researchFetches) holdsResult() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.done) > 0
}

// holdsOutcome reports whether work outside a pass recorded something the
// next pass has yet to collect.
func (o *offPassOutcomes) holdsOutcome() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.accepted > 0 || len(o.errs) > 0
}
