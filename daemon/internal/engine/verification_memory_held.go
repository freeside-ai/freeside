package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// A verification reservation is released only on ward's strict proof that
// every container the job started is absent and every command joined
// (releaseVerification). Two reachable paths end a task with that proof still
// failing: a command a daemon crash interrupted, which can never prove it
// joined, and an owned container the runtime refuses to delete. Nothing then
// runs the proof again, so the budget would lose the job's share for good
// with nothing telling the operator (#2977). This file closes that gap:
//
//   - The lane re-proves, once per daemon process, each reservation whose task
//     has ended, and releases it on the strict proof.
//   - When that proof fails, it files one verification_memory_held alert per
//     invocation, and concludes it once the reservation is released.
//   - Engine.ReleaseVerificationMemory lets the operator re-run the proof,
//     and, having confirmed the old CLI is gone, run it without the joined
//     condition.
//
// Every proof runs under the lane's reconcileMu, so no verification pass of
// the lane runs beside it: a proof never races a job about to launch under
// the same invocation, and releasing memory a pass is about to use cannot
// happen.

const (
	verificationMemoryHeldItemPrefix = "verification-memory-held-"
	verificationMemoryHeldCode       = "verification_memory_held"
)

// ErrPublicationLaneBusy refuses an operator release while a lane pass holds
// the lane; the operator retries once the pass returns.
var ErrPublicationLaneBusy = errors.New("the publication lane is running a pass; retry when it finishes")

func verificationMemoryHeldPrefix(id domain.InvocationID) string {
	return verificationMemoryHeldItemPrefix + string(id) + "-"
}

// verificationMemoryHeldInvocation returns the invocation an alert's item id
// names. The remainder after the invocation is the filing revision alone, so
// the alert of "inv-a" never reads as one for "inv-a-b".
func verificationMemoryHeldInvocation(itemID domain.ItemID) (domain.InvocationID, bool) {
	rest, ok := strings.CutPrefix(string(itemID), verificationMemoryHeldItemPrefix)
	if !ok {
		return "", false
	}
	cut := strings.LastIndexByte(rest, '-')
	if cut <= 0 {
		return "", false
	}
	revision := rest[cut+1:]
	if revision == "" || strings.Trim(revision, "0123456789") != "" {
		return "", false
	}
	return domain.InvocationID(rest[:cut]), true
}

// reproveEndedVerificationReservations is the end-of-task proof. reconcile
// calls it after its pending loop, still under reconcileMu, with the run of
// every pending task the pass read. pendingKnown is false when a pending task
// could not be read: that task's run is then unknown, so nothing counts as
// ended and no proof runs, though alerts whose reservation something else
// released still conclude. A reservation whose run has no pending task
// belongs to a task that has ended: a pending one's next pass runs its own
// proof. Each such reservation is re-proved once per process, so a held one
// costs at most one proof timeout per start; a restart or the operator
// command retries.
func (w *productionPublicationWorkflow) reproveEndedVerificationReservations(
	ctx context.Context, pendingRuns map[domain.RunID]bool, pendingKnown bool,
) error {
	if w.proveVerificationAbsent == nil {
		// Unbound rooms write no ownership records, so no later proof exists.
		return nil
	}
	bound, err := currentLaunchBound(ctx, w.store, w.launchBound)
	if err != nil || !bound.Budgeted() {
		return err
	}
	var open []domain.LaunchReservation
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		open, err = tx.ListOpenVerificationReservations(ctx)
		return err
	}); err != nil {
		return err
	}
	if err := w.concludeReleasedVerificationAlerts(ctx, open); err != nil {
		return err
	}
	if !pendingKnown {
		return nil
	}
	if w.verificationReproved == nil {
		w.verificationReproved = make(map[domain.InvocationID]bool)
	}
	for _, reservation := range open {
		if pendingRuns[reservation.RunID] || w.verificationReproved[reservation.InvocationID] {
			continue
		}
		if ctx.Err() != nil {
			// Shutting down: a proof cut short would read as held.
			return nil
		}
		w.verificationReproved[reservation.InvocationID] = true
		run, err := w.verificationRun(ctx, reservation.RunID)
		if err != nil {
			return err
		}
		proof := w.proveHeldVerification(ctx, run, reservation.InvocationID, false)
		switch {
		case proof == nil:
			if err := w.releaseHeldVerification(ctx, reservation.InvocationID,
				"Resolved when the end-of-task proof showed every container gone and every command joined."); err != nil {
				return err
			}
		case errors.Is(proof, ward.ErrVerificationAbsenceUnproven):
			if err := w.fileVerificationMemoryHeld(ctx, run, reservation, proof); err != nil {
				return err
			}
		default:
			return fmt.Errorf("prove verification %s absent: %w", reservation.InvocationID, proof)
		}
	}
	return nil
}

