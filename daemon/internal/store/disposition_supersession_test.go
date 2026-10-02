package store_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var supersessionAt = time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)

// supersessionOtherRun is a second run in the same store. It has no
// diminishing-returns item, so its records can pass the scope check and never
// the authority check.
const supersessionOtherRun = domain.RunID("run-supersession-other")

// supersessionFixture is a store holding one valid supersession's parents and
// the record itself, not yet written.
//
// The main run is seedDriftDiminishingDecision's: round 1 lists finding-a,
// whose fixed disposition names round 2's review as its remediation. Round 2's
// audit is over_hardened and reverses finding-a, and round 2's item parks on
// that audit and is concluded by the given action.
//
// The other run has three rounds. Round 1 lists a fixed, a kept (fixed but
// never reversed), a declined, and a deferred finding. Round 2's audit is
// over_hardened, reversing the fixed finding, and round 3's is converged. Its
// reversing round is the main run's, so its record differs from an accepted
// one in the run alone.
type supersessionFixture struct {
	st       *store.Store
	decision store.ReviewDiminishingDecision
	record   domain.FindingDispositionSupersession
	// otherRecord passes the scope check in the other run. Its authority
	// names the main run's item.
	otherRecord    domain.FindingDispositionSupersession
	otherConverged domain.DriftAudit
}

