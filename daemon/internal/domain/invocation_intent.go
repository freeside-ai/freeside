package domain

import (
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// InvocationIntentKind names a durable dispatch-intent protocol member. The
// zero value is invalid by design.
type InvocationIntentKind string

// Invocation dispatch intent kinds are durable protocol vocabulary shared by
// the lanes that create them and readers that authenticate a started attempt.
const (
	AgentInvocationRequestedKind       InvocationIntentKind = "agent_invocation_requested"
	ProductionInvocationRequestedKind  InvocationIntentKind = "production_invocation_requested"
	ElaborationInvocationRequestedKind InvocationIntentKind = "elaboration_invocation_requested"
)

var AllInvocationIntentKinds = []InvocationIntentKind{
	AgentInvocationRequestedKind,
	ProductionInvocationRequestedKind,
	ElaborationInvocationRequestedKind,
}

func (k InvocationIntentKind) valid() bool {
	switch k {
	case AgentInvocationRequestedKind, ProductionInvocationRequestedKind,
		ElaborationInvocationRequestedKind:
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
	case ElaborationInvocationRequestedKind:
		var request struct {
			ElaborationRunID RunID        `json:"elaboration_run_id"`
			InvocationID     InvocationID `json:"invocation_id"`
		}
		if err := strictjson.DecodeAllowingUnknownFields(entry.Payload, &request, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
			return fmt.Errorf("decode elaboration invocation intent: %w", err)
		}
		if request.InvocationID != invocation || request.ElaborationRunID != runID ||
			stageID != StageID("elaborate-"+string(runID)) {
			return fmt.Errorf("elaboration invocation intent does not bind run %q stage %q: %w", runID, stageID, ErrParentKeyMismatch)
		}
		return nil
	}
	return fmt.Errorf("intent %q has non-execution kind %q: %w", entry.IdempotencyKey, entry.Kind, ErrParentKeyMismatch)
}
