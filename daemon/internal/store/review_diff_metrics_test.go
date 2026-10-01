package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// roundDiffMetrics builds the metrics adjudicationReviewRecord's commits call
// for: every record is bound to "base" and round N's head is "head-N".
func roundDiffMetrics(round int) domain.ReviewRoundDiffMetrics {
	head := "head-" + strconv.Itoa(round)
	cumulative := domain.DiffStats{
		FilesChanged: 2 + round, Additions: 100 * round, Deletions: 5 * round,
		BaseSHA: "base", HeadSHA: head,
	}
	if round == 1 {
		return domain.ReviewRoundDiffMetrics{Cumulative: cumulative, Round: cumulative}
	}
	return domain.ReviewRoundDiffMetrics{
		Cumulative: cumulative,
		Round: domain.DiffStats{
			FilesChanged: 1, Additions: 100, Deletions: 5,
			BaseSHA: "head-" + strconv.Itoa(round-1), HeadSHA: head,
		},
	}
}

// seedDiffMetricsRun opens a store with one run and the given clean review
// rounds. withMetrics names the rounds whose metrics are written as the round
// is recorded, the only order the accessor allows.
func seedDiffMetricsRun(
	t *testing.T, path string, runID domain.RunID, rounds int, withMetrics ...int,
) *store.Store {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	st := storetest.Open(t, path, store.Options{})
	recorded := make(map[int]bool, len(withMetrics))
	for _, round := range withMetrics {
		recorded[round] = true
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: adjPolicyDigest,
		}); err != nil {
			return err
		}
		for round := 1; round <= rounds; round++ {
			finding := adjudicationFinding(
				domain.FindingID("finding-"+string(runID)+"-"+strconv.Itoa(round)), runID, "daemon/a.go", at)
			record := adjudicationReviewRecord(
				t, runID, round, []domain.FindingID{finding.ID}, at.Add(time.Duration(round)*time.Minute))
			if err := tx.PutReviewRecord(ctx, record, []domain.Finding{finding}); err != nil {
				return err
			}
			if recorded[round] {
				if err := tx.PutReviewRoundDiffMetrics(ctx, runID, round, roundDiffMetrics(round)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed diff metrics run: %v", err)
	}
	return st
}

func TestReviewRoundDiffMetricsRoundTripAndReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-metrics")
	st := seedDiffMetricsRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 2, 1, 2)
	defer func() { _ = st.Close() }()

	// A byte-identical replay converges, even for a round that now has a
	// successor.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRoundDiffMetrics(ctx, runID, 1, roundDiffMetrics(1))
	}); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		for round := 1; round <= 2; round++ {
			got, err := tx.GetReviewRoundDiffMetrics(ctx, runID, round)
			if err != nil {
				return err
			}
			if got != roundDiffMetrics(round) {
				t.Fatalf("round %d metrics = %+v", round, got)
			}
		}
		list, err := tx.ListReviewRoundDiffMetrics(ctx, runID)
		if err != nil {
			return err
		}
		want := map[int]domain.ReviewRoundDiffMetrics{1: roundDiffMetrics(1), 2: roundDiffMetrics(2)}
		if !reflect.DeepEqual(list, want) {
			t.Fatalf("list = %+v, want %+v", list, want)
		}
		other, err := tx.ListReviewRoundDiffMetrics(ctx, "run-other")
		if err != nil {
			return err
		}
		if len(other) != 0 {
			t.Fatalf("another run lists %d rows", len(other))
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestReviewRoundDiffMetricsBindingFailures proves a write is refused unless
// the round exists and both pairs name the review records' own commits.
func TestReviewRoundDiffMetricsBindingFailures(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-metrics-binding")
	st := seedDiffMetricsRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 2)
	defer func() { _ = st.Close() }()

	for _, tc := range []struct {
		name   string
		round  int
		mutate func(*domain.ReviewRoundDiffMetrics)
		want   error
	}{
		{
			name: "absent round", round: 9, mutate: func(*domain.ReviewRoundDiffMetrics) {},
			want: domain.ErrParentKeyMismatch,
		},
		{name: "cumulative from another base", round: 2, mutate: func(m *domain.ReviewRoundDiffMetrics) {
			m.Cumulative.BaseSHA = "base-other"
		}, want: domain.ErrParentKeyMismatch},
		{name: "another head", round: 2, mutate: func(m *domain.ReviewRoundDiffMetrics) {
			m.Cumulative.HeadSHA = "head-other"
			m.Round.HeadSHA = "head-other"
		}, want: domain.ErrParentKeyMismatch},
		{name: "round pair skips the previous head", round: 2, mutate: func(m *domain.ReviewRoundDiffMetrics) {
			m.Round = m.Cumulative
		}, want: domain.ErrParentKeyMismatch},
		{name: "first round pair from elsewhere", round: 1, mutate: func(m *domain.ReviewRoundDiffMetrics) {
			m.Round.BaseSHA = "head-0"
		}, want: domain.ErrParentKeyMismatch},
		{name: "invalid metrics", round: 2, mutate: func(m *domain.ReviewRoundDiffMetrics) {
			m.Round.Additions = -1
		}, want: domain.ErrReviewYieldHistoryInconsistent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metrics := roundDiffMetrics(min(tc.round, 2))
			tc.mutate(&metrics)
			err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutReviewRoundDiffMetrics(ctx, runID, tc.round, metrics)
			})
			if !errors.Is(err, tc.want) {
				t.Fatalf("put = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestReviewRoundDiffMetricsImmutableConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-metrics-conflict")
	st := seedDiffMetricsRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 1, 1)
	defer func() { _ = st.Close() }()

	different := roundDiffMetrics(1)
	different.Cumulative.Additions++
	different.Round.Additions++
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRoundDiffMetrics(ctx, runID, 1, different)
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("different metrics for the round = %v, want ErrImmutableConflict", err)
	}
}

