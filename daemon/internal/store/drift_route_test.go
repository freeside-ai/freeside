package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var driftRouteAt = time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)

// driftRouteBaseline is a gate input that routes automatically: round 2's
// over_hardened audit reverses a medium finding round 1 fixed, under a policy
// that routes automatically with rounds to spare.
func driftRouteBaseline(t *testing.T) store.DriftRouteInput {
	t.Helper()
	runID := domain.RunID("run-route")
	fixed := adjudicationFinding("finding-fixed", runID, "daemon/a.go", driftRouteAt)
	fixed.Severity = domain.FindingSeverityP2
	return store.DriftRouteInput{
		Audit: newDriftAudit(t, driftAuditInputFor(runID, 2, domain.DriftVerdictOverHardened, fixed.ID)),
		Record: adjudicationReviewRecord(
			t, runID, 2, []domain.FindingID{"finding-current"}, driftRouteAt),
		Policy: store.ReviewConvergencePolicy{
			HardRoundLimit: 25, DriftAuditAfter: 2, DriftAuditRoute: domain.DriftAuditRouteAuto,
			AdjudicationConfidenceThreshold: domain.DispatchThresholdHigh,
		},
		Findings: map[domain.FindingID]domain.Finding{fixed.ID: fixed},
		Effective: []store.EffectiveFindingDisposition{{
			Stored: domain.ReviewDispositionRecord{
				FindingID: fixed.ID, RunID: runID, Round: 1, Disposition: domain.ReviewDispositionFixed,
				RemediationInvocationID: "review-run-route-2",
			},
			Effective: domain.ReviewDispositionFixed,
		}},
	}
}

