package engine

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A remediate route leaves no disposition behind, so the adjudication gate is
// re-entered until the export arrives. The queued invocation is the durable
// fact that the round's intent was committed; re-preparing it re-put the
// input artifact and stopped the daemon on the first live remediation.
func TestRemediationDispatchedReflectsQueuedInvocation(t *testing.T) {
	ctx := context.Background()
	_, st := newQuarantineEngine(t, ctx)
	w := &productionPublicationWorkflow{store: st}
	task := productionPublicationTask{RunID: "run-dispatch-guard"}
	record := domain.ReviewRecord{Round: 1}
	if dispatched, err := w.remediationDispatched(ctx, task, record); err != nil || dispatched {
		t.Fatalf("before queueing: dispatched %t, err %v", dispatched, err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, string(remediationInvocationID(task.RunID, 1)),
			KindRemediationInvocationRequested, []byte("{}"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if dispatched, err := w.remediationDispatched(ctx, task, record); err != nil || !dispatched {
		t.Fatalf("after queueing: dispatched %t, err %v", dispatched, err)
	}
	if dispatched, err := w.remediationDispatched(ctx, task, domain.ReviewRecord{Round: 2}); err != nil || dispatched {
		t.Fatalf("other round: dispatched %t, err %v", dispatched, err)
	}
}
