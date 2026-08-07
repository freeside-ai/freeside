package engine

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestProductionReviewHardLimitItemIDPreservesLegacyAndSeparatesRecovery(t *testing.T) {
	runID := domain.RunID("run")
	if got, want := productionReviewHardLimitItemID(runID, 1, false),
		productionReviewItemID(runID, 1); got != want {
		t.Fatalf("non-contradiction hard-limit id = %q, want legacy %q", got, want)
	}

	// The first attempted recovery namespace produced exactly this collision:
	// its fixed "exhausted" segment was indistinguishable from a legal RunID
	// in the normal review namespace. The recovered prefix now diverges before
	// either function appends a caller-controlled run coordinate.
	recovered := productionReviewHardLimitItemID(runID, 1, true)
	foreignNormal := productionReviewItemID(domain.RunID("exhausted-run"), 1)
	if recovered == foreignNormal {
		t.Fatalf("recovered hard-limit id %q collides with foreign normal item", recovered)
	}
	if recovered == productionReviewItemID(runID, 1) {
		t.Fatalf("recovered hard-limit id %q reused its contradiction carrier", recovered)
	}
}
