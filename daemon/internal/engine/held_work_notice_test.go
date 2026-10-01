package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// heldWorkScenario is one hold path driven to a recorded refusal hold. Each
// pass builds a fresh Engine over the scenario's current store, so a repeated
// refusing pass is also the daemon-restart case: no pacing state survives it.
type heldWorkScenario struct {
	runID  domain.RunID
	reason domain.RunHoldReason
	store  func() *store.Store
	// refuse runs one more reconcile pass that still refuses.
	refuse func(t *testing.T)
	// recover lifts the refusal and runs one reconcile pass.
	recover func(t *testing.T)
}

func reconcileOnce(t *testing.T, e *Engine) {
	t.Helper()
	if _, err := e.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

// heldWorkDispatchScenario holds a specification attempt at dispatch: the
// admission floor is raised before the first pass, so the invocation is never
// handed to the driver.
func heldWorkDispatchScenario(t *testing.T) heldWorkScenario {
	t.Helper()
	f := newSpecificationFixture(t, true, 2)
	driver := f.newDriver(t)
	if err := specifyfake.Script(driver, specificationInvocationID("specification-run", 1), 0, 1, specify.Output{
		Specification: &specify.Specification{
			Summary: "The implementation contract is ready.", Body: "# Specification\n\nDispatch after the hold.",
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	f = f.withRaisedFloor(t)
	reconcileOnce(t, f.newEngine(t, driver))
	f.requireHold(t, domain.HoldAdmissionPolicyRefused)
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		observation, err := tx.ObserveRun(t.Context(), "specification-run")
		if err != nil {
			return err
		}
		for _, milestone := range observation.Milestones {
			if milestone.Kind == domain.MilestoneInvocationStarted {
				t.Fatal("the refused invocation started; the hold is not a dispatch hold")
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("observe run: %v", err)
	}
	return heldWorkScenario{
		runID: "specification-run", reason: domain.HoldAdmissionPolicyRefused,
		store:  func() *store.Store { return f.store },
		refuse: func(t *testing.T) { reconcileOnce(t, f.newEngine(t, driver)) },
		recover: func(t *testing.T) {
			f = f.reopen(t)
			reconcileOnce(t, f.newEngine(t, driver))
		},
	}
}

func heldWorkSpecificationCollectScenario(t *testing.T) heldWorkScenario {
	t.Helper()
	f, driver := scriptedSpecificationRefusal(t)
	return heldWorkScenario{
		runID: "specification-run", reason: domain.HoldAdmissionPolicyRefused,
		store:  func() *store.Store { return f.store },
		refuse: func(t *testing.T) { reconcileOnce(t, f.newEngine(t, driver)) },
		recover: func(t *testing.T) {
			f = f.reopen(t)
			reconcileOnce(t, f.newEngine(t, driver))
		},
	}
}

// heldWorkLeakMarker rides the production scenario's refusal error so a test
// can prove error text never reaches the notice.
const heldWorkLeakMarker = "refusal-detail-that-must-not-leak"

func heldWorkProductionCollectScenario(t *testing.T) heldWorkScenario {
	t.Helper()
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
	driver := &switchableInspectDriver{
		StageDriver: f.driver,
		err:         fmt.Errorf("inspect %s: %w", heldWorkLeakMarker, store.ErrBackendNotConformant),
	}
	reconcileOnce(t, f.newEngine(t, driver))
	if hold, found := f.runHold(t); !found || hold.Reason != domain.HoldBackendNotConformant {
		t.Fatalf("run hold = (%+v, found %t), want %s", hold, found, domain.HoldBackendNotConformant)
	}
	return heldWorkScenario{
		runID: f.run.ID, reason: domain.HoldBackendNotConformant,
		store:  func() *store.Store { return f.store },
		refuse: func(t *testing.T) { reconcileOnce(t, f.newEngine(t, driver)) },
		recover: func(t *testing.T) {
			driver.err = nil
			reconcileOnce(t, f.newEngine(t, driver))
		},
	}
}

// backdateRunHold ages the run's current hold by age without changing its
// cause, and returns the new start. The store keeps a
// hold's first instant while the cause is unchanged, so the engine's next
// refusing pass extends this span instead of restarting it.
func backdateRunHold(t *testing.T, st *store.Store, runID domain.RunID, age time.Duration) time.Time {
	t.Helper()
	var first time.Time
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		hold, found, err := tx.GetRunHold(t.Context(), runID)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("no hold to backdate")
		}
		first = hold.FirstObservedAt.Add(-age)
		hold.FirstObservedAt, hold.LastObservedAt = first, first
		if err := tx.ClearRunHold(t.Context(), runID); err != nil {
			return err
		}
		return tx.RecordRunHold(t.Context(), hold)
	}); err != nil {
		t.Fatalf("backdate run hold: %v", err)
	}
	return first
}

func openHeldWorkNotices(t *testing.T, st *store.Store) []domain.AttentionItem {
	t.Helper()
	var notices []domain.AttentionItem
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(t.Context(), domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range items {
			if strings.HasPrefix(string(item.ID), heldWorkNoticeItemPrefix) {
				notices = append(notices, item)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("list held-work notices: %v", err)
	}
	return notices
}

func getHeldWorkNotice(t *testing.T, st *store.Store, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(t.Context(), id)
		return err
	}); err != nil {
		t.Fatalf("get held-work notice %s: %v", id, err)
	}
	return item
}

// TestHeldWorkNoticeRaisesOnceAndClears is issue #766's uniform lifecycle: on
// the dispatch path and both collect paths, a refusal hold raises nothing
// before the threshold, raises exactly one notice after it, keeps that one
// notice across further passes on fresh engines, and resolves it on the pass
// after the hold clears.
func TestHeldWorkNoticeRaisesOnceAndClears(t *testing.T) {
	paths := []struct {
		name  string
		setup func(*testing.T) heldWorkScenario
	}{
		{"dispatch", heldWorkDispatchScenario},
		{"production collect", heldWorkProductionCollectScenario},
		{"specification collect", heldWorkSpecificationCollectScenario},
	}
	for _, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			s := path.setup(t)
			if open := openHeldWorkNotices(t, s.store()); len(open) != 0 {
				t.Fatalf("a hold under the threshold raised %d notice(s): %+v", len(open), open)
			}

			holdStart := backdateRunHold(t, s.store(), s.runID, heldWorkNoticeAfter+time.Minute)
			s.refuse(t)
			open := openHeldWorkNotices(t, s.store())
			if len(open) != 1 {
				t.Fatalf("open notices after the threshold = %d, want 1: %+v", len(open), open)
			}
			notice := open[0]
			var run domain.Run
			if err := s.store().Read(t.Context(), func(tx *store.ReadTx) error {
				var err error
				run, err = tx.GetRun(t.Context(), s.runID)
				return err
			}); err != nil {
				t.Fatalf("get run: %v", err)
			}
			if notice.ID != heldWorkNoticeID(s.runID, holdStart) {
				t.Errorf("notice id = %s, want %s", notice.ID, heldWorkNoticeID(s.runID, holdStart))
			}
			if notice.Posture == nil || *notice.Posture != domain.HealthPostureAdvisory {
				t.Errorf("notice posture = %v, want advisory", notice.Posture)
			}
			if notice.Subject.Type != domain.SubjectTask || notice.Subject.ID != domain.SubjectID(run.TaskID) ||
				notice.Subject.RunID != nil || notice.ProjectID != run.ProjectID {
				t.Errorf("notice subject = %+v in project %s, want task %s in project %s",
					notice.Subject, notice.ProjectID, run.TaskID, run.ProjectID)
			}
			if notice.HealthDiagnostic == nil || notice.HealthDiagnostic.Code != heldWorkDiagnosticCode {
				t.Errorf("notice diagnostic = %+v, want code %s", notice.HealthDiagnostic, heldWorkDiagnosticCode)
			}
			if !slices.Equal(notice.RequestedDecision, []domain.Action{domain.ActionAcknowledge}) {
				t.Errorf("notice actions = %v, want acknowledge only", notice.RequestedDecision)
			}
			for _, want := range []string{string(s.runID), string(s.reason), holdStart.Format(time.RFC3339)} {
				if !strings.Contains(notice.Reason, want) {
					t.Errorf("notice reason %q does not name %q", notice.Reason, want)
				}
			}

			// Two more refusing passes, each on a fresh engine as after a
			// daemon restart, leave the one notice untouched.
			s.refuse(t)
			s.refuse(t)
			open = openHeldWorkNotices(t, s.store())
			if len(open) != 1 || open[0].ID != notice.ID || open[0].ItemVersion != 1 {
				t.Fatalf("notices after repeated passes = %+v, want only %s at version 1", open, notice.ID)
			}

			s.recover(t)
			if open := openHeldWorkNotices(t, s.store()); len(open) != 0 {
				t.Fatalf("open notices after recovery = %+v, want none", open)
			}
			resolved := getHeldWorkNotice(t, s.store(), notice.ID)
			if resolved.Status != domain.StatusResolved || resolved.ItemVersion != 2 {
				t.Errorf("notice after recovery = status %s version %d, want resolved at version 2",
					resolved.Status, resolved.ItemVersion)
			}
		})
	}
}