// concludeReleasedVerificationAlerts concludes the open alert of every
// invocation whose reservation is no longer open, whatever released it: this
// lane's proof, a later pass of a task that re-entered, the operator command,
// or a confirmed task stop. The first pass of a process loads the alerts an
// earlier process left open; later passes read attention items only when a
// tracked invocation's reservation has gone.
func (w *productionPublicationWorkflow) concludeReleasedVerificationAlerts(
	ctx context.Context, open []domain.LaunchReservation,
) error {
	if w.verificationHeldAlerts == nil {
		tracked := make(map[domain.InvocationID]bool)
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
			for _, item := range items {
				if id, ok := verificationMemoryHeldInvocation(item.ID); ok {
					tracked[id] = true
				}
			}
			return err
		}); err != nil {
			return err
		}
		w.verificationHeldAlerts = tracked
	}
	held := make(map[domain.InvocationID]bool, len(open))
	for _, reservation := range open {
		held[reservation.InvocationID] = true
	}
	var released []domain.InvocationID
	for id := range w.verificationHeldAlerts {
		if !held[id] {
			released = append(released, id)
		}
	}
	if len(released) == 0 {
		return nil
	}
	now := w.now().UTC()
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		for _, id := range released {
			if err := concludeVerificationMemoryHeld(ctx, tx, id, now,
				"Resolved when the reservation was released."); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	for _, id := range released {
		delete(w.verificationHeldAlerts, id)
	}
	return nil
}

func (w *productionPublicationWorkflow) verificationRun(ctx context.Context, id domain.RunID) (domain.Run, error) {
	var run domain.Run
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, id)
		return err
	})
	return run, err
}

// proveHeldVerification runs ward's proof for one invocation, the strict one
// unless the operator confirmed the command gone. Like releaseVerification it
// runs detached from its caller and bounded like a room's own cleanup, so a
// pass canceled at shutdown does not turn a provable release into a held one.
func (w *productionPublicationWorkflow) proveHeldVerification(
	ctx context.Context, run domain.Run, invocation domain.InvocationID, commandGone bool,
) error {
	prove := w.proveVerificationAbsent
	if commandGone {
		prove = w.proveVerificationContainersAbsent
	}
	if prove == nil {
		return errors.New("this daemon has no verification ownership records to prove from")
	}
	proofCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ward.VerificationCleanupTimeout)
	defer cancel()
	return prove(proofCtx, run, invocation)
}

// releaseHeldVerification releases a proved reservation and concludes its
// alert in one write.
func (w *productionPublicationWorkflow) releaseHeldVerification(
	ctx context.Context, invocation domain.InvocationID, reason string,
) error {
	now := w.now().UTC()
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.ReleaseVerificationReservation(ctx, invocation, now); err != nil {
			return err
		}
		return concludeVerificationMemoryHeld(ctx, tx, invocation, now, reason)
	}); err != nil {
		return err
	}
	delete(w.verificationHeldAlerts, invocation)
	return nil
}

