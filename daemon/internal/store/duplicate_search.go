package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const putDuplicateSearchSQL = `
INSERT INTO duplicate_searches (run_id, adjudication_digest, finding_id, content_digest, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (adjudication_digest, finding_id) DO NOTHING`

// validateDuplicateSearchBinding re-runs the joins the record's copied values
// claim (plan §7 Finding Adjudication): the adjudication artifact exists, is
// the run's, and routes the finding the way the record's kind says. Each row
// it reads is immutable, so a record that bound once keeps binding.
func (tx *ReadTx) validateDuplicateSearchBinding(ctx context.Context, search domain.DuplicateSearch) error {
	artifact, err := tx.GetFindingAdjudication(ctx, search.AdjudicationDigest)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("adjudication %q: %w", search.AdjudicationDigest, domain.ErrParentKeyMismatch)
	}
	if err != nil {
		return err
	}
	if artifact.RunID != search.RunID {
		return fmt.Errorf("adjudication run %q: %w", artifact.RunID, domain.ErrParentKeyMismatch)
	}
	route, err := followUpSourceRoute(search.Kind)
	if err != nil {
		return err
	}
	if !slices.ContainsFunc(artifact.Entries, func(entry domain.FindingAdjudicationEntry) bool {
		return entry.FindingID == search.FindingID && entry.Route == route
	}) {
		return fmt.Errorf("finding %q has no %s entry: %w", search.FindingID, route, domain.ErrParentKeyMismatch)
	}
	return nil
}

// PutDuplicateSearch records one finding's duplicate search under one
// adjudication artifact. A byte-identical replay converges; a different
// record for the same entry is an immutable conflict.
func (tx *WriteTx) PutDuplicateSearch(ctx context.Context, search domain.DuplicateSearch) error {
	body, err := encode(search)
	if err != nil {
		return fmt.Errorf("put duplicate search %q finding %q: %w", search.AdjudicationDigest, search.FindingID, err)
	}
	// Decode what is about to be stored through the read path's decoder, so a
	// body the reads would refuse (over the size bound) is never written.
	if _, err := domain.DecodeDuplicateSearch([]byte(body)); err != nil {
		return fmt.Errorf("put duplicate search %q finding %q: %w", search.AdjudicationDigest, search.FindingID, err)
	}
	if err := tx.validateDuplicateSearchBinding(ctx, search); err != nil {
		return fmt.Errorf("put duplicate search %q finding %q binding: %w",
			search.AdjudicationDigest, search.FindingID, err)
	}
	// Reconstruct an existing row before the replay comparison, so corruption
	// in a copied lookup column cannot hide behind an unchanged body.
	if _, err := tx.GetDuplicateSearch(ctx, search.AdjudicationDigest, search.FindingID); err != nil &&
		!errors.Is(err, ErrNotFound) {
		return fmt.Errorf("put duplicate search %q finding %q existing row: %w",
			search.AdjudicationDigest, search.FindingID, err)
	}
	if err := tx.putImmutable(ctx, putDuplicateSearchSQL,
		[]any{search.RunID, search.AdjudicationDigest, search.FindingID, search.Digest, reviewBodyDigest(body), body},
		`SELECT body_digest || body FROM duplicate_searches WHERE adjudication_digest = ? AND finding_id = ?`,
		[]any{search.AdjudicationDigest, search.FindingID}, reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put duplicate search %q finding %q: %w", search.AdjudicationDigest, search.FindingID, err)
	}
	return nil
}

// GetDuplicateSearch reconstructs the search for one finding under one
// adjudication artifact. ErrNotFound means the finding was never searched
// under it.
//
// Reconstruction re-runs the body integrity digest, the strict decode with
// the handle rule (so a stored duplicate_of outside the record's own
// candidates is refused), the agreement of the copied keys with the decoded
// body, and the binding to the adjudication. No copied column is trusted.
func (tx *ReadTx) GetDuplicateSearch(
	ctx context.Context, adjudicationDigest domain.Digest, findingID domain.FindingID,
) (domain.DuplicateSearch, error) {
	var runID, storedAdjudication, storedFinding, contentDigest, bodyDigest string
	var body []byte
	err := tx.tx.QueryRowContext(ctx, `SELECT run_id, adjudication_digest, finding_id, content_digest, body_digest, body
		FROM duplicate_searches WHERE adjudication_digest = ? AND finding_id = ?`,
		adjudicationDigest, findingID).Scan(&runID, &storedAdjudication, &storedFinding, &contentDigest, &bodyDigest, &body)
	if err != nil {
		return domain.DuplicateSearch{}, fmt.Errorf("get duplicate search %q finding %q: %w",
			adjudicationDigest, findingID, notFoundOr(err))
	}
	fail := func(err error) (domain.DuplicateSearch, error) {
		return domain.DuplicateSearch{}, fmt.Errorf("get duplicate search %q finding %q: %w",
			adjudicationDigest, findingID, err)
	}
	if bodyDigest != reviewBodyDigest(string(body)) {
		return fail(errRowInconsistent)
	}
	search, err := domain.DecodeDuplicateSearch(body)
	if err != nil {
		return fail(fmt.Errorf("stored row invalid: %w: %w", errRowInconsistent, err))
	}
	if string(search.RunID) != runID || string(search.AdjudicationDigest) != storedAdjudication ||
		string(search.FindingID) != storedFinding || string(search.Digest) != contentDigest {
		return fail(errRowInconsistent)
	}
	if err := tx.validateDuplicateSearchBinding(ctx, search); err != nil {
		return fail(err)
	}
	return search, nil
}