// TestEvaluateDriftAuditRoute changes one thing at a time in an input that
// routes automatically, so each case names the one condition that parks it.
func TestEvaluateDriftAuditRoute(t *testing.T) {
	t.Parallel()
	type gate struct{ listValid, roundRemains, auto bool }
	routed := gate{listValid: true, roundRemains: true, auto: true}
	parked := gate{listValid: true, roundRemains: true}
	invalid := gate{roundRemains: true}
	audit := func(mutate func(*domain.DriftAuditInput)) func(*testing.T, *store.DriftRouteInput) {
		return func(t *testing.T, input *store.DriftRouteInput) {
			t.Helper()
			changed := driftAuditInputFor(input.Audit.RunID, 2, domain.DriftVerdictOverHardened, "finding-fixed")
			mutate(&changed)
			input.Audit = newDriftAudit(t, changed)
		}
	}
	severity := func(value domain.FindingSeverity) func(*testing.T, *store.DriftRouteInput) {
		return func(_ *testing.T, input *store.DriftRouteInput) {
			finding := input.Findings["finding-fixed"]
			finding.Severity = value
			input.Findings["finding-fixed"] = finding
		}
	}
	effective := func(mutate func(*store.EffectiveFindingDisposition)) func(*testing.T, *store.DriftRouteInput) {
		return func(_ *testing.T, input *store.DriftRouteInput) { mutate(&input.Effective[0]) }
	}
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *store.DriftRouteInput)
		want   gate
	}{
		{"baseline routes", func(*testing.T, *store.DriftRouteInput) {}, routed},
		{"low severity routes", severity(domain.FindingSeverityP3), routed},
		{
			"medium confidence meets a medium threshold",
			func(t *testing.T, input *store.DriftRouteInput) {
				audit(func(a *domain.DriftAuditInput) { a.Confidence = domain.ConfidenceMedium })(t, input)
				input.Policy.AdjudicationConfidenceThreshold = domain.DispatchThresholdMedium
			},
			routed,
		},

		{"stuck verdict", audit(func(a *domain.DriftAuditInput) {
			a.Verdict, a.Reversals = domain.DriftVerdictStuck, nil
		}), invalid},
		{"converged verdict", audit(func(a *domain.DriftAuditInput) {
			a.Verdict, a.Reversals = domain.DriftVerdictConverged, nil
		}), invalid},
		{"unknown finding", func(_ *testing.T, input *store.DriftRouteInput) {
			delete(input.Findings, "finding-fixed")
		}, invalid},
		{"finding of another run", func(_ *testing.T, input *store.DriftRouteInput) {
			finding := input.Findings["finding-fixed"]
			finding.RunID = "run-elsewhere"
			input.Findings["finding-fixed"] = finding
		}, invalid},
		{"finding in the audited round's own batch", func(_ *testing.T, input *store.DriftRouteInput) {
			input.Record.FindingIDs = append(input.Record.FindingIDs, "finding-fixed")
		}, invalid},
		{"review record of another round", func(_ *testing.T, input *store.DriftRouteInput) {
			input.Record.Round = 3
		}, invalid},
		{"no disposition", func(_ *testing.T, input *store.DriftRouteInput) {
			input.Effective = nil
		}, invalid},
		{"declined disposition", effective(func(d *store.EffectiveFindingDisposition) {
			d.Stored.Disposition, d.Effective = domain.ReviewDispositionDeclined, domain.ReviewDispositionDeclined
		}), invalid},
		{"deferred disposition", effective(func(d *store.EffectiveFindingDisposition) {
			d.Stored.Disposition, d.Effective = domain.ReviewDispositionDeferred, domain.ReviewDispositionDeferred
		}), invalid},
		{"fix already reversed", effective(func(d *store.EffectiveFindingDisposition) {
			d.Effective = domain.ReviewDispositionDeclined
		}), invalid},
		{"disposition written in the audited round", effective(func(d *store.EffectiveFindingDisposition) {
			d.Stored.Round = 2
		}), invalid},
		{"disposition of another run", effective(func(d *store.EffectiveFindingDisposition) {
			d.Stored.RunID = "run-elsewhere"
		}), invalid},
		{
			"one invalid reversal fails the whole list",
			func(t *testing.T, input *store.DriftRouteInput) {
				input.Audit = newDriftAudit(t, driftAuditInputFor(
					input.Audit.RunID, 2, domain.DriftVerdictOverHardened, "finding-fixed", "finding-unknown"))
			},
			invalid,
		},

		{"critical finding", severity(domain.FindingSeverityP0), parked},
		{"high finding", severity(domain.FindingSeverityP1), parked},
		{"missing severity reads as high", severity(""), parked},
		{"confidence below the threshold", audit(func(a *domain.DriftAuditInput) {
			a.Confidence = domain.ConfidenceMedium
		}), parked},
		{
			"low confidence never meets the threshold",
			func(t *testing.T, input *store.DriftRouteInput) {
				audit(func(a *domain.DriftAuditInput) { a.Confidence = domain.ConfidenceLow })(t, input)
				input.Policy.AdjudicationConfidenceThreshold = domain.DispatchThresholdMedium
			},
			parked,
		},
		{"unresolved threshold", func(_ *testing.T, input *store.DriftRouteInput) {
			input.Policy.AdjudicationConfidenceThreshold = ""
		}, parked},
		{"route park", func(_ *testing.T, input *store.DriftRouteInput) {
			input.Policy.DriftAuditRoute = domain.DriftAuditRoutePark
		}, parked},
		{"second over_hardened verdict", func(_ *testing.T, input *store.DriftRouteInput) {
			input.EarlierOverHardened = true
		}, parked},
		{"no round left", func(_ *testing.T, input *store.DriftRouteInput) {
			input.Policy.HardRoundLimit = 2
		}, gate{listValid: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := driftRouteBaseline(t)
			tc.mutate(t, &input)
			got := store.EvaluateDriftAuditRoute(input)
			if (gate{got.ListValid, got.RoundRemains, got.Auto}) != tc.want {
				t.Fatalf("gate = list %t round %t auto %t, want %+v",
					got.ListValid, got.RoundRemains, got.Auto, tc.want)
			}
			if got.ListValid != (len(got.Superseded) == len(input.Audit.Reversals) && len(got.Superseded) > 0) {
				t.Fatalf("superseded = %+v for list valid %t", got.Superseded, got.ListValid)
			}
			if got.ListValid && !reflect.DeepEqual(got.Superseded[0], input.Effective[0].Stored) {
				t.Fatalf("superseded = %+v, want the stored fixed row", got.Superseded)
			}
		})
	}
}

func readDiminishingDecision(st *store.Store, id domain.ItemID) (store.ReviewDiminishingDecision, error) {
	ctx := context.Background()
	var decision store.ReviewDiminishingDecision
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		decision, readErr = tx.ReviewDiminishingDecision(ctx, id)
		return readErr
	})
	return decision, err
}

