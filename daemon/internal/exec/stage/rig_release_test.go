package stage

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// Recovery of an intent copy read before a Cancel made the intent terminal
// leaves the terminal record alone and starts no handoff, so a terminal
// release stays final.
func TestReconcileIntentSkipsACopyThatBecameTerminal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	gate := &stubGate{
		cancelFn:       func(string) error { return ward.ErrJournalRecordNotFound },
		handoffStarted: func(string) (bool, error) { return false, nil },
	}
	d := newTestDriver(t, gate, newStubExports())
	stale := orphan(t, d, phaseRunning, nil)
	if err := d.Cancel(ctx, testInvoke); err != nil {
		t.Fatal(err)
	}
	if err := d.reconcileIntent(ctx, stale); err != nil {
		t.Fatal(err)
	}
	waitSessionDone(t, d, testInvoke)
	if result, err := d.Collect(ctx, testInvoke); err != nil || result.Status != exec.StatusCanceled {
		t.Fatalf("Collect = %+v, %v; want the committed cancellation", result, err)
	}
	gate.mu.Lock()
	handoffs := len(gate.specs)
	gate.mu.Unlock()
	if handoffs != 0 {
		t.Fatalf("stale recovery started %d handoffs, want none", handoffs)
	}
}
