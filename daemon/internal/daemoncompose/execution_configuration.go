package daemoncompose

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const executionConfigurationItemPrefix = "system-health-execution-configuration-"

// ExecutionNotConfiguredCode is the driverless card's diagnostic code.
// Prelaunch reads it: on a deputy, the next start that passes its start-up
// checks closes the card.
const ExecutionNotConfiguredCode = "execution_not_configured"

// The installed daemon remains useful for pairing and state without pretending
// to execute work. Keep that limitation, and why it holds, visible until a
// driver runs: a card whose reason no longer matches the state is concluded
// and the current one raised, so a restart that changes the reason, or the
// detail a reason names (the section, project, or error), updates the card.
func convergeExecutionConfiguration(
	ctx context.Context, st *store.Store, state ExecutionState, detail string, now func() time.Time,
) error {
	if !state.valid() {
		return fmt.Errorf("invalid execution state %q", state)
	}
	at := time.Now().UTC()
	if now != nil {
		at = now().UTC()
	}
	code, reason := executionStateDiagnostic(state, detail)
	return st.Write(ctx, func(tx *store.WriteTx) error {
		items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		found := false
		for _, item := range items {
			if !strings.HasPrefix(string(item.ID), executionConfigurationItemPrefix) {
				continue
			}
			if !state.Runs() && item.HealthDiagnostic != nil && item.HealthDiagnostic.Code == code &&
				item.Reason == reason {
				found = true
				continue
			}
			resolution := "Resolved when an execution driver was configured."
			if !state.Runs() {
				resolution = "Replaced by the card for the reason this daemon now runs no agents."
			}
			item, err := item.ConcludeByDaemon(domain.ConcludedByDaemon, at, resolution, nil)
			if err != nil {
				return err
			}
			item.ItemVersion++
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		if state.Runs() || found {
			return nil
		}
		serverState, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
		names, err := tx.DisplayNamesFor(ctx, "project-system", subject)
		if err != nil {
			return err
		}
		posture := domain.HealthPostureBlocking
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID:        domain.ItemID(fmt.Sprintf("%s%d", executionConfigurationItemPrefix, serverState.Revision+1)),
			ProjectID: "project-system", Subject: subject,
			Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
			Reason:            reason,
			RequestedDecision: []domain.Action{domain.ActionAcknowledge},
			HealthDiagnostic: &domain.HealthDiagnostic{
				Code: code, Impairs: domain.ImpairedCapabilityUnattendedAdmission,
			},
			DisplayNames: names, ItemVersion: 1,
			InterruptionClass: domain.InterruptionExceptional,
			CreatedAt:         &at, Posture: &posture, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	})
}

const executionDriverlessScope = "This daemon provides pairing, stored state, and backups, " +
	"but does not run agents or simulate work."

// executionStateDiagnostic is the card a driverless state raises: its code
// and a reason that tells the operator what restarts execution. detail is
// what the installed daemon's stored states name: the refusal, the enabled
// projects, or the composition failure; the other states ignore it.
func executionStateDiagnostic(state ExecutionState, detail string) (code, reason string) {
	switch state {
	case ExecutionConfigured:
		return "", ""
	case ExecutionNotConfigured:
		return ExecutionNotConfiguredCode, "Agent execution is not configured. " + executionDriverlessScope +
			" Follow the README's real-run setup instructions to enable execution. On a deputy, this card " +
			"closes on the next deputy start whose start-up checks pass."
	case ExecutionAuthorityInUse:
		return "app_authority_in_use", "Agent execution is off: another process, most likely a deputy run, " +
			"held the App authority when this daemon started. " + executionDriverlessScope +
			" This daemon restarts with execution on its own once that process lets go, and waits while a " +
			"production rig lease exists. If no deputy run is running, a stale lease needs " +
			"freesided rig recover. Run freesided execution resume to retry at once."
	case ExecutionRigLeasePresent:
		return "rig_lease_present", "Agent execution is off: a production rig lease existed when this daemon " +
			"started, so a deputy run holds or held the App authority. " + executionDriverlessScope +
			" Once the deputy run ends, run freesided execution resume to restart this daemon with execution. " +
			"If no deputy run is running, the lease is stale: run freesided rig recover first."
	case ExecutionPaused:
		return "execution_paused", "Agent execution is paused: an operator ran freesided execution pause, " +
			"and this daemon finished its running work and restarted without execution, so a deputy run " +
			"can use the App authority. " + executionDriverlessScope +
			" Once the deputy run ends, run freesided execution resume to restart this daemon with execution."
	case ExecutionSettingsRefused:
		return string(state), "Agent execution is on, but the stored settings cannot run: " + detail + ". " +
			executionDriverlessScope + " Fix the section with freesided settings set, then run freesided " +
			"execution resume to restart this daemon with execution."
	case ExecutionNoEnabledProject:
		return string(state), "Agent execution is on, but no project is enabled. " + executionDriverlessScope +
			" Enable one with freesided settings set -project <project> -section enabled " +
			"-value '{\"enabled\":true}', then run " +
			"freesided execution resume to restart this daemon with execution."
	case ExecutionSeveralProjectsEnabled:
		return string(state), "Agent execution is on, but more than one project is enabled (" + detail +
			"), and this daemon runs one. " + executionDriverlessScope + " Turn all but one off with freesided " +
			"settings set, then run freesided execution resume to restart this daemon with execution."
	case ExecutionCompositionFailed:
		return string(state), "Agent execution is on, but the driver could not be composed from the stored " +
			"settings: " + detail + ". " + executionDriverlessScope + " Fix the cause, then run freesided " +
			"execution resume to restart this daemon with execution."
	case ExecutionHostCheckFailed:
		return string(state), "Agent execution is on, but a start check failed: " + detail + ". " +
			executionDriverlessScope + " Once it is fixed, run freesided execution resume to restart this " +
			"daemon with execution."
	}
	return "", ""
}