// TestDriftDiminishingDecisionLoadsByVerdict shows the decision load accepts
// cause drift_audit only for a verdict that parks, and needs the audit.
func TestDriftDiminishingDecisionLoadsByVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		verdict domain.DriftVerdict
		loads   bool
	}{
		{domain.DriftVerdictOverHardened, true},
		{domain.DriftVerdictStuck, true},
		{domain.DriftVerdictConverged, false},
	} {
		t.Run(string(tc.verdict), func(t *testing.T) {
			t.Parallel()
			st, seeded, _ := seedDriftDiminishingDecision(
				t, filepath.Join(t.TempDir(), "store.db"), "run-drift-verdict", driftRouteAt, "",
				driftSeed{verdict: tc.verdict})
			defer func() { _ = st.Close() }()
			decision, err := readDiminishingDecision(st, seeded.Item.ID)
			if !tc.loads {
				if !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("decision on a %s audit = %v, want ErrParentKeyMismatch", tc.verdict, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("decision on a %s audit: %v", tc.verdict, err)
			}
			if decision.Binding.Cause != domain.ReviewDiminishingDriftAudit || decision.Command != nil {
				t.Fatalf("decision = %+v, want an open drift_audit stop", decision)
			}
		})
	}

	t.Run("audit removed", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "store.db")
		seededStore, seeded, audit := seedDriftDiminishingDecision(
			t, path, "run-drift-unaudited", driftRouteAt, "", driftSeed{})
		st := tamperStore(t, path, seededStore,
			`DELETE FROM drift_audits WHERE content_digest = ?`, string(audit.Digest))
		defer func() { _ = st.Close() }()
		_, err := readDiminishingDecision(st, seeded.Item.ID)
		// A missing audit must not read as a missing item.
		if !errors.Is(err, domain.ErrParentKeyMismatch) || errors.Is(err, store.ErrNotFound) {
			t.Fatalf("decision without its audit = %v, want ErrParentKeyMismatch and not ErrNotFound", err)
		}
	})
}

// driftSupersession is the record a routed round writes for the seeded drift
// run's finding-a.
func driftSupersession(
	decision store.ReviewDiminishingDecision, audit domain.DriftAudit,
	authority domain.DispositionSupersessionAuthority,
) domain.FindingDispositionSupersession {
	runID := decision.Binding.RunID
	return domain.FindingDispositionSupersession{
		RunID: runID, ReversingRound: decision.Binding.Round, FindingID: "finding-a", SupersededRound: 1,
		RemediationInvocationID: domain.InvocationID("review-" + string(runID) + "-2"),
		DriftAuditDigest:        audit.Digest, Authority: authority,
		CreatedAt: driftRouteAt.Add(time.Hour),
	}
}

func humanAuthority(decision store.ReviewDiminishingDecision) domain.DispositionSupersessionAuthority {
	return domain.DispositionSupersessionAuthority{
		Kind: domain.DispositionSupersessionHumanCommand,
		Command: &domain.DispositionSupersessionCommand{
			ItemID: decision.Item.ID, ItemVersion: decision.Command.ItemVersion,
			CommandID: decision.Command.CommandID,
		},
	}
}