func seedSupersession(
	t *testing.T, path string, runID domain.RunID, action domain.Action,
) supersessionFixture {
	t.Helper()
	ctx := context.Background()
	st, decision, audit := seedDriftDiminishingDecision(t, path, runID, supersessionAt, action, driftSeed{simplification: true})

	other := supersessionOtherRun
	otherFindings := []domain.Finding{
		adjudicationFinding("other-declined", other, "daemon/a.go", supersessionAt),
		adjudicationFinding("other-deferred", other, "daemon/a.go", supersessionAt),
		adjudicationFinding("other-fixed", other, "daemon/a.go", supersessionAt),
		adjudicationFinding("other-kept", other, "daemon/a.go", supersessionAt),
	}
	otherIDs := make([]domain.FindingID, len(otherFindings))
	for i, finding := range otherFindings {
		otherIDs[i] = finding.ID
	}
	otherRounds := []domain.ReviewRecord{
		adjudicationReviewRecord(t, other, 1, otherIDs, supersessionAt),
		adjudicationReviewRecord(t, other, 2,
			[]domain.FindingID{driftRoundFinding(other, 2)}, supersessionAt.Add(time.Minute)),
		adjudicationReviewRecord(t, other, 3,
			[]domain.FindingID{driftRoundFinding(other, 3)}, supersessionAt.Add(2*time.Minute)),
	}
	// An adjudication covers its round's whole batch: the two final
	// dispositions it authorizes and the two findings routed to remediation.
	modelEntry := func(
		id domain.FindingID, goal domain.GoalRelationship, route domain.AdjudicationRoute,
	) domain.FindingAdjudicationEntry {
		entry, err := domain.NewModelAdjudicationEntry(
			id, goal, nil, route, domain.ConfidenceHigh,
			"authorizes the final disposition", nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatalf("adjudication entry %q: %v", id, err)
		}
		return entry
	}
	otherAdjudication, err := domain.NewFindingAdjudication(
		other, 1, adjSpecDigest, adjInstructionDigest, adjPolicyDigest,
		[]domain.FindingAdjudicationEntry{
			modelEntry("other-declined", domain.GoalContradictory, domain.RouteDecline),
			modelEntry("other-deferred", domain.GoalAdjacent, domain.RouteDefer),
			adjudicationEngineEntry(t, "other-fixed"),
			adjudicationEngineEntry(t, "other-kept"),
		}, "", supersessionAt.Add(time.Second))
	if err != nil {
		t.Fatalf("other run adjudication: %v", err)
	}
	otherOverHardened := newDriftAudit(t,
		driftAuditInputFor(other, 2, domain.DriftVerdictOverHardened, "other-fixed"))
	otherConverged := newDriftAudit(t, driftAuditInputFor(other, 3, domain.DriftVerdictConverged))

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: other, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
		}); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, otherRounds[0], otherFindings); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, otherAdjudication); err != nil {
			return err
		}
		for round := 2; round <= 3; round++ {
			finding := adjudicationFinding(driftRoundFinding(other, round), other, "daemon/a.go", supersessionAt)
			if err := tx.PutReviewRecord(ctx, otherRounds[round-1], []domain.Finding{finding}); err != nil {
				return err
			}
			// Each audit is written while its round is the run's latest.
			roundAudit := otherOverHardened
			if round == 3 {
				roundAudit = otherConverged
			}
			if err := tx.PutDriftAudit(ctx, roundAudit); err != nil {
				return err
			}
		}
		for _, disposition := range []domain.ReviewDispositionRecord{
			{
				FindingID: "other-declined", Disposition: domain.ReviewDispositionDeclined,
				AdjudicationDigest: otherAdjudication.Digest,
			},
			{
				FindingID: "other-deferred", Disposition: domain.ReviewDispositionDeferred,
				AdjudicationDigest: otherAdjudication.Digest,
			},
			{
				FindingID: "other-fixed", Disposition: domain.ReviewDispositionFixed,
				RemediationInvocationID: otherRounds[1].InvocationID,
			},
			{
				FindingID: "other-kept", Disposition: domain.ReviewDispositionFixed,
				RemediationInvocationID: otherRounds[1].InvocationID,
			},
		} {
			disposition.RunID, disposition.Round = other, 1
			disposition.Reason = "seeded " + string(disposition.Disposition)
			disposition.CreatedAt = supersessionAt.Add(3 * time.Minute)
			if err := tx.PutFindingDisposition(ctx, disposition); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed supersession parents: %v", err)
	}

	authority := domain.DispositionSupersessionAuthority{Kind: domain.DispositionSupersessionHumanCommand}
	if decision.Command != nil {
		authority.Command = &domain.DispositionSupersessionCommand{
			ItemID: decision.Item.ID, ItemVersion: decision.Command.ItemVersion,
			CommandID: decision.Command.CommandID,
		}
	} else {
		authority.Command = &domain.DispositionSupersessionCommand{
			ItemID: decision.Item.ID, ItemVersion: decision.Item.ItemVersion, CommandID: "command-absent",
		}
	}
	return supersessionFixture{
		st: st, decision: decision, otherConverged: otherConverged,
		record: domain.FindingDispositionSupersession{
			RunID: runID, ReversingRound: decision.Binding.Round,
			FindingID: "finding-a", SupersededRound: 1,
			RemediationInvocationID: domain.InvocationID("review-" + string(runID) + "-2"),
			DriftAuditDigest:        audit.Digest, Authority: authority,
			CreatedAt: supersessionAt.Add(time.Hour),
		},
		otherRecord: domain.FindingDispositionSupersession{
			RunID: other, ReversingRound: 2, FindingID: "other-fixed", SupersededRound: 1,
			RemediationInvocationID: otherRounds[1].InvocationID,
			DriftAuditDigest:        otherOverHardened.Digest, Authority: authority,
			CreatedAt: supersessionAt.Add(time.Hour),
		},
	}
}

func putSupersession(
	st *store.Store, record domain.FindingDispositionSupersession,
) error {
	ctx := context.Background()
	return st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutFindingDispositionSupersession(ctx, record)
	})
}

// putSupersessionSuccessor moves the run past round 2 with a recorded round 3.
func putSupersessionSuccessor(t *testing.T, st *store.Store, runID domain.RunID) {
	t.Helper()
	ctx := context.Background()
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return putDriftRound(ctx, t, tx, runID, 3)
	}); err != nil {
		t.Fatalf("record round 3: %v", err)
	}
}

func TestDispositionSupersessionRoundTripAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-supersession")
	path := filepath.Join(t.TempDir(), "store.db")
	fx := seedSupersession(t, path, runID, domain.ActionContinueUnderPolicy)

	if err := fx.st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetFindingDispositionSupersession(ctx, "finding-a", 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("get before any write = %v, want ErrNotFound", err)
		}
		listed, err := tx.ListFindingDispositionSupersessions(ctx, runID)
		if err != nil || len(listed) != 0 {
			t.Fatalf("list before any write = %v, %v", listed, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := putSupersession(fx.st, fx.record); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := putSupersession(fx.st, fx.record); err != nil {
		t.Fatalf("identical replay: %v", err)
	}
	different := fx.record
	different.CreatedAt = different.CreatedAt.Add(time.Second)
	if err := putSupersession(fx.st, different); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("second record for the same disposition = %v, want ErrImmutableConflict", err)
	}

	if err := fx.st.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, path, store.Options{})
	defer func() { _ = reopened.Close() }()
	if err := reopened.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetFindingDispositionSupersession(ctx, "finding-a", 1)
		if err != nil {
			return err
		}
		listed, err := tx.ListFindingDispositionSupersessions(ctx, runID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, fx.record) ||
			!reflect.DeepEqual(listed, []domain.FindingDispositionSupersession{fx.record}) {
			t.Fatalf("read back = %#v and %#v, want %#v", got, listed, fx.record)
		}
		elsewhere, err := tx.ListFindingDispositionSupersessions(ctx, supersessionOtherRun)
		if err != nil || len(elsewhere) != 0 {
			t.Fatalf("another run's list = %v, %v", elsewhere, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read after restart: %v", err)
	}
}

// TestDispositionSupersessionWriteRefusals changes one thing at a time in a
// record the store accepts. want names the sentinel and stage names the check
// that must refuse it, so a refusal by a different check fails the case.
func TestDispositionSupersessionWriteRefusals(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-supersession-refusals")
	fx := seedSupersession(t, filepath.Join(t.TempDir(), "store.db"), runID, domain.ActionContinueUnderPolicy)
	defer func() { _ = fx.st.Close() }()

	type record = domain.FindingDispositionSupersession
	main := func(mutate func(*record)) record {
		changed := fx.record
		mutate(&changed)
		return changed
	}
	other := func(mutate func(*record)) record {
		changed := fx.otherRecord
		mutate(&changed)
		return changed
	}
	command := func(mutate func(*domain.DispositionSupersessionCommand)) record {
		return main(func(r *record) {
			changed := *r.Authority.Command
			mutate(&changed)
			r.Authority.Command = &changed
		})
	}
	const (
		dispositionStage = "superseded disposition"
		auditStage       = "drift audit"
		itemStage        = "authority item"
		commandStage     = "authority command"
	)
	for _, tc := range []struct {
		name   string
		record record
		want   error
		stage  string
	}{
		{
			"unknown disposition", main(func(r *record) { r.FindingID = "finding-c" }),
			domain.ErrParentKeyMismatch, dispositionStage,
		},
		{
			"another run's disposition",
			main(func(r *record) {
				r.FindingID = "other-fixed"
				r.RemediationInvocationID = fx.otherRecord.RemediationInvocationID
			}),
			domain.ErrParentKeyMismatch, dispositionStage,
		},
		{
			"declined disposition", other(func(r *record) { r.FindingID = "other-declined" }),
			domain.ErrParentKeyMismatch, dispositionStage,
		},
		{
			"deferred disposition", other(func(r *record) { r.FindingID = "other-deferred" }),
			domain.ErrParentKeyMismatch, dispositionStage,
		},
		{
			"different remediation invocation",
			main(func(r *record) { r.RemediationInvocationID = "review-elsewhere" }),
			domain.ErrParentKeyMismatch, dispositionStage,
		},
		{
			"unknown audit",
			main(func(r *record) { r.DriftAuditDigest = domain.Digest("sha256:" + strings.Repeat("0", 64)) }),
			domain.ErrParentKeyMismatch, auditStage,
		},
		{
			"another run's audit",
			main(func(r *record) { r.DriftAuditDigest = fx.otherRecord.DriftAuditDigest }),
			domain.ErrParentKeyMismatch, auditStage,
		},
		{
			"audit with another verdict",
			other(func(r *record) { r.ReversingRound, r.DriftAuditDigest = 3, fx.otherConverged.Digest }),
			domain.ErrParentKeyMismatch, auditStage,
		},
		{
			"audit that does not list the finding", other(func(r *record) { r.FindingID = "other-kept" }),
			domain.ErrParentKeyMismatch, auditStage,
		},
		{
			"reversing round is not the audit's", other(func(r *record) { r.ReversingRound = 3 }),
			domain.ErrParentKeyMismatch, auditStage,
		},
		{
			"automatic route",
			main(func(r *record) {
				r.Authority = domain.DispositionSupersessionAuthority{Kind: domain.DispositionSupersessionAutoRoute}
			}),
			store.ErrSupersessionRouteUnproven, "",
		},
		{
			"unknown item",
			command(func(c *domain.DispositionSupersessionCommand) { c.ItemID = "item-missing" }),
			domain.ErrParentKeyMismatch, itemStage,
		},
		{
			"wrong command",
			command(func(c *domain.DispositionSupersessionCommand) { c.CommandID = "command-other" }),
			domain.ErrParentKeyMismatch, commandStage,
		},
		{
			"wrong item version",
			command(func(c *domain.DispositionSupersessionCommand) { c.ItemVersion++ }),
			domain.ErrParentKeyMismatch, commandStage,
		},
		{
			// The other run's record is in scope for the same round; its item
			// is the main run's.
			"item for another run", fx.otherRecord,
			domain.ErrParentKeyMismatch, commandStage,
		},
		{
			"malformed record", main(func(r *record) { r.CreatedAt = time.Time{} }),
			domain.ErrMissingTimestamp, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := putSupersession(fx.st, tc.record)
			if !errors.Is(err, tc.want) || !strings.Contains(err.Error(), tc.stage) {
				t.Fatalf("put = %v, want %v from the %q check", err, tc.want, tc.stage)
			}
			// A missing parent must not read as a missing record.
			if errors.Is(err, store.ErrNotFound) {
				t.Fatalf("refusal wraps ErrNotFound: %v", err)
			}
		})
	}
	// The unchanged record is still accepted, so each case above was refused
	// for its one change.
	if err := putSupersession(fx.st, fx.record); err != nil {
		t.Fatalf("unchanged record: %v", err)
	}
}

