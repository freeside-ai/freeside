package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// errBaseLeftBranch is the refusal image selection returns for a base the
// fetch found off its branch.
var errBaseLeftBranch = fmt.Errorf("select project image: base 1111 is no longer on refs/heads/main: %w",
	ErrRunBaseLeftBranch)

// recordRunBase stores base as the run's recorded base, as its first
// admission would have.
func recordRunBase(t *testing.T, st *store.Store, id domain.RunID, base domain.BaseRevision) {
	t.Helper()
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		run, err := tx.GetRun(t.Context(), id)
		if err != nil {
			return err
		}
		run.Base = &base
		return tx.PutRun(t.Context(), run)
	}); err != nil {
		t.Fatal(err)
	}
}

func outboxDispatched(t *testing.T, st *store.Store, id domain.InvocationID) bool {
	t.Helper()
	var entry store.QueueEntry
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), string(id))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return entry.Dispatched()
}

func attentionItemExists(t *testing.T, st *store.Store, id domain.ItemID) bool {
	t.Helper()
	var found bool
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(t.Context(), id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		found = err == nil
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

// A run with no recorded base selects at the stored tip, which the tip watch
// may not have reread since a force-push. Its base off the branch holds the
// attempt as input_unavailable and keeps the intent pending, so a later pass
// admits it at the new tip. Once the run has recorded that base, the same
// refusal stops the attempt with a failed specification and its card, and
// ends the hold.
func TestASpecificationStopsOnlyWhenItsRecordedBaseLeftTheBranch(t *testing.T) {
	t.Parallel()
	h := newSelectionHoldFixture(t)
	h.selectErr = errBaseLeftBranch
	failureItem := specificationFailureItemID(h.invocationID)

	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d at a tip off its branch", started)
	}
	if hold, found := h.hold(t); !found || hold.Reason != domain.HoldInputUnavailable {
		t.Fatalf("hold = %+v, found %t; want input_unavailable", hold, found)
	}
	if outboxDispatched(t, h.fixture.store, h.invocationID) || attentionItemExists(t, h.fixture.store, failureItem) {
		t.Fatal("a run with no recorded base was stopped")
	}

	recordRunBase(t, h.fixture.store, "specification-run", testTip("1111111111111111111111111111111111111111"))
	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d at a recorded base off its branch", started)
	}
	if !outboxDispatched(t, h.fixture.store, h.invocationID) {
		t.Fatal("the stopped specification's intent is still pending")
	}
	if !attentionItemExists(t, h.fixture.store, failureItem) {
		t.Fatal("the stopped specification raised no failure item")
	}
	if hold, found := h.hold(t); found {
		t.Fatalf("hold = %+v after the stop, want it ended", hold)
	}
	selections := len(h.bases)
	h.dispatch(t)
	if len(h.bases) != selections {
		t.Fatal("a pass after the stop selected the image again")
	}
}

// A production attempt whose run's recorded base could not be read holds as
// input_unavailable and stays pending. Once the fetch finds the base off its
// branch, the attempt stops with an execution-failure card, leaves the
// pending outbox, ends the hold, and records no attempt.
func TestAProductionAttemptStopsWhenItsRecordedBaseLeftTheBranch(t *testing.T) {
	t.Parallel()
	h := newSelectionHoldFixture(t)
	h.selected = selectionTestImage
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("specification started %d, want 1", started)
	}
	if result, err := h.engine.Reconcile(t.Context()); err != nil || result.ResultsAccepted != 1 {
		t.Fatalf("specification reconcile = %+v, %v", result, err)
	}
	h.engine.admission.environment.OperatingMode = domain.ModeUnattended
	const runID = domain.RunID("implementation-run")
	implementationID := productionInvocationID(runID)
	recordRunBase(t, h.fixture.store, runID, testTip("1111111111111111111111111111111111111111"))
	runHold := func() (domain.RunHoldObservation, bool) {
		t.Helper()
		var (
			hold  domain.RunHoldObservation
			found bool
		)
		if err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			hold, found, err = tx.GetRunHold(t.Context(), runID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return hold, found
	}

	h.selectErr = fmt.Errorf("select project image: %w", errors.Join(exec.ErrInputUnavailable, errors.New("fetch failed")))
	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d while the base could not be read", started)
	}
	if hold, found := runHold(); !found || hold.Reason != domain.HoldInputUnavailable {
		t.Fatalf("hold = %+v, found %t; want input_unavailable", hold, found)
	}
	if outboxDispatched(t, h.fixture.store, implementationID) {
		t.Fatal("a transient base read stopped the attempt")
	}

	h.selectErr = errBaseLeftBranch
	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d at a recorded base off its branch", started)
	}
	if !outboxDispatched(t, h.fixture.store, implementationID) {
		t.Fatal("the stopped attempt's intent is still pending")
	}
	if !attentionItemExists(t, h.fixture.store, domain.ExecutionFailureItemID(implementationID)) {
		t.Fatal("the stopped attempt raised no execution-failure item")
	}
	if hold, found := runHold(); found {
		t.Fatalf("hold = %+v after the stop, want it ended", hold)
	}
	if run := getRun(t, h.fixture.store, runID); attemptRecorded(run, implementationID) {
		t.Fatal("the stopped attempt was recorded")
	}
}
