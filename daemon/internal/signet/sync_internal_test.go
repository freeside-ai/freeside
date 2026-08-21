package signet

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestPublicationAuthorityExclusivityRejectsReadyAndBlocked(t *testing.T) {
	err := validatePublicationAuthorityExclusivity("run-1", true, true)
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("validatePublicationAuthorityExclusivity() = %v, want ErrParentKeyMismatch", err)
	}
}

func TestAuthoritativeStatusOverridesLaggingObservation(t *testing.T) {
	invocation := domain.InvocationID("invocation-1")
	observedAt := time.Date(2026, 8, 15, 4, 0, 0, 0, time.UTC)
	observation := domain.RunObservation{
		RunID: "run-1",
		Milestones: []domain.RunMilestone{{
			RunID: "run-1", Kind: domain.MilestoneExecutionExportRecorded,
			InvocationID: &invocation, RecordedAt: observedAt.Add(-time.Minute),
		}},
		Invocations: []domain.InvocationObservation{{
			InvocationID: invocation, RunID: "run-1", Status: domain.ObservedStatusRunning,
			Live: true, ObservedAt: observedAt,
		}},
	}

	projected := withAuthoritativeInvocationStatuses(observation)
	if projected.Invocations[0].Status != domain.ObservedStatusCompleted || projected.Invocations[0].Live {
		t.Fatalf("projected invocation = %+v, want completed and not live", projected.Invocations[0])
	}
	if projected.Invocations[0].ObservedAt != observedAt {
		t.Fatalf("projected observed_at = %v, want %v", projected.Invocations[0].ObservedAt, observedAt)
	}
	if observation.Invocations[0].Status != domain.ObservedStatusRunning || !observation.Invocations[0].Live {
		t.Fatalf("input observation was mutated: %+v", observation.Invocations[0])
	}
}

func TestNormalizeAttentionItemNormalizesNestedAdjudicationArraysWithoutMutation(t *testing.T) {
	item := domain.AttentionItem{
		FindingAdjudication: &domain.FindingAdjudicationBinding{
			Proposals: []domain.FindingAdjudicationProposal{{}},
		},
	}

	normalized := normalizeAttentionItem(item)
	proposal := normalized.FindingAdjudication.Proposals[0]
	if proposal.CitedRules == nil || proposal.Assumptions == nil ||
		proposal.OpenQuestions == nil || proposal.OfferedAlternatives == nil {
		t.Fatalf("normalized proposal retains nil arrays: %+v", proposal)
	}
	original := item.FindingAdjudication.Proposals[0]
	if original.CitedRules != nil || original.Assumptions != nil ||
		original.OpenQuestions != nil || original.OfferedAlternatives != nil {
		t.Fatalf("input proposal mutated: %+v", original)
	}
}
