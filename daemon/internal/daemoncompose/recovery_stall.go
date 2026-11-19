package daemoncompose

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const StageRecoveryStalledItemPrefix = "stage-recovery-stalled-"

func stageRecoveryStalledPrefix(id domain.InvocationID) string {
	return StageRecoveryStalledItemPrefix + string(id) + "-"
}

// IsStageRecoveryNotice reports whether itemID is a stalled-recovery notice
// filed for exactly this invocation. The remainder after the prefix is the
// filing revision alone, so invocation "inv-a" never matches the notice of
// "inv-a-b".
func IsStageRecoveryNotice(itemID string, id domain.InvocationID) bool {
	revision, ok := strings.CutPrefix(itemID, stageRecoveryStalledPrefix(id))
	return ok && revision != "" && strings.Trim(revision, "0123456789") == ""
}

// StageRecoveryNotice returns the stage driver's RecoveryStall hook (#3428):
// it files one advisory stage_recovery_stalled health notice when recovery of
// a released export has kept failing retryably, and resolves it when that
// deferral ends. Until the grace abandons recovery the run reads as running,
// so without the notice the operator would not hear of the stall until the
// execution-failure card. The notice holds nothing and changes no recovery.
//
// Like InvocationStallNotice, the subject is the system, not the run, so a
// self-resolving advisory notice never ends a supervised run; the reason
// names the invocation instead.
func StageRecoveryNotice(
	st *store.Store, now func() time.Time,
) func(context.Context, stage.RecoveryStall) error {
	return func(ctx context.Context, report stage.RecoveryStall) error {
		return st.Write(ctx, func(tx *store.WriteTx) error {
			open, err := OpenInvocationStallNotices(ctx, tx, func(itemID string) bool {
				return IsStageRecoveryNotice(itemID, report.InvocationID)
			})
			if err != nil {
				return err
			}
			if !report.Stalled {
				return ResolveInvocationStallNotices(ctx, tx, open, now().UTC(),
					"Resolved when the stage's recovery succeeded, concluded, or was abandoned.")
			}
			if len(open) > 0 {
				return nil
			}
			item, err := StageRecoveryStalledItem(ctx, tx, report, now().UTC())
			if err != nil {
				return err
			}
			return tx.PutAttentionItem(ctx, item)
		})
	}
}

func StageRecoveryStalledItem(
	ctx context.Context, tx *store.WriteTx, report stage.RecoveryStall, createdAt time.Time,
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
		ID: domain.ItemID(fmt.Sprintf("%s%d",
			stageRecoveryStalledPrefix(report.InvocationID), state.Revision+1)),
		ProjectID: "project-system", Subject: subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason: fmt.Sprintf(
			"Recovery of invocation %s (ward run %s, phase %s) has failed for %s. "+
				"The run still shows as running while recovery retries; recovery is abandoned "+
				"as failed %s after its first failure. Latest error: %s. "+
				"This notice resolves itself when recovery succeeds, concludes, or is abandoned.",
			report.InvocationID, report.RunID, report.Phase,
			report.Elapsed.Round(time.Second), report.Grace, report.Error),
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "stage_recovery_stalled", Impairs: domain.ImpairedCapabilityNone,
		},
		DisplayNames: names, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional,
		CreatedAt:         &createdAt, Posture: &posture, Status: domain.StatusOpen,
	}, nil)
}