// TestHeldWorkNoticeHasNoSideEffects: the notice neither blocks unattended
// admission nor appears in the run-scoped open-item list run supervision
// reads, and it carries none of the refusal's error text.
func TestHeldWorkNoticeHasNoSideEffects(t *testing.T) {
	s := heldWorkProductionCollectScenario(t)
	backdateRunHold(t, s.store(), s.runID, heldWorkNoticeAfter+time.Minute)
	s.refuse(t)
	open := openHeldWorkNotices(t, s.store())
	if len(open) != 1 {
		t.Fatalf("open notices = %d, want 1", len(open))
	}
	body, err := json.Marshal(open[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), heldWorkLeakMarker) {
		t.Errorf("notice carries refusal error text: %s", body)
	}
	if err := s.store().Read(t.Context(), func(tx *store.ReadTx) error {
		if err := tx.RequireUnattendedOperationOpen(t.Context()); err != nil {
			t.Errorf("unattended operation with the notice open: %v", err)
		}
		forRun, err := tx.ListOpenAttentionItemsForRun(t.Context(), s.runID)
		if err != nil {
			return err
		}
		// Run supervision reads the records variant of the same list.
		records, err := tx.ListOpenAttentionItemRecordsForRun(t.Context(), s.runID)
		if err != nil {
			return err
		}
		for _, item := range append(forRun, records...) {
			if item.ID == open[0].ID {
				t.Errorf("notice %s is in the run-scoped open-item list", item.ID)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read side effects: %v", err)
	}
}

// TestHeldWorkNoticeNewCauseResolvesAndRestarts: a hold that restarts for a
// different refusal reason resolves the notice raised for the old span, and
// raises nothing for the new one until that span passes the threshold itself.
func TestHeldWorkNoticeNewCauseResolvesAndRestarts(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
	driver := &switchableInspectDriver{
		StageDriver: f.driver, err: fmt.Errorf("inspect: %w", store.ErrBackendNotConformant),
	}
	reconcileOnce(t, f.newEngine(t, driver))
	oldStart := backdateRunHold(t, f.store, f.run.ID, heldWorkNoticeAfter+time.Minute)
	reconcileOnce(t, f.newEngine(t, driver))
	oldID := heldWorkNoticeID(f.run.ID, oldStart)
	if open := openHeldWorkNotices(t, f.store); len(open) != 1 || open[0].ID != oldID {
		t.Fatalf("open notices for the first cause = %+v, want %s", open, oldID)
	}

	driver.err = fmt.Errorf("inspect: %w", domain.ErrCapabilityBelowFloor)
	reconcileOnce(t, f.newEngine(t, driver))
	if hold, found := f.runHold(t); !found || hold.Reason != domain.HoldAdmissionPolicyRefused {
		t.Fatalf("hold after the cause changed = (%+v, found %t), want %s",
			hold, found, domain.HoldAdmissionPolicyRefused)
	}
	if open := openHeldWorkNotices(t, f.store); len(open) != 0 {
		t.Fatalf("open notices right after the cause changed = %+v, want none", open)
	}
	if old := getHeldWorkNotice(t, f.store, oldID); old.Status != domain.StatusResolved {
		t.Errorf("first cause's notice status = %s, want resolved", old.Status)
	}

	// A real second span starts after the first one ended; age this one less
	// so the two starts stay distinct.
	newStart := backdateRunHold(t, f.store, f.run.ID, heldWorkNoticeAfter)
	reconcileOnce(t, f.newEngine(t, driver))
	newID := heldWorkNoticeID(f.run.ID, newStart)
	if open := openHeldWorkNotices(t, f.store); len(open) != 1 || open[0].ID != newID {
		t.Fatalf("open notices for the second cause = %+v, want %s", open, newID)
	}
}

// TestHeldWorkNoticeIgnoresHoldsOutsideTheRefusalClass: a long hold whose
// reason is not an admission refusal raises nothing.
func TestHeldWorkNoticeIgnoresHoldsOutsideTheRefusalClass(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
	engine := f.newEngine(t, f.driver)
	if err := engine.observeRunHold(
		t.Context(), f.run.ID, f.attempt.InvocationID, domain.HoldIdentityParallelism,
	); err != nil {
		t.Fatal(err)
	}
	backdateRunHold(t, f.store, f.run.ID, heldWorkNoticeAfter+time.Minute)
	if err := f.newEngine(t, f.driver).observeRunHold(
		t.Context(), f.run.ID, f.attempt.InvocationID, domain.HoldIdentityParallelism,
	); err != nil {
		t.Fatal(err)
	}
	if hold, found := f.runHold(t); !found || time.Since(hold.FirstObservedAt) < heldWorkNoticeAfter {
		t.Fatalf("hold = (%+v, found %t), want a span past the threshold", hold, found)
	}
	if open := openHeldWorkNotices(t, f.store); len(open) != 0 {
		t.Fatalf("a hold outside the refusal class raised %+v", open)
	}
}

func TestHeldWorkNoticeIDRoundTrip(t *testing.T) {
	start := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	runID, holdStart, ok := parseHeldWorkNoticeID(heldWorkNoticeID("run-with-hyphens-7", start))
	if !ok || runID != "run-with-hyphens-7" || holdStart != start.Unix() {
		t.Fatalf("parse = (%s, %d, %t), want (run-with-hyphens-7, %d, true)", runID, holdStart, ok, start.Unix())
	}
	for _, id := range []domain.ItemID{
		"invocation-stalled-inv-1-7", "work-held-", "work-held-run", "work-held-run-", "work-held-run-12x", "work-held--12",
	} {
		if runID, holdStart, ok := parseHeldWorkNoticeID(id); ok {
			t.Errorf("parse(%q) = (%s, %d), want a refusal", id, runID, holdStart)
		}
	}
}

// stopRunTask records an operator stop for the run's task.
func stopRunTask(t *testing.T, st *store.Store, runID domain.RunID) {
	t.Helper()
	state, err := st.ServerState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		run, err := tx.GetRun(t.Context(), runID)
		if err != nil {
			return err
		}
		_, _, err = tx.StopTask(t.Context(), domain.StopTaskRequest{
			CommandID: "stop-held-task", DeviceID: "device-1",
			TaskID: run.TaskID, ProjectID: run.ProjectID,
			ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
		}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatalf("stop task: %v", err)
	}
}

// TestHeldWorkNoticeResolvesWhenTheTaskIsStopped: stopping the held task
// fences its run, so the engine stops retrying it. The notice resolves, and
// further passes over the leftover hold row raise nothing for that span.
func TestHeldWorkNoticeResolvesWhenTheTaskIsStopped(t *testing.T) {
	s := heldWorkDispatchScenario(t)
	backdateRunHold(t, s.store(), s.runID, heldWorkNoticeAfter+time.Minute)
	s.refuse(t)
	open := openHeldWorkNotices(t, s.store())
	if len(open) != 1 {
		t.Fatalf("open notices = %d, want 1", len(open))
	}

	stopRunTask(t, s.store(), s.runID)
	s.refuse(t)
	s.refuse(t)
	if open := openHeldWorkNotices(t, s.store()); len(open) != 0 {
		t.Fatalf("open notices after the task was stopped = %+v, want none", open)
	}
	if resolved := getHeldWorkNotice(t, s.store(), open[0].ID); resolved.Status != domain.StatusResolved ||
		resolved.ItemVersion != 2 {
		t.Errorf("notice after the stop = status %s version %d, want resolved at version 2",
			resolved.Status, resolved.ItemVersion)
	}
}

// TestHeldWorkNoticeSkipsAStoppedTask: a hold that crosses the threshold
// after its task was stopped raises nothing, in the raise path itself.
func TestHeldWorkNoticeSkipsAStoppedTask(t *testing.T) {
	f := newFailedImplementationFixture(t, execfake.OutcomeFail, "unused")
	stopRunTask(t, f.store, f.run.ID)
	now := time.Now().UTC()
	start := now.Add(-heldWorkNoticeAfter - time.Minute)
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		if err := recordRunHold(
			t.Context(), tx, f.run.ID, f.attempt.InvocationID, domain.HoldBackendNotConformant, start,
		); err != nil {
			return err
		}
		return raiseHeldWorkNotice(t.Context(), tx, f.run.ID, domain.HoldBackendNotConformant, now)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.signet.GetAttentionItem(t.Context(), heldWorkNoticeID(f.run.ID, start)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a stopped task's hold raised a notice: %v", err)
	}
}