// TestDispositionSupersessionRefusesOtherDecisions covers the authority
// refusals that need a differently concluded item.
func TestDispositionSupersessionRefusesOtherDecisions(t *testing.T) {
	t.Parallel()
	for name, action := range map[string]domain.Action{
		"open item with no command": "",
		"finish_now":                domain.ActionFinishNow,
		"apply_then_finish":         domain.ActionApplyThenFinish,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx := seedSupersession(t, filepath.Join(t.TempDir(), "store.db"), "run-supersession-decision", action)
			defer func() { _ = fx.st.Close() }()
			err := putSupersession(fx.st, fx.record)
			if !errors.Is(err, domain.ErrParentKeyMismatch) || !strings.Contains(err.Error(), "authority command") {
				t.Fatalf("put = %v, want ErrParentKeyMismatch from the authority command check", err)
			}
		})
	}
}

// TestDispositionSupersessionRefusesAnotherRoundsItem records a reversal that
// is in scope for round 3 and cites the same run's round-2 decision.
func TestDispositionSupersessionRefusesAnotherRoundsItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-supersession-round")
	fx := seedSupersession(t, filepath.Join(t.TempDir(), "store.db"), runID, domain.ActionContinueUnderPolicy)
	defer func() { _ = fx.st.Close() }()
	input := driftAuditInputFor(runID, 3, domain.DriftVerdictOverHardened, "finding-a")
	input.ResolvedPolicyDigest = fx.decision.Binding.PolicyDigest
	audit := newDriftAudit(t, input)
	if err := fx.st.Write(ctx, func(tx *store.WriteTx) error {
		if err := putDriftRound(ctx, t, tx, runID, 3); err != nil {
			return err
		}
		return tx.PutDriftAudit(ctx, audit)
	}); err != nil {
		t.Fatalf("audit round 3: %v", err)
	}
	record := fx.record
	record.ReversingRound, record.DriftAuditDigest = 3, audit.Digest
	err := putSupersession(fx.st, record)
	if !errors.Is(err, domain.ErrParentKeyMismatch) || !strings.Contains(err.Error(), "authority command") {
		t.Fatalf("put = %v, want ErrParentKeyMismatch from the authority command check", err)
	}
}

