package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestCardFactValidation(t *testing.T) {
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	itemID := domain.ItemID("item-spec")
	hold := domain.HoldTrustBlocked
	rule := domain.TrustRuleTrustProfileDrift

	valid := []interface{ Validate() error }{
		domain.DisplayName{Text: "owner/repo", Source: domain.DisplayNameSourceName},
		domain.DisplayNames{
			Project:  domain.DisplayName{Text: "owner/repo", Source: domain.DisplayNameSourceName},
			WorkUnit: domain.DisplayName{Text: "#724", Source: domain.DisplayNameSourceName},
		},
		domain.CostSoFar{Currency: "USD", Amount: "12.50", Invocations: 2, Complete: true},
		domain.ExecutionFailureFacts{
			Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
			InvocationID: "inv-1",
		},
		domain.PublishBlockFacts{HoldReason: &hold},
		domain.PublishBlockFacts{TrustRule: &rule},
		domain.DiffStats{FilesChanged: 1, Additions: 2, BaseSHA: "base", HeadSHA: "head"},
		domain.BlockedWait{Kind: domain.BlockedWaitSpecApproval, Since: at, ItemID: &itemID},
		domain.HealthDiagnostic{Code: "doctor.credentials", Impairs: domain.ImpairedCapabilityAgentCredential},
		domain.ReviewDisputeBinding{
			RunID: "run-1", Round: 1, FindingIDs: []domain.FindingID{"finding-1"},
			CompletionEvidence: "sha256:evidence",
		},
	}
	for _, value := range valid {
		if err := value.Validate(); err != nil {
			t.Errorf("%T.Validate() = %v", value, err)
		}
	}

	invalid := []interface{ Validate() error }{
		domain.DisplayName{Source: domain.DisplayNameSourceName},
		domain.CostSoFar{Currency: "usd", Amount: "01", Invocations: 0},
		domain.ExecutionFailureFacts{Outcome: domain.ExecutionOutcomeFailed},
		domain.PublishBlockFacts{},
		domain.PublishBlockFacts{HoldReason: &hold, TrustRule: &rule},
		domain.DiffStats{FilesChanged: -1, BaseSHA: "base", HeadSHA: "head"},
		domain.BlockedWait{Kind: domain.BlockedWaitSpecApproval, Since: at},
		domain.HealthDiagnostic{Code: "UPPER", Impairs: domain.ImpairedCapabilityNone},
		domain.ReviewDisputeBinding{
			RunID: "run-1", Round: 1, FindingIDs: []domain.FindingID{"finding-1", "finding-1"},
			CompletionEvidence: "sha256:evidence",
		},
	}
	for _, value := range invalid {
		if err := value.Validate(); !errors.Is(err, domain.ErrCardFactInconsistent) {
			t.Errorf("%T.Validate() = %v, want ErrCardFactInconsistent", value, err)
		}
	}
}

func TestAttentionItemCardFactRules(t *testing.T) {
	for _, in := range cardFactInputs() {
		if _, err := domain.NewAttentionItem(in, nil); err != nil {
			t.Errorf("valid %s fact rejected: %v", in.Type, err)
		}
	}

	for _, source := range cardFactInputs()[1:] {
		wrong := validItemInput(domain.AttentionSpecApproval)
		wrong.DisplayNames = source.DisplayNames
		wrong.BillableCostSoFar = source.BillableCostSoFar
		wrong.ExecutionFailure = source.ExecutionFailure
		wrong.PublishBlock = source.PublishBlock
		wrong.DiffStats = source.DiffStats
		wrong.BlockedOn = source.BlockedOn
		wrong.HealthDiagnostic = source.HealthDiagnostic
		wrong.ReviewDispute = source.ReviewDispute
		if _, err := domain.NewAttentionItem(wrong, nil); !errors.Is(err, domain.ErrCardFactOutsideItem) {
			t.Errorf("%s fact on spec approval = %v, want ErrCardFactOutsideItem", source.Type, err)
		}
	}

	ready := validItemInput(domain.AttentionReadyForFinalReview)
	ready.PRHeadSHA = "head"
	ready.DiffStats = &domain.DiffStats{BaseSHA: "base", HeadSHA: "other"}
	if _, err := domain.NewAttentionItem(ready, nil); !errors.Is(err, domain.ErrCardFactInconsistent) {
		t.Fatalf("mismatched diff head = %v, want ErrCardFactInconsistent", err)
	}

	blocked := validItemInput(domain.AttentionBlocked)
	created := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	blocked.CreatedAt = &created
	itemID := domain.ItemID("item-spec")
	blocked.BlockedOn = &domain.BlockedWait{
		Kind: domain.BlockedWaitSpecApproval, Since: created.Add(time.Second), ItemID: &itemID,
	}
	if _, err := domain.NewAttentionItem(blocked, nil); !errors.Is(err, domain.ErrCardFactInconsistent) {
		t.Fatalf("future blocked since = %v, want ErrCardFactInconsistent", err)
	}
}

