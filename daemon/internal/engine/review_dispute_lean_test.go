package engine

import (
	"database/sql"
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

// TestLatestModelAdjudicationChecksSkippedHumanRow proves the read that skips
// a human revision still checks that row's stored columns, so a corrupt one
// fails closed instead of exposing the model revision before it.
func TestLatestModelAdjudicationChecksSkippedHumanRow(t *testing.T) {
	for name, tamper := range map[string]string{
		"predecessor column": `UPDATE finding_adjudications SET predecessor_digest = 'forged' WHERE revision = 2`,
		"spec column":        `UPDATE finding_adjudications SET approved_spec_digest = 'forged' WHERE revision = 2`,
		// A corrupted key column would hide the row from a keyed read.
		"round column": `UPDATE finding_adjudications SET round = 7 WHERE revision = 2`,
		"run column":   `UPDATE finding_adjudications SET run_id = 'run-elsewhere' WHERE revision = 2`,
	} {
		t.Run(name, func(t *testing.T) {
			f, command := parkedReviewDispute(t, true)
			if _, err := f.signet.Submit(f.ctx, command); err != nil {
				t.Fatal(err)
			}
			if _, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot); err != nil {
				t.Fatal(err)
			}
			latest := func() (domain.FindingAdjudication, error) {
				var artifact domain.FindingAdjudication
				err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
					var err error
					artifact, err = tx.LatestModelFindingAdjudication(f.ctx, f.task.RunID, 1)
					return err
				})
				return artifact, err
			}
			// The intact round reads through the human revision 2 to the
			// model's revision 1, so the failure below is the tamper.
			if artifact, err := latest(); err != nil || artifact.Revision != 1 || artifact.Decision != nil {
				t.Fatalf("intact read = revision %d, decision %v, %v; want model revision 1",
					artifact.Revision, artifact.Decision, err)
			}
			db, err := sql.Open("sqlite", f.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.ExecContext(f.ctx, tamper); err != nil {
				t.Fatal(err)
			}
			if _, err := latest(); !errors.Is(err, store.ErrRowInconsistent) {
				t.Fatalf("corrupt human row = %v, want ErrRowInconsistent", err)
			}
		})
	}
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

// TestReviewDisputeGateRederivesAdjudication proves the store gate re-derives
// each stored block on write and on reconstruction: any change to it fails
// closed, and only a row with no block at all passes, as a pre-#2059 row.
func TestReviewDisputeGateRederivesAdjudication(t *testing.T) {
	put := func(f *findingAdjudicationFixture, itemID domain.ItemID, mutate func(*domain.DisputedFinding)) error {
		return f.store.Write(f.ctx, func(tx *store.WriteTx) error {
			item, err := tx.GetAttentionItem(f.ctx, itemID)
			if err != nil {
				return err
			}
			mutate(&item.ReviewDispute.Findings[0])
			item.ItemVersion++
			return tx.PutAttentionItem(f.ctx, item)
		})
	}
	for name, mutate := range map[string]func(*domain.DisputedFinding){
		"lean": func(row *domain.DisputedFinding) {
			lean := domain.RouteRemediate
			block := *row.Adjudication
			block.Lean = &lean
			row.Adjudication = &block
		},
		"no lean": func(row *domain.DisputedFinding) {
			block := *row.Adjudication
			block.Lean = nil
			row.Adjudication = &block
		},
		"confidence": func(row *domain.DisputedFinding) {
			block := *row.Adjudication
			block.Confidence = domain.ConfidenceLow
			row.Adjudication = &block
		},
		"rationale": func(row *domain.DisputedFinding) {
			block := *row.Adjudication
			block.Rationale = "forged"
			row.Adjudication = &block
		},
		"threshold flag": func(row *domain.DisputedFinding) {
			block := *row.Adjudication
			block.BelowThreshold = !block.BelowThreshold
			row.Adjudication = &block
		},
	} {
		t.Run(name, func(t *testing.T) {
			f, command := parkedReviewDispute(t, true)
			if err := put(f, command.Payload.ItemID, mutate); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("tampered block = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
	t.Run("block without adjudication", func(t *testing.T) {
		f, command := parkedReviewDispute(t, false)
		err := put(f, command.Payload.ItemID, func(row *domain.DisputedFinding) {
			row.Adjudication = &domain.DisputedFindingAdjudication{
				Confidence: domain.ConfidenceHigh, Rationale: "invented", BelowThreshold: false,
			}
		})
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("invented block = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("tampered on reconstruction", func(t *testing.T) {
		f, command := parkedReviewDispute(t, true)
		db, err := sql.Open("sqlite", f.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.ExecContext(f.ctx, `UPDATE attention_items
			SET body = json_set(body, '$.review_dispute.findings[0].adjudication.lean', 'remediate') WHERE id = ?`,
			command.Payload.ItemID); err != nil {
			t.Fatal(err)
		}
		err = f.store.Read(f.ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetAttentionItem(f.ctx, command.Payload.ItemID)
			return err
		})
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("tampered stored block = %v, want ErrParentKeyMismatch", err)
		}
	})
	t.Run("legacy row", func(t *testing.T) {
		f, command := parkedReviewDispute(t, true)
		// The binding is immutable once written, so a pre-#2059 row is
		// reproduced in place: its stored body has no block at all.
		db, err := sql.Open("sqlite", f.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if _, err := db.ExecContext(f.ctx, `UPDATE attention_items
			SET body = json_remove(body, '$.review_dispute.findings[0].adjudication') WHERE id = ?`,
			command.Payload.ItemID); err != nil {
			t.Fatal(err)
		}
		var version int
		read := func() domain.AttentionItem {
			var item domain.AttentionItem
			if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
				var err error
				item, err = tx.GetAttentionItem(f.ctx, command.Payload.ItemID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			return item
		}
		version = read().ItemVersion
		// Reconciling again keeps the legacy item as it is rather than
		// rebuilding it under the person deciding it.
		if _, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot); err != nil {
			t.Fatal(err)
		}
		if item := read(); item.ItemVersion != version || item.ReviewDispute.Findings[0].Adjudication != nil {
			t.Fatalf("legacy item rebuilt: version %d -> %d, block %+v",
				version, item.ItemVersion, item.ReviewDispute.Findings[0].Adjudication)
		}
	})
}