func TestDispositionSupersessionRefusesARoundWithASuccessor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-supersession-successor")

	t.Run("later recorded round", func(t *testing.T) {
		t.Parallel()
		fx := seedSupersession(t, filepath.Join(t.TempDir(), "store.db"), runID, domain.ActionContinueUnderPolicy)
		defer func() { _ = fx.st.Close() }()
		putSupersessionSuccessor(t, fx.st, runID)
		if err := putSupersession(fx.st, fx.record); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("new record after a later round = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("later failed round", func(t *testing.T) {
		t.Parallel()
		fx := seedSupersession(t, filepath.Join(t.TempDir(), "store.db"), runID, domain.ActionContinueUnderPolicy)
		defer func() { _ = fx.st.Close() }()
		if err := fx.st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutReviewFailure(ctx, domain.ReviewFailure{
				InvocationID: "review-failed-3", RunID: runID, Round: 3,
				BaseSHA: "base", HeadSHA: "head-3", Class: domain.ReviewFailureTransient,
				Reason: "reviewer request failed", ObservedAt: supersessionAt.Add(2 * time.Hour),
			})
		}); err != nil {
			t.Fatalf("seed failed round: %v", err)
		}
		if err := putSupersession(fx.st, fx.record); !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("new record past a failed round = %v, want ErrImmutableConflict", err)
		}
	})

	t.Run("replay and reads after a later round", func(t *testing.T) {
		t.Parallel()
		fx := seedSupersession(t, filepath.Join(t.TempDir(), "store.db"), runID, domain.ActionContinueUnderPolicy)
		defer func() { _ = fx.st.Close() }()
		if err := putSupersession(fx.st, fx.record); err != nil {
			t.Fatalf("put while round 2 is latest: %v", err)
		}
		putSupersessionSuccessor(t, fx.st, runID)
		if err := putSupersession(fx.st, fx.record); err != nil {
			t.Fatalf("identical replay after a later round: %v", err)
		}
		if err := fx.st.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetFindingDispositionSupersession(ctx, "finding-a", 1)
			return err
		}); err != nil {
			t.Fatalf("read after a later round: %v", err)
		}
	})
}