func TestAttentionItemCardFactTransitions(t *testing.T) {
	for _, populated := range cardFactInputs()[2:] {
		oldInput := validItemInput(populated.Type)
		if populated.Type == domain.AttentionExecutionFailure || populated.Type == domain.AttentionReviewDispute {
			runID := domain.RunID("run-1")
			oldInput.Subject.RunID = &runID
		}
		oldInput.CreatedAt = populated.CreatedAt
		old := mustItem(t, oldInput)
		populated.ItemVersion = 2
		attached := mustItem(t, populated)
		if err := domain.ValidateAttentionItemTransition(old, attached); err != nil {
			t.Errorf("nil to populated %s fact rejected: %v", populated.Type, err)
		}

		changed := attached
		changed.ItemVersion = 3
		switch {
		case changed.ExecutionFailure != nil:
			copy := *changed.ExecutionFailure
			changed.ExecutionFailure = &copy
			changed.ExecutionFailure.InvocationID = "inv-other"
		case changed.PublishBlock != nil:
			hold := domain.HoldExternalConflict
			changed.PublishBlock = &domain.PublishBlockFacts{HoldReason: &hold}
			if err := domain.ValidateAttentionItemTransition(attached, changed); err != nil {
				t.Errorf("updating %s fact = %v, want accepted successor", populated.Type, err)
			}
			continue
		case changed.DiffStats != nil:
			copy := *changed.DiffStats
			changed.DiffStats = &copy
			changed.DiffStats.Additions++
		case changed.BlockedOn != nil:
			copy := *changed.BlockedOn
			changed.BlockedOn = &copy
			changed.BlockedOn.Since = changed.BlockedOn.Since.Add(-time.Second)
		case changed.HealthDiagnostic != nil:
			copy := *changed.HealthDiagnostic
			changed.HealthDiagnostic = &copy
			changed.HealthDiagnostic.Code = "doctor.other"
		case changed.ReviewDispute != nil:
			copy := *changed.ReviewDispute
			copy.FindingIDs = append([]domain.FindingID(nil), changed.ReviewDispute.FindingIDs...)
			changed.ReviewDispute = &copy
			changed.ReviewDispute.FindingIDs = append(changed.ReviewDispute.FindingIDs, "finding-2")
		}
		if err := domain.ValidateAttentionItemTransition(attached, changed); !errors.Is(err, domain.ErrImmutableTransition) {
			t.Errorf("changing %s fact = %v, want ErrImmutableTransition", populated.Type, err)
		}
	}
}

func TestAttentionItemCostTransitions(t *testing.T) {
	baseInput := cardFactInputs()[1]
	baseInput.BillableCostSoFar = &domain.CostSoFar{
		Currency: "USD", Amount: "9.90", Invocations: 2, Complete: true,
	}
	base := mustItem(t, baseInput)

	valid := []struct {
		name string
		cost *domain.CostSoFar
	}{
		{"larger amount and count", &domain.CostSoFar{
			Currency: "USD", Amount: "10.00", Invocations: 3, Complete: true,
		}},
		{"new incomplete invocation", &domain.CostSoFar{
			Currency: "USD", Amount: "9.90", Invocations: 3,
		}},
	}
	for _, tc := range valid {
		t.Run(tc.name, func(t *testing.T) {
			updated := base
			updated.ItemVersion++
			updated.BillableCostSoFar = tc.cost
			if err := domain.ValidateAttentionItemTransition(base, updated); err != nil {
				t.Fatalf("transition = %v", err)
			}
		})
	}

	invalid := []struct {
		name string
		cost *domain.CostSoFar
	}{
		{"removed", nil},
		{"currency changed", &domain.CostSoFar{
			Currency: "EUR", Amount: "9.90", Invocations: 2, Complete: true,
		}},
		{"numeric amount regressed", &domain.CostSoFar{
			Currency: "USD", Amount: "9.10", Invocations: 2, Complete: true,
		}},
		{"invocation count regressed", &domain.CostSoFar{
			Currency: "USD", Amount: "9.90", Invocations: 1, Complete: true,
		}},
		{"completeness regressed without a new invocation", &domain.CostSoFar{
			Currency: "USD", Amount: "9.90", Invocations: 2,
		}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			updated := base
			updated.ItemVersion++
			updated.BillableCostSoFar = tc.cost
			if err := domain.ValidateAttentionItemTransition(base, updated); !errors.Is(err, domain.ErrImmutableTransition) {
				t.Fatalf("transition = %v, want ErrImmutableTransition", err)
			}
		})
	}

	legacyInput := validItemInput(domain.AttentionReviewDiminishing)
	legacy := mustItem(t, legacyInput)
	legacyInput.ItemVersion++
	legacyInput.BillableCostSoFar = &domain.CostSoFar{
		Currency: "USD", Amount: "1.00", Invocations: 1,
	}
	if err := domain.ValidateAttentionItemTransition(legacy, mustItem(t, legacyInput)); err != nil {
		t.Fatalf("nil to populated transition = %v", err)
	}
}

