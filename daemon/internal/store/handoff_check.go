package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Handoff checks (migration 0126; issue #2644) record each publication's
// handoff: the tip the audit resolved, sealed once, and for a moved tip the
// verified merge into it, recorded once. The rows are ordinary state (Tier
// B): they decode with plain validation. The authority they grant is
// re-checked where it is spent, in the publication decision
// (publish.storePublicationDecision).

const handoffCheckColumns = `run_id, publication_id, head_sha, evidence_base_sha, tip_sha,
    merge_sha, verification_proof_digest`

// SealHandoffTip records the tip a publication's handoff resolved. It is the
// first write of the row and takes no merge. Sealing the same values again
// is a no-op, so a retry converges; any other value, a different tip above
// all, is ErrImmutableConflict: the tip is never re-resolved for the
// publication.
func (tx *InternalTx) SealHandoffTip(ctx context.Context, check domain.HandoffCheck, at time.Time) error {
	if err := check.Validate(); err != nil {
		return fmt.Errorf("seal handoff tip: %w", err)
	}
	if check.Merge != nil {
		return fmt.Errorf("seal handoff tip with a merge: %w", domain.ErrHandoffCheckInconsistent)
	}
	existing, err := tx.GetHandoffCheck(ctx, check.RunID, check.PublicationID)
	switch {
	case err == nil:
		existing.Merge = nil
		if existing != check {
			return fmt.Errorf("seal handoff tip for %s: %w", check.PublicationID, ErrImmutableConflict)
		}
		return nil
	case !errors.Is(err, ErrNotFound):
		return err
	}
	if _, err := tx.tx.ExecContext(ctx, `INSERT INTO publication_handoff_checks
        (run_id, publication_id, head_sha, evidence_base_sha, tip_sha, resolved_at)
        VALUES (?, ?, ?, ?, ?, ?)`,
		check.RunID, check.PublicationID, check.HeadSHA, check.EvidenceBaseSHA, check.TipSHA,
		formatTime(at)); err != nil {
		return fmt.Errorf("seal handoff tip for %s: %w", check.PublicationID, err)
	}
	return nil
}

// ProveHandoffMerge records the verified merge on a sealed handoff whose tip
// moved. The proof must already be recorded and name this head, this tip,
// and this merge. Recording the same merge again is a no-op; another merge
// is ErrImmutableConflict.
func (tx *InternalTx) ProveHandoffMerge(
	ctx context.Context, runID domain.RunID, publicationID domain.InvocationID,
	merge domain.HandoffMerge, at time.Time,
) error {
	check, err := tx.GetHandoffCheck(ctx, runID, publicationID)
	if err != nil {
		return fmt.Errorf("prove handoff merge: %w", err)
	}
	if check.Merge != nil {
		if *check.Merge == merge {
			return nil
		}
		return fmt.Errorf("prove handoff merge for %s: %w", publicationID, ErrImmutableConflict)
	}
	check.Merge = &merge
	if err := check.Validate(); err != nil {
		return fmt.Errorf("prove handoff merge: %w", err)
	}
	proof, err := tx.GetCheckProof(ctx, merge.VerificationProofDigest)
	if err != nil {
		return fmt.Errorf("prove handoff merge: %w", err)
	}
	if proof.CandidateHead != check.HeadSHA || proof.Base == nil ||
		proof.Base.BaseSHA != check.TipSHA || proof.MergeSHA != merge.MergeSHA {
		return fmt.Errorf("handoff merge proof %s does not prove %s into %s: %w",
			merge.VerificationProofDigest, check.HeadSHA, check.TipSHA, domain.ErrParentKeyMismatch)
	}
	if _, err := tx.tx.ExecContext(ctx, `UPDATE publication_handoff_checks
        SET merge_sha = ?, verification_proof_digest = ?, proved_at = ?
        WHERE run_id = ? AND publication_id = ? AND merge_sha IS NULL`,
		merge.MergeSHA, merge.VerificationProofDigest, formatTime(at), runID, publicationID); err != nil {
		return fmt.Errorf("prove handoff merge for %s: %w", publicationID, err)
	}
	return nil
}

// GetHandoffCheck returns the publication's handoff check, ErrNotFound when
// its tip was never sealed.
func (tx *ReadTx) GetHandoffCheck(
	ctx context.Context, runID domain.RunID, publicationID domain.InvocationID,
) (domain.HandoffCheck, error) {
	var (
		check       domain.HandoffCheck
		mergeSHA    sql.NullString
		proofDigest sql.NullString
	)
	err := tx.tx.QueryRowContext(ctx, `SELECT `+handoffCheckColumns+`
        FROM publication_handoff_checks WHERE run_id = ? AND publication_id = ?`,
		runID, publicationID).Scan(
		&check.RunID, &check.PublicationID, &check.HeadSHA, &check.EvidenceBaseSHA, &check.TipSHA,
		&mergeSHA, &proofDigest)
	if err != nil {
		return domain.HandoffCheck{}, fmt.Errorf("handoff check %s: %w", publicationID, notFoundOr(err))
	}
	if mergeSHA.Valid != proofDigest.Valid {
		return domain.HandoffCheck{}, fmt.Errorf("handoff check %s: %w", publicationID, errRowInconsistent)
	}
	if mergeSHA.Valid {
		check.Merge = &domain.HandoffMerge{
			MergeSHA: mergeSHA.String, VerificationProofDigest: domain.Digest(proofDigest.String),
		}
	}
	if err := check.Validate(); err != nil {
		return domain.HandoffCheck{}, fmt.Errorf("handoff check %s: %w", publicationID, errors.Join(err, errRowInconsistent))
	}
	return check, nil
}