// TestDispositionSupersessionAutoRouteIsReDerived shows the store accepts the
// automatic authority exactly when the route gate, re-derived from stored
// records, routes the round automatically.
func TestDispositionSupersessionAutoRouteIsReDerived(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	auto := domain.DispositionSupersessionAuthority{Kind: domain.DispositionSupersessionAutoRoute}
	for _, tc := range []struct {
		name   string
		seed   driftSeed
		routes bool
	}{
		{"medium finding routes", driftSeed{severity: domain.FindingSeverityP2}, true},
		{"high finding parks", driftSeed{severity: domain.FindingSeverityP1}, false},
		{"route park parks", driftSeed{
			severity:   domain.FindingSeverityP2,
			policyKeys: map[string]string{"review.drift_audit_route": "park"},
		}, false},
		{"no round left parks", driftSeed{severity: domain.FindingSeverityP2, hardRoundLimit: 2}, false},
		{"invalid list parks", driftSeed{
			severity: domain.FindingSeverityP2, extraReversals: []domain.FindingID{"finding-b"},
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runID := domain.RunID("run-auto-route")
			st, decision, audit := seedDriftDiminishingDecision(
				t, filepath.Join(t.TempDir(), "store.db"), runID, driftRouteAt, "", tc.seed)
			defer func() { _ = st.Close() }()
			record := driftSupersession(decision, audit, auto)
			err := putSupersession(st, record)
			if !tc.routes {
				if !errors.Is(err, store.ErrSupersessionRouteUnproven) {
					t.Fatalf("put = %v, want ErrSupersessionRouteUnproven", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("put: %v", err)
			}
			// The record names the audited round, so the gate that authorized
			// it still derives once the record exists.
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				got, err := tx.GetFindingDispositionSupersession(ctx, "finding-a", 1)
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(got, record) {
					t.Fatalf("read back = %+v, want %+v", got, record)
				}
				gate, err := tx.DriftAuditRouteGate(ctx, runID, decision.Binding.Round)
				if err != nil {
					return err
				}
				if !gate.Auto || len(gate.Superseded) != 1 || gate.Superseded[0].FindingID != "finding-a" {
					t.Fatalf("gate after the record = %+v, want the same automatic route", gate)
				}
				effective, err := tx.EffectiveFindingDispositions(ctx, runID, decision.Binding.Round)
				if err != nil {
					return err
				}
				if len(effective) != 1 || effective[0].Effective != domain.ReviewDispositionDeclined {
					t.Fatalf("effective dispositions = %+v, want finding-a declined", effective)
				}
				return nil
			}); err != nil {
				t.Fatalf("read after the record: %v", err)
			}
		})
	}
}

func TestDriftAuditRouteGateWithoutAnAuditIsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, decision := seedReviewDiminishingDecision(
		t, filepath.Join(t.TempDir(), "store.db"), "run-gate-unaudited", driftRouteAt, "")
	defer func() { _ = st.Close() }()
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.DriftAuditRouteGate(ctx, decision.Binding.RunID, decision.Binding.Round)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("gate for an unaudited round = %v, want ErrNotFound", err)
	}
}

// TestDispositionSupersessionHumanAuthorityNeedsADriftStop shows a
// continue_under_policy on a stop of any other cause orders an ordinary review
// round, never a reversal, even when the round carries an over_hardened audit
// whose list would pass the gate.
func TestDispositionSupersessionHumanAuthorityNeedsADriftStop(t *testing.T) {
	t.Parallel()
	fx := seedDiminishingFacts(
		t, filepath.Join(t.TempDir(), "store.db"), "run-human-other-stop", domain.ActionContinueUnderPolicy)
	defer func() { _ = fx.st.Close() }()
	if fx.decision.Binding.Cause != domain.ReviewDiminishingFixedRecurrence {
		t.Fatalf("seeded cause = %q, want fixed_recurrence", fx.decision.Binding.Cause)
	}
	record := driftSupersession(fx.decision, fx.decisionAudit, humanAuthority(fx.decision))
	err := putSupersession(fx.st, record)
	if !errors.Is(err, domain.ErrParentKeyMismatch) || !strings.Contains(err.Error(), "authority command") {
		t.Fatalf("put = %v, want ErrParentKeyMismatch from the authority command check", err)
	}
}

// TestDispositionSupersessionHumanAuthorityNeedsAValidList shows a command
// orders the round but cannot make an invalid reversal list actionable.
func TestDispositionSupersessionHumanAuthorityNeedsAValidList(t *testing.T) {
	t.Parallel()
	st, decision, audit := seedDriftDiminishingDecision(
		t, filepath.Join(t.TempDir(), "store.db"), "run-human-invalid-list", driftRouteAt,
		domain.ActionContinueUnderPolicy, driftSeed{extraReversals: []domain.FindingID{"finding-b"}})
	defer func() { _ = st.Close() }()
	err := putSupersession(st, driftSupersession(decision, audit, humanAuthority(decision)))
	if !errors.Is(err, store.ErrSupersessionRouteUnproven) {
		t.Fatalf("put = %v, want ErrSupersessionRouteUnproven", err)
	}
}

