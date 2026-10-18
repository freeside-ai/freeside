package engine

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// parkedDisputeRow reconciles one review round with the given adjudicator
// answer and returns the round's dispute row and stored artifact.
func parkedDisputeRow(
	t *testing.T, f *findingAdjudicationFixture, answer *domain.FindingAdjudicationEntry,
) (domain.DisputedFinding, *domain.FindingAdjudication) {
	t.Helper()
	if answer == nil {
		f.workflow.findingAdjudicator = nil
	} else {
		f.workflow.findingAdjudicator = &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{*answer}}
	}
	state, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPending {
		t.Fatalf("park = %v, %v", state, err)
	}
	var item domain.AttentionItem
	var artifact *domain.FindingAdjudication
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(f.ctx, productionReviewItemID(f.task.RunID, 1))
		if err != nil {
			return err
		}
		got, err := tx.GetFindingAdjudicationForRound(f.ctx, f.task.RunID, 1)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		artifact = &got
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if item.Type != domain.AttentionReviewDispute || item.ReviewDispute == nil || len(item.ReviewDispute.Findings) != 1 {
		t.Fatalf("item = %#v, want one disputed finding", item)
	}
	return item.ReviewDispute.Findings[0], artifact
}

// TestReviewDisputeCarriesAdjudicatorLean pins the #2059 block on each case a
// dispute is raised from: a demoted answer shows its lean below the
// threshold, a ceiling-held answer shows its accepted lean, and a batch with
// no adjudication shows none.
func TestReviewDisputeCarriesAdjudicatorLean(t *testing.T) {
	inScope := &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1}
	outOfScope := &domain.FindingLocation{Path: "app/a.go", StartLine: 1, EndLine: 1}

	t.Run("demoted remediation in scope", func(t *testing.T) {
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, inScope, "low", "high")
		f.writePath(t, f.headRoot)
		answer, err := domain.NewEngineModelAdjudicationEntry(f.finding.ID, domain.GoalRequired,
			domain.ConfidenceMedium, "the spec requires this fixed", nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		row, artifact := parkedDisputeRow(t, f, &answer)
		remediate := domain.RouteRemediate
		want := &domain.DisputedFindingAdjudication{
			Lean: &remediate, Confidence: domain.ConfidenceMedium,
			Rationale: "the spec requires this fixed", BelowThreshold: true,
		}
		if !reflect.DeepEqual(row.Adjudication, want) {
			t.Fatalf("block = %+v, want %+v", row.Adjudication, want)
		}
		// The lean is shown, never routed: the stored entry still parks, and the
		// accepted routes dispatch no remediation.
		routes, err := findingRoutesFromDecision(*artifact, nil)
		if err != nil || routes[f.finding.ID] != domain.RouteParkUnknown {
			t.Fatalf("routes = %v, %v; want park_unknown", routes, err)
		}
		if domain.AcceptFindingAdjudication(artifact.Entries).Remediates {
			t.Fatal("demoted lean dispatches remediation")
		}
		if !slices.Equal(row.OfferedRoutes, []domain.AdjudicationRoute{
			domain.RouteRemediate, domain.RouteDecline, domain.RouteDefer,
		}) {
			t.Fatalf("offered routes = %v", row.OfferedRoutes)
		}
	})

	t.Run("demoted park out of scope", func(t *testing.T) {
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, outOfScope, "low", "high")
		f.writePath(t, f.headRoot)
		answer := modelRouteEntry(t, f.finding.ID, domain.RouteParkSeparateWork, domain.ConfidenceLow)
		row, _ := parkedDisputeRow(t, f, &answer)
		if row.Adjudication == nil || row.Adjudication.Lean == nil ||
			*row.Adjudication.Lean != domain.RouteParkSeparateWork || !row.Adjudication.BelowThreshold {
			t.Fatalf("block = %+v, want a below-threshold separate-work lean", row.Adjudication)
		}
		if slices.Contains(row.OfferedRoutes, domain.RouteRemediate) {
			t.Fatalf("out-of-scope finding offers remediation: %v", row.OfferedRoutes)
		}
	})

	t.Run("unknown answer has no lean", func(t *testing.T) {
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, inScope, "low", "high")
		f.writePath(t, f.headRoot)
		answer := modelRouteEntry(t, f.finding.ID, domain.RouteParkUnknown, domain.ConfidenceLow)
		row, _ := parkedDisputeRow(t, f, &answer)
		if row.Adjudication == nil || row.Adjudication.Lean != nil || !row.Adjudication.BelowThreshold {
			t.Fatalf("block = %+v, want no lean below the threshold", row.Adjudication)
		}
	})

	t.Run("ceiling-held decline", func(t *testing.T) {
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP1, inScope, "low", "high")
		f.writePath(t, f.headRoot)
		answer := modelRouteEntry(t, f.finding.ID, domain.RouteDecline, domain.ConfidenceHigh)
		row, _ := parkedDisputeRow(t, f, &answer)
		if row.Adjudication == nil || row.Adjudication.Lean == nil ||
			*row.Adjudication.Lean != domain.RouteDecline || row.Adjudication.BelowThreshold ||
			row.Adjudication.Confidence != domain.ConfidenceHigh {
			t.Fatalf("block = %+v, want an accepted decline lean", row.Adjudication)
		}
	})

	t.Run("no adjudication", func(t *testing.T) {
		f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, inScope, "low", "high")
		f.writePath(t, f.headRoot)
		row, artifact := parkedDisputeRow(t, f, nil)
		if row.Adjudication != nil || artifact != nil {
			t.Fatalf("block = %+v, artifact = %v; want neither", row.Adjudication, artifact)
		}
	})
}

// TestReviewDisputeLeanNeverWidensChoices proves the signet gate still takes
// only an offered route: the row's lean names a route the row does not
// offer, and choosing it is refused.
func TestReviewDisputeLeanNeverWidensChoices(t *testing.T) {
	f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2,
		&domain.FindingLocation{Path: "app/a.go", StartLine: 1, EndLine: 1}, "low", "high")
	f.writePath(t, f.headRoot)
	answer := modelRouteEntry(t, f.finding.ID, domain.RouteParkSeparateWork, domain.ConfidenceLow)
	row, _ := parkedDisputeRow(t, f, &answer)
	var item domain.AttentionItem
	var snapshot store.Snapshot
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		var err error
		item, snapshot, err = tx.GetAttentionItemSnapshot(f.ctx, productionReviewItemID(f.task.RunID, 1))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	submit := func(route domain.AdjudicationRoute) error {
		_, err := f.signet.Submit(f.ctx, signet.ClientCommand{
			CommandID: "lean-" + string(route), DeviceID: "device-test",
			ExpectedEntityVersion: snapshot.EntityVersion,
			Payload: signet.DecisionPayload{
				ItemID: item.ID, Action: domain.ActionAdjudicateFindings, ItemVersion: item.ItemVersion,
				PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
				FindingRoutes: []signet.AlternativeChoice{{FindingID: f.finding.ID, Route: route}},
			},
		})
		return err
	}
	for _, route := range []domain.AdjudicationRoute{*row.Adjudication.Lean, domain.RouteRemediate} {
		if err := submit(route); err == nil {
			t.Fatalf("route %s outside offered_routes accepted", route)
		}
	}
	// The same command with an offered route succeeds, so the refusals above
	// were the route, not the command's other coordinates.
	if err := submit(domain.RouteDecline); err != nil {
		t.Fatalf("offered route refused: %v", err)
	}
}
