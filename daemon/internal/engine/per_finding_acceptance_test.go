package engine

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestFindingAdjudicationAcceptsEachAnswer(t *testing.T) {
	for _, acceptedRoute := range []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer, domain.RouteRemediate} {
		for _, waitingRoute := range []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer, domain.RouteParkUnknown} {
			t.Run(string(acceptedRoute)+"/"+string(waitingRoute), func(t *testing.T) {
				f := newFindingAdjudicationFixtureWithOptions(t, domain.FindingSeverityP2,
					&domain.FindingLocation{Path: "daemon/a.go"}, "low", "high",
					findingAdjudicationFixtureOptions{extraFindingIDs: []domain.FindingID{"finding-wait"}})
				f.writePath(t, f.headRoot)
				var accepted domain.FindingAdjudicationEntry
				if acceptedRoute == domain.RouteRemediate {
					var err error
					accepted, err = domain.NewEngineModelAdjudicationEntry(f.finding.ID, domain.GoalRequired, domain.ConfidenceHigh, "fix in scope", nil, nil, nil, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					accepted = modelRouteEntry(t, f.finding.ID, acceptedRoute, domain.ConfidenceHigh)
				}
				waiting := modelRouteEntry(t, "finding-wait", waitingRoute, domain.ConfidenceMedium)
				f.workflow.findingAdjudicator = &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{accepted, waiting}}
				var before domain.ReviewDispositionRecord
				var item domain.AttentionItem
				var snapshot store.Snapshot
				for pass := range 2 {
					state, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
					if err != nil || state != productionReviewPending {
						t.Fatalf("pass %d: state=%v error=%v", pass, state, err)
					}
					if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
						artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.task.RunID, 1)
						if err != nil {
							return err
						}
						if len(artifact.Entries) != 2 {
							t.Fatal("lost an answer")
						}
						var getErr error
						item, snapshot, getErr = tx.GetAttentionItemSnapshot(f.ctx, productionReviewItemID(f.task.RunID, 1))
						if getErr != nil {
							return getErr
						}
						if item.Type != domain.AttentionReviewDispute || len(item.ReviewDispute.Findings) != 1 || item.ReviewDispute.Findings[0].FindingID != waiting.FindingID || len(item.ReviewDispute.FindingIDs) != 2 {
							t.Fatalf("binding = %#v", item.ReviewDispute)
						}
						if _, err := tx.GetFindingDisposition(f.ctx, waiting.FindingID, 1); !errors.Is(err, store.ErrNotFound) {
							t.Fatalf("waiting disposition = %v", err)
						}
						got, err := tx.GetFindingDisposition(f.ctx, accepted.FindingID, 1)
						if acceptedRoute == domain.RouteRemediate {
							if !errors.Is(err, store.ErrNotFound) {
								t.Fatalf("premature fix = %v", err)
							}
							return nil
						}
						if err != nil {
							return err
						}
						if pass == 0 {
							before = got
						} else if !reflect.DeepEqual(before, got) {
							t.Fatal("replay changed outcome")
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					dispatched, err := f.workflow.remediationDispatched(f.ctx, f.task, f.record)
					if err != nil || dispatched {
						t.Fatalf("premature remediation = %t, %v", dispatched, err)
					}
				}
				command := signet.ClientCommand{
					CommandID: "answer-waiting", DeviceID: "device-test", ExpectedEntityVersion: snapshot.EntityVersion,
					Payload: signet.DecisionPayload{
						ItemID: item.ID, ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests,
						Action: domain.ActionAdjudicateFindings, FindingRoutes: []signet.AlternativeChoice{{FindingID: waiting.FindingID, Route: domain.RouteDecline}},
					},
				}
				if _, err := f.signet.Submit(f.ctx, command); err != nil {
					t.Fatal(err)
				}
				later := f.workflow.now().Add(time.Hour)
				f.workflow.now = func() time.Time { return later }
				for range 2 {
					state, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
					if err != nil {
						t.Fatal(err)
					}
					if acceptedRoute != domain.RouteRemediate && state != productionReviewPassed {
						t.Fatalf("answered state = %v", state)
					}
					if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
						artifact, err := tx.GetFindingAdjudicationForRound(f.ctx, f.task.RunID, 1)
						if err != nil {
							return err
						}
						index := slices.IndexFunc(artifact.Entries, func(e domain.FindingAdjudicationEntry) bool { return e.FindingID == accepted.FindingID })
						if index < 0 || !reflect.DeepEqual(artifact.Entries[index], accepted) {
							t.Fatal("human answer rewrote accepted entry")
						}
						if acceptedRoute == domain.RouteRemediate {
							return nil
						}
						got, err := tx.GetFindingDisposition(f.ctx, accepted.FindingID, 1)
						if err == nil && !reflect.DeepEqual(before, got) {
							t.Fatal("human answer rewrote recorded outcome")
						}
						return err
					}); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestFindingAdjudicationRebuildsUnacceptedRequiredAnswer(t *testing.T) {
	entry, err := domain.NewEngineModelAdjudicationEntry("finding-a", domain.GoalRequired, domain.ConfidenceLow, "uncertain fix", []string{"evidence"}, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := acceptedModelAdjudicationEntries([]findingAdjudicationInput{{Finding: domain.Finding{ID: entry.FindingID}, Compatibility: domain.CompatibilityAllowed}}, []domain.FindingAdjudicationEntry{entry}, domain.DispatchThresholdHigh)
	if err != nil {
		t.Fatal(err)
	}
	got := entries[0]
	if got.Route != domain.RouteParkUnknown || got.Producer != domain.AdjudicationProducerModel || got.Compatibility == nil || *got.Compatibility != domain.CompatibilityUnknown || *got.Confidence != *entry.Confidence || got.Rationale != entry.Rationale || !slices.Equal(got.Evidence, entry.Evidence) {
		t.Fatalf("rebuilt answer = %#v", got)
	}
	// The lean survives demotion for the operator (#2059) but carries no authority.
	if got.ProposedRoute == nil || *got.ProposedRoute != domain.RouteRemediate {
		t.Fatalf("proposed_route = %v, want remediate", got.ProposedRoute)
	}
}

func TestFindingAdjudicationMixedBatchPreservesSeverityCeiling(t *testing.T) {
	for _, severity := range []domain.FindingSeverity{domain.FindingSeverityP0, domain.FindingSeverityP1} {
		for _, route := range []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer} {
			t.Run(string(severity)+"/"+string(route), func(t *testing.T) {
				f := newFindingAdjudicationFixtureWithOptions(t, severity, nil, "low", "high",
					findingAdjudicationFixtureOptions{extraFindingIDs: []domain.FindingID{"finding-minor"}, extraFindingSeverity: domain.FindingSeverityP2})
				f.workflow.findingAdjudicator = &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{
					modelRouteEntry(t, f.finding.ID, route, domain.ConfidenceHigh),
					modelRouteEntry(t, "finding-minor", domain.RouteDecline, domain.ConfidenceHigh),
				}}
				state, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
				if err != nil || state != productionReviewPending {
					t.Fatalf("state=%v error=%v", state, err)
				}
				if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
					if _, err := tx.GetFindingDisposition(f.ctx, f.finding.ID, 1); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("ceiling-held outcome = %v", err)
					}
					if _, err := tx.GetFindingDisposition(f.ctx, "finding-minor", 1); err != nil {
						return err
					}
					item, err := tx.GetAttentionItem(f.ctx, productionReviewItemID(f.task.RunID, 1))
					if err == nil && (len(item.ReviewDispute.Findings) != 1 || item.ReviewDispute.Findings[0].FindingID != f.finding.ID) {
						t.Fatal("dispute did not isolate ceiling-held finding")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestFindingAdjudicationFailedCallStillParksWholeBatch(t *testing.T) {
	f := newFindingAdjudicationFixtureWithOptions(t, domain.FindingSeverityP2, nil, "low", "high",
		findingAdjudicationFixtureOptions{extraFindingIDs: []domain.FindingID{"finding-b"}})
	f.workflow.findingAdjudicator = &scriptedFindingAdjudicator{err: errors.New("adjudicator unavailable")}
	state, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
	if err != nil || state != productionReviewPending {
		t.Fatalf("state=%v error=%v", state, err)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetFindingAdjudicationForRound(f.ctx, f.task.RunID, 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("failed call stored artifact: %v", err)
		}
		item, err := tx.GetAttentionItem(f.ctx, productionReviewItemID(f.task.RunID, 1))
		if err != nil {
			return err
		}
		if item.ReviewDispute == nil || len(item.ReviewDispute.Findings) != 2 {
			t.Fatalf("failed-call dispute = %#v", item.ReviewDispute)
		}
		for _, id := range f.record.FindingIDs {
			if _, err := tx.GetFindingDisposition(f.ctx, id, 1); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("failed call recorded %s: %v", id, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
