package daemonshell

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/atomicfile"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const ReadinessFileName = "readiness.json"

func publishReadiness(stateDir string, ready Readiness) error {
	if stateDir == "" {
		return nil
	}
	body, err := json.Marshal(ready)
	if err != nil {
		return fmt.Errorf("encode readiness file: %w", err)
	}
	body = append(body, '\n')
	path := filepath.Join(stateDir, ReadinessFileName)
	if err := atomicfile.WriteFile(path, body, 0o600); err != nil {
		return fmt.Errorf("publish readiness file %q: %w", path, err)
	}
	return nil
}

type ComponentKind string

const (
	ComponentHTTP                  ComponentKind = "http"
	ComponentPairingControl        ComponentKind = "pairing_control"
	ComponentWorkflow              ComponentKind = "workflow"
	ComponentLocalBackups          ComponentKind = "local_backups"
	ComponentScheduler             ComponentKind = "scheduler"
	ComponentProductionPublication ComponentKind = "production_publication"
	ComponentActiveResource        ComponentKind = "active_resource"
	ComponentLabelIntake           ComponentKind = "label_intake"
	ComponentFollowUpFiling        ComponentKind = "follow_up_filing"
	ComponentReviewReplies         ComponentKind = "review_replies"
	ComponentHandBackLabels        ComponentKind = "hand_back_labels"
	ComponentAttentionDeliveries   ComponentKind = "attention_deliveries"
	ComponentPanic                 ComponentKind = "panic"
)

// AllComponentKinds is the complete exit-classification registration set.
var AllComponentKinds = []ComponentKind{
	ComponentHTTP,
	ComponentPairingControl,
	ComponentWorkflow,
	ComponentLocalBackups,
	ComponentScheduler,
	ComponentProductionPublication,
	ComponentActiveResource,
	ComponentLabelIntake,
	ComponentFollowUpFiling,
	ComponentReviewReplies,
	ComponentHandBackLabels,
	ComponentAttentionDeliveries,
	ComponentPanic,
}

func (k ComponentKind) valid() bool {
	switch k {
	case ComponentHTTP,
		ComponentPairingControl,
		ComponentWorkflow,
		ComponentLocalBackups,
		ComponentScheduler,
		ComponentProductionPublication,
		ComponentActiveResource,
		ComponentLabelIntake,
		ComponentFollowUpFiling,
		ComponentReviewReplies,
		ComponentHandBackLabels,
		ComponentAttentionDeliveries,
		ComponentPanic:
		return true
	default:
		return false
	}
}

type exitDisposition string

const (
	exitDurableStop exitDisposition = "durable_stop"
	exitRestartSafe exitDisposition = "restart_safe"
	exitInvoluntary exitDisposition = "involuntary"
)

// AllExitDispositions is the complete process-exit classification set.
var AllExitDispositions = []exitDisposition{
	exitDurableStop,
	exitRestartSafe,
	exitInvoluntary,
}

func (d exitDisposition) valid() bool {
	switch d {
	case exitDurableStop, exitRestartSafe, exitInvoluntary:
		return true
	default:
		return false
	}
}

func classifyComponentExit(kind ComponentKind) exitDisposition {
	switch kind {
	case ComponentWorkflow,
		ComponentLocalBackups,
		ComponentScheduler,
		ComponentProductionPublication,
		ComponentActiveResource,
		ComponentLabelIntake,
		ComponentFollowUpFiling,
		ComponentReviewReplies,
		ComponentHandBackLabels,
		ComponentAttentionDeliveries:
		return exitDurableStop
	case ComponentHTTP, ComponentPairingControl:
		return exitRestartSafe
	case ComponentPanic:
		return exitInvoluntary
	}
	return exitInvoluntary
}

func (s *Shell) ComponentExited(
	lifetimeCtx, workerCtx context.Context,
	kind ComponentKind,
	err error,
) {
	if lifetimeCtx.Err() != nil || workerCtx.Err() != nil {
		return
	}
	if err == nil {
		err = errors.New("component stopped unexpectedly")
	}
	cause := fmt.Errorf("%s: %w", kind, err)
	switch classifyComponentExit(kind) {
	case exitDurableStop:
		s.durableStop(lifetimeCtx, cause)
	case exitRestartSafe:
		s.errs <- cause
	case exitInvoluntary:
		panic(cause)
	}
}

