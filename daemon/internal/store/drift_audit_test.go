package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var driftAuditAt = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// driftRoundFinding is the one finding seedDriftAuditRun records per round.
func driftRoundFinding(runID domain.RunID, round int) domain.FindingID {
	return domain.FindingID("finding-" + string(runID) + "-" + strconv.Itoa(round))
}

// driftAuditInputFor builds the audit adjudicationReviewRecord's commits and
// the seeded run's digests call for. An over_hardened audit reverses the given
// findings; the other verdicts take none.
func driftAuditInputFor(
	runID domain.RunID, round int, verdict domain.DriftVerdict, reversed ...domain.FindingID,
) domain.DriftAuditInput {
	input := domain.DriftAuditInput{
		RunID: runID, Round: round, BaseSHA: "base", HeadSHA: "head-" + strconv.Itoa(round),
		ApprovedSpecDigest: adjSpecDigest, ResolvedPolicyDigest: adjPolicyDigest,
		Verdict: verdict, Confidence: domain.ConfidenceHigh,
		Explanation: "audit of round " + strconv.Itoa(round),
		CreatedAt:   driftAuditAt.Add(time.Duration(round) * time.Minute),
	}
	for _, id := range reversed {
		input.Reversals = append(input.Reversals, domain.DriftReversal{
			FindingID: id, Undo: "undo the fix for " + string(id),
			Rationale: "the specification does not need it",
		})
	}
	return input
}

func newDriftAudit(t *testing.T, input domain.DriftAuditInput) domain.DriftAudit {
	t.Helper()
	audit, err := domain.NewDriftAudit(input)
	if err != nil {
		t.Fatalf("drift audit: %v", err)
	}
	return audit
}

// driftAuditFor is the audit seedDriftAuditRun writes for a round: an
// over_hardened verdict reverses the round's own finding.
func driftAuditFor(t *testing.T, runID domain.RunID, round int, verdict domain.DriftVerdict) domain.DriftAudit {
	t.Helper()
	if verdict == domain.DriftVerdictOverHardened {
		return newDriftAudit(t, driftAuditInputFor(runID, round, verdict, driftRoundFinding(runID, round)))
	}
	return newDriftAudit(t, driftAuditInputFor(runID, round, verdict))
}

func putDriftRound(
	ctx context.Context, t *testing.T, tx *store.WriteTx, runID domain.RunID, round int,
) error {
	t.Helper()
	finding := adjudicationFinding(driftRoundFinding(runID, round), runID, "daemon/a.go", driftAuditAt)
	record := adjudicationReviewRecord(
		t, runID, round, []domain.FindingID{finding.ID}, driftAuditAt.Add(time.Duration(round)*time.Minute))
	return tx.PutReviewRecord(ctx, record, []domain.Finding{finding})
}