// TestReviewRoundDiffMetricsRefusesBackfill proves the never-backfilled rule:
// a round that already has a successor cannot gain metrics, because an item
// may already carry that round's history without them.
func TestReviewRoundDiffMetricsRefusesBackfill(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-metrics-backfill")
	st := seedDiffMetricsRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 2)
	defer func() { _ = st.Close() }()

	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRoundDiffMetrics(ctx, runID, 1, roundDiffMetrics(1))
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("backfill of a superseded round = %v, want ErrImmutableConflict", err)
	}
	// The latest round has no successor and is still writable.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRoundDiffMetrics(ctx, runID, 2, roundDiffMetrics(2))
	}); err != nil {
		t.Fatalf("latest round: %v", err)
	}
}

// TestReviewRoundDiffMetricsRefusesBackfillPastAFailedRound proves a failed
// round counts as a successor: failures take the run's round numbers, so the
// run has moved past the round even though no later record exists.
func TestReviewRoundDiffMetricsRefusesBackfillPastAFailedRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-metrics-failed-successor")
	st := seedDiffMetricsRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 2)
	defer func() { _ = st.Close() }()

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewFailure(ctx, domain.ReviewFailure{
			InvocationID: "review-failed-3", RunID: runID, Round: 3,
			BaseSHA: "base", HeadSHA: "head-3", Class: domain.ReviewFailureTransient,
			Reason:     "reviewer request failed",
			ObservedAt: time.Date(2026, 9, 30, 13, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("seed failed round: %v", err)
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutReviewRoundDiffMetrics(ctx, runID, 2, roundDiffMetrics(2))
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("backfill past a failed round = %v, want ErrImmutableConflict", err)
	}
}

// TestDeriveReviewYieldHistoryRechecksDiffMetrics proves the derivation does
// not trust its metrics argument: it runs the same record check as the
// accessor, so a caller that skips the accessor gains nothing.
func TestDeriveReviewYieldHistoryRechecksDiffMetrics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-metrics-derive")
	st := seedDiffMetricsRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 2)
	defer func() { _ = st.Close() }()

	var records []domain.ReviewRecord
	findings := map[domain.FindingID]domain.Finding{}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if records, err = tx.ListReviewRecords(ctx, runID); err != nil {
			return err
		}
		for _, record := range records {
			for _, id := range record.FindingIDs {
				finding, err := tx.GetFinding(ctx, id)
				if err != nil {
					return err
				}
				findings[id] = finding
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}

	history, err := store.DeriveReviewYieldHistory(records, nil, findings,
		map[int]domain.ReviewRoundDiffMetrics{2: roundDiffMetrics(2)})
	if err != nil {
		t.Fatalf("valid metrics: %v", err)
	}
	if history.Rounds[0].DiffMetrics != nil || history.Rounds[1].DiffMetrics == nil ||
		*history.Rounds[1].DiffMetrics != roundDiffMetrics(2) {
		t.Fatalf("derived metrics = %+v", history.Rounds)
	}

	_, err = store.DeriveReviewYieldHistory(records, nil, findings,
		map[int]domain.ReviewRoundDiffMetrics{3: roundDiffMetrics(3)})
	if !errors.Is(err, domain.ErrReviewYieldHistoryInconsistent) {
		t.Fatalf("metrics for an absent round = %v, want ErrReviewYieldHistoryInconsistent", err)
	}
	// Round 1's metrics presented as round 2's name the wrong commits.
	_, err = store.DeriveReviewYieldHistory(records, nil, findings,
		map[int]domain.ReviewRoundDiffMetrics{2: roundDiffMetrics(1)})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("metrics naming other commits = %v, want ErrParentKeyMismatch", err)
	}
}

