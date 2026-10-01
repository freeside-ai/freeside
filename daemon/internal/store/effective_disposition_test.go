package store_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func effectiveDispositions(
	t *testing.T, st *store.Store, runID domain.RunID, throughRound int,
) []store.EffectiveFindingDisposition {
	t.Helper()
	ctx := context.Background()
	var out []store.EffectiveFindingDisposition
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		out, err = tx.EffectiveFindingDispositions(ctx, runID, throughRound)
		return err
	}); err != nil {
		t.Fatalf("effective dispositions through round %d: %v", throughRound, err)
	}
	return out
}

// TestEffectiveDispositionsWithoutSupersessionAreTheStoredOnes pins that a run
// with no supersession record reads exactly as before: the reader returns each
// finding's latest stored disposition unchanged, and the existing disposition
// and decision reads still load.
func TestEffectiveDispositionsWithoutSupersessionAreTheStoredOnes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-effective-unreversed")
	fx := seedSupersession(t, filepath.Join(t.TempDir(), "store.db"), runID, domain.ActionContinueUnderPolicy)
	defer func() { _ = fx.st.Close() }()

	var stored, otherStored []domain.ReviewDispositionRecord
	if err := fx.st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if stored, err = tx.ListFindingDispositions(ctx, runID); err != nil {
			return err
		}
		if otherStored, err = tx.ListFindingDispositions(ctx, supersessionOtherRun); err != nil {
			return err
		}
		if _, err := tx.ReviewDiminishingDecision(ctx, fx.decision.Item.ID); err != nil {
			return err
		}
		listed, err := tx.ListFindingDispositionSupersessions(ctx, runID)
		if err != nil || len(listed) != 0 {
			t.Fatalf("supersessions = %v, %v, want none", listed, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("existing reads: %v", err)
	}
	if len(stored) != 1 || len(otherStored) != 4 {
		t.Fatalf("seeded dispositions = %d and %d, want 1 and 4", len(stored), len(otherStored))
	}
	for _, tc := range []struct {
		runID  domain.RunID
		stored []domain.ReviewDispositionRecord
	}{{runID, stored}, {supersessionOtherRun, otherStored}} {
		want := make([]store.EffectiveFindingDisposition, len(tc.stored))
		for i, disposition := range tc.stored {
			want[i] = store.EffectiveFindingDisposition{Stored: disposition, Effective: disposition.Disposition}
		}
		for _, throughRound := range []int{1, 2, 9} {
			if got := effectiveDispositions(t, fx.st, tc.runID, throughRound); !reflect.DeepEqual(got, want) {
				t.Fatalf("%s through round %d = %#v, want the stored dispositions %#v",
					tc.runID, throughRound, got, want)
			}
		}
	}
}

func TestEffectiveDispositionReadsDeclinedFromTheReversingRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-effective-reversed")
	fx := seedSupersession(t, filepath.Join(t.TempDir(), "store.db"), runID, domain.ActionContinueUnderPolicy)
	defer func() { _ = fx.st.Close() }()
	if err := putSupersession(fx.st, fx.record); err != nil {
		t.Fatalf("put: %v", err)
	}

	before := effectiveDispositions(t, fx.st, runID, 1)
	if len(before) != 1 || before[0].Stored.FindingID != "finding-a" ||
		before[0].Effective != domain.ReviewDispositionFixed || before[0].Supersession != nil {
		t.Fatalf("before the reversing round = %#v, want the stored fixed disposition", before)
	}
	for _, throughRound := range []int{2, 3, 9} {
		got := effectiveDispositions(t, fx.st, runID, throughRound)
		if len(got) != 1 || !reflect.DeepEqual(got[0].Stored, before[0].Stored) ||
			got[0].Stored.Disposition != domain.ReviewDispositionFixed ||
			got[0].Effective != domain.ReviewDispositionDeclined ||
			got[0].Supersession == nil || !reflect.DeepEqual(*got[0].Supersession, fx.record) {
			t.Fatalf("through round %d = %#v, want declined through the record over the stored fixed row",
				throughRound, got)
		}
	}
	// The other run has no record, so its findings are untouched.
	for _, effective := range effectiveDispositions(t, fx.st, supersessionOtherRun, 9) {
		if effective.Effective != effective.Stored.Disposition || effective.Supersession != nil {
			t.Fatalf("unreversed finding changed: %#v", effective)
		}
	}

	if err := fx.st.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.EffectiveFindingDispositions(ctx, runID, 0)
		return err
	}); !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("through round 0 = %v, want ErrNonPositive", err)
	}
}

