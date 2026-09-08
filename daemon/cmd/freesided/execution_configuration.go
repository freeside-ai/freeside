package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const executionConfigurationItemPrefix = "system-health-execution-configuration-"

// The installed daemon remains useful for pairing and state without pretending
// to execute work. Keep that limitation visible until a driver is configured.
func convergeExecutionConfiguration(ctx context.Context, st *store.Store, enabled bool, now func() time.Time) error {
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
			found = true
			if enabled {
				item.Status = domain.StatusResolved
				item.ItemVersion++
				if err := tx.PutAttentionItem(ctx, item); err != nil {
					return err
				}
			}
		}
		if enabled || found {
			return nil
		}
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		subject := domain.Subject{Type: domain.SubjectSystem, ID: "daemon"}
		names, err := tx.DisplayNamesFor(ctx, "project-system", subject)
		if err != nil {
			return err
		}
		createdAt := time.Now().UTC()
		if now != nil {
			createdAt = now().UTC()
		}
		posture := domain.HealthPostureBlocking
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID:        domain.ItemID(fmt.Sprintf("%s%d", executionConfigurationItemPrefix, state.Revision+1)),
			ProjectID: "project-system", Subject: subject,
			Type: domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
			Reason: "Agent execution is not configured. This daemon provides pairing, stored state, and backups, " +
				"but does not run agents or simulate work. Follow the README's real-run setup instructions to enable execution.",
			RequestedDecision: []domain.Action{domain.ActionAcknowledge},
			HealthDiagnostic: &domain.HealthDiagnostic{
				Code: "execution_not_configured", Impairs: domain.ImpairedCapabilityUnattendedAdmission,
			},
			DisplayNames: names, ItemVersion: 1,
			InterruptionClass: domain.InterruptionExceptional,
			CreatedAt:         &createdAt, Posture: &posture, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	})
}