// TestReviewRoundDiffMetricsReadsFailClosed proves every read re-runs the
// write-time checks: no copied column and no stored commit name is trusted.
func TestReviewRoundDiffMetricsReadsFailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		want   error
		tamper func(*testing.T, *sql.DB, domain.RunID)
	}{
		{name: "body changed under its digest", want: store.ErrRowInconsistent, tamper: func(t *testing.T, db *sql.DB, runID domain.RunID) {
			t.Helper()
			if _, err := db.Exec(`UPDATE review_round_diff_metrics
SET body = json_set(body, '$.metrics.cumulative.additions', 1) WHERE run_id = ? AND round = 2`, runID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "coherent body naming another head", want: domain.ErrParentKeyMismatch, tamper: func(t *testing.T, db *sql.DB, runID domain.RunID) {
			t.Helper()
			var body string
			if err := db.QueryRow(`SELECT json_set(body,
    '$.metrics.cumulative.head_sha', 'head-other', '$.metrics.round.head_sha', 'head-other')
FROM review_round_diff_metrics WHERE run_id = ? AND round = 2`, runID).Scan(&body); err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE review_round_diff_metrics
SET body = ?, body_digest = ? WHERE run_id = ? AND round = 2`,
				body, contentaddr.Sum([]byte(body)), runID); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "copied round key moved", want: store.ErrRowInconsistent, tamper: func(t *testing.T, db *sql.DB, runID domain.RunID) {
			t.Helper()
			if _, err := db.Exec(`UPDATE review_round_diff_metrics
SET round = 3 WHERE run_id = ? AND round = 2`, runID); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "store.db")
			runID := domain.RunID("run-metrics-tamper")
			// Round 3 exists without metrics so a moved round key still
			// satisfies the foreign key and reaches reconstruction.
			st := seedDiffMetricsRun(t, path, runID, 3, 1, 2)
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			raw, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			tc.tamper(t, raw, runID)
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := storetest.Open(t, path, store.Options{})
			defer func() { _ = reopened.Close() }()
			for name, read := range map[string]func(*store.ReadTx) error{
				"list": func(tx *store.ReadTx) error {
					_, err := tx.ListReviewRoundDiffMetrics(ctx, runID)
					return err
				},
				"yield history": func(tx *store.ReadTx) error {
					_, err := tx.ReviewYieldHistory(ctx, runID)
					return err
				},
			} {
				if err := reopened.Read(ctx, read); !errors.Is(err, tc.want) {
					t.Fatalf("%s over a tampered metrics row = %v, want %v", name, err, tc.want)
				}
			}
		})
	}
}

// TestReviewYieldHistoryCarriesRecordedDiffMetrics proves the derived history
// carries each recorded round's metrics, shows an unrecorded round as a gap,
// and at a decision excludes metrics from later rounds.
func TestReviewYieldHistoryCarriesRecordedDiffMetrics(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-metrics-history")
	st := seedDiffMetricsRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 3, 1, 3)
	defer func() { _ = st.Close() }()

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		history, err := tx.ReviewYieldHistory(ctx, runID)
		if err != nil {
			return err
		}
		if len(history.Rounds) != 3 {
			t.Fatalf("history has %d rounds", len(history.Rounds))
		}
		for index, want := range []*domain.ReviewRoundDiffMetrics{
			new(roundDiffMetrics(1)), nil, new(roundDiffMetrics(3)),
		} {
			if got := history.Rounds[index].DiffMetrics; !reflect.DeepEqual(got, want) {
				t.Fatalf("round %d metrics = %+v, want %+v", index+1, got, want)
			}
		}
		atDecision, err := tx.ReviewYieldHistoryAtDecision(ctx, runID, 2)
		if err != nil {
			return err
		}
		if len(atDecision.Rounds) != 2 || atDecision.Rounds[0].DiffMetrics == nil ||
			atDecision.Rounds[1].DiffMetrics != nil {
			t.Fatalf("history at round 2 = %+v", atDecision.Rounds)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}
