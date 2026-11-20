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

// TestFindingAdjudicationAcceptsEachAnswer holds an accepted answer beside
// one that waits for a person. The card lists both, the accepted one with its
// settled route, and nothing is recorded until the person answers; leaving
// the settled row out then records it under the model's own entry. Each
// finding's route takes its own branches, and none depends on the other
// finding's route, so the rows use each accepted route and each waiting
// route once rather than every pairing.
func TestFindingAdjudicationAcceptsEachAnswer(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ acceptedRoute, waitingRoute domain.AdjudicationRoute }{
		{domain.RouteDecline, domain.RouteParkUnknown},
		{domain.RouteDefer, domain.RouteDecline},
		{domain.RouteRemediate, domain.RouteDefer},
	} {
		acceptedRoute, waitingRoute := row.acceptedRoute, row.waitingRoute
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
					rows := item.ReviewDispute.Findings
					if item.Type != domain.AttentionReviewDispute || len(rows) != 2 ||
						rows[0].FindingID != accepted.FindingID || rows[0].SettledRoute == nil || *rows[0].SettledRoute != acceptedRoute ||
						rows[1].FindingID != waiting.FindingID || rows[1].SettledRoute != nil {
						t.Fatalf("binding = %#v", item.ReviewDispute)
					}
					for _, id := range []domain.FindingID{accepted.FindingID, waiting.FindingID} {
						if _, err := tx.GetFindingDisposition(f.ctx, id, 1); !errors.Is(err, store.ErrNotFound) {
							t.Fatalf("outcome for %s recorded while the card is open: %v", id, err)
						}
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
					Action: domain.ActionAdjudicateFindings, FindingRoutes: []signet.FindingRouteChoice{{FindingID: waiting.FindingID, Route: domain.RouteDecline}},
				},
			}
			if _, err := f.signet.Submit(f.ctx, command); err != nil {
				t.Fatal(err)
			}
			later := f.workflow.now().Add(time.Hour)
			f.workflow.now = func() time.Time { return later }
			var first domain.ReviewDispositionRecord
			for pass := range 2 {
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
						t.Fatal("human answer rewrote the kept entry")
					}
					if acceptedRoute == domain.RouteRemediate {
						return nil
					}
					got, err := tx.GetFindingDisposition(f.ctx, accepted.FindingID, 1)
					if err != nil {
						return err
					}
					if got.AdjudicationDigest != artifact.Digest || artifact.Decision == nil || !got.CreatedAt.Equal(artifact.CreatedAt) {
						t.Fatalf("kept outcome = %#v, want the answered artifact %s", got, artifact.Digest)
					}
					if pass == 0 {
						first = got
					} else if !reflect.DeepEqual(first, got) {
						t.Fatal("replay changed outcome")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestFindingAdjudicationRebuildsUnacceptedRequiredAnswer(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
					for _, id := range []domain.FindingID{f.finding.ID, "finding-minor"} {
						if _, err := tx.GetFindingDisposition(f.ctx, id, 1); !errors.Is(err, store.ErrNotFound) {
							t.Fatalf("outcome for %s recorded while the card is open: %v", id, err)
						}
					}
					item, err := tx.GetAttentionItem(f.ctx, productionReviewItemID(f.task.RunID, 1))
					if err != nil {
						return err
					}
					rows := item.ReviewDispute.Findings
					if len(rows) != 2 || rows[0].FindingID != f.finding.ID || rows[0].SettledRoute != nil ||
						rows[1].FindingID != "finding-minor" || rows[1].SettledRoute == nil || *rows[1].SettledRoute != domain.RouteDecline {
						t.Fatalf("dispute did not hold the ceiling-held finding for a person: %#v", rows)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// TestFindingAdjudicationLeavesRecordedOutcomesOffTheCard: a round an
// earlier build began recorded its settled outcomes before raising the card.
// Such a finding is not offered as settled, since an override could no longer
// change its recorded outcome or the reply already posted.
func TestFindingAdjudicationLeavesRecordedOutcomesOffTheCard(t *testing.T) {
	t.Parallel()
	f := newFindingAdjudicationFixtureWithOptions(t, domain.FindingSeverityP1, nil, "low", "high",
		findingAdjudicationFixtureOptions{extraFindingIDs: []domain.FindingID{"finding-minor"}, extraFindingSeverity: domain.FindingSeverityP2})
	f.workflow.findingAdjudicator = &scriptedFindingAdjudicator{entries: []domain.FindingAdjudicationEntry{
		modelRouteEntry(t, f.finding.ID, domain.RouteDecline, domain.ConfidenceHigh),
		modelRouteEntry(t, "finding-minor", domain.RouteDecline, domain.ConfidenceHigh),
	}}
	if _, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot); err != nil {
		t.Fatal(err)
	}
	var artifact domain.FindingAdjudication
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		latest, err := tx.LatestModelFindingAdjudication(f.ctx, f.task.RunID, 1)
		if err != nil {
			return err
		}
		artifact = latest
		return persistFindingRouteDispositions(f.ctx, tx, artifact,
			map[domain.FindingID]domain.AdjudicationRoute{"finding-minor": domain.RouteDecline}, artifact.CreatedAt)
	}); err != nil {
		t.Fatal(err)
	}
	split, err := f.workflow.pendingAdjudicatedFindings(f.ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(split.mustAnswer, []domain.FindingID{f.finding.ID}) || len(split.settled) != 0 {
		t.Fatalf("split = %#v, want only the held finding", split)
	}
}

func TestFindingAdjudicationFailedCallStillParksWholeBatch(t *testing.T) {
	t.Parallel()
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
