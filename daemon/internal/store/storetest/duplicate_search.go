package storetest

import (
	"context"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// PutNewDuplicateSearch records that a follow-up filing's source was searched
// and no open issue tracks it: an empty candidate set answering "new". The
// filing gate refuses to admit or approve a filing without one, so a test
// that allocates a filing writes it first, after the adjudication.
func PutNewDuplicateSearch(
	ctx context.Context,
	tx *store.WriteTx,
	runID domain.RunID,
	source domain.FollowUpFilingSource,
	repository domain.FollowUpFilingRepository,
	at time.Time,
) error {
	search, err := domain.NewDuplicateSearch(domain.DuplicateSearchInput{
		RunID: runID, AdjudicationDigest: source.AdjudicationDigest, FindingID: source.FindingID,
		Kind: source.Kind, Repository: repository, CandidateLimit: domain.MaxDuplicateSearchCandidates,
		CreatedAt: at.UTC(),
	})
	if err != nil {
		return err
	}
	return tx.PutDuplicateSearch(ctx, search)
}
