package signet

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestCapabilityRetryAcceptsOnlyAnOfferedDigest(t *testing.T) {
	manifest, err := domain.NewCapabilityManifest("Provider web read", domain.EgressProviderWebRead)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("run-capability-retry")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-capability-retry", ProjectID: "proj-1",
		Subject: domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID,
		},
		Type: domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason: "implementation failed",
		RequestedDecision: []domain.Action{
			domain.ActionRetryWithCapability, domain.ActionDiscuss, domain.ActionStop,
		},
		ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: domain.ExecutionOutcomeFailed, Stage: domain.StageNameImplementation,
			InvocationID:     "inv-implement-run-capability-retry",
			OfferedManifests: []domain.CapabilityManifestOffer{manifest.Offer()},
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "command-capability-retry", DeviceID: "device-1",
		ItemID: item.ID, ItemVersion: item.ItemVersion,
		Action: domain.ActionRetryWithCapability, Message: string(manifest.Digest),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCapabilityRetryDecision(command, item); err != nil {
		t.Fatalf("offered manifest rejected: %v", err)
	}
	if err := (&Service{}).validateCommandContent(command); err != nil {
		t.Fatalf("typed digest rejected by shared content gate: %v", err)
	}

	other, err := domain.NewCapabilityManifest("Clean verification", domain.EgressCleanVerification)
	if err != nil {
		t.Fatal(err)
	}
	command.Message = string(other.Digest)
	if err := validateCapabilityRetryDecision(command, item); !errors.Is(err, ErrCapabilityManifestNotOffered) {
		t.Fatalf("unoffered digest error = %v", err)
	}
	command.Message = "not-a-digest"
	if err := validateCapabilityRetryDecision(command, item); !errors.Is(err, ErrInvalidCapabilityRetryDecisionPayload) {
		t.Fatalf("malformed digest error = %v", err)
	}
}