// TestDispositionSupersessionReadsFailClosed proves every read re-runs the
// write-time checks: no copied column and no stored round, invocation, digest,
// or command reference is trusted. A rewritten body carries a recomputed body
// digest, so the integrity check can't mask the one under test.
func TestDispositionSupersessionReadsFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-supersession-tamper")

	rewrite := func(mutate func(*domain.FindingDispositionSupersession)) func(
		*testing.T, *sql.DB, domain.FindingDispositionSupersession) {
		return func(t *testing.T, db *sql.DB, record domain.FindingDispositionSupersession) {
			t.Helper()
			mutate(&record)
			body, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE finding_disposition_supersessions SET body = ?, body_digest = ?`,
				string(body), contentaddr.Sum(body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	exec := func(query string) func(*testing.T, *sql.DB, domain.FindingDispositionSupersession) {
		return func(t *testing.T, db *sql.DB, _ domain.FindingDispositionSupersession) {
			t.Helper()
			result, err := db.Exec(query)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				t.Fatalf("tamper changed %d rows, %v", changed, err)
			}
		}
	}
	for _, tc := range []struct {
		name   string
		want   error
		tamper func(*testing.T, *sql.DB, domain.FindingDispositionSupersession)
		// keyedReadMisses marks a tamper that moves the row out of the keyed
		// lookup, where only the whole-table list can still fail closed.
		keyedReadMisses bool
		// successor records round 3 before the tamper. Without it the replay
		// below must be refused by the tampered row itself, not by the
		// written-once rule.
		successor bool
	}{
		{name: "body changed under its digest", want: store.ErrRowInconsistent, tamper: exec(
			`UPDATE finding_disposition_supersessions
SET body = json_set(body, '$.created_at', '2026-10-01T17:00:00Z')`)},
		{name: "copied run key moved", want: store.ErrRowInconsistent, tamper: exec(
			`UPDATE finding_disposition_supersessions SET run_id = '` + string(supersessionOtherRun) + `'`)},
		// Round 3 exists so the moved round still names a recorded round.
		{name: "copied reversing round moved", want: store.ErrRowInconsistent, successor: true, tamper: exec(
			`UPDATE finding_disposition_supersessions SET reversing_round = 3`)},
		{
			name: "copied disposition key moved", want: store.ErrRowInconsistent,
			keyedReadMisses: true, successor: true,
			tamper: exec(`UPDATE finding_disposition_supersessions SET finding_id = 'other-fixed'`),
		},
		{
			name: "coherent record naming another remediation", want: domain.ErrParentKeyMismatch,
			tamper: rewrite(func(r *domain.FindingDispositionSupersession) {
				r.RemediationInvocationID = "review-elsewhere"
			}),
		},
		{
			name: "coherent record naming another audit", want: domain.ErrParentKeyMismatch,
			tamper: rewrite(func(r *domain.FindingDispositionSupersession) {
				r.DriftAuditDigest = domain.Digest("sha256:" + strings.Repeat("0", 64))
			}),
		},
		{
			name: "coherent record naming another command", want: domain.ErrParentKeyMismatch,
			tamper: rewrite(func(r *domain.FindingDispositionSupersession) {
				command := *r.Authority.Command
				command.CommandID = "command-other"
				r.Authority.Command = &command
			}),
		},
		{
			name: "coherent record claiming the automatic route", want: store.ErrSupersessionRouteUnproven,
			tamper: rewrite(func(r *domain.FindingDispositionSupersession) {
				r.Authority = domain.DispositionSupersessionAuthority{Kind: domain.DispositionSupersessionAutoRoute}
			}),
		},
		{
			name: "record body no longer valid", want: domain.ErrDispositionSupersessionInvalid,
			tamper: rewrite(func(r *domain.FindingDispositionSupersession) { r.Authority.Command = nil }),
		},
		{name: "audit removed", want: domain.ErrParentKeyMismatch, tamper: exec(
			`DELETE FROM drift_audits WHERE run_id = '` + string(runID) + `'`)},
		{name: "command removed", want: domain.ErrParentKeyMismatch, tamper: exec(
			`DELETE FROM commands WHERE item_id = '` + string(store.ReviewDiminishingItemID(runID, 2)) + `'`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.db")
			fx := seedSupersession(t, path, runID, domain.ActionContinueUnderPolicy)
			if err := putSupersession(fx.st, fx.record); err != nil {
				t.Fatalf("put: %v", err)
			}
			if tc.successor {
				putSupersessionSuccessor(t, fx.st, runID)
			}
			if err := fx.st.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			tc.tamper(t, raw, fx.record)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := storetest.Open(t, path, store.Options{})
			defer func() { _ = reopened.Close() }()

			err = reopened.Read(ctx, func(tx *store.ReadTx) error {
				_, err := tx.ListFindingDispositionSupersessions(ctx, runID)
				return err
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("list over a tampered row = %v, want %v", err, tc.want)
			}
			err = reopened.Read(ctx, func(tx *store.ReadTx) error {
				_, err := tx.GetFindingDispositionSupersession(ctx, "finding-a", 1)
				return err
			})
			if tc.keyedReadMisses {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("keyed read of a moved row = %v, want ErrNotFound", err)
				}
			} else if !errors.Is(err, tc.want) || errors.Is(err, store.ErrNotFound) {
				// A missing parent must not read as a missing record: a caller
				// takes ErrNotFound to mean the fix was never reversed.
				t.Fatalf("keyed read over a tampered row = %v, want %v and not ErrNotFound", err, tc.want)
			}
			// A replay of the original record can't paper over the tampered row.
			err = putSupersession(reopened, fx.record)
			if err == nil || (!tc.successor && !errors.Is(err, tc.want)) {
				t.Fatalf("replay over a tampered row = %v, want %v", err, tc.want)
			}
		})
	}
}