// TestDispositionSupersessionHumanAuthorityNeedsThePromise shows a continue on
// a drift card that promised no simplification round is not authority for a
// reversal, even when the reversal list would pass the gate.
func TestDispositionSupersessionHumanAuthorityNeedsThePromise(t *testing.T) {
	t.Parallel()
	st, decision, audit := seedDriftDiminishingDecision(
		t, filepath.Join(t.TempDir(), "store.db"), "run-human-no-promise", driftRouteAt,
		domain.ActionContinueUnderPolicy, driftSeed{})
	defer func() { _ = st.Close() }()
	err := putSupersession(st, driftSupersession(decision, audit, humanAuthority(decision)))
	if !errors.Is(err, store.ErrSupersessionRouteUnproven) {
		t.Fatalf("put = %v, want ErrSupersessionRouteUnproven", err)
	}
}

// TestSimplificationPromiseIsReDerived shows the store accepts card facts that
// promise a simplification round only when the route gate derives a valid list
// and a remaining round, on write and on every read.
func TestSimplificationPromiseIsReDerived(t *testing.T) {
	t.Parallel()

	// A decided item carrying the promise reads through both tiers and loads as
	// a decision: its gate reads only records older than its round, never the
	// item it is rebuilding.
	t.Run("proven promise loads", func(t *testing.T) {
		t.Parallel()
		st, seeded, _ := seedDriftDiminishingDecision(
			t, filepath.Join(t.TempDir(), "store.db"), "run-promise", driftRouteAt,
			domain.ActionContinueUnderPolicy, driftSeed{simplification: true})
		defer func() { _ = st.Close() }()
		snapshot, record, err := readItemBothTiers(st, seeded.Item.ID)
		if err != nil {
			t.Fatalf("read the decided item: %v", err)
		}
		for tier, got := range map[string]domain.AttentionItem{"snapshot": snapshot, "record": record} {
			if got.ReviewDiminishing == nil || got.ReviewDiminishing.DriftAudit == nil ||
				!got.ReviewDiminishing.DriftAudit.SimplificationOnContinue {
				t.Fatalf("%s facts = %+v, want the promise", tier, got.ReviewDiminishing)
			}
		}
		decision, err := readDiminishingDecision(st, seeded.Item.ID)
		if err != nil {
			t.Fatalf("decision load: %v", err)
		}
		if decision.Command == nil || decision.Command.Action != domain.ActionContinueUnderPolicy {
			t.Fatalf("decision command = %+v, want continue_under_policy", decision.Command)
		}
	})

	for name, seed := range map[string]driftSeed{
		"invalid list":  {extraReversals: []domain.FindingID{"finding-b"}},
		"no round left": {hardRoundLimit: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st, seeded, audit := seedDriftDiminishingDecision(
				t, filepath.Join(t.TempDir(), "store.db"), "run-promise-refused", driftRouteAt, "", seed)
			defer func() { _ = st.Close() }()
			next := seeded.Item
			facts := driftFactsOf(audit)
			next.ReviewDiminishing = &facts
			next.ItemVersion++
			// The same facts without the promise are accepted: parking is
			// always allowed.
			promised := next
			promisedFacts := driftFactsOf(audit)
			promisedFacts.DriftAudit.SimplificationOnContinue = true
			promised.ReviewDiminishing = &promisedFacts
			if err := tryPutItem(st, promised); !errors.Is(err, store.ErrReviewDiminishingSimplificationUnproven) {
				t.Fatalf("put with the promise = %v, want ErrReviewDiminishingSimplificationUnproven", err)
			}
			if err := tryPutItem(st, next); err != nil {
				t.Fatalf("put without the promise: %v", err)
			}
		})
	}
}

// putReemittedRound records round 3 of the seeded drift run with one finding
// whose fingerprint is finding-a's, the fix round 2's audit reversed.
func putReemittedRound(
	t *testing.T, st *store.Store, decision store.ReviewDiminishingDecision,
) domain.ReviewRecord {
	t.Helper()
	ctx := context.Background()
	runID := decision.Binding.RunID
	at := driftRouteAt.Add(3 * time.Hour)
	original := adjudicationFinding("finding-a", runID, "daemon/a.go", at)
	again := adjudicationFinding("finding-a-again", runID, "daemon/a.go", at)
	again.Message, again.RawText = original.Message, original.RawText
	record := adjudicationReviewRecord(t, runID, 3, []domain.FindingID{again.ID}, at)
	adjudication, err := domain.NewFindingAdjudication(
		runID, 3, adjSpecDigest, record.InstructionDigest, decision.Binding.PolicyDigest,
		[]domain.FindingAdjudicationEntry{adjudicationEngineEntry(t, again.ID)}, "", at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(ctx, record, []domain.Finding{again}); err != nil {
			return err
		}
		return tx.PutFindingAdjudication(ctx, adjudication)
	}); err != nil {
		t.Fatalf("record the re-emitted round: %v", err)
	}
	return record
}

