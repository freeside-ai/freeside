package store_test

import (
	"cmp"
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestRunPostCandidatesListsTheSubjectsFilingsAndComments shows the read
// lists a subject's filing and comment instances, card or no card, in
// instance order, and nothing from another subject.
func TestRunPostCandidatesListsTheSubjectsFilingsAndComments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-outcomes")
	other := seedFiling(t, ctx, st, "project-other")

	filing, err := allocateFiling(ctx, st, fx.proposal(t, fx.input(fx.deferredSource())), "event-filing")
	if err != nil {
		t.Fatal(err)
	}
	comment, err := allocateFiling(ctx, st, fx.commentProposal(t, seedComment(t, ctx, st, fx)), "event-comment")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := allocateFiling(ctx, st, other.proposal(t, other.input(other.deferredSource())), "event-other"); err != nil {
		t.Fatal(err)
	}

	read := func(handle domain.OpaqueSubjectHandle) []store.RunPostCandidate {
		t.Helper()
		var got []store.RunPostCandidate
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, err = tx.RunPostCandidates(ctx, handle)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	want := []store.RunPostCandidate{
		{InstanceID: filing.ID, Kind: domain.EffectFollowUpFiling},
		{InstanceID: comment.ID, Kind: domain.EffectIssueComment},
	}
	slices.SortFunc(want, func(a, b store.RunPostCandidate) int {
		return cmp.Compare(a.InstanceID, b.InstanceID)
	})
	if got := read(fx.handle); !slices.Equal(got, want) {
		t.Fatalf("candidates = %#v, want %#v", got, want)
	}
	if got := read("work-unit-with-nothing"); got == nil || len(got) != 0 {
		t.Fatalf("empty subject = %#v, want an empty, non-nil list", got)
	}
}
