package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const putFindingAdjudicationSQL = `
INSERT INTO finding_adjudications
    (run_id, round, content_digest, finding_batch_digest, approved_spec_digest,
     instruction_snapshot_digest, resolved_policy_digest, created_at, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, round) DO NOTHING`

// validateFindingAdjudicationBinding re-runs every authoritative join instead of
// trusting the artifact's copied keys: the review round must exist, the artifact's
// entry finding set must equal that round's finding set exactly, its instruction
// snapshot must equal the round's authoritative instruction binding, and its
// approved-spec and resolved-policy digests must equal the run's authoritative
// values. A missing record, a foreign or missing finding, a duplicate entry, or an
// instruction/spec/policy digest that disagrees with its authority fails with
// ErrParentKeyMismatch. Together these re-gate every caller-supplied trust bit the
// artifact carries: the finding batch, the instruction snapshot, and the spec and
// policy the adjudication's routing decisions rest on.
func (tx *ReadTx) validateFindingAdjudicationBinding(
	ctx context.Context, artifact domain.FindingAdjudication,
) error {
	var invocationID string
	if err := tx.tx.QueryRowContext(ctx, `SELECT invocation_id FROM review_records
        WHERE run_id = ? AND round = ?`, artifact.RunID, artifact.Round).Scan(&invocationID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	record, err := tx.GetReviewRecord(ctx, domain.InvocationID(invocationID))
	if err != nil {
		return err
	}
	entryIDs := make([]domain.FindingID, 0, len(artifact.Entries))
	seen := make(map[domain.FindingID]struct{}, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		if _, duplicate := seen[entry.FindingID]; duplicate {
			return domain.ErrParentKeyMismatch
		}
		seen[entry.FindingID] = struct{}{}
		entryIDs = append(entryIDs, entry.FindingID)
	}
	recordIDs := slices.Clone(record.FindingIDs)
	slices.Sort(recordIDs)
	slices.Sort(entryIDs)
	if !slices.Equal(entryIDs, recordIDs) {
		return domain.ErrParentKeyMismatch
	}
	// The instruction snapshot must equal the review round's authoritative
	// instruction binding (already loaded above): an adjudication naming a
	// different snapshot would be reconstructed as though it used trusted
	// repository instructions the bound reviewer never received, so its cited
	// rules and compatibility routing would rest on instructions outside the
	// round's trusted base.
	if artifact.InstructionSnapshotDigest != record.InstructionDigest {
		return domain.ErrParentKeyMismatch
	}
	// The run's spec and policy digests are fixed at creation, so an artifact
	// naming different ones would record an adjudication bound to a spec or
	// policy the run is not — authorizing routing under a different work contract
	// or policy. They are the authority right here; take them from the run rather
	// than trusting the artifact's copied digests (mirrors requireRecordedAttempt).
	run, err := tx.GetRun(ctx, artifact.RunID)
	if err != nil {
		return err
	}
	if artifact.ApprovedSpecDigest != run.SpecDigest || artifact.ResolvedPolicyDigest != run.PolicyDigest {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// PutFindingAdjudication persists one round's immutable adjudication artifact.
// It binds the artifact to its review round and requires the entry finding set
// to equal that round's finding set, then writes write-once by (run_id, round):
// a byte-identical replay converges, and a differing artifact for the same round
// is an immutable conflict.
func (tx *WriteTx) PutFindingAdjudication(
	ctx context.Context, artifact domain.FindingAdjudication,
) error {
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("put finding adjudication %q round %d: %w", artifact.RunID, artifact.Round, err)
	}
	if err := tx.validateFindingAdjudicationBinding(ctx, artifact); err != nil {
		return fmt.Errorf("put finding adjudication %q round %d binding: %w", artifact.RunID, artifact.Round, err)
	}
	// A replay must reconstruct the already-persisted row before putImmutable's
	// byte comparison. Otherwise corruption limited to a copied lookup column
	// (content_digest, created_at, ...) could be hidden by an unchanged canonical
	// body, and every later keyed read would then fail its cross-check (mirrors
	// PutFindingDisposition).
	if _, err := tx.GetFindingAdjudicationForRound(ctx, artifact.RunID, artifact.Round); err != nil &&
		!errors.Is(err, ErrNotFound) {
		return fmt.Errorf("put finding adjudication %q round %d existing row: %w", artifact.RunID, artifact.Round, err)
	}
	body, err := encode(artifact)
	if err != nil {
		return fmt.Errorf("put finding adjudication %q round %d: %w", artifact.RunID, artifact.Round, err)
	}
	if err := tx.putImmutable(ctx, putFindingAdjudicationSQL,
		[]any{
			artifact.RunID, artifact.Round, artifact.Digest, artifact.FindingBatchDigest,
			artifact.ApprovedSpecDigest, artifact.InstructionSnapshotDigest,
			artifact.ResolvedPolicyDigest, formatTime(artifact.CreatedAt),
			reviewBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM finding_adjudications WHERE run_id = ? AND round = ?`,
		[]any{artifact.RunID, artifact.Round}, reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put finding adjudication %q round %d: %w", artifact.RunID, artifact.Round, err)
	}
	return nil
}

// findingAdjudicationRow holds the extracted lookup and integrity columns of one
// stored artifact, cross-checked against the decoded body in reconstruct.
type findingAdjudicationRow struct {
	runID                     string
	round                     int
	contentDigest             string
	findingBatchDigest        string
	approvedSpecDigest        string
	instructionSnapshotDigest string
	resolvedPolicyDigest      string
	createdAt                 string
	bodyDigest                string
	body                      []byte
}

const selectFindingAdjudicationColumns = `run_id, round, content_digest,
    finding_batch_digest, approved_spec_digest, instruction_snapshot_digest,
    resolved_policy_digest, created_at, body_digest, body`

func scanFindingAdjudicationRow(sc scanner) (findingAdjudicationRow, error) {
	var row findingAdjudicationRow
	err := sc.Scan(&row.runID, &row.round, &row.contentDigest, &row.findingBatchDigest,
		&row.approvedSpecDigest, &row.instructionSnapshotDigest, &row.resolvedPolicyDigest,
		&row.createdAt, &row.bodyDigest, &row.body)
	return row, err
}

// reconstructFindingAdjudication decodes one row and re-runs every check a
// decoded trust bit demands: the body integrity digest, the full validation
// backstop (which recomputes the content and finding-batch digests), the
// agreement of every extracted lookup column with the decoded body, and the
// finding-set binding against current review state. No copied column is trusted.
func (tx *ReadTx) reconstructFindingAdjudication(
	ctx context.Context, row findingAdjudicationRow,
) (domain.FindingAdjudication, error) {
	if row.bodyDigest != reviewBodyDigest(string(row.body)) {
		return domain.FindingAdjudication{}, errRowInconsistent
	}
	artifact, err := decode[domain.FindingAdjudication](row.body)
	if err != nil {
		return domain.FindingAdjudication{}, err
	}
	if string(artifact.RunID) != row.runID || artifact.Round != row.round ||
		string(artifact.Digest) != row.contentDigest ||
		string(artifact.FindingBatchDigest) != row.findingBatchDigest ||
		string(artifact.ApprovedSpecDigest) != row.approvedSpecDigest ||
		string(artifact.InstructionSnapshotDigest) != row.instructionSnapshotDigest ||
		string(artifact.ResolvedPolicyDigest) != row.resolvedPolicyDigest ||
		formatTime(artifact.CreatedAt) != row.createdAt {
		return domain.FindingAdjudication{}, errRowInconsistent
	}
	if err := tx.validateFindingAdjudicationBinding(ctx, artifact); err != nil {
		return domain.FindingAdjudication{}, err
	}
	return artifact, nil
}

// GetFindingAdjudication reconstructs one artifact by its content digest.
func (tx *ReadTx) GetFindingAdjudication(
	ctx context.Context, digest domain.Digest,
) (domain.FindingAdjudication, error) {
	row, err := scanFindingAdjudicationRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectFindingAdjudicationColumns+
			` FROM finding_adjudications WHERE content_digest = ?`, digest))
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q: %w", digest, notFoundOr(err))
	}
	artifact, err := tx.reconstructFindingAdjudication(ctx, row)
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q: %w", digest, err)
	}
	return artifact, nil
}

// GetFindingAdjudicationForRound reconstructs the artifact for one review round.
func (tx *ReadTx) GetFindingAdjudicationForRound(
	ctx context.Context, runID domain.RunID, round int,
) (domain.FindingAdjudication, error) {
	row, err := scanFindingAdjudicationRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectFindingAdjudicationColumns+
			` FROM finding_adjudications WHERE run_id = ? AND round = ?`, runID, round))
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q round %d: %w", runID, round, notFoundOr(err))
	}
	artifact, err := tx.reconstructFindingAdjudication(ctx, row)
	if err != nil {
		return domain.FindingAdjudication{}, fmt.Errorf("get finding adjudication %q round %d: %w", runID, round, err)
	}
	return artifact, nil
}

// ListFindingAdjudications returns one run's adjudication artifacts in round
// order. It enumerates the whole table and reconstructs every row before the run
// filter, so a corrupted copied run key cannot move a row out of all keyed reads
// and make a run's history look complete by omission (the review-record list
// pattern).
func (tx *ReadTx) ListFindingAdjudications(
	ctx context.Context, runID domain.RunID,
) ([]domain.FindingAdjudication, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT `+selectFindingAdjudicationColumns+
			` FROM finding_adjudications ORDER BY run_id, round`)
	if err != nil {
		return nil, fmt.Errorf("list finding adjudications %q: %w", runID, err)
	}
	var raw []findingAdjudicationRow
	for rows.Next() {
		row, err := scanFindingAdjudicationRow(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list finding adjudications %q row %d: %w", runID, len(raw)+1, err)
		}
		raw = append(raw, row)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list finding adjudications %q: %w", runID, err)
	}
	out := make([]domain.FindingAdjudication, 0, len(raw))
	for i, row := range raw {
		artifact, err := tx.reconstructFindingAdjudication(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("list finding adjudications %q row %d: %w", runID, i+1, err)
		}
		if artifact.RunID == runID {
			out = append(out, artifact)
		}
	}
	return out, nil
}
