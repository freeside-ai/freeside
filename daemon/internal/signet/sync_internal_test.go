package signet

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestPublicationAuthorityExclusivityRejectsReadyAndBlocked(t *testing.T) {
	err := validatePublicationAuthorityExclusivity("run-1", true, true)
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("validatePublicationAuthorityExclusivity() = %v, want ErrParentKeyMismatch", err)
	}
}

func TestTerminalObservationRejectsAContradictoryLiveStatus(t *testing.T) {
	err := validateTerminalObservation(domain.InvocationObservation{
		InvocationID: "invocation-1", Status: domain.ObservedStatusRunning,
	}, map[domain.InvocationID]domain.ObservedInvocationStatus{
		"invocation-1": domain.ObservedStatusFailed,
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("validateTerminalObservation() = %v, want ErrParentKeyMismatch", err)
	}
}