// TestEffectiveDispositionFollowsALaterStoredRow covers a finding that several
// rounds list. Round 3 lists the reversed finding again and it is fixed again,
// so from round 3 on the later row stands and the record, which targets the
// round-1 row, no longer applies.
func TestEffectiveDispositionFollowsALaterStoredRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-effective-relisted")
	fx := seedSupersession(t, filepath.Join(t.TempDir(), "store.db"), runID, domain.ActionContinueUnderPolicy)
	defer func() { _ = fx.st.Close() }()
	if err := putSupersession(fx.st, fx.record); err != nil {
		t.Fatalf("put: %v", err)
	}
	relisted := adjudicationReviewRecord(
		t, runID, 3, []domain.FindingID{"finding-a"}, supersessionAt.Add(3*time.Hour))
	if err := fx.st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRecord(ctx, relisted, []domain.Finding{
			adjudicationFinding("finding-a", runID, "daemon/a.go", supersessionAt),
		}); err != nil {
			return err
		}
		if err := putDriftRound(ctx, t, tx, runID, 4); err != nil {
			return err
		}
		return tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
			FindingID: "finding-a", RunID: runID, Round: 3,
			Disposition: domain.ReviewDispositionFixed, Reason: "fixed again",
			RemediationInvocationID: domain.InvocationID("review-" + string(runID) + "-4"),
			CreatedAt:               supersessionAt.Add(4 * time.Hour),
		})
	}); err != nil {
		t.Fatalf("relist and fix again: %v", err)
	}

	if got := effectiveDispositions(t, fx.st, runID, 2); len(got) != 1 || got[0].Stored.Round != 1 ||
		got[0].Effective != domain.ReviewDispositionDeclined {
		t.Fatalf("through round 2 = %#v, want the reversed round-1 row", got)
	}
	for _, throughRound := range []int{3, 4} {
		got := effectiveDispositions(t, fx.st, runID, throughRound)
		if len(got) != 1 || got[0].Stored.Round != 3 ||
			got[0].Effective != domain.ReviewDispositionFixed || got[0].Supersession != nil {
			t.Fatalf("through round %d = %#v, want the later fixed row", throughRound, got)
		}
	}
}

// TestEffectiveDispositionsFailClosedOnATamperedRecord proves the reader never
// falls back to the stored disposition when a record no longer verifies.
func TestEffectiveDispositionsFailClosedOnATamperedRecord(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-effective-tamper")
	path := filepath.Join(t.TempDir(), "store.db")
	fx := seedSupersession(t, path, runID, domain.ActionContinueUnderPolicy)
	if err := putSupersession(fx.st, fx.record); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := fx.st.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE finding_disposition_supersessions SET run_id = ?`,
		supersessionOtherRun); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := storetest.Open(t, path, store.Options{})
	defer func() { _ = reopened.Close() }()
	for _, id := range []domain.RunID{runID, supersessionOtherRun} {
		if err := reopened.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.EffectiveFindingDispositions(ctx, id, 2)
			return err
		}); !errors.Is(err, store.ErrRowInconsistent) {
			t.Fatalf("%s over a tampered record = %v, want ErrRowInconsistent", id, err)
		}
	}
}
