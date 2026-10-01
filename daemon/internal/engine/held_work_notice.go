package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// heldWorkNoticeAfter is how long one admission-refusal hold must last before
// the engine raises a notice for it. A transient refusal (a backend recheck in
// progress) clears well inside it and stays silent (issue #766).
const heldWorkNoticeAfter = 15 * time.Minute

const (
	heldWorkNoticeItemPrefix = "work-held-"
	heldWorkDiagnosticCode   = "work_held"
)

// heldWorkNoticeID names the notice for one hold span: the run plus the
// instant its current cause began. The store keeps that instant while the
// cause is unchanged, so one span has one ID across reconcile passes and
// daemon restarts, and a new cause gets a new one.
func heldWorkNoticeID(runID domain.RunID, holdStart time.Time) domain.ItemID {
	return domain.ItemID(fmt.Sprintf("%s%s-%d", heldWorkNoticeItemPrefix, runID, holdStart.Unix()))
}

// parseHeldWorkNoticeID inverts heldWorkNoticeID. The span suffix must be
// digits alone because a run ID can itself contain hyphens.
func parseHeldWorkNoticeID(id domain.ItemID) (domain.RunID, int64, bool) {
	rest, ok := strings.CutPrefix(string(id), heldWorkNoticeItemPrefix)
	if !ok {
		return "", 0, false
	}
	cut := strings.LastIndexByte(rest, '-')
	if cut <= 0 {
		return "", 0, false
	}
	suffix := rest[cut+1:]
	if suffix == "" || strings.Trim(suffix, "0123456789") != "" {
		return "", 0, false
	}
	holdStart, err := strconv.ParseInt(suffix, 10, 64)
	if err != nil {
		return "", 0, false
	}
	return domain.RunID(rest[:cut]), holdStart, true
}

// raiseHeldWorkNotice files the advisory work_held notice once the run's
// refusal hold has lasted heldWorkNoticeAfter. It runs in the transaction
// that just recorded the hold, so every path that holds on a refusal shares
// it. The span is read back from the stored row, not taken from now: only
// the row knows when the current cause began.
//
// The existence check covers every status, so a span whose notice was
// already resolved (its task was stopped) is not raised a second time.
func raiseHeldWorkNotice(
	ctx context.Context, tx *store.WriteTx, runID domain.RunID,
	reason domain.RunHoldReason, now time.Time,
) error {
	if !slices.Contains(refusalHoldReasons, reason) {
		return nil
	}
	hold, found, err := tx.GetRunHold(ctx, runID)
	if err != nil || !found {
		return err
	}
	if now.Sub(hold.FirstObservedAt) < heldWorkNoticeAfter {
		return nil
	}
	_, err = tx.GetAttentionItem(ctx, heldWorkNoticeID(runID, hold.FirstObservedAt))
	if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if stopped, err := heldWorkTaskStopped(ctx, &tx.ReadTx, runID); err != nil || stopped {
		return err
	}
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	item, err := heldWorkNoticeItem(ctx, tx, run, hold, now)
	if err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}

// heldWorkNoticeItem builds the notice. The subject is the run's task, not
// the run: an open run-subject item with a requested decision reads as
// attention_required to run supervision, and a self-resolving advisory must
// not end a supervised run. The reason names the run instead, and is built
// only from the run ID, the hold's reason code, and its start: refusal error
// text never reaches the item.
func heldWorkNoticeItem(
	ctx context.Context, tx *store.WriteTx, run domain.Run,
	hold domain.RunHoldObservation, createdAt time.Time,
) (domain.AttentionItem, error) {
	taskID := run.TaskID
	subject := domain.Subject{Type: domain.SubjectTask, ID: domain.SubjectID(taskID), TaskID: &taskID}
	names, err := tx.DisplayNamesFor(ctx, run.ProjectID, subject)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	posture := domain.HealthPostureAdvisory
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        heldWorkNoticeID(run.ID, hold.FirstObservedAt),
		ProjectID: run.ProjectID, Subject: subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Run %s has been held since %s with hold reason %s. "+
				"Freeside keeps retrying it. "+
				"This notice resolves itself when the hold clears, its cause changes, or the task is stopped.",
			run.ID, hold.FirstObservedAt.Format(time.RFC3339), hold.Reason),
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: heldWorkDiagnosticCode, Impairs: domain.ImpairedCapabilityUnattendedAdmission,
		},
		DisplayNames: names, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional,
		CreatedAt:         &createdAt, Posture: &posture, Status: domain.StatusOpen,
	}, nil)
}

// heldWorkTaskStopped reports whether the operator stopped the run's task.
// The cancellation fence ends the engine's retries without touching the hold
// row, so a stopped task's row can stand for good; the notice must not.
func heldWorkTaskStopped(ctx context.Context, tx *store.ReadTx, runID domain.RunID) (bool, error) {
	err := requireTaskExecutionOpen(ctx, tx, runID)
	if errors.Is(err, store.ErrTaskCancellationFenced) {
		return true, nil
	}
	return false, err
}

// reconcileHeldWorkNotices resolves every open held-work notice whose hold
// span has ended: the row is gone, names a reason outside the refusal class,
// or restarted for a new cause, or the run's task was stopped. It is the
// only resolver because a hold ends in several places (the collectors'
// clearRefusalHold, any milestone, a different cause replacing the row). The
// read comes first so the ordinary pass, with no notice open, opens no write
// transaction.
func (e *Engine) reconcileHeldWorkNotices(ctx context.Context) error {
	var stale []domain.AttentionItem
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stale, err = e.staleHeldWorkNotices(ctx, tx)
		return err
	}); err != nil || len(stale) == 0 {
		return err
	}
	return e.store.Write(ctx, func(tx *store.WriteTx) error {
		// Recomputed inside the write, so each resolve advances the item's
		// current version, not the one the read saw.
		stale, err := e.staleHeldWorkNotices(ctx, &tx.ReadTx)
		if err != nil {
			return err
		}
		for _, item := range stale {
			item.ItemVersion++
			item.Status = domain.StatusResolved
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	})
}

func (e *Engine) staleHeldWorkNotices(ctx context.Context, tx *store.ReadTx) ([]domain.AttentionItem, error) {
	items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
	if err != nil {
		return nil, err
	}
	var stale []domain.AttentionItem
	for _, item := range items {
		runID, holdStart, ok := parseHeldWorkNoticeID(item.ID)
		if !ok {
			continue
		}
		hold, found, err := tx.GetRunHold(ctx, runID)
		if err != nil {
			// The hold row is projection the store never trusts; the next
			// hold write overwrites an unreadable one. Leave the notice open
			// instead of failing the pass on it.
			e.logger.Warn("held-work notice: run hold unreadable",
				"item", string(item.ID), "run", string(runID), "error", err)
			continue
		}
		if found && slices.Contains(refusalHoldReasons, hold.Reason) &&
			hold.FirstObservedAt.Unix() == holdStart {
			stopped, err := heldWorkTaskStopped(ctx, tx, runID)
			if err != nil {
				return nil, err
			}
			if !stopped {
				continue
			}
		}
		stale = append(stale, item)
	}
	return stale, nil
}
