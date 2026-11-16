package integration_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// newOverlapHarnesses returns two runs' exported publication tasks on one
// store, both run by p's engine.
func newOverlapHarnesses(t *testing.T) (p, q *productionPublicationHarness) {
	t.Helper()
	p = newProductionPublicationHarness(t, "")
	// The second submission's daemon reconciles p's run too, so p's
	// invocation must already have its publication task.
	p.startAndRecordExport(t)
	q = newProductionPublicationHarnessWithMetadata(
		t, p.publicationHarness, "", nil, nil, nil, engine.ProductionPublication{}, p, "", "second-publication",
	)
	if q.runID == p.runID {
		t.Fatal("the second submission reused the first run")
	}
	// A client submission publishes only the candidate's public account.
	q.replay = withPublicAccount(t, q, q.replay, publicAccount)
	q.startAndRecordExport(t)
	// p's engine runs both tasks, so its reviewer answers both reviews.
	p.reviewer.Script(engine.ProductionReviewInvocationID(q.runID, 1), fake.ReviewScript{
		Outcome: fake.OutcomeComplete,
		Result: exec.ReviewResult{
			BaseSHA: q.baseSHA, HeadSHA: q.replay.HeadSHA,
			CompletedAt:        q.now.UTC(),
			CompletionEvidence: productionDigest([]byte("clean review")),
		},
	})
	return p, q
}

// overlapPublished reports whether both runs have one pull request each and
// no publication task left pending.
func overlapPublished(t *testing.T, p, q *productionPublicationHarness) bool {
	t.Helper()
	if _, prs := p.forge.Counts(); prs != 2 {
		return false
	}
	for _, runID := range []domain.RunID{p.runID, q.runID} {
		var task store.QueueEntry
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			var err error
			task, err = tx.GetOutbox(p.ctx, "production-publication/"+string(runID))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !task.Dispatched() {
			return false
		}
	}
	return true
}

// overlapState describes where each run's publication stands, for a
// failure message: its hold, if any, and the pending tasks.
func overlapState(t *testing.T, p, q *productionPublicationHarness) string {
	t.Helper()
	var b strings.Builder
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		for _, runID := range []domain.RunID{p.runID, q.runID} {
			hold, found, err := tx.GetRunHold(p.ctx, runID)
			if err != nil {
				return err
			}
			fmt.Fprintf(&b, "run %s hold found=%t %+v; ", runID, found, hold)
		}
		pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
		for _, entry := range pending {
			fmt.Fprintf(&b, "pending %s; ", entry.IdempotencyKey)
		}
		return err
	}); err != nil {
		return err.Error()
	}
	for _, runID := range []domain.RunID{p.runID, q.runID} {
		if item, err := p.attention.GetAttentionItem(p.ctx, domain.ItemID("production-publish-blocked-"+string(runID))); err == nil {
			fmt.Fprintf(&b, "run %s blocked: %s; ", runID, item.Item.Reason)
		}
	}
	return b.String()
}

// TestIndependentTasksPublishAtOnce (#3214): with two runs' tasks pending,
// one task parks in its base fetch while the other fetches, verifies, is
// reviewed, and opens its pull request; the parked task then finishes on
// the same loop. Under the serial lane the second task waited for the first.
func TestIndependentTasksPublishAtOnce(t *testing.T) {
	t.Parallel()
	p, q := newOverlapHarnesses(t)

	entered, release := p.transport.BlockNextFetch()
	t.Cleanup(release)
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	loop := make(chan error, 1)
	go func() { loop <- p.workflow.RunProductionPublications(ctx, time.Millisecond) }()
	select {
	case <-entered:
	case err := <-loop:
		t.Fatalf("publication loop returned before a task fetched: %v", err)
	case <-time.After(publicationBoundaryWait):
		t.Fatal("no task reached its base fetch")
	}

	await := func(phase string, done func() bool) {
		t.Helper()
		deadline := time.After(publicationBoundaryWait)
		for !done() {
			select {
			case err := <-loop:
				t.Fatalf("%s: publication loop returned: %v", phase, err)
			case <-deadline:
				t.Fatalf("%s: timed out: %s", phase, overlapState(t, p, q))
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	await("one pull request while the other task is parked in its fetch", func() bool {
		_, prs := p.forge.Counts()
		return prs >= 1
	})
	if _, prs := p.forge.Counts(); prs != 1 {
		t.Fatalf("pull requests while one task is parked = %d, want 1", prs)
	}

	release()
	await("both published after the parked fetch is released", func() bool { return overlapPublished(t, p, q) })
	cancel()
	if err := <-loop; err != nil {
		t.Fatalf("publication loop = %v", err)
	}
}

// TestOverlappingVerificationsSurviveARestart (#3214): two runs' tasks are
// in verification at once, which the serial lane never allowed, and a
// shutdown in the middle cancels both; a fresh engine over the same store
// then publishes both.
func TestOverlappingVerificationsSurviveARestart(t *testing.T) {
	t.Parallel()
	p, q := newOverlapHarnesses(t)
	var (
		mu      sync.Mutex
		arrived int
	)
	both := make(chan struct{})
	p.room.gate = func(ctx context.Context) error {
		mu.Lock()
		arrived++
		if arrived == 2 {
			close(both)
		}
		mu.Unlock()
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, cancel := context.WithCancel(p.ctx)
	defer cancel()
	loop := make(chan error, 1)
	go func() { loop <- p.workflow.RunProductionPublications(ctx, time.Millisecond) }()
	select {
	case <-both:
	case err := <-loop:
		t.Fatalf("publication loop returned before both verifications started: %v", err)
	case <-time.After(publicationBoundaryWait):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("%d verifications in the room at once, want 2", arrived)
	}
	cancel()
	select {
	case err := <-loop:
		if err != nil {
			t.Fatalf("canceled publication loop = %v", err)
		}
	case <-time.After(publicationBoundaryWait):
		t.Fatal("canceled publication loop leaked a task goroutine")
	}
	if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
		t.Fatalf("interrupted verifications caused effects: %d refs, %d PRs", refs, prs)
	}

	p.room.gate = nil
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	for pass := 0; ; pass++ {
		if _, err := p.workflow.ReconcileProductionPublications(p.ctx); err != nil {
			t.Fatalf("pass %d after restart: %v", pass, err)
		}
		if overlapPublished(t, p, q) {
			break
		}
		if pass == 20 {
			t.Fatalf("the restarted lane did not publish both runs: %s", overlapState(t, p, q))
		}
	}
}
