package domain

import (
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// InvocationIntentKind names a durable dispatch-intent protocol member. The
// zero value is invalid by design.
type InvocationIntentKind string

// Invocation dispatch intent kinds are durable protocol vocabulary shared by
// the lanes that create them and readers that authenticate a started attempt.
const (
	AgentInvocationRequestedKind         InvocationIntentKind = "agent_invocation_requested"
	ProductionInvocationRequestedKind    InvocationIntentKind = "production_invocation_requested"
	SpecificationInvocationRequestedKind InvocationIntentKind = "specification_invocation_requested"
	SpecificationDiscussionRequestedKind InvocationIntentKind = "specification_discussion_requested"
)

var AllInvocationIntentKinds = []InvocationIntentKind{
	AgentInvocationRequestedKind,
	ProductionInvocationRequestedKind,
	SpecificationInvocationRequestedKind,
	SpecificationDiscussionRequestedKind,
}

func (k InvocationIntentKind) valid() bool {
	switch k {
	case AgentInvocationRequestedKind, ProductionInvocationRequestedKind,
		SpecificationInvocationRequestedKind, SpecificationDiscussionRequestedKind:
		return true
	default:
		return false
	}
}

// InvocationDispatchIntent is the store-independent part of an outbox row.
// Keeping this boundary in domain lets projection readers authenticate an
// already-dispatched intent without importing an execution lane.
type InvocationDispatchIntent struct {
	Kind           string
	IdempotencyKey string
	Payload        []byte
}

// ConversationInvocationIntent is the durable payload of a conversation-lane
// invocation request. Its binding to a run is reconstructed by the reader
// through the named invocation and attention item.
type ConversationInvocationIntent struct {
	InvocationID   InvocationID   `json:"invocation_id"`
	ConversationID ConversationID `json:"conversation_id"`
	ItemID         ItemID         `json:"item_id"`
	ItemVersion    int            `json:"item_version"`
}

const SpecificationDiscussionInvocationIntentVersion = "freeside.specification-discussion-request/v1"

// SpecificationDiscussionInvocationIntent is the complete signed dispatch
// intent for one provider-only specification discussion. Readers reconstruct
// every authority-bearing field before trusting a started attempt.
type SpecificationDiscussionInvocationIntent struct {
	Version             string         `json:"version"`
	SpecificationRunID  RunID          `json:"specification_run_id"`
	ImplementationRunID RunID          `json:"implementation_run_id"`
	ProjectID           ProjectID      `json:"project_id"`
	Iteration           int            `json:"iteration"`
	InvocationID        InvocationID   `json:"invocation_id"`
	DiscussInvocationID InvocationID   `json:"discuss_invocation_id"`
	ConversationID      ConversationID `json:"conversation_id"`
	ThroughSequence     int            `json:"through_sequence"`
	PrefixDigest        Digest         `json:"prefix_digest"`
	ItemID              ItemID         `json:"item_id"`
	ItemVersion         int            `json:"item_version"`
	InputArtifactIDs    []ArtifactID   `json:"input_artifact_ids"`
	SpecArtifactID      ArtifactID     `json:"spec_artifact_id"`
	PolicyArtifactID    ArtifactID     `json:"policy_artifact_id"`
}

func (r SpecificationDiscussionInvocationIntent) Validate() error {
	if !strings.HasPrefix(string(r.DiscussInvocationID), "inv-") {
		return fmt.Errorf("invalid specification discussion invocation identity: %w", ErrParentKeyMismatch)
	}
	commandID := strings.TrimPrefix(string(r.DiscussInvocationID), "inv-")
	discussionArtifactID := ArtifactID("spec-discussion-" + commandID)
	if r.Version != SpecificationDiscussionInvocationIntentVersion || r.SpecificationRunID == "" ||
		r.ImplementationRunID == "" || r.ProjectID == "" || r.Iteration < 1 || commandID == "" ||
		!specificationDiscussionInvocationIDMatches(r.InvocationID, commandID) || r.ConversationID == "" ||
		r.ThroughSequence < 1 || r.ItemID == "" || r.ItemVersion < 2 || r.SpecArtifactID == "" ||
		r.PolicyArtifactID == "" || !contentaddr.Valid(string(r.PrefixDigest)) || len(r.InputArtifactIDs) == 0 {
		return fmt.Errorf("invalid specification discussion request identity: %w", ErrParentKeyMismatch)
	}
	seen := make(map[ArtifactID]struct{}, len(r.InputArtifactIDs))
	for _, id := range r.InputArtifactIDs {
		if id == "" {
			return ErrEmptyID
		}
		if _, duplicate := seen[id]; duplicate {
			return ErrDuplicate
		}
		seen[id] = struct{}{}
	}
	if _, ok := seen[r.SpecArtifactID]; !ok {
		return fmt.Errorf("discussion specification is not an invocation input: %w", ErrParentKeyMismatch)
	}
	if _, ok := seen[discussionArtifactID]; !ok {
		return fmt.Errorf("discussion context is not an invocation input: %w", ErrParentKeyMismatch)
	}
	return nil
}

func DecodeSpecificationDiscussionInvocationIntent(payload []byte) (SpecificationDiscussionInvocationIntent, error) {
	var request SpecificationDiscussionInvocationIntent
	if err := strictjson.Decode(payload, &request, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		return SpecificationDiscussionInvocationIntent{}, fmt.Errorf("decode specification discussion invocation intent: %w", err)
	}
	if err := request.Validate(); err != nil {
		return SpecificationDiscussionInvocationIntent{}, err
	}
	return request, nil
}

func DecodeConversationInvocationIntent(payload []byte) (ConversationInvocationIntent, error) {
	var request ConversationInvocationIntent
	if err := strictjson.Decode(payload, &request, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		return ConversationInvocationIntent{}, fmt.Errorf("decode conversation invocation intent: %w", err)
	}
	if request.InvocationID == "" || request.ConversationID == "" || request.ItemID == "" || request.ItemVersion < 1 {
		return ConversationInvocationIntent{}, fmt.Errorf("conversation invocation intent has incomplete identity: %w", ErrParentKeyMismatch)
	}
	return request, nil
}

// AuthenticateInvocationDispatchIntent proves that a dispatched outbox row
// names the supplied invocation and its run/stage binding. Conversation
// intents do not encode a run or stage, so their durable admission supplies
// that second half of the binding at the caller.
func AuthenticateInvocationDispatchIntent(
	entry InvocationDispatchIntent, invocation InvocationID, runID RunID, stageID StageID,
) error {
	if entry.IdempotencyKey != string(invocation) {
		return fmt.Errorf("intent key %q does not name invocation %q: %w",
			entry.IdempotencyKey, invocation, ErrParentKeyMismatch)
	}
	switch InvocationIntentKind(entry.Kind) {
	case AgentInvocationRequestedKind:
		request, err := DecodeConversationInvocationIntent(entry.Payload)
		if err != nil {
			return err
		}
		if request.InvocationID != invocation {
			return fmt.Errorf("conversation invocation intent does not bind invocation %q: %w", invocation, ErrParentKeyMismatch)
		}
		return nil
	case ProductionInvocationRequestedKind:
		var request struct {
			InvocationID InvocationID `json:"invocation_id"`
			RunID        RunID        `json:"run_id"`
			StageID      StageID      `json:"stage_id"`
		}
		if err := strictjson.DecodeAllowingUnknownFields(entry.Payload, &request, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
			return fmt.Errorf("decode production invocation intent: %w", err)
		}
		if request.InvocationID != invocation || request.RunID != runID || request.StageID != stageID {
			return fmt.Errorf("production invocation intent does not bind run %q stage %q: %w", runID, stageID, ErrParentKeyMismatch)
		}
		return nil
	case SpecificationInvocationRequestedKind:
		var request struct {
			SpecificationRunID RunID        `json:"specification_run_id"`
			InvocationID       InvocationID `json:"invocation_id"`
		}
		if err := strictjson.DecodeAllowingUnknownFields(entry.Payload, &request, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
			return fmt.Errorf("decode specification invocation intent: %w", err)
		}
		if request.InvocationID != invocation || request.SpecificationRunID != runID ||
			stageID != SpecificationStageID(runID) {
			return fmt.Errorf("specification invocation intent does not bind run %q stage %q: %w", runID, stageID, ErrParentKeyMismatch)
		}
		return nil
	case SpecificationDiscussionRequestedKind:
		request, err := DecodeSpecificationDiscussionInvocationIntent(entry.Payload)
		if err != nil {
			return err
		}
		if request.InvocationID != invocation || request.SpecificationRunID != runID ||
			stageID != SpecificationStageID(runID) {
			return fmt.Errorf("specification discussion intent does not bind run %q stage %q: %w", runID, stageID, ErrParentKeyMismatch)
		}
		return nil
	}
	return fmt.Errorf("intent %q has non-execution kind %q: %w", entry.IdempotencyKey, entry.Kind, ErrParentKeyMismatch)
}
