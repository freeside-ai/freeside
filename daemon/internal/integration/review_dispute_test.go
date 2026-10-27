package integration_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The external cycle's production ending stays in #1989. Seed its decision
// surface after the review record and before that ending, proving #1016's
// external admission, human artifact, continuation, and producer gates together.
func TestExternalReviewDisputeHumanDecisionSurvivesRestart(t *testing.T) {
	t.Parallel()
	for _, route := range []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteRemediate} {
		t.Run(string(route), func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			p.configureAdjudicator(t)
			_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")
			finding := findings[0]
			p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
			p.workflow = p.newEngine(t, productionCrashSeams{transitionHook: func(transition engine.DurableTransition, side engine.DurableTransitionSide) error {
				if transition == engine.DurableTransitionReviewResult && side == engine.DurableTransitionAfter {
					return errors.New("pause after durable review")
				}
				return nil
			}}, true)
			if _, err := p.reconcileLanes(); err == nil {
				t.Fatal("review boundary did not interrupt")
			}
			var record domain.ReviewRecord
			var policy domain.ResolvedPolicy
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				var err error
				record, err = tx.GetReviewRecord(p.ctx, engine.ProductionReviewInvocationID(p.runID, 2))
				if err != nil {
					return err
				}
				policy, err = tx.GetResolvedPolicy(p.ctx, p.runID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			runID := p.runID
			item, err := domain.NewAttentionItem(domain.AttentionItemInput{
				ID: domain.ProductionFindingAdjudicationItemID(runID, 2, 1), ProjectID: p.projectID,
				Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
				Type:    domain.AttentionReviewDispute, Priority: domain.PriorityHigh, Reason: "Choose a route for the admitted external finding.",
				RequestedDecision: []domain.Action{domain.ActionAdjudicateFindings, domain.ActionDiscuss, domain.ActionStop},
				ReviewDispute: &domain.ReviewDisputeBinding{
					RunID: runID, Round: 2, FindingIDs: []domain.FindingID{finding.ID}, CompletionEvidence: record.CompletionEvidence,
					Findings: []domain.DisputedFinding{{
						FindingID: finding.ID, Message: finding.Message, Location: finding.Location, Severity: finding.Severity,
						External:      &domain.DisputedFindingExternal{ReviewerLogin: finding.External.ReviewerLogin, ThreadID: finding.External.ThreadID},
						OfferedRoutes: domain.ReviewDisputeRoutes(finding, domain.CanonicalDeclaredPaths(policy)),
					}},
				},
				PRHeadSHA: record.HeadSHA, ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
			}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.attention.PutItem(p.ctx, item); err != nil {
				t.Fatal(err)
			}
			if err := p.store.Write(p.ctx, func(tx *store.WriteTx) error {
				return tx.PutDevice(p.ctx, domain.Device{ID: "human-device", DisplayName: "Human decision device", Status: domain.DeviceActive, PairedAt: p.now})
			}); err != nil {
				t.Fatal(err)
			}
			snapshot, err := p.attention.GetAttentionItem(p.ctx, item.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := p.attention.Submit(p.ctx, signet.ClientCommand{
				CommandID: "external-human-decision", DeviceID: "human-device", ExpectedEntityVersion: snapshot.EntityVersion,
				Payload: signet.DecisionPayload{
					ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests, Action: domain.ActionAdjudicateFindings,
					FindingRoutes: []signet.FindingRouteChoice{externalChoice(finding.ID, route)},
				},
			}); err != nil {
				t.Fatal(err)
			}
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			var digest domain.Digest
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				artifact, err := tx.GetFindingAdjudicationForRound(p.ctx, runID, 2)
				if err != nil {
					return err
				}
				if artifact.Revision != 1 || artifact.Decision == nil || artifact.Entries[0].Route != route {
					t.Fatalf("artifact = %#v", artifact)
				}
				digest = artifact.Digest
				if route == domain.RouteRemediate {
					_, err = tx.GetOutbox(p.ctx, "inv-remediate-2-"+string(runID))
					return err
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if route == domain.RouteDecline {
				dispositions := p.externalDispositions(t)
				if len(dispositions) != 1 || dispositions[0].Disposition != domain.ReviewDispositionDeclined || dispositions[0].AdjudicationDigest == nil || *dispositions[0].AdjudicationDigest != digest {
					t.Fatalf("dispositions = %#v", dispositions)
				}
			}
		})
	}
}
