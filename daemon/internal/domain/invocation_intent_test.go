package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func validSpecificationDiscussionIntent(t *testing.T) []byte {
	t.Helper()
	payload, err := json.Marshal(SpecificationDiscussionInvocationIntent{
		Version:            SpecificationDiscussionInvocationIntentVersion,
		SpecificationRunID: "run-1", ImplementationRunID: "implementation-1", ProjectID: "project-1",
		Iteration: 1, InvocationID: "specification-discussion-1", DiscussInvocationID: "inv-1",
		ConversationID: "conversation-1", ThroughSequence: 1,
		PrefixDigest: Digest("sha256:" + strings.Repeat("a", 64)), ItemID: "item-1", ItemVersion: 2,
		InputArtifactIDs: []ArtifactID{"spec-1", "spec-discussion-1"},
		SpecArtifactID:   "spec-1", PolicyArtifactID: "policy-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestAuthenticateInvocationDispatchIntentBindsEveryExecutionLane(t *testing.T) {
	invocation := InvocationID("inv-1")
	runID := RunID("run-1")
	cases := []struct {
		name    string
		entry   InvocationDispatchIntent
		stageID StageID
	}{
		{
			name: "conversation",
			entry: InvocationDispatchIntent{
				Kind:           string(AgentInvocationRequestedKind),
				IdempotencyKey: string(invocation),
				Payload:        []byte(`{"invocation_id":"inv-1","conversation_id":"conversation-1","item_id":"item-1","item_version":1}`),
			},
			stageID: "stage-1",
		},
		{
			name: "production",
			entry: InvocationDispatchIntent{
				Kind:           string(ProductionInvocationRequestedKind),
				IdempotencyKey: string(invocation),
				Payload:        []byte(`{"invocation_id":"inv-1","run_id":"run-1","stage_id":"stage-1","publication":{}}`),
			},
			stageID: "stage-1",
		},
		{
			name: "specification",
			entry: InvocationDispatchIntent{
				Kind:           string(SpecificationInvocationRequestedKind),
				IdempotencyKey: string(invocation),
				Payload:        []byte(`{"invocation_id":"inv-1","specification_run_id":"run-1"}`),
			},
			stageID: "specify-run-1",
		},
		{
			name: "specification discussion",
			entry: InvocationDispatchIntent{
				Kind:           string(SpecificationDiscussionRequestedKind),
				IdempotencyKey: "specification-discussion-1",
				Payload:        validSpecificationDiscussionIntent(t),
			},
			stageID: "specify-run-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			invocationID := invocation
			if tc.name == "specification discussion" {
				invocationID = "specification-discussion-1"
			}
			if err := AuthenticateInvocationDispatchIntent(tc.entry, invocationID, runID, tc.stageID); err != nil {
				t.Fatalf("AuthenticateInvocationDispatchIntent() = %v", err)
			}
		})
	}
}

func TestSpecificationDiscussionInvocationIntentRequiresCompleteStrictBinding(t *testing.T) {
	valid := validSpecificationDiscussionIntent(t)
	var request map[string]any
	if err := json.Unmarshal(valid, &request); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"conversation_id", "item_id", "item_version", "prefix_digest", "discuss_invocation_id",
		"through_sequence", "input_artifact_ids", "spec_artifact_id", "policy_artifact_id",
	} {
		t.Run(field, func(t *testing.T) {
			copy := make(map[string]any, len(request))
			for key, value := range request {
				copy[key] = value
			}
			delete(copy, field)
			payload, err := json.Marshal(copy)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeSpecificationDiscussionInvocationIntent(payload); err == nil {
				t.Fatalf("DecodeSpecificationDiscussionInvocationIntent accepted missing %s", field)
			}
		})
	}
	withUnknown := append(valid[:len(valid)-1], []byte(`,"unexpected":true}`)...)
	if _, err := DecodeSpecificationDiscussionInvocationIntent(withUnknown); err == nil {
		t.Fatal("DecodeSpecificationDiscussionInvocationIntent accepted an unknown field")
	}
}

func TestInvocationIntentKindRegistration(t *testing.T) {
	for _, kind := range AllInvocationIntentKinds {
		if !kind.valid() {
			t.Errorf("registered kind %q is invalid", kind)
		}
	}
	if InvocationIntentKind("").valid() || InvocationIntentKind("future_lane").valid() {
		t.Error("unregistered invocation intent kind validates")
	}
}

func TestAuthenticateInvocationDispatchIntentRejectsSubstitutedBindings(t *testing.T) {
	entry := InvocationDispatchIntent{
		Kind: string(ProductionInvocationRequestedKind), IdempotencyKey: "inv-1",
		Payload: []byte(`{"invocation_id":"inv-1","run_id":"other-run","stage_id":"stage-1"}`),
	}
	if err := AuthenticateInvocationDispatchIntent(entry, "inv-1", "run-1", "stage-1"); !errors.Is(err, ErrParentKeyMismatch) {
		t.Fatalf("AuthenticateInvocationDispatchIntent() = %v, want ErrParentKeyMismatch", err)
	}
	entry.Kind = "arbitrary_dispatched_work"
	if err := AuthenticateInvocationDispatchIntent(entry, "inv-1", "run-1", "stage-1"); !errors.Is(err, ErrParentKeyMismatch) {
		t.Fatalf("AuthenticateInvocationDispatchIntent() arbitrary kind = %v, want ErrParentKeyMismatch", err)
	}
}
