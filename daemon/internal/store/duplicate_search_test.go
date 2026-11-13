package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// duplicateRecord is a search of finding under fx's adjudication that names
// its first candidate, open issue 41, as already tracking it.
func duplicateRecord(
	t *testing.T, fx filingFixture, finding domain.FindingID, kind domain.FollowUpSourceKind,
) domain.DuplicateSearch {
	t.Helper()
	search, err := domain.NewDuplicateSearch(domain.DuplicateSearchInput{
		RunID: fx.runID, AdjudicationDigest: fx.adjudication.Digest, FindingID: finding, Kind: kind,
		Repository:     domain.FollowUpFilingRepository{Repo: fx.target.Repo, RepositoryID: fx.target.RepositoryID},
		CandidateLimit: domain.MaxDuplicateSearchCandidates,
		Candidates: []domain.DuplicateSearchCandidate{
			{Handle: domain.DuplicateSearchHandle(1), Number: 41, Title: "Bound the sync client's retries"},
			{Handle: domain.DuplicateSearchHandle(2), Number: 7, Title: "Unrelated cleanup"},
		},
		DuplicateOf: domain.DuplicateSearchHandle(1), Confidence: domain.ConfidenceHigh,
		Reason:    "candidate-1 already tracks the retry budget.",
		CreatedAt: filingAt.Add(3 * time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	return search
}

func putDuplicateSearch(ctx context.Context, st *store.Store, search domain.DuplicateSearch) error {
	return st.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDuplicateSearch(ctx, search) })
}

func getDuplicateSearch(
	ctx context.Context, st *store.Store, adjudication domain.Digest, finding domain.FindingID,
) (domain.DuplicateSearch, error) {
	var search domain.DuplicateSearch
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		search, err = tx.GetDuplicateSearch(ctx, adjudication, finding)
		return err
	})
	return search, err
}

// TestDuplicateSearchStoreRoundTripAndBinding: a record reads back as
// written, a replay converges, a different record for the same entry
// conflicts, and a record whose copied values the adjudication does not back
// is refused.
func TestDuplicateSearchStoreRoundTripAndBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	fx := seedFiling(t, ctx, st, "project-1")
	pending := fx.findings[3].ID
	search := duplicateRecord(t, fx, pending, domain.FollowUpSourceDeferredDisposition)
	if err := putDuplicateSearch(ctx, st, search); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := getDuplicateSearch(ctx, st, fx.adjudication.Digest, pending)
	if err != nil || got.Digest != search.Digest {
		t.Fatalf("read back = %v, %v; want digest %s", got.Digest, err, search.Digest)
	}
	if err := putDuplicateSearch(ctx, st, search); err != nil {
		t.Fatalf("replay: %v", err)
	}
	other := search
	other.Reason = "a different judgment"
	if other.Digest, err = other.ComputeDigest(); err != nil {
		t.Fatal(err)
	}
	if err := putDuplicateSearch(ctx, st, other); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("second record for the entry = %v, want ErrImmutableConflict", err)
	}
	if _, err := getDuplicateSearch(ctx, st, fx.adjudication.Digest, fx.findings[2].ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read of an unsearched finding = %v, want ErrNotFound", err)
	}

	unknown := domain.Digest(contentaddr.Sum([]byte("no such adjudication")))
	for name, mutate := range map[string]func(*domain.DuplicateSearchInput){
		"a declined finding as deferred": func(in *domain.DuplicateSearchInput) { in.FindingID = fx.findings[2].ID },
		"a deferred finding as separate work": func(in *domain.DuplicateSearchInput) {
			in.Kind = domain.FollowUpSourceSeparateWorkVerdict
		},
		"another run":          func(in *domain.DuplicateSearchInput) { in.RunID = "project-2-run" },
		"unknown adjudication": func(in *domain.DuplicateSearchInput) { in.AdjudicationDigest = unknown },
	} {
		input := domain.DuplicateSearchInput{
			RunID: fx.runID, AdjudicationDigest: fx.adjudication.Digest, FindingID: pending,
			Kind: domain.FollowUpSourceDeferredDisposition, CandidateLimit: 1,
			Repository: domain.FollowUpFilingRepository{Repo: fx.target.Repo, RepositoryID: fx.target.RepositoryID},
			CreatedAt:  filingAt,
		}
		mutate(&input)
		unbound, err := domain.NewDuplicateSearch(input)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := putDuplicateSearch(ctx, st, unbound); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s: put = %v, want ErrParentKeyMismatch", name, err)
		}
	}
}

// TestDuplicateSearchReadRefusesTamperedTarget: a stored record whose
// duplicate_of names a handle outside its own candidates is refused on read,
// even with every digest recomputed to match.
func TestDuplicateSearchReadRefusesTamperedTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	st := storetest.Open(t, path, store.Options{})
	fx := seedFiling(t, ctx, st, "project-1")
	source := fx.deferredSource()
	st = forgeDuplicateTarget(t, ctx, path, st, source)
	if _, err := getDuplicateSearch(ctx, st, source.AdjudicationDigest, source.FindingID); !errors.Is(err, domain.ErrDuplicateSearchInconsistent) {
		t.Fatalf("read of a forged target = %v, want ErrDuplicateSearchInconsistent", err)
	}
}

// forgeDuplicateTarget rewrites the source's stored search so its
// duplicate_of names a handle outside its candidates, with the content and
// body digests recomputed, so only the handle rule can refuse it. It returns
// the reopened store.
func forgeDuplicateTarget(
	t *testing.T, ctx context.Context, path string, st *store.Store, source domain.FollowUpFilingSource,
) *store.Store {
	t.Helper()
	forged, err := getDuplicateSearch(ctx, st, source.AdjudicationDigest, source.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	forged.DuplicateOf = domain.DuplicateSearchHandle(9)
	if forged.Digest, err = forged.ComputeDigest(); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	return tamperStore(t, path, st, `UPDATE duplicate_searches SET body = ?, body_digest = ?, content_digest = ?
		WHERE adjudication_digest = ? AND finding_id = ?`,
		string(body), contentaddr.Sum(body), string(forged.Digest),
		string(source.AdjudicationDigest), string(source.FindingID))
}