func TestAttentionItemCardFactsDetachCallerInput(t *testing.T) {
	inputs := cardFactInputs()

	publish := inputs[3]
	publishItem := mustItem(t, publish)
	*publish.PublishBlock.TrustRule = domain.TrustRuleVerificationFailed
	if got := *publishItem.PublishBlock.TrustRule; got != domain.TrustRuleTrustProfileDrift {
		t.Errorf("publish block trust rule changed through caller input: %q", got)
	}

	blocked := inputs[5]
	blockedItem := mustItem(t, blocked)
	*blocked.BlockedOn.ItemID = "item-other"
	if got := *blockedItem.BlockedOn.ItemID; got != "item-spec" {
		t.Errorf("blocked item id changed through caller input: %q", got)
	}

	dispute := inputs[7]
	disputeItem := mustItem(t, dispute)
	dispute.ReviewDispute.FindingIDs[0] = "finding-other"
	if got := disputeItem.ReviewDispute.FindingIDs[0]; got != "finding-1" {
		t.Errorf("review dispute finding changed through caller input: %q", got)
	}
}

func cardFactInputs() []domain.AttentionItemInput {
	at := time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)
	runID := domain.RunID("run-1")
	itemID := domain.ItemID("item-spec")
	rule := domain.TrustRuleTrustProfileDrift

	display := validItemInput(domain.AttentionSpecApproval)
	display.DisplayNames = &domain.DisplayNames{
		Project:  domain.DisplayName{Text: "owner/repo", Source: domain.DisplayNameSourceName},
		WorkUnit: domain.DisplayName{Text: "#724", Source: domain.DisplayNameSourceName},
	}
	cost := validItemInput(domain.AttentionReviewDiminishing)
	cost.BillableCostSoFar = &domain.CostSoFar{Currency: "USD", Amount: "1.00", Invocations: 1, Complete: true}
	failure := validItemInput(domain.AttentionExecutionFailure)
	failure.Subject.RunID = &runID
	failure.ExecutionFailure = &domain.ExecutionFailureFacts{
		Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
		InvocationID: "inv-1",
	}
	publish := validItemInput(domain.AttentionPublishBlocked)
	publish.PublishBlock = &domain.PublishBlockFacts{TrustRule: &rule}
	ready := validItemInput(domain.AttentionReadyForFinalReview)
	ready.PRHeadSHA = "head"
	ready.DiffStats = &domain.DiffStats{FilesChanged: 1, Additions: 2, BaseSHA: "base", HeadSHA: "head"}
	blocked := validItemInput(domain.AttentionBlocked)
	blocked.CreatedAt = &at
	blocked.BlockedOn = &domain.BlockedWait{Kind: domain.BlockedWaitSpecApproval, Since: at, ItemID: &itemID}
	health := validItemInput(domain.AttentionSystemHealth)
	health.HealthDiagnostic = &domain.HealthDiagnostic{Code: "doctor.store", Impairs: domain.ImpairedCapabilityRunVisibility}
	dispute := validItemInput(domain.AttentionReviewDispute)
	dispute.Subject.RunID = &runID
	dispute.ReviewDispute = &domain.ReviewDisputeBinding{
		RunID: runID, Round: 1, FindingIDs: []domain.FindingID{"finding-1"},
		CompletionEvidence: "sha256:evidence",
	}
	return []domain.AttentionItemInput{display, cost, failure, publish, ready, blocked, health, dispute}
}
