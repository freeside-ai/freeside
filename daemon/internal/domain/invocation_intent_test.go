package domain

import (
	"errors"
	"testing"
)

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
			name: "elaboration",
			entry: InvocationDispatchIntent{
				Kind:           string(ElaborationInvocationRequestedKind),
				IdempotencyKey: string(invocation),
				Payload:        []byte(`{"invocation_id":"inv-1","elaboration_run_id":"run-1"}`),
			},
			stageID: "elaborate-run-1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := AuthenticateInvocationDispatchIntent(tc.entry, invocation, runID, tc.stageID); err != nil {
				t.Fatalf("AuthenticateInvocationDispatchIntent() = %v", err)
			}
		})
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
