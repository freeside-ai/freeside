package domain_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestDeriveSpecDiffPreservesInteriorMatchesAboveMatrixBound(t *testing.T) {
	before := make([]string, 1_001)
	for index := range before {
		before[index] = fmt.Sprintf("requirement %04d", index)
	}
	after := append([]string(nil), before...)
	after[100] = "requirement 0100, revised"
	after[900] = "requirement 0900, revised"

	diff := domain.DeriveSpecDiff(strings.Join(before, "\n"), strings.Join(after, "\n"))
	if diff.LinesAdded != 2 || diff.LinesRemoved != 2 || diff.Truncated {
		t.Fatalf("large separated diff = %+v", diff)
	}
	for _, line := range []string{"-requirement 0100", "+requirement 0100, revised", "-requirement 0900", "+requirement 0900, revised"} {
		if !strings.Contains(diff.Unified, line) {
			t.Errorf("diff does not contain %q", line)
		}
	}
}

func TestDeriveSpecDiffPreservesRepeatedInteriorMatchesAboveMatrixBound(t *testing.T) {
	before := make([]string, 1_001)
	for index := range before {
		before[index] = "repeated requirement"
	}
	after := append([]string(nil), before...)
	after[100] = "first revised requirement"
	after[900] = "second revised requirement"

	diff := domain.DeriveSpecDiff(strings.Join(before, "\n"), strings.Join(after, "\n"))
	if diff.LinesAdded != 2 || diff.LinesRemoved != 2 || diff.Truncated {
		t.Fatalf("large repeated-line diff = %+v", diff)
	}
	for _, line := range []string{"+first revised requirement", "+second revised requirement"} {
		if !strings.Contains(diff.Unified, line) {
			t.Errorf("diff does not contain %q", line)
		}
	}
}

func TestDeriveSpecDiffPreservesTrailingWhitespace(t *testing.T) {
	diff := domain.DeriveSpecDiff("# Hi", "# Hi  ")
	if diff.LinesAdded != 1 || diff.LinesRemoved != 1 {
		t.Fatalf("trailing-whitespace diff = %+v", diff)
	}
	if !strings.Contains(diff.Unified, "+# Hi  ") {
		t.Fatalf("trailing whitespace missing from %q", diff.Unified)
	}
	if err := diff.Validate(); err != nil {
		t.Fatalf("trailing-whitespace diff validation: %v", err)
	}
}

func TestDeriveSpecDiffAcceptsCRLFBeforeHunkEnd(t *testing.T) {
	before := strings.Join([]string{"one", "two", "three", "four", "five", "six", "seven", "eight"}, "\r\n")
	after := strings.Join([]string{"one", "revised", "three", "four", "five", "six", "seven", "eight"}, "\r\n")

	diff := domain.DeriveSpecDiff(before, after)
	if diff.LinesAdded != 1 || diff.LinesRemoved != 1 {
		t.Fatalf("CRLF diff = %+v", diff)
	}
	if strings.Contains(diff.Unified, "\r") {
		t.Fatalf("CRLF diff contains carriage return: %q", diff.Unified)
	}
	if err := diff.Validate(); err != nil {
		t.Fatalf("CRLF diff validation: %v", err)
	}
}

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

