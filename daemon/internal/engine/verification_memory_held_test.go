package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// heldFixture is a releaseFixture whose task has ended with its verification
// reservation held: no pending task names its run. containersProved is what
// the fake container-only proof reports.
type heldFixture struct {
	*releaseFixture
	containersProved error
	containersAsked  int
}

func newHeldFixture(t *testing.T) *heldFixture {
	t.Helper()
	hf := &heldFixture{releaseFixture: newReleaseFixture(t, true)}
	// A pass that reads a pending task takes its lock under workDir.
	hf.w.workDir = t.TempDir()
	hf.w.proveVerificationContainersAbsent = func(ctx context.Context, got domain.Run, invocation domain.InvocationID) error {
		if got.ID != hf.run.ID || invocation != hf.task.verificationInvocationID() || ctx.Err() != nil {
			t.Errorf("container proof asked for run %q invocation %q under a done context %v", got.ID, invocation, ctx.Err())
		}
		hf.containersAsked++
		return hf.containersProved
	}
	return hf
}

// pass runs one lane pass; the task has ended, so the pass reads no pending
// task and the end-of-task proof runs after its loop.
func (hf *heldFixture) pass(t *testing.T) {
	t.Helper()
	if _, err := hf.w.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func (hf *heldFixture) release(t *testing.T, commandGone bool) VerificationMemoryRelease {
	t.Helper()
	report, err := (&Engine{productionPublication: hf.w}).ReleaseVerificationMemory(t.Context(), hf.run.TaskID, commandGone)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Invocations) != 1 || report.Invocations[0].InvocationID != hf.task.verificationInvocationID() ||
		report.CommandGone != commandGone {
		t.Fatalf("report = %+v, want the task's one invocation", report)
	}
	return report
}