// seedDriftAuditRun opens a store with one run and the given review rounds,
// each with one finding. audits names the rounds whose audit is written while
// the round is still the run's latest, the only order the accessor allows.
func seedDriftAuditRun(
	t *testing.T, path string, runID domain.RunID, rounds int, audits map[int]domain.DriftVerdict,
) *store.Store {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t, path, store.Options{})
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
		}); err != nil {
			return err
		}
		for round := 1; round <= rounds; round++ {
			if err := putDriftRound(ctx, t, tx, runID, round); err != nil {
				return err
			}
			if verdict, ok := audits[round]; ok {
				if err := tx.PutDriftAudit(ctx, driftAuditFor(t, runID, round, verdict)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed drift audit run: %v", err)
	}
	return st
}

func TestDriftAuditRoundTripAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-drift")
	verdicts := map[int]domain.DriftVerdict{
		1: domain.DriftVerdictConverged, 2: domain.DriftVerdictOverHardened, 3: domain.DriftVerdictStuck,
	}
	st := seedDriftAuditRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 3, verdicts)
	defer func() { _ = st.Close() }()

	// A byte-identical replay converges, even for a round that now has a
	// successor.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDriftAudit(ctx, driftAuditFor(t, runID, 1, verdicts[1]))
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var want []domain.DriftAudit
		for round := 1; round <= 3; round++ {
			audit := driftAuditFor(t, runID, round, verdicts[round])
			want = append(want, audit)
			byRound, err := tx.GetDriftAuditForRound(ctx, runID, round)
			if err != nil {
				return err
			}
			byDigest, err := tx.GetDriftAudit(ctx, audit.Digest)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(byRound, audit) || !reflect.DeepEqual(byDigest, audit) {
				t.Fatalf("round %d audit = %+v and %+v, want %+v", round, byRound, byDigest, audit)
			}
		}
		list, err := tx.ListDriftAudits(ctx, runID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(list, want) {
			t.Fatalf("list = %+v, want %+v", list, want)
		}
		other, err := tx.ListDriftAudits(ctx, "run-other")
		if err != nil {
			return err
		}
		if len(other) != 0 {
			t.Fatalf("another run lists %d audits", len(other))
		}
		if _, err := tx.GetDriftAudit(ctx, adjudicationDigest("f")); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown digest = %v, want ErrNotFound", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestDriftAuditWithoutARowIsNotFound proves an unaudited round reads as
// absent, not as an error: the audit is off, did not run, or failed.
func TestDriftAuditWithoutARowIsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-drift-absent")
	st := seedDriftAuditRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 1, nil)
	defer func() { _ = st.Close() }()
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetDriftAuditForRound(ctx, runID, 1); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unaudited round = %v, want ErrNotFound", err)
		}
		list, err := tx.ListDriftAudits(ctx, runID)
		if err != nil || len(list) != 0 {
			t.Fatalf("list = %v, %v, want empty", list, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDriftAuditBindingFailures proves a write is refused unless the round is
// a recorded round with findings, the commits and digests are the run's own,
// and every reversal names a finding the run's records list by that round.
func TestDriftAuditBindingFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-drift-binding")
	otherRun := domain.RunID("run-drift-binding-other")
	st := seedDriftAuditRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 2, nil)
	defer func() { _ = st.Close() }()
	// Round 3 is clean, and another run owns a finding of its own.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		clean, err := domain.NewReviewRecord(domain.ReviewRecord{
			InvocationID: "review-clean-3", RunID: runID, Round: 3,
			Provider: "openai", ModelConfiguration: "gpt-codex/high",
			ConfigurationDigest: adjudicationDigest("c"), InstructionDigest: adjInstructionDigest,
			CostOwner: "owner", BaseSHA: "base", HeadSHA: "head-3",
			CompletedAt:        driftAuditAt.Add(3 * time.Minute),
			CompletionEvidence: adjudicationDigest("e"), Outcome: domain.ReviewClean,
		})
		if err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, clean, nil); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: otherRun, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
		}); err != nil {
			return err
		}
		return putDriftRound(ctx, t, tx, otherRun, 1)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, tc := range []struct {
		name  string
		input func() domain.DriftAuditInput
		want  error
	}{
		{"absent round", func() domain.DriftAuditInput {
			return driftAuditInputFor(runID, 9, domain.DriftVerdictConverged)
		}, domain.ErrParentKeyMismatch},
		{"clean round", func() domain.DriftAuditInput {
			return driftAuditInputFor(runID, 3, domain.DriftVerdictConverged)
		}, domain.ErrParentKeyMismatch},
		{"another base", func() domain.DriftAuditInput {
			input := driftAuditInputFor(runID, 2, domain.DriftVerdictConverged)
			input.BaseSHA = "base-other"
			return input
		}, domain.ErrParentKeyMismatch},
		{"another round's head", func() domain.DriftAuditInput {
			input := driftAuditInputFor(runID, 2, domain.DriftVerdictConverged)
			input.HeadSHA = "head-1"
			return input
		}, domain.ErrParentKeyMismatch},
		{"another specification", func() domain.DriftAuditInput {
			input := driftAuditInputFor(runID, 2, domain.DriftVerdictConverged)
			input.ApprovedSpecDigest = adjPolicyDigest
			return input
		}, domain.ErrParentKeyMismatch},
		{"another policy", func() domain.DriftAuditInput {
			input := driftAuditInputFor(runID, 2, domain.DriftVerdictConverged)
			input.ResolvedPolicyDigest = adjSpecDigest
			return input
		}, domain.ErrParentKeyMismatch},
		{"reversal names an unknown finding", func() domain.DriftAuditInput {
			return driftAuditInputFor(runID, 2, domain.DriftVerdictOverHardened, "finding-unknown")
		}, domain.ErrParentKeyMismatch},
		{"reversal names another run's finding", func() domain.DriftAuditInput {
			return driftAuditInputFor(runID, 2, domain.DriftVerdictOverHardened, driftRoundFinding(otherRun, 1))
		}, domain.ErrParentKeyMismatch},
		{"reversal names a later round's finding", func() domain.DriftAuditInput {
			return driftAuditInputFor(runID, 1, domain.DriftVerdictOverHardened, driftRoundFinding(runID, 2))
		}, domain.ErrParentKeyMismatch},
		{"one unknown finding among known ones", func() domain.DriftAuditInput {
			return driftAuditInputFor(runID, 2, domain.DriftVerdictOverHardened,
				driftRoundFinding(runID, 1), "finding-unknown", driftRoundFinding(runID, 2))
		}, domain.ErrParentKeyMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			audit := newDriftAudit(t, tc.input())
			err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDriftAudit(ctx, audit) })
			if !errors.Is(err, tc.want) {
				t.Fatalf("put = %v, want %v", err, tc.want)
			}
		})
	}

	// A hand-built artifact that skipped the constructor is refused before any
	// binding is read.
	forged := newDriftAudit(t, driftAuditInputFor(runID, 2, domain.DriftVerdictConverged))
	forged.Verdict = domain.DriftVerdictStuck
	err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDriftAudit(ctx, forged) })
	if !errors.Is(err, domain.ErrDriftAuditDigestMismatch) {
		t.Fatalf("forged artifact put = %v, want ErrDriftAuditDigestMismatch", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		list, err := tx.ListDriftAudits(ctx, runID)
		if err != nil || len(list) != 0 {
			t.Fatalf("refused writes left rows: %v, %v", list, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDriftAuditReversalMayNameAnEarlierRoundsFinding proves the reversal
// binding covers the run's findings through the audited round, not only the
// round's own batch: an audit reverses fixes made in earlier rounds.
func TestDriftAuditReversalMayNameAnEarlierRoundsFinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-drift-earlier")
	st := seedDriftAuditRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 3, nil)
	defer func() { _ = st.Close() }()
	audit := newDriftAudit(t, driftAuditInputFor(runID, 3, domain.DriftVerdictOverHardened,
		driftRoundFinding(runID, 1), driftRoundFinding(runID, 2)))
	if err := st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDriftAudit(ctx, audit) }); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetDriftAudit(ctx, audit.Digest)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, audit) {
			t.Fatalf("audit = %+v, want %+v", got, audit)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestDriftAuditImmutableConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-drift-conflict")
	st := seedDriftAuditRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 1,
		map[int]domain.DriftVerdict{1: domain.DriftVerdictConverged})
	defer func() { _ = st.Close() }()

	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDriftAudit(ctx, driftAuditFor(t, runID, 1, domain.DriftVerdictStuck))
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("different audit for the round = %v, want ErrImmutableConflict", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetDriftAuditForRound(ctx, runID, 1)
		if err != nil {
			return err
		}
		if got.Verdict != domain.DriftVerdictConverged {
			t.Fatalf("stored verdict = %q, want the first write's", got.Verdict)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestDriftAuditRefusesARoundWithASuccessor proves the write-only rule: a
// round that already has a later round, recorded or failed, cannot gain an
// audit, because yield histories already snapshotted show it without a verdict.
func TestDriftAuditRefusesARoundWithASuccessor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("later recorded round", func(t *testing.T) {
		t.Parallel()
		runID := domain.RunID("run-drift-successor")
		st := seedDriftAuditRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 2, nil)
		defer func() { _ = st.Close() }()
		err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutDriftAudit(ctx, driftAuditFor(t, runID, 1, domain.DriftVerdictConverged))
		})
		if !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("audit for a superseded round = %v, want ErrImmutableConflict", err)
		}
		// The latest round has no successor and is still writable.
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutDriftAudit(ctx, driftAuditFor(t, runID, 2, domain.DriftVerdictConverged))
		}); err != nil {
			t.Fatalf("latest round: %v", err)
		}
	})

	t.Run("later failed round", func(t *testing.T) {
		t.Parallel()
		runID := domain.RunID("run-drift-failed-successor")
		st := seedDriftAuditRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 2, nil)
		defer func() { _ = st.Close() }()
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutReviewFailure(ctx, domain.ReviewFailure{
				InvocationID: "review-failed-3", RunID: runID, Round: 3,
				BaseSHA: "base", HeadSHA: "head-3", Class: domain.ReviewFailureTransient,
				Reason: "reviewer request failed", ObservedAt: driftAuditAt.Add(time.Hour),
			})
		}); err != nil {
			t.Fatalf("seed failed round: %v", err)
		}
		err := st.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutDriftAudit(ctx, driftAuditFor(t, runID, 2, domain.DriftVerdictConverged))
		})
		if !errors.Is(err, store.ErrImmutableConflict) {
			t.Fatalf("audit past a failed round = %v, want ErrImmutableConflict", err)
		}
	})
}

