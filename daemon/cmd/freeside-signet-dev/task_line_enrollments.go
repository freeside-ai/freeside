package main

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// convergenceSeededAt stamps the seeded identities, enrollments, and
// discovery record. It is fixed, so seeding a store again writes the bytes
// already there and converges.
var convergenceSeededAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// seedConvergenceEnrollments records the enrollments a client's task line
// selections name (#2598): signet refuses a selection whose enrollment the
// store lacks. The Claude enrollment has a discovered catalog, so the
// harness checks a selection's model and effort against it; the Codex one
// has none, so any well-formed model and effort pass. Neither runs anything:
// the harness admits no attempt.
func seedConvergenceEnrollments(ctx context.Context, st *store.Store) error {
	claude := domain.AuthIdentity{
		ID: "convergence-claude", Provider: "claude", AccountBinding: "acct-convergence-claude",
		AuthStoreMutationLease: true, Enabled: true,
	}
	codex := domain.AuthIdentity{
		ID: "convergence-codex", Provider: "openai", AccountBinding: "acct-convergence-codex",
		AuthStoreMutationLease: true, Enabled: true,
	}
	enrollments := []domain.ClientEnrollment{{
		ID: "convergence-claude/claude_code", AuthIdentityID: claude.ID,
		HarnessClient: domain.HarnessClientClaudeCode, Route: "anthropic_claude_subscription",
		AuthMethod: domain.AuthMethodSetupToken, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: claude.AccountBinding,
	}, {
		ID: "convergence-codex/codex_cli", AuthIdentityID: codex.ID,
		HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
		AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: codex.AccountBinding,
	}}
	catalog := domain.DiscoveredModelCatalog{
		EnrollmentID: enrollments[0].ID, HarnessBuild: "2.1.0 (Claude Code)", ObservedAt: convergenceSeededAt,
		Models: []domain.DiscoveredModel{{
			ID: "opus", ResolvedModel: new("claude-opus-5-5"), DisplayName: "Opus",
			EffortLevels: []string{"low", "medium", "high", "xhigh", "max"},
		}, {
			ID: "sonnet", ResolvedModel: new("claude-sonnet-5-5"), DisplayName: "Sonnet",
			EffortLevels: []string{"low", "medium", "high"},
		}},
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		for _, identity := range []domain.AuthIdentity{claude, codex} {
			if err := tx.RecordAuthIdentity(ctx, identity, convergenceSeededAt); err != nil {
				return err
			}
		}
		for _, enrollment := range enrollments {
			if err := tx.RecordClientEnrollment(ctx, enrollment, convergenceSeededAt); err != nil {
				return err
			}
		}
		return tx.RecordDiscoveredModelCatalog(ctx, catalog)
	})
	if err != nil {
		return fmt.Errorf("seed convergence enrollments: %w", err)
	}
	return nil
}
