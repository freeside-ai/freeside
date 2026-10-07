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

func TestReadinessReentryExhaustionItemIDSeparatesCyclesAndNamespaces(t *testing.T) {
	successor := domain.PublicationSuccessor{
		RunID: "run", PredecessorItemID: "production-ready-run", ReviewRound: 2,
		Origin: domain.PublicationSuccessorReadinessInvalidation,
	}
	itemID := readinessReentryExhaustionItemID(successor)
	if got := readinessReentryExhaustionItemID(successor); got != itemID {
		t.Fatalf("same cycle changed identity: %q != %q", got, itemID)
	}
	// A later cycle remains distinct even if it starts at the same round.
	successor.PredecessorItemID = "production-ready-reentry-later"
	if got := readinessReentryExhaustionItemID(successor); got == itemID {
		t.Fatalf("later cycle reused exhaustion identity %q", itemID)
	}
	// The fixed prefixes diverge before any run coordinate. Even run IDs
	// that resemble the new namespace cannot enter it through another writer.
	for _, runID := range []domain.RunID{"run", "readiness-review-exhaustion-run", domain.RunID(itemID)} {
		for _, other := range []domain.ItemID{
			productionReviewItemID(runID, 1),
			productionReviewHardLimitItemID(runID, 1, false),
			productionReviewHardLimitItemID(runID, 1, true),
			externalReviewExhaustionItemID(runID, 2),
		} {
			if itemID == other {
				t.Fatalf("readiness exhaustion identity %q collides for run %q", itemID, runID)
			}
		}
	}
}
