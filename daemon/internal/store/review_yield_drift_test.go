package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestReviewYieldHistoryShowsAVerdictOnceALaterRoundIsRecorded proves the
// yield-history rule: an audited round shows its verdict only in a history
// that also holds a later round, so recording the audit never changes a
// history that ended at the audited round.
func TestReviewYieldHistoryShowsAVerdictOnceALaterRoundIsRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-drift-history")
	st := seedDriftAuditRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 3,
		map[int]domain.DriftVerdict{
			1: domain.DriftVerdictOverHardened, 3: domain.DriftVerdictStuck,
		})
	defer func() { _ = st.Close() }()

	verdicts := func(history domain.ReviewYieldHistory) []string {
		got := make([]string, len(history.Rounds))
		for index, round := range history.Rounds {
			if round.DriftVerdict != nil {
				got[index] = string(*round.DriftVerdict)
			}
		}
		return got
	}
	read := func(throughRound int) []string {
		t.Helper()
		var history domain.ReviewYieldHistory
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			if throughRound == 0 {
				history, err = tx.ReviewYieldHistory(ctx, runID)
			} else {
				history, err = tx.ReviewYieldHistoryAtDecision(ctx, runID, throughRound)
			}
			return err
		}); err != nil {
			t.Fatalf("yield history through %d: %v", throughRound, err)
		}
		return verdicts(history)
	}

	// Round 2 was never audited; round 3 is the last round, so its audit does
	// not show yet.
	if got, want := read(0), []string{"over_hardened", "", ""}; !slices.Equal(got, want) {
		t.Fatalf("verdicts = %q, want %q", got, want)
	}
	// A history cut at the audited round is the history an item snapshotted
	// before the audit existed.
	if got, want := read(1), []string{""}; !slices.Equal(got, want) {
		t.Fatalf("verdicts through round 1 = %q, want %q", got, want)
	}
	if got, want := read(2), []string{"over_hardened", ""}; !slices.Equal(got, want) {
		t.Fatalf("verdicts through round 2 = %q, want %q", got, want)
	}

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return putDriftRound(ctx, t, tx, runID, 4)
	}); err != nil {
		t.Fatalf("record round 4: %v", err)
	}
	if got, want := read(0), []string{"over_hardened", "", "stuck", ""}; !slices.Equal(got, want) {
		t.Fatalf("verdicts after round 4 = %q, want %q", got, want)
	}
	if got, want := read(3), []string{"over_hardened", "", ""}; !slices.Equal(got, want) {
		t.Fatalf("verdicts through round 3 after round 4 = %q, want %q", got, want)
	}
}

// TestDeriveReviewYieldHistoryPlacesDriftVerdicts proves the derivation
// applies the rule itself: it refuses a verdict for a round it was not given
// and drops the last round's verdict whatever the caller passes.
func TestDeriveReviewYieldHistoryPlacesDriftVerdicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-drift-derive")
	st := seedDriftAuditRun(t, filepath.Join(t.TempDir(), "store.db"), runID, 2, nil)
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

	history, err := store.DeriveReviewYieldHistory(records, nil, findings, nil,
		map[int]domain.DriftVerdict{
			1: domain.DriftVerdictConverged, 2: domain.DriftVerdictStuck,
		})
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got := history.Rounds[0].DriftVerdict; got == nil || *got != domain.DriftVerdictConverged {
		t.Fatalf("round 1 verdict = %v, want converged", got)
	}
	if got := history.Rounds[1].DriftVerdict; got != nil {
		t.Fatalf("last round verdict = %q, want none", *got)
	}

	_, err = store.DeriveReviewYieldHistory(records, nil, findings, nil,
		map[int]domain.DriftVerdict{3: domain.DriftVerdictStuck})
	if !errors.Is(err, domain.ErrReviewYieldHistoryInconsistent) {
		t.Fatalf("verdict for an absent round = %v, want ErrReviewYieldHistoryInconsistent", err)
	}
	_, err = store.DeriveReviewYieldHistory(records, nil, findings, nil,
		map[int]domain.DriftVerdict{1: "drifting"})
	if !errors.Is(err, domain.ErrReviewYieldHistoryInconsistent) {
		t.Fatalf("unknown verdict = %v, want ErrReviewYieldHistoryInconsistent", err)
	}
}
