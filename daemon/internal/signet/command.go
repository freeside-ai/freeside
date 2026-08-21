package signet

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ClientCommand is the in-process form of the API contract's ClientCommand
// (api/openapi.yaml): a client-prepared decision submission. The provisional
// expected_bindings map is deliberately not carried: for a decision command
// the authoritative binding set is the payload's item_version, pr_head_sha,
// and artifact_digests, which the acceptance boundary cross-checks against
// the live item (the spec's own note on expected_bindings).
type ClientCommand struct {
	// CommandID is the client-generated idempotency key: a retry with the same
	// CommandID returns the original recorded result, never a second effect.
	CommandID string
	DeviceID  domain.DeviceID
	// ExpectedEntityVersion is the store's per-row entity_version the command
	// was prepared against; distinct from the payload's domain ItemVersion. A
	// mismatch rejects the command with the replacement item.
	ExpectedEntityVersion int64
	Payload               DecisionPayload
}

// DecisionPayload mirrors the API contract's DecisionPayload: the decision and
// the exact bindings it was rendered against.
type DecisionPayload struct {
	ItemID      domain.ItemID
	Action      domain.Action
	ItemVersion int
	PRHeadSHA   string
	// ArtifactDigests is the binding set exactly as rendered to the user;
	// acceptance canonicalizes it (domain.NewCommand), so order and duplicates
	// do not affect the recorded command.
	ArtifactDigests []domain.Digest
	// Message and Attachments carry conversation content for the actions that
	// ride the conversation channel (discuss, plan §5.14). Which actions
	// require or forbid them is acceptance policy (validateCommandContent);
	// attachment order is authored content, preserved as sent.
	Message     string
	Attachments []domain.Digest
	// RunProposalRevision and SnoozeUntil are the typed wire inputs for the
	// two parameterized run-proposal decisions. Submit canonicalizes exactly
	// one of them into the durable Command.Message representation so retries
	// retain the existing write-once command identity.
	RunProposalRevision *RunProposalRevisionInput
	SnoozeUntil         *time.Time
	AlternativeChoices  []AlternativeChoice
}

// AlternativeChoice replaces one finding's recommended route with an offered
// route. Findings omitted from the list retain their recommendations.
type AlternativeChoice struct {
	FindingID domain.FindingID         `json:"finding_id"`
	Route     domain.AdjudicationRoute `json:"route"`
}

// RunProposalRevisionInput deliberately omits SubjectHandle. The store keeps
// that opaque authority binding fixed to the proposal the operator reviewed.
type RunProposalRevisionInput struct {
	Intent            domain.RunProposalIntent `json:"intent"`
	ExpectedCostUnits int                      `json:"expected_cost_units"`
	Scope             domain.RunProposalScope  `json:"scope"`
}

func (in RunProposalRevisionInput) validate() error {
	// Reuse the domain validator with a non-empty sentinel handle; the real
	// trusted handle is overlaid from the current proposal at decision time.
	return (domain.RunProposalParameters{
		SubjectHandle: "server-bound", Intent: in.Intent,
		ExpectedCostUnits: in.ExpectedCostUnits, Scope: in.Scope,
	}).Validate()
}

func decisionMessage(payload DecisionPayload) (string, error) {
	switch payload.Action {
	case domain.ActionStartWithChanges:
		if payload.RunProposalRevision == nil || payload.SnoozeUntil != nil || payload.Message != "" ||
			payload.AlternativeChoices != nil {
			return "", ErrInvalidProposalDecisionPayload
		}
		if err := payload.RunProposalRevision.validate(); err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
		}
		body, err := json.Marshal(payload.RunProposalRevision)
		if err != nil {
			return "", fmt.Errorf("%w: %w", ErrInvalidProposalDecisionPayload, err)
		}
		return string(body), nil
	case domain.ActionSnooze:
		if payload.SnoozeUntil == nil || payload.RunProposalRevision != nil || payload.Message != "" ||
			payload.SnoozeUntil.Location() != time.UTC || payload.AlternativeChoices != nil {
			return "", ErrInvalidProposalDecisionPayload
		}
		return payload.SnoozeUntil.Format(time.RFC3339Nano), nil
	case domain.ActionChooseAlternativeRoute:
		if payload.RunProposalRevision != nil || payload.SnoozeUntil != nil ||
			payload.Message != "" || len(payload.AlternativeChoices) == 0 {
			return "", ErrInvalidFindingAdjudicationDecisionPayload
		}
		return canonicalAlternativeChoices(payload.AlternativeChoices)
	case domain.ActionAcceptRecommendedRoute:
		if payload.RunProposalRevision != nil || payload.SnoozeUntil != nil ||
			payload.Message != "" || payload.AlternativeChoices != nil {
			return "", ErrInvalidFindingAdjudicationDecisionPayload
		}
		return "", nil
	default:
		if payload.RunProposalRevision != nil || payload.SnoozeUntil != nil || payload.AlternativeChoices != nil {
			return "", ErrInvalidProposalDecisionPayload
		}
		return payload.Message, nil
	}
}

// CommandResult is the committed outcome of an accepted command: the durable
// decision record and the server revision of the transaction that applied it.
// A retry of the same CommandID returns this exact value (§5.14 test 4).
type CommandResult struct {
	Record   domain.Command `json:"record"`
	Revision int64          `json:"revision"`
}