// TestDriftAuditReadsFailClosed proves every read re-runs the write-time
// checks: no copied column and no stored commit, digest, or finding name is
// trusted. Each tampered body that should reach a later check carries a
// recomputed body digest, and where needed a recomputed content digest, so an
// earlier check can't mask the one under test.
func TestDriftAuditReadsFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-drift-tamper")
	original := driftAuditFor(t, runID, 2, domain.DriftVerdictOverHardened)

	// replaceWith overwrites round 2's row with a coherent, validly digested
	// artifact that the binding must still refuse.
	replaceWith := func(input domain.DriftAuditInput) func(*testing.T, *sql.DB) {
		return func(t *testing.T, db *sql.DB) {
			t.Helper()
			body, err := newDriftAudit(t, input).Encode()
			if err != nil {
				t.Fatal(err)
			}
			audit, err := domain.DecodeDriftAudit(body)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE drift_audits
SET body = ?, body_digest = ?, content_digest = ? WHERE run_id = ? AND round = 2`,
				string(body), contentaddr.Sum(body), string(audit.Digest), runID); err != nil {
				t.Fatal(err)
			}
		}
	}
	exec := func(query string) func(*testing.T, *sql.DB) {
		return func(t *testing.T, db *sql.DB) {
			t.Helper()
			result, err := db.Exec(query, runID)
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
		tamper func(*testing.T, *sql.DB)
		// keyedReadsMiss marks a tamper that moves the row out of the keyed
		// lookups, where only the whole-table list can still fail closed.
		keyedReadsMiss bool
	}{
		{name: "body changed under its digest", want: store.ErrRowInconsistent, tamper: exec(
			`UPDATE drift_audits SET body = json_set(body, '$.explanation', 'edited')
WHERE run_id = ? AND round = 2`)},
		{
			name: "content changed under the artifact digest", want: domain.ErrDriftAuditDigestMismatch,
			tamper: func(t *testing.T, db *sql.DB) {
				t.Helper()
				var body string
				if err := db.QueryRow(`SELECT json_set(body, '$.explanation', 'edited')
FROM drift_audits WHERE run_id = ? AND round = 2`, runID).Scan(&body); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE drift_audits SET body = ?, body_digest = ?
WHERE run_id = ? AND round = 2`, body, contentaddr.Sum([]byte(body)), runID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "verdict changed under its reversal list", want: domain.ErrDriftAuditInconsistent,
			tamper: func(t *testing.T, db *sql.DB) {
				t.Helper()
				var body string
				if err := db.QueryRow(`SELECT json_set(body, '$.verdict', 'converged')
FROM drift_audits WHERE run_id = ? AND round = 2`, runID).Scan(&body); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(`UPDATE drift_audits SET body = ?, body_digest = ?
WHERE run_id = ? AND round = 2`, body, contentaddr.Sum([]byte(body)), runID); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "coherent audit naming another head", want: domain.ErrParentKeyMismatch,
			tamper: func(t *testing.T, db *sql.DB) {
				t.Helper()
				input := driftAuditInputFor(runID, 2, domain.DriftVerdictConverged)
				input.HeadSHA = "head-other"
				replaceWith(input)(t, db)
			},
		},
		{
			name: "coherent audit naming another policy", want: domain.ErrParentKeyMismatch,
			tamper: func(t *testing.T, db *sql.DB) {
				t.Helper()
				input := driftAuditInputFor(runID, 2, domain.DriftVerdictConverged)
				input.ResolvedPolicyDigest = adjSpecDigest
				replaceWith(input)(t, db)
			},
		},
		{
			name: "coherent audit reversing a later round's finding", want: domain.ErrParentKeyMismatch,
			tamper: replaceWith(driftAuditInputFor(
				runID, 2, domain.DriftVerdictOverHardened, driftRoundFinding(runID, 3))),
		},
		{name: "copied round key moved", want: store.ErrRowInconsistent, keyedReadsMiss: true, tamper: exec(
			`UPDATE drift_audits SET round = 3 WHERE run_id = ? AND round = 2`)},
		{name: "copied content digest changed", want: store.ErrRowInconsistent, keyedReadsMiss: true, tamper: exec(
			`UPDATE drift_audits SET content_digest = 'sha256:` + strings.Repeat("0", 64) + `'
WHERE run_id = ? AND round = 2`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.db")
			// Round 3 exists without an audit so a moved round key still
			// satisfies the foreign key and reaches reconstruction.
			st := seedDriftAuditRun(t, path, runID, 3,
				map[int]domain.DriftVerdict{2: domain.DriftVerdictOverHardened})
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			tc.tamper(t, raw)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := storetest.Open(t, path, store.Options{})
			defer func() { _ = reopened.Close() }()
			reads := map[string]func(*store.ReadTx) error{
				"list": func(tx *store.ReadTx) error {
					_, err := tx.ListDriftAudits(ctx, runID)
					return err
				},
			}
			if !tc.keyedReadsMiss {
				reads["by round"] = func(tx *store.ReadTx) error {
					_, err := tx.GetDriftAuditForRound(ctx, runID, 2)
					return err
				}
			}
			for name, read := range reads {
				if err := reopened.Read(ctx, read); !errors.Is(err, tc.want) {
					t.Fatalf("%s over a tampered audit row = %v, want %v", name, err, tc.want)
				}
			}
			// The original digest either reaches the same refusal or no row at
			// all; it never returns the tampered audit.
			err = reopened.Read(ctx, func(tx *store.ReadTx) error {
				_, err := tx.GetDriftAudit(ctx, original.Digest)
				return err
			})
			if !errors.Is(err, tc.want) && !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("by digest over a tampered audit row = %v, want %v or ErrNotFound", err, tc.want)
			}
			// A replay of the original audit can't paper over the tampered row.
			err = reopened.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDriftAudit(ctx, original) })
			if err == nil {
				t.Fatal("replay over a tampered audit row succeeded")
			}
		})
	}
}