const DurableStopItemPrefix = "system-health-daemon-stop-"

func (s *Shell) durableStop(ctx context.Context, cause error) {
	s.logger.Error("daemon entered a durable stop", "error", cause)
	for {
		if err := s.fileDurableStop(ctx, cause); err == nil {
			s.logger.Error("daemon durable stop recorded", "error", cause)
			return
		} else {
			s.logger.Error("record daemon durable stop", "error", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *Shell) fileDurableStop(ctx context.Context, cause error) error {
	return s.store.Write(ctx, func(tx *store.WriteTx) error {
		open, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		reason := durableStopReason(cause)
		alreadyRecorded := false
		for _, item := range open {
			if strings.HasPrefix(string(item.ID), DurableStopItemPrefix) {
				alreadyRecorded = alreadyRecorded || item.Reason == reason
				next := item
				if item.Offers(domain.ActionResumeUnattended) {
					next.ItemVersion++
					next.RequestedDecision = withoutAction(next.RequestedDecision, domain.ActionResumeUnattended)
					if err := tx.PutAttentionItem(ctx, next); err != nil {
						return err
					}
				}
			}
		}
		if alreadyRecorded {
			return nil
		}
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		posture := domain.HealthPostureBlocking
		createdAt := time.Now().UTC()
		if s.now != nil {
			createdAt = s.now().UTC()
		}
		subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
		displayNames, err := tx.DisplayNamesFor(ctx, "project-system", subject)
		if err != nil {
			return err
		}
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID:        domain.ItemID(fmt.Sprintf("%s%d", DurableStopItemPrefix, state.Revision+1)),
			ProjectID: domain.ProjectID("project-system"),
			Subject:   subject,
			Type:      domain.AttentionSystemHealth,
			Priority:  domain.PriorityHigh,
			Reason:    durableStopReason(cause),
			RequestedDecision: []domain.Action{
				domain.ActionAcknowledge,
				domain.ActionRunDoctor,
			},
			HealthDiagnostic: &domain.HealthDiagnostic{
				Code: "daemon_durable_stop", Impairs: domain.ImpairedCapabilityUnattendedAdmission,
			},
			DisplayNames:      displayNames,
			ItemVersion:       1,
			InterruptionClass: domain.InterruptionExceptional,
			CreatedAt:         &createdAt,
			Posture:           &posture,
			Status:            domain.StatusOpen,
		}, nil)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	})
}

func durableStopReason(cause error) string {
	return fmt.Sprintf(
		"The daemon stopped unattended operation after a health failure requiring a durable stop: %v. "+
			"Acknowledge and Run Doctor leave unattended admission closed. "+
			"Restart the daemon, then choose Resume Unattended on each durable-stop item to reopen admission. "+
			"A restart alone does not reopen it.",
		cause,
	)
}

// EnableDurableStopRecovery offers the explicit operator recovery only after
// a fresh daemon start. A same-process recurrence strips the offer again,
// because the failed loop has returned and is not safe to resume in place.
func (s *Shell) EnableDurableStopRecovery(ctx context.Context) error {
	return s.store.Write(ctx, func(tx *store.WriteTx) error {
		open, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		for _, item := range open {
			if !strings.HasPrefix(string(item.ID), DurableStopItemPrefix) ||
				item.Offers(domain.ActionResumeUnattended) {
				continue
			}
			next := item
			next.ItemVersion++
			next.RequestedDecision = append(next.RequestedDecision, domain.ActionResumeUnattended)
			if err := tx.PutAttentionItem(ctx, next); err != nil {
				return err
			}
		}
		return nil
	})
}

func withoutAction(actions []domain.Action, removed domain.Action) []domain.Action {
	next := make([]domain.Action, 0, len(actions))
	for _, action := range actions {
		if action != removed {
			next = append(next, action)
		}
	}
	return next
}