func evaluateConvergence(
	t *testing.T, st *store.Store, record domain.ReviewRecord,
) (domain.ReviewDiminishingCause, bool) {
	t.Helper()
	ctx := context.Background()
	var cause domain.ReviewDiminishingCause
	var stop bool
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ReviewConvergenceStateAtDecision(ctx, record)
		if err != nil {
			return err
		}
		cause, stop, err = store.EvaluateReviewConvergence(state, record)
		return err
	}); err != nil {
		t.Fatalf("evaluate round %d: %v", record.Round, err)
	}
	return cause, stop
}

// TestReversedFixIsNotARecurrence is the supersession rule at the convergence
// boundary: the re-emission of a fix a drift audit reversed is a known decline.
// Without the record the same round stops on fixed_recurrence.
//
// The human-authority case doubles as the loop test. Its record names round
// 2's item, and round 3's rebuild loads round 2's decision, so a rebuild that
// followed the authority would re-enter itself.
func TestReversedFixIsNotARecurrence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	auto := domain.DispositionSupersessionAuthority{Kind: domain.DispositionSupersessionAutoRoute}
	for _, tc := range []struct {
		name      string
		action    domain.Action
		authority func(store.ReviewDiminishingDecision) *domain.DispositionSupersessionAuthority
		wantStop  bool
	}{
		{"no record", "", func(store.ReviewDiminishingDecision) *domain.DispositionSupersessionAuthority {
			return nil
		}, true},
		{
			"continue without a record", domain.ActionContinueUnderPolicy,
			func(store.ReviewDiminishingDecision) *domain.DispositionSupersessionAuthority { return nil }, true,
		},
		{"automatic route", "", func(store.ReviewDiminishingDecision) *domain.DispositionSupersessionAuthority {
			return &auto
		}, false},
		{
			"human command", domain.ActionContinueUnderPolicy,
			func(decision store.ReviewDiminishingDecision) *domain.DispositionSupersessionAuthority {
				authority := humanAuthority(decision)
				return &authority
			}, false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runID := domain.RunID("run-reemitted")
			st, decision, audit := seedDriftDiminishingDecision(
				t, filepath.Join(t.TempDir(), "store.db"), runID, driftRouteAt, tc.action,
				driftSeed{severity: domain.FindingSeverityP2, simplification: true})
			defer func() { _ = st.Close() }()
			authority := tc.authority(decision)
			if authority != nil {
				if err := putSupersession(st, driftSupersession(decision, audit, *authority)); err != nil {
					t.Fatalf("put: %v", err)
				}
			}
			record := putReemittedRound(t, st, decision)

			cause, stop := evaluateConvergence(t, st, record)
			if stop != tc.wantStop || (stop && cause != domain.ReviewDiminishingFixedRecurrence) {
				t.Fatalf("round 3 = %q stop %t, want stop %t", cause, stop, tc.wantStop)
			}

			// Round 2's own rebuild never applies its own record: the stored
			// item still re-derives, and so does every read that follows the
			// record's authority.
			if _, _, err := readItemBothTiers(st, decision.Item.ID); err != nil {
				t.Fatalf("read round 2's item after round 3: %v", err)
			}
			if _, err := readDiminishingDecision(st, decision.Item.ID); err != nil {
				t.Fatalf("round 2's decision after round 3: %v", err)
			}
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				listed, err := tx.ListFindingDispositionSupersessions(ctx, runID)
				if err != nil {
					return err
				}
				if (len(listed) == 1) != (authority != nil) {
					t.Fatalf("supersessions = %+v", listed)
				}
				_, err = tx.EffectiveFindingDispositions(ctx, runID, 3)
				return err
			}); err != nil {
				t.Fatalf("full reads after round 3: %v", err)
			}
		})
	}
}
