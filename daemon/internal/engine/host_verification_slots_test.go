package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func hostSlotsNotice(t *testing.T, w *productionPublicationWorkflow, runID domain.RunID, occurrence int) (domain.AttentionItem, bool) {
	t.Helper()
	item, found, err := readProductionQuarantineItem(t.Context(), w.store,
		productionQuarantineOccurrenceID(hostSlotsQuarantinedPrefix, runID, occurrence))
	if err != nil {
		t.Fatal(err)
	}
	return item, found
}

// slotRefusal is a lease refusal wrapped the way it reaches the reconcile
// loop: verify's recipe-command wrap, then runVerification's.
func slotRefusal(quarantined ...string) error {
	refused := &macverify.NoFreeSlotError{ExecutorRoot: "/executor", Slots: 2, Quarantined: quarantined}
	return fmt.Errorf("clean production verification: %w", productionPublicationRetryableError(
		fmt.Errorf("recipe command %q: %w", "xcodebuild", refused)))
}

// TestQuarantinedHostSlotsRaiseOneNotice: a verification job whose executor
// has every slot quarantined keeps the publication_environment hold and
// raises one notice naming the executor and the count; a second pass
// converges on it. A busy executor holds the same way and raises none (#3004).
func TestQuarantinedHostSlotsRaiseOneNotice(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	run, err := f.run(runID)
	if err != nil {
		t.Fatal(err)
	}
	w := &productionPublicationWorkflow{
		store: f.store, attention: f.signet, now: func() time.Time { return *f.now },
	}
	task := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID, VerificationID: "inv-verification"}

	if err := w.recordRetryableHold(ctx, task, slotRefusal("slot1")); err != nil {
		t.Fatal(err)
	}
	if hold := storedRunHold(t, f.store, runID); hold == nil || hold.Reason != domain.HoldPublicationEnvironment {
		t.Fatalf("hold = %+v, want publication_environment", hold)
	}
	if _, found := hostSlotsNotice(t, w, runID, 1); found {
		t.Fatal("a busy executor raised a notice")
	}

	for range 2 {
		if err := w.recordRetryableHold(ctx, task, slotRefusal("slot1", "slot2")); err != nil {
			t.Fatal(err)
		}
	}
	if hold := storedRunHold(t, f.store, runID); hold == nil || hold.Reason != domain.HoldPublicationEnvironment {
		t.Fatalf("hold = %+v, want publication_environment beside the notice", hold)
	}
	item, found := hostSlotsNotice(t, w, runID, 1)
	if !found || item.Status != domain.StatusOpen || item.Type != domain.AttentionExecutionFailure {
		t.Fatalf("notice = %+v (found %v), want an open execution_failure", item, found)
	}
	for _, want := range []string{"/executor", "all 2 of its 2 slots", "slot1, slot2"} {
		if !strings.Contains(item.Reason, want) {
			t.Fatalf("notice reason %q does not name %q", item.Reason, want)
		}
	}
	if _, found := hostSlotsNotice(t, w, runID, 2); found {
		t.Fatal("a second pass raised a second notice")
	}
}

// TestHostSlotsNoticeRetiresOnceASlotServes: the notice stays open until a
// host command gets past slot leasing and the job does not end on the
// all-quarantined refusal; a pass that never asked the executor, or an
// image-only recipe, leaves it alone.
func TestHostSlotsNoticeRetiresOnceASlotServes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newSpecificationFixture(t, false, 4)
	f.submit(t)
	runID := domain.RunID("specification-run")
	run, err := f.run(runID)
	if err != nil {
		t.Fatal(err)
	}
	w := &productionPublicationWorkflow{
		store: f.store, attention: f.signet, now: func() time.Time { return *f.now },
	}
	task := productionPublicationTask{RunID: runID, ProjectID: run.ProjectID, VerificationID: "inv-verification"}
	quarantined := slotRefusal("slot1", "slot2")
	if err := w.recordRetryableHold(ctx, task, quarantined); err != nil {
		t.Fatal(err)
	}
	hostRooms := func(served bool) map[verify.ExecutionClass]verify.Room {
		return map[verify.ExecutionClass]verify.Room{verify.ClassMacOSHost: &hostSlotsRoom{served: served}}
	}

	for name, keep := range map[string]struct {
		rooms map[verify.ExecutionClass]verify.Room
		err   error
	}{
		"an image-only recipe":              {map[verify.ExecutionClass]verify.Room{}, nil},
		"a pass that never asked the host":  {hostRooms(false), errors.New("image room fault")},
		"a later command meeting the slots": {hostRooms(true), quarantined},
	} {
		if err := w.retireHostSlotsNotice(ctx, task, keep.rooms, keep.err); err != nil {
			t.Fatal(err)
		}
		if item, _ := hostSlotsNotice(t, w, runID, 1); item.Status != domain.StatusOpen {
			t.Fatalf("notice status = %s after %s, want open", item.Status, name)
		}
	}

	if err := w.retireHostSlotsNotice(ctx, task, hostRooms(true), nil); err != nil {
		t.Fatal(err)
	}
	item, _ := hostSlotsNotice(t, w, runID, 1)
	if item.Status == domain.StatusOpen || item.Conclusion == nil ||
		item.Conclusion.Cause != domain.ConcludedBySupersession {
		t.Fatalf("notice = %+v, want it superseded once a slot served", item)
	}
}

// TestHostSlotsRoomRecordsAServedCommand: the room counts a command served
// unless the executor refused it with every slot quarantined.
func TestHostSlotsRoomRecordsAServedCommand(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err    error
		served bool
	}{
		"ran":             {nil, true},
		"busy":            {&macverify.NoFreeSlotError{Slots: 2, Quarantined: []string{"slot1"}}, true},
		"all quarantined": {slotRefusal("slot1", "slot2"), false},
	} {
		room := &hostSlotsRoom{Room: stubRoom{err: tc.err}}
		if _, err := room.Run(t.Context(), "", nil); !errors.Is(err, tc.err) {
			t.Fatalf("%s: Run = %v, want %v passed through", name, err, tc.err)
		}
		if room.served != tc.served {
			t.Fatalf("%s: served = %v, want %v", name, room.served, tc.served)
		}
	}
}

type stubRoom struct{ err error }

func (r stubRoom) Run(context.Context, string, []string) (verify.StepResult, error) {
	return verify.StepResult{}, r.err
}