func TestSpecRevisionFacts(t *testing.T) {
	in := specRevisionInput(t)
	item := mustItem(t, in)
	if item.SpecRevision == nil || item.SpecRevision.Iteration != 2 {
		t.Fatalf("spec revision = %+v", item.SpecRevision)
	}

	tests := []struct {
		name   string
		mutate func(*domain.AttentionItemInput)
	}{
		{"wrong item type", func(in *domain.AttentionItemInput) { in.Type = domain.AttentionBlocked }},
		{"same prior item", func(in *domain.AttentionItemInput) { in.SpecRevision.PriorItemID = in.ID }},
		{"comment digest", func(in *domain.AttentionItemInput) { in.SpecRevision.PriorComments[0].Digest = "sha256:other" }},
		{"comment artifact id", func(in *domain.AttentionItemInput) {
			in.SpecRevision.PriorComments[0].ArtifactID = "spec-feedback-other"
		}},
		{"comment item iteration", func(in *domain.AttentionItemInput) {
			in.SpecRevision.PriorComments[0].RaisedOnItemID = "spec-approval-run-9"
		}},
		{"prior item", func(in *domain.AttentionItemInput) {
			in.SpecRevision.PriorItemID = "spec-approval-run-9"
		}},
		{"duplicate comment", func(in *domain.AttentionItemInput) {
			in.SpecRevision.PriorComments = append(in.SpecRevision.PriorComments, in.SpecRevision.PriorComments[0])
		}},
		{"unknown addressal", func(in *domain.AttentionItemInput) { in.SpecRevision.ClaimedAddressals[0].CommentID = "unknown" }},
		{"duplicate addressal", func(in *domain.AttentionItemInput) {
			in.SpecRevision.ClaimedAddressals = append(in.SpecRevision.ClaimedAddressals, in.SpecRevision.ClaimedAddressals[0])
		}},
		{"addressals digest", func(in *domain.AttentionItemInput) { in.SpecRevision.AddressalsDigest = "sha256:other" }},
		{"missing claim", func(in *domain.AttentionItemInput) { in.AgentClaims = in.AgentClaims[:1] }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := specRevisionInput(t)
			tc.mutate(&candidate)
			if _, err := domain.NewAttentionItem(candidate, nil); !errors.Is(err, domain.ErrCardFactInconsistent) &&
				!errors.Is(err, domain.ErrCardFactOutsideItem) {
				t.Fatalf("NewAttentionItem = %v", err)
			}
		})
	}

	changed := item
	changed.ItemVersion++
	copy := *changed.SpecRevision
	copy.Diff.LinesAdded++
	changed.SpecRevision = &copy
	if err := domain.ValidateAttentionItemTransition(item, changed); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("changed spec revision transition = %v", err)
	}

	detached := specRevisionInput(t)
	detachedItem := mustItem(t, detached)
	detached.SpecRevision.PriorComments[0].Body = "mutated"
	detached.SpecRevision.ClaimedAddressals[0].Response = "mutated"
	if detachedItem.SpecRevision.PriorComments[0].Body == "mutated" ||
		detachedItem.SpecRevision.ClaimedAddressals[0].Response == "mutated" {
		t.Fatal("spec revision slices remained caller-owned")
	}
}

func specRevisionInput(t *testing.T) domain.AttentionItemInput {
	t.Helper()
	commentBody := "Bound the request body."
	commentDigest := domain.Digest(contentaddr.Sum([]byte(commentBody)))
	addressals := []domain.SpecAddressalClaim{{CommentID: "revise-spec", Response: "Added a 1 MiB bound."}}
	body, err := json.Marshal(addressals)
	if err != nil {
		t.Fatal(err)
	}
	addressalsDigest := domain.Digest(contentaddr.Sum(body))
	provenance := domain.Provenance{
		ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-specify-2",
		HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
	}
	runID := domain.RunID("run-1")
	in := validItemInput(domain.AttentionSpecApproval)
	in.ID = "spec-approval-run-2"
	in.Subject.RunID = &runID
	in.AgentClaims = []domain.AgentClaim{
		{
			Label: "Specification", Artifact: "spec-run-2", Digest: "sha256:spec-2",
			Provenance: provenance,
			Metadata:   claimMeta(domain.EvidenceMediaImagePNG),
		},
		{
			Label: "Addressals", Artifact: "spec-addressals-run-2", Digest: addressalsDigest,
			Provenance: provenance,
			Metadata:   claimMeta(domain.EvidenceMediaImagePNG),
		},
	}
	in.SpecRevision = &domain.SpecRevisionFacts{
		Iteration: 2, PriorItemID: "spec-approval-run-1",
		PriorSpecArtifactID: "spec-run-1", PriorSpecDigest: "sha256:spec-1",
		Diff: domain.SpecDiff{
			LinesAdded: 1, LinesRemoved: 1,
			Unified: "@@ -1,1 +1,1 @@\n-old\n+new",
		},
		PriorComments: []domain.SpecRevisionComment{{
			CommentID: "revise-spec", ArtifactID: "spec-feedback-revise-spec", Digest: commentDigest,
			RaisedOnItemID: "spec-approval-run-1", Iteration: 1, Body: commentBody,
		}},
		ClaimedAddressals: addressals, AddressalsDigest: addressalsDigest,
	}
	return in
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
