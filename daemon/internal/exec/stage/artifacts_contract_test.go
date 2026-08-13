package stage

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/contract"
)

// stubArtifactsHarness binds the in-memory stubArtifacts fake to the shared
// Artifacts.RecordClaims contract, so the fake the stage-driver tests lean on is
// held to the same write-once semantics as the production store adapter.
type stubArtifactsHarness struct{ artifacts *stubArtifacts }

// PrepareInvocation is a no-op: the fake has no foreign key to satisfy.
func (h *stubArtifactsHarness) PrepareInvocation(*testing.T, domain.InvocationID) {}

func (h *stubArtifactsHarness) RecordClaims(
	_ *testing.T, id domain.InvocationID, claims []domain.AgentClaim,
) error {
	return h.artifacts.RecordClaims(context.Background(), id, claims)
}

func (h *stubArtifactsHarness) ReadClaims(
	_ *testing.T, id domain.InvocationID,
) ([]domain.AgentClaim, bool, error) {
	h.artifacts.mu.Lock()
	defer h.artifacts.mu.Unlock()
	claims, ok := h.artifacts.claims[id]
	return claims, ok, nil
}

func (h *stubArtifactsHarness) IsConflict(err error) bool {
	return errors.Is(err, errStubClaimConflict)
}

func TestArtifactsContract(t *testing.T) {
	contract.RunArtifactsContract(t, contract.ArtifactsFactory{
		New: func(*testing.T) contract.ArtifactsHarness {
			return &stubArtifactsHarness{artifacts: newStubArtifacts()}
		},
	})
}

var _ contract.ArtifactsHarness = (*stubArtifactsHarness)(nil)
