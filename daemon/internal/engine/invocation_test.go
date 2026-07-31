package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestDecodeInvocationRequestRejectsMalformedPayloads(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload string
		wantErr error
	}{
		{"empty", ``, nil},
		{"trailing value", `{"invocation_id":"inv-1","conversation_id":"conv-1","item_id":"item-1","item_version":1} {}`, nil},
		{"unknown field", `{"invocation_id":"inv-1","conversation_id":"conv-1","item_id":"item-1","item_version":1,"run_id":"run-foreign"}`, nil},
		{"missing invocation", `{"conversation_id":"conv-1","item_id":"item-1","item_version":1}`, domain.ErrEmptyID},
		{"missing conversation", `{"invocation_id":"inv-1","item_id":"item-1","item_version":1}`, domain.ErrEmptyID},
		{"missing item", `{"invocation_id":"inv-1","conversation_id":"conv-1","item_version":1}`, domain.ErrEmptyID},
		{"zero item version", `{"invocation_id":"inv-1","conversation_id":"conv-1","item_id":"item-1","item_version":0}`, domain.ErrNonPositive},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeInvocationRequest([]byte(tc.payload))
			if err == nil {
				t.Fatal("decode accepted malformed payload")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("decode error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestDecodeInvocationRequestAcceptsCanonicalPayload(t *testing.T) {
	t.Parallel()
	got, err := decodeInvocationRequest([]byte(
		`{"invocation_id":"inv-1","conversation_id":"conv-1","item_id":"item-1","item_version":2}`,
	))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.InvocationID != "inv-1" || got.ConversationID != "conv-1" ||
		got.ItemID != "item-1" || got.ItemVersion != 2 {
		t.Fatalf("decoded request = %#v", got)
	}
}

func TestUnattendedDispatchRefusalIsAHold(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		&exec.CapabilityRefusal{
			Backend: "claude", Missing: []exec.Capability{exec.CapPostExitExport},
		},
		exec.ErrPreJobRefused,
		store.ErrBackendNotConformant,
		domain.ErrConformanceConfigurationUnbound,
		domain.ErrAdmissionConfigurationMismatch,
		domain.ErrAdmissionExceedsConformance,
	} {
		if !unattendedDispatchRefusal(fmt.Errorf("record admission: %w", err)) {
			t.Errorf("%v was not classified as an unattended hold", err)
		}
	}
	if unattendedDispatchRefusal(errors.New("database is corrupt")) {
		t.Fatal("unrelated correctness error was classified as an operating-state hold")
	}
	if unattendedDispatchRefusal(exec.ErrInputUnavailable) {
		t.Fatal("invocation-specific input hold was classified as a global operating-state hold")
	}
	if !invocationDispatchHold(fmt.Errorf("materialize: %w", exec.ErrInputUnavailable)) {
		t.Fatal("input unavailability was not classified as an invocation-specific hold")
	}
	if invocationDispatchHold(store.ErrBackendNotConformant) {
		t.Fatal("global conformance refusal was classified as invocation-specific")
	}
}

func TestMutableAdmissionPolicyRefusalIsAHold(t *testing.T) {
	t.Parallel()
	for _, err := range []error{
		store.ErrBackendNotConformant,
		domain.ErrConformanceConfigurationUnbound,
		domain.ErrAdmissionConfigurationMismatch,
		domain.ErrAdmissionExceedsConformance,
		domain.ErrUnknownAdmissionFloor,
		domain.ErrCapabilityBelowFloor,
		domain.ErrCredentialModeNotApproved,
		domain.ErrWaiverNotConfigured,
		domain.ErrBackupHealthUnavailable,
		domain.ErrCheckpointNotEncrypted,
		domain.ErrCheckpointNotCurrent,
		domain.ErrArtifactClosureIncomplete,
		domain.ErrRestoreTestStale,
		domain.ErrInvalidBackupHealthStatus,
		store.ErrRepositoryUntrusted,
		publish.ErrJanitorInactive,
		domain.ErrRepositoryIdentityMismatch,
		domain.ErrTrustProfileSuperseded,
	} {
		if !MutableAdmissionPolicyRefusal(fmt.Errorf("accept result: %w", err)) {
			t.Errorf("%v was not classified as mutable policy drift", err)
		}
		if !unattendedDispatchRefusal(fmt.Errorf("record admission: %w", err)) {
			t.Errorf("%v was not classified as a dispatch hold", err)
		}
	}
	if MutableAdmissionPolicyRefusal(errors.New("database is corrupt")) {
		t.Fatal("unrelated correctness error was classified as mutable policy drift")
	}
	for _, err := range []error{
		domain.ErrAdmissionInconsistent,
		domain.ErrWaiverRepositoryMismatch,
		domain.ErrTrustProfileInconsistent,
	} {
		if MutableAdmissionPolicyRefusal(err) {
			t.Errorf("%v was classified as mutable policy drift", err)
		}
	}
}