func concludeVerificationMemoryHeld(
	ctx context.Context, tx *store.WriteTx, invocation domain.InvocationID, now time.Time, reason string,
) error {
	items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return err
	}
	for _, item := range items {
		if id, ok := verificationMemoryHeldInvocation(item.ID); !ok || id != invocation {
			continue
		}
		concluded, err := item.ConcludeByDaemon(domain.ConcludedByDaemon, now, reason, nil)
		if err != nil {
			return err
		}
		concluded.ItemVersion++
		if err := tx.PutAttentionItem(ctx, concluded); err != nil {
			return err
		}
	}
	return nil
}

// fileVerificationMemoryHeld files the alert for a held invocation unless one
// exists in any status: an operator who acknowledged it, or a restart, never
// gets a second.
func (w *productionPublicationWorkflow) fileVerificationMemoryHeld(
	ctx context.Context, run domain.Run, reservation domain.LaunchReservation, proof error,
) error {
	createdAt := w.attentionCreatedAt()
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		rows, err := tx.ListAttentionItemRows(ctx)
		if err != nil {
			return err
		}
		for _, row := range rows {
			// A refused row's id is its own column, enough to say an alert
			// was filed.
			if id, ok := verificationMemoryHeldInvocation(row.ID); ok && id == reservation.InvocationID {
				return nil
			}
		}
		item, err := verificationMemoryHeldItem(ctx, tx, run, reservation, proof, createdAt)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		return err
	}
	if w.verificationHeldAlerts != nil {
		w.verificationHeldAlerts[reservation.InvocationID] = true
	}
	return nil
}

// verificationMemoryHeldItem is the alert. Its subject is the system, as an
// invocation stall notice's is: an open run-subject item with a requested
// decision reads as attention_required to run supervision, and the task has
// ended. The reason names the task, run, and invocation instead.
func verificationMemoryHeldItem(
	ctx context.Context, tx *store.WriteTx, run domain.Run, reservation domain.LaunchReservation,
	proof error, createdAt time.Time,
) (domain.AttentionItem, error) {
	state, err := tx.ServerState(ctx)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
	names, err := tx.DisplayNamesFor(ctx, "project-system", subject)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	posture := domain.HealthPostureAdvisory
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        domain.ItemID(fmt.Sprintf("%s%d", verificationMemoryHeldPrefix(reservation.InvocationID), state.Revision+1)),
		ProjectID: "project-system", Subject: subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason:            verificationMemoryHeldReason(run, reservation, proof),
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: verificationMemoryHeldCode, Impairs: domain.ImpairedCapabilityUnattendedAdmission,
		},
		DisplayNames: names, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional,
		CreatedAt:         &createdAt, Posture: &posture, Status: domain.StatusOpen,
	}, nil)
}

func verificationMemoryHeldReason(run domain.Run, reservation domain.LaunchReservation, proof error) string {
	command := fmt.Sprintf("freesided release-verification-memory -task %s", run.TaskID)
	var cause string
	if errors.Is(proof, ward.ErrVerificationCommandUnjoined) {
		cause = fmt.Sprintf(
			"A verification command has no quiescence proof, as after a daemon crash mid-command, so its "+
				"container CLI may still be running. Confirm no container run process from an earlier daemon is "+
				"still running, then run `%s -command-gone`.", command)
	} else {
		cause = fmt.Sprintf(
			"An owned verification container could not be removed or proved absent. Remove it, then run `%s`.",
			command)
	}
	return fmt.Sprintf(
		"Verification %s of task %s (run %s) still holds %d MiB of the memory budget after its task ended. %s "+
			"Until then the budget keeps that memory reserved. Proof: %s",
		reservation.InvocationID, run.TaskID, run.ID, reservation.TotalMiB(), cause,
		strings.ReplaceAll(proof.Error(), "\n", "; "))
}

// VerificationMemoryRelease is the operator release's report for one task.
type VerificationMemoryRelease struct {
	TaskID      domain.TaskID                  `json:"task_id"`
	CommandGone bool                           `json:"command_gone"`
	Invocations []VerificationMemoryInvocation `json:"invocations"`
}

