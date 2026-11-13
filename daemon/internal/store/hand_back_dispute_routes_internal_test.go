package store

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// disputeOverExternal builds a review dispute for round that lists the
// round's own finding and one admitted external finding, whose row offers
// routes. Every other field is what the engine writes.
func disputeOverExternal(
	t *testing.T, f externalReviewFixture, round int, internal, external domain.Finding,
	routes []domain.AdjudicationRoute,
) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := f.st.Read(t.Context(), func(tx *ReadTx) error {
		record, err := tx.reviewRecordForRound(t.Context(), f.run.ID, round)
		if err != nil {
			return err
		}
		policy, err := tx.GetResolvedPolicy(t.Context(), f.run.ID)
		if err != nil {
			return err
		}
		binding := &domain.ReviewDisputeBinding{
			RunID: f.run.ID, Round: round,
			FindingIDs: []domain.FindingID{internal.ID, external.ID}, CompletionEvidence: record.CompletionEvidence,
			Findings: []domain.DisputedFinding{{
				FindingID: internal.ID, Message: internal.Message, Location: internal.Location, Severity: internal.Severity,
				OfferedRoutes: domain.ReviewDisputeRoutes(internal, domain.CanonicalDeclaredPaths(policy), false),
			}, {
				FindingID: external.ID, Message: external.Message, Location: external.Location, Severity: external.Severity,
				External:      &domain.DisputedFindingExternal{ReviewerLogin: external.External.ReviewerLogin, ThreadID: external.External.ThreadID},
				OfferedRoutes: routes,
			}},
		}
		item, err = domain.NewAttentionItem(domain.AttentionItemInput{
			ID:        domain.ProductionFindingAdjudicationItemID(f.run.ID, round, 1),
			ProjectID: f.run.ProjectID, Subject: f.item.Subject,
			Type: domain.AttentionReviewDispute, Priority: domain.PriorityNormal, Reason: "choose routes",
			RequestedDecision: []domain.Action{domain.ActionAdjudicateFindings, domain.ActionStop},
			PRHeadSHA:         record.HeadSHA, ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
			Status: domain.StatusOpen, ReviewDispute: binding,
		}, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

// TestDisputeOffersAFixOnlyForTheHandBackComment is the store's re-derivation
// of the one widened route (issue #3160): a dispute row for the locationless
// comment a hand_back authority consumed must offer a fix while the run
// declares a path, and the same comment under an external_review authority,
// or under a run that declares none, must not. A row that disagrees either
// way is refused, so a forged fix route never reaches a person's card and a
// dropped one is not silently kept.
func TestDisputeOffersAFixOnlyForTheHandBackComment(t *testing.T) {
	t.Parallel()
	fix := []domain.AdjudicationRoute{domain.RouteRemediate, domain.RouteDecline, domain.RouteDefer}
	reply := []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer}
	for _, tc := range []struct {
		name     string
		handBack bool
		paths    string
		routes   []domain.AdjudicationRoute
		valid    bool
	}{
		{"hand_back offers a fix", true, "daemon/**", fix, true},
		{"hand_back drops the fix", true, "daemon/**", reply, false},
		{"hand_back with no declared path offers a fix", true, "", fix, false},
		{"hand_back with no declared path offers a reply", true, "", reply, true},
		{"external_review offers a fix", false, "daemon/**", fix, false},
		{"external_review offers a reply", false, "daemon/**", reply, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var (
				f        externalReviewFixture
				internal domain.Finding
				external domain.Finding
			)
			if tc.handBack {
				back := seedHandBack(t, handBackOptions{paths: tc.paths})
				if err := back.record(t, back.authority); err != nil {
					t.Fatal(err)
				}
				internal = cycleInternalFinding("finding-hand-back-internal", back.run.ID)
				putCycleReview(t, back.st, back.run.ID, back.authority.ReviewRound,
					back.authority.Reentry.BaseSHA, back.authority.Reentry.HeadSHA, internal)
				f, external = back.externalReviewFixture, back.comment
			} else {
				f = seedExternalReview(t, externalReviewOptions{paths: tc.paths})
				if err := f.record(t, f.authority); err != nil {
					t.Fatal(err)
				}
				internal = cycleInternalFinding("finding-cycle-internal", f.run.ID)
				putCycleReview(t, f.st, f.run.ID, f.authority.ReviewRound,
					f.authority.Reentry.BaseSHA, f.authority.Reentry.HeadSHA, internal)
				external = f.finding
			}
			if external.Location != nil {
				t.Fatal("the fixture's comment has a location")
			}
			item := disputeOverExternal(t, f, f.authority.ReviewRound, internal, external, tc.routes)
			err := f.st.Write(t.Context(), func(tx *WriteTx) error { return tx.PutAttentionItem(t.Context(), item) })
			if tc.valid && err != nil {
				t.Fatal(err)
			}
			if !tc.valid && !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("forged routes %v = %v, want a mismatch", tc.routes, err)
			}
		})
	}
}
