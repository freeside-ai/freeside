package seedfixture

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// representativeInventory is what seedRepresentative writes:
//   - runs: the awaiting campaign's specification run; the retry campaign's
//     specification run and both implementation attempts; the ready,
//     completed, question, and failed verification runs;
//   - tasks: one per campaign or standalone run;
//   - attention items: one open item per type in OpenItemTypes, plus the retry
//     campaign's approved spec_approval and the completed run's resolved
//     ready_for_final_review.
var representativeInventory = Inventory{
	Projects: 2, Tasks: 6, Runs: 8, AttentionItems: 14, WorkUnitPRBindings: 2,
}

// OpenItemTypes are the attention types the representative fixture leaves
// one open item of: every type except task_proposal and effect_proposal,
// whose only trusted creation path is a dedicated opener.
var OpenItemTypes = []domain.AttentionType{
	domain.AttentionSpecApproval, domain.AttentionExecutionFailure, domain.AttentionAgentQuestion,
	domain.AttentionReviewDiminishing, domain.AttentionReviewDispute, domain.AttentionReviewContradiction,
	domain.AttentionReviewConfiguration, domain.AttentionFindingAdjudication,
	domain.AttentionReadyForFinalReview, domain.AttentionPublishBlocked,
	domain.AttentionSystemHealth, domain.AttentionBlocked,
}

func seedRepresentative(ctx context.Context, st *store.Store) (Inventory, error) {
	s := seeder{ctx: ctx, st: st}
	if err := registerProjects(s, freesideProject, orioleProject); err != nil {
		return Inventory{}, fmt.Errorf("register projects: %w", err)
	}
	awaiting, approval, err := seedAwaitingApproval(s)
	if err != nil {
		return Inventory{}, err
	}
	failed, active, err := seedRetryCampaign(s)
	if err != nil {
		return Inventory{}, err
	}
	if _, err := s.publish(freesideProject, readyRunID, 654, 651, domain.DisplayName{
		Text: "Summarize review rounds on the card", Source: domain.DisplayNameSourceAgent,
	}, base.Add(-3*time.Hour)); err != nil {
		return Inventory{}, err
	}
	if _, err := seedCompletedRun(s); err != nil {
		return Inventory{}, err
	}
	verification, err := seedFailedVerification(s)
	if err != nil {
		return Inventory{}, err
	}
	if err := seedItems(s, failed, active, verification, awaiting, approval); err != nil {
		return Inventory{}, err
	}
	return representativeInventory, nil
}