// alerts returns every verification_memory_held item, in any status.
func (hf *heldFixture) alerts(t *testing.T) []domain.AttentionItem {
	t.Helper()
	var found []domain.AttentionItem
	if err := hf.w.store.Read(t.Context(), func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(t.Context())
		for _, item := range items {
			if strings.HasPrefix(string(item.Value.ID), verificationMemoryHeldItemPrefix) {
				found = append(found, item.Value)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return found
}

// wantOneAlert wants exactly one alert, in status, whose reason names every
// part of want.
func (hf *heldFixture) wantOneAlert(t *testing.T, status domain.ItemStatus, want ...string) {
	t.Helper()
	alerts := hf.alerts(t)
	if len(alerts) != 1 || alerts[0].Status != status {
		t.Fatalf("alerts = %+v, want one %s", alerts, status)
	}
	alert := alerts[0]
	if alert.HealthDiagnostic == nil || alert.HealthDiagnostic.Code != verificationMemoryHeldCode ||
		alert.HealthDiagnostic.Impairs != domain.ImpairedCapabilityUnattendedAdmission ||
		alert.Type != domain.AttentionSystemHealth || alert.Subject.Type != domain.SubjectSystem {
		t.Fatalf("alert = %+v, want a system verification_memory_held health item", alert)
	}
	for _, part := range append(want, string(hf.run.TaskID), string(hf.run.ID),
		string(hf.task.verificationInvocationID()), fmt.Sprintf("%d MiB", hf.size)) {
		if !strings.Contains(alert.Reason, part) {
			t.Fatalf("alert reason %q does not name %q", alert.Reason, part)
		}
	}
}

var unjoinedProof = errors.Join(ward.ErrVerificationAbsenceUnproven,
	fmt.Errorf("verification inv-verification %w", ward.ErrVerificationCommandUnjoined))

// TestHeldVerificationAfterACrashInterruptedCall: an unjoined command keeps
// the reservation through the end-of-task proof, which files one alert naming
// the unjoined command. The release without -command-gone keeps it held; with
// it, and no owned container left, the release frees it and concludes the
// alert.
func TestHeldVerificationAfterACrashInterruptedCall(t *testing.T) {
	t.Parallel()
	hf := newHeldFixture(t)
	hf.proved = unjoinedProof
	hf.pass(t)
	if len(hf.asked) != 1 || !hf.held(t) {
		t.Fatalf("end-of-task proof ran %d times, held = %v; want once and held", len(hf.asked), hf.held(t))
	}
	hf.wantOneAlert(t, domain.StatusOpen, "no quiescence proof", "-command-gone")

	if report := hf.release(t, false); report.Invocations[0].Released || !hf.held(t) ||
		!strings.Contains(report.Invocations[0].Reason, "quiescence") {
		t.Fatalf("release without -command-gone = %+v, held = %v", report, hf.held(t))
	}
	if hf.containersAsked != 0 {
		t.Fatal("the release without -command-gone ran the container-only proof")
	}
	if report := hf.release(t, true); !report.Invocations[0].Released || hf.held(t) {
		t.Fatalf("release with -command-gone = %+v, held = %v", report, hf.held(t))
	}
	hf.wantOneAlert(t, domain.StatusResolved)
}

// TestHeldVerificationAfterAPersistentCleanupFailure: a container the runtime
// will not delete keeps the reservation, and the alert names the container.
// Once the runtime allows the delete, the release without -command-gone frees
// it and concludes the alert.
func TestHeldVerificationAfterAPersistentCleanupFailure(t *testing.T) {
	t.Parallel()
	hf := newHeldFixture(t)
	hf.proved = errors.Join(ward.ErrVerificationAbsenceUnproven,
		errors.New("verification inv-verification container verification-1: runtime refused the delete"))
	hf.pass(t)
	if !hf.held(t) {
		t.Fatal("an unproven container's reservation was released")
	}
	hf.wantOneAlert(t, domain.StatusOpen, "verification-1", "could not be removed")

	hf.proved = nil
	if report := hf.release(t, false); !report.Invocations[0].Released || hf.held(t) {
		t.Fatalf("release once the delete succeeds = %+v, held = %v", report, hf.held(t))
	}
	hf.wantOneAlert(t, domain.StatusResolved)
}

// TestEndOfTaskProofReleasesAProvedReservation: a reservation held at task end
// whose proof now passes is released by the next pass, with no alert.
func TestEndOfTaskProofReleasesAProvedReservation(t *testing.T) {
	t.Parallel()
	hf := newHeldFixture(t)
	hf.pass(t)
	if len(hf.asked) != 1 || hf.held(t) {
		t.Fatalf("proof ran %d times, held = %v; want once and released", len(hf.asked), hf.held(t))
	}
	if alerts := hf.alerts(t); len(alerts) != 0 {
		t.Fatalf("alerts = %+v, want none", alerts)
	}
}

// TestEndOfTaskProofRunsOncePerProcess: later passes do not re-prove an
// invocation already re-proved. A new workflow, as after a restart, re-proves
// once and files no second alert.
func TestEndOfTaskProofRunsOncePerProcess(t *testing.T) {
	t.Parallel()
	hf := newHeldFixture(t)
	hf.proved = unjoinedProof
	hf.pass(t)
	hf.pass(t)
	if len(hf.asked) != 1 {
		t.Fatalf("proof ran %d times over two passes, want once", len(hf.asked))
	}
	restarted := *hf.releaseFixture
	restarted.w = &productionPublicationWorkflow{
		store: hf.w.store, attention: hf.w.attention, now: hf.w.now, launchBound: hf.w.launchBound,
		proveVerificationAbsent: hf.w.proveVerificationAbsent,
	}
	if _, err := restarted.w.reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(hf.asked) != 2 || !hf.held(t) {
		t.Fatalf("restarted proof count = %d, held = %v; want one more and held", len(hf.asked), hf.held(t))
	}
	hf.wantOneAlert(t, domain.StatusOpen)
}

// TestEndOfTaskProofConcludesAnAlertWhateverReleased: an alert whose
// reservation something else released, here a confirmed task stop, is
// concluded by the next pass, including one an earlier process filed.
func TestEndOfTaskProofConcludesAnAlertWhateverReleased(t *testing.T) {
	t.Parallel()
	hf := newHeldFixture(t)
	hf.proved = unjoinedProof
	hf.pass(t)
	if err := hf.w.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.ReleaseVerificationReservation(t.Context(), hf.task.verificationInvocationID(), hf.at)
	}); err != nil {
		t.Fatal(err)
	}
	hf.w.verificationHeldAlerts = nil // a restart: the alert was filed by an earlier process
	hf.pass(t)
	hf.wantOneAlert(t, domain.StatusResolved)
}

// TestEndOfTaskProofLeavesPendingTasks: the lane does not re-prove a
// reservation whose run still has a pending task, nor any when a pending task
// could not be read, though it still concludes an alert whose reservation was
// released; and the release refuses a task with a pending task.
func TestEndOfTaskProofLeavesPendingTasks(t *testing.T) {
	t.Parallel()
	t.Run("pending-run", func(t *testing.T) {
		t.Parallel()
		hf := newHeldFixture(t)
		task := validPublicationTask(t, hf.run.ID, hf.run.ProjectID)
		payload, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		if err := hf.w.store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(t.Context(), task.intentKey(), KindProductionPublicationRequested, payload)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		// The pass's own handling of the task may fail in this fixture; only
		// the end-of-task proof's choice is under test.
		_, _ = hf.w.reconcile(t.Context())
		if len(hf.asked) != 0 || !hf.held(t) || len(hf.alerts(t)) != 0 {
			t.Fatalf("a pending run's reservation was re-proved %d times, held = %v, alerts %v",
				len(hf.asked), hf.held(t), hf.alerts(t))
		}
		_, err = (&Engine{productionPublication: hf.w}).ReleaseVerificationMemory(t.Context(), hf.run.TaskID, true)
		if err == nil || !strings.Contains(err.Error(), "pending publication task") || len(hf.asked) != 0 || !hf.held(t) {
			t.Fatalf("release of a pending task = %v, proofs %d, held = %v", err, len(hf.asked), hf.held(t))
		}
	})
	t.Run("unreadable-pending-task", func(t *testing.T) {
		t.Parallel()
		hf := newHeldFixture(t)
		if err := hf.w.store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(t.Context(), productionPublicationTaskKey("another-run"),
				KindProductionPublicationRequested, []byte(`{}`))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		_, _ = hf.w.reconcile(t.Context())
		if len(hf.asked) != 0 || !hf.held(t) {
			t.Fatalf("a pass that could not read a pending task re-proved %d times, held = %v", len(hf.asked), hf.held(t))
		}
	})
	t.Run("unreadable-pending-task-still-concludes", func(t *testing.T) {
		t.Parallel()
		hf := newHeldFixture(t)
		hf.proved = unjoinedProof
		hf.pass(t)
		if err := hf.w.store.Write(t.Context(), func(tx *store.WriteTx) error {
			return tx.ReleaseVerificationReservation(t.Context(), hf.task.verificationInvocationID(), hf.at)
		}); err != nil {
			t.Fatal(err)
		}
		if err := hf.w.store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
			_, _, err := tx.EnqueueOutbox(t.Context(), productionPublicationTaskKey("another-run"),
				KindProductionPublicationRequested, []byte(`{}`))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		_, _ = hf.w.reconcile(t.Context())
		hf.wantOneAlert(t, domain.StatusResolved)
	})
}

// TestReleaseWithCommandGoneStillNeedsContainerAbsence: the operator's word
// that the command is gone never stands in for container absence.
func TestReleaseWithCommandGoneStillNeedsContainerAbsence(t *testing.T) {
	t.Parallel()
	hf := newHeldFixture(t)
	hf.containersProved = errors.Join(ward.ErrVerificationAbsenceUnproven,
		errors.New("verification inv-verification container verification-1: runtime refused the delete"))
	if report := hf.release(t, true); report.Invocations[0].Released || !hf.held(t) ||
		!strings.Contains(report.Invocations[0].Reason, "verification-1") {
		t.Fatalf("release = %+v, held = %v; want held naming the container", report, hf.held(t))
	}
	if len(hf.asked) != 0 || hf.containersAsked != 1 {
		t.Fatalf("proofs: strict %d, container-only %d; want the container-only proof alone", len(hf.asked), hf.containersAsked)
	}
}

// TestReleaseReportsABusyLane: a pass holding the lane refuses the release
// at once instead of waiting.
func TestReleaseReportsABusyLane(t *testing.T) {
	t.Parallel()
	hf := newHeldFixture(t)
	hf.w.reconcileMu.Lock()
	defer hf.w.reconcileMu.Unlock()
	_, err := (&Engine{productionPublication: hf.w}).ReleaseVerificationMemory(t.Context(), hf.run.TaskID, false)
	if !errors.Is(err, ErrPublicationLaneBusy) || len(hf.asked) != 0 {
		t.Fatalf("release under a busy lane = %v, proofs %d", err, len(hf.asked))
	}
}

func TestVerificationMemoryHeldInvocation(t *testing.T) {
	t.Parallel()
	for id, want := range map[domain.ItemID]domain.InvocationID{
		"verification-memory-held-inv-a-12":                "inv-a",
		"verification-memory-held-inv-a-b-3":               "inv-a-b",
		"verification-memory-held-v/reevaluation/cmd-1-41": "v/reevaluation/cmd-1",
		"verification-memory-held-inv-a-":                  "",
		"verification-memory-held-inv-a-x1":                "",
		"verification-memory-held--4":                      "",
		"invocation-stalled-inv-a-4":                       "",
	} {
		got, ok := verificationMemoryHeldInvocation(id)
		if got != want || ok != (want != "") {
			t.Errorf("%s: got %q, %v; want %q", id, got, ok, want)
		}
	}
}
