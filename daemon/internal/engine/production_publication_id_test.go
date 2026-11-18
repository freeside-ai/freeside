package engine

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestProductionReviewHardLimitItemIDPreservesLegacyAndSeparatesRecovery(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// Production's ready and blocked item IDs hash the retired fake lane's task
// kind, so every stored item is keyed by these exact strings. The expected
// IDs were copied from the functions before the lane's codec was deleted;
// a change here orphans every stored production ready and blocked item.
func TestFakePublicationTerminalItemIDsUseWorkflowNamespace(t *testing.T) {
	t.Parallel()
	if got, want := readyItemID("run-1"), domain.ItemID(
		"fake-publication-ready-92f7db10592864199ce732ead6522282cdb21a91b9763c8b80ca11425631d21f",
	); got != want {
		t.Fatalf("readyItemID(run-1) = %q, want %q", got, want)
	}
	if got, want := blockedItemID("run-1"), domain.ItemID(
		"fake-publication-blocked-2caaee1e0f1b2cdd71018066c1e281e38b405e3dcf41a4d3bdcb5946d7d210f4",
	); got != want {
		t.Fatalf("blockedItemID(run-1) = %q, want %q", got, want)
	}
	ids := []struct {
		id     domain.ItemID
		prefix string
	}{
		{readyItemID("run-1"), "fake-publication-ready-"},
		{blockedItemID("run-1"), "fake-publication-blocked-"},
		{readyItemID("run-2"), "fake-publication-ready-"},
		{blockedItemID("run-2"), "fake-publication-blocked-"},
	}
	seen := map[domain.ItemID]bool{}
	for _, item := range ids {
		if !strings.HasPrefix(string(item.id), item.prefix) {
			t.Fatalf("terminal item id %q is outside namespace %q", item.id, item.prefix)
		}
		if seen[item.id] {
			t.Fatalf("terminal item id collision: %q", item.id)
		}
		seen[item.id] = true
	}
}