// VerificationMemoryInvocation is one open verification reservation of the
// task: released, or held with the proof's reason.
type VerificationMemoryInvocation struct {
	InvocationID domain.InvocationID `json:"invocation_id"`
	RunID        domain.RunID        `json:"run_id"`
	MemoryMiB    int                 `json:"memory_mib"`
	Released     bool                `json:"released"`
	Reason       string              `json:"reason,omitempty"`
}

// ReleaseVerificationMemory re-runs the absence proof for every open
// verification reservation of a task whose publication work has ended, and
// releases each one the proof passes. With commandGone the operator confirms
// that no CLI of a crash-interrupted command still runs, and the proof skips
// only the joined-command condition: it still refuses a live call, retries
// owned cleanup, and proves every owned container absent.
//
// It refuses while a lane pass runs instead of waiting for it, and refuses a
// task with a pending publication task, or when a pending task cannot be
// read: that task's next pass runs its own proof, and its run is unknown.
func (e *Engine) ReleaseVerificationMemory(
	ctx context.Context, taskID domain.TaskID, commandGone bool,
) (VerificationMemoryRelease, error) {
	report := VerificationMemoryRelease{TaskID: taskID, CommandGone: commandGone, Invocations: []VerificationMemoryInvocation{}}
	if taskID == "" {
		return report, domain.ErrEmptyID
	}
	w := e.productionPublication
	if w == nil {
		return report, errors.New("this daemon runs no publication lane")
	}
	if !w.reconcileMu.TryLock() {
		return report, ErrPublicationLaneBusy
	}
	defer w.reconcileMu.Unlock()
	pending, _, _, err := w.readPendingTasks(ctx)
	if err != nil {
		return report, err
	}
	pendingRuns := make(map[domain.RunID]bool, len(pending))
	for _, entry := range pending {
		task, err := w.decodePendingTask(ctx, entry)
		if err != nil {
			return report, fmt.Errorf("pending publication task %q cannot be read, so whether this task has ended is unknown: %w",
				entry.IdempotencyKey, err)
		}
		pendingRuns[task.RunID] = true
	}
	type held struct {
		run         domain.Run
		reservation domain.LaunchReservation
	}
	var owned []held
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		open, err := tx.ListOpenVerificationReservations(ctx)
		if err != nil {
			return err
		}
		for _, reservation := range open {
			run, err := tx.GetRun(ctx, reservation.RunID)
			if err != nil {
				return err
			}
			if run.TaskID == taskID {
				owned = append(owned, held{run, reservation})
			}
		}
		return nil
	}); err != nil {
		return report, err
	}
	for _, h := range owned {
		if pendingRuns[h.run.ID] {
			return report, fmt.Errorf("task %s run %s still has a pending publication task; its next pass proves its own containers",
				taskID, h.run.ID)
		}
	}
	for _, h := range owned {
		row := VerificationMemoryInvocation{
			InvocationID: h.reservation.InvocationID, RunID: h.run.ID, MemoryMiB: h.reservation.TotalMiB(),
		}
		proof := w.proveHeldVerification(ctx, h.run, h.reservation.InvocationID, commandGone)
		if proof != nil {
			row.Reason = strings.ReplaceAll(proof.Error(), "\n", "; ")
			report.Invocations = append(report.Invocations, row)
			continue
		}
		reason := "Resolved when the operator's release proved every container gone and every command joined."
		if commandGone {
			reason = "Resolved when the operator confirmed the command gone and the release proved every container gone."
		}
		if err := w.releaseHeldVerification(ctx, h.reservation.InvocationID, reason); err != nil {
			return report, err
		}
		if commandGone && e.logger != nil {
			e.logger.Warn("released verification memory on the operator's word that its command is gone",
				"task", taskID, "run", h.run.ID, "invocation", h.reservation.InvocationID)
		}
		row.Released = true
		report.Invocations = append(report.Invocations, row)
	}
	return report, nil
}
