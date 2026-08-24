package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const putShadowReviewRecordSQL = `
INSERT INTO shadow_review_records
    (invocation_id, run_id, shadowed_round, source, provider, base_sha, head_sha,
     outcome, completed_at, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (invocation_id) DO NOTHING`

func (tx *ReadTx) ensureFindingNotShadowLinked(ctx context.Context, id domain.FindingID) error {
	var linked bool
	if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM shadow_review_record_findings WHERE finding_id = ?)`, id).Scan(&linked); err != nil {
		return err
	}
	if linked {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *ReadTx) ensureFindingNotRoutedLinked(ctx context.Context, id domain.FindingID) error {
	var linked bool
	if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM review_record_findings WHERE finding_id = ?)`, id).Scan(&linked); err != nil {
		return err
	}
	if linked {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *ReadTx) ensureInvocationNotShadowRecorded(
	ctx context.Context, id domain.InvocationID,
) error {
	var linked bool
	if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM shadow_review_records WHERE invocation_id = ?)`, id).Scan(&linked); err != nil {
		return err
	}
	if linked {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *ReadTx) ensureShadowInvocationNotRoutedRecorded(
	ctx context.Context, id domain.InvocationID,
) error {
	var linked bool
	if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM review_records WHERE invocation_id = ?
		UNION ALL
		SELECT 1 FROM review_failures WHERE invocation_id = ?
		UNION ALL
		SELECT 1 FROM review_retries WHERE invocation_id = ?)`, id, id, id).Scan(&linked); err != nil {
		return err
	}
	if linked {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *ReadTx) shadowFindingParent(
	ctx context.Context, id domain.FindingID,
) (int, domain.InvocationID, error) {
	var count int
	var parent string
	if err := tx.tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MIN(invocation_id), '')
		FROM shadow_review_record_findings WHERE finding_id = ?`, id).Scan(&count, &parent); err != nil {
		return 0, "", err
	}
	return count, domain.InvocationID(parent), nil
}

func (tx *ReadTx) ensureFindingAvailableForShadow(
	ctx context.Context, id domain.FindingID, invocationID domain.InvocationID,
) error {
	count, parent, err := tx.shadowFindingParent(ctx, id)
	if err != nil {
		return err
	}
	if count > 1 || count == 1 && parent != invocationID {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *ReadTx) ensureFindingShadowParent(
	ctx context.Context, id domain.FindingID, invocationID domain.InvocationID,
) error {
	count, parent, err := tx.shadowFindingParent(ctx, id)
	if err != nil {
		return err
	}
	if count != 1 || parent != invocationID {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// validateShadowReviewRecordBinding re-loads the routed authority for the
// claimed run and round, then proves both reviewers saw the same candidate.
// The shadow worker's copied round and SHAs are observations, never authority.
func (tx *ReadTx) validateShadowReviewRecordBinding(
	ctx context.Context, record domain.ShadowReviewRecord,
) error {
	routed, err := tx.reviewRecordForRound(ctx, record.RunID, record.ShadowedRound)
	if err != nil {
		return err
	}
	if routed.RunID != record.RunID || routed.Round != record.ShadowedRound ||
		routed.BaseSHA != record.BaseSHA || routed.HeadSHA != record.HeadSHA {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// PutShadowReviewRecord atomically persists one observation-only shadow pass
// and its immutable raw findings. The separate table is the routing boundary:
// review_records and review_record_findings remain the sole evidence surfaces
// for readiness, round derivation, adjudication, and remediation.
func (tx *WriteTx) PutShadowReviewRecord(
	ctx context.Context, record domain.ShadowReviewRecord, findings []domain.Finding,
) error {
	if err := record.Validate(); err != nil {
		return fmt.Errorf("put shadow review record %q: %w", record.InvocationID, err)
	}
	if err := tx.validateShadowReviewRecordBinding(ctx, record); err != nil {
		return fmt.Errorf("put shadow review record %q routed candidate binding: %w",
			record.InvocationID, err)
	}
	if err := tx.ensureShadowInvocationNotRoutedRecorded(ctx, record.InvocationID); err != nil {
		return fmt.Errorf("put shadow review record %q routed invocation binding: %w",
			record.InvocationID, err)
	}
	if len(findings) != len(record.FindingIDs) {
		return fmt.Errorf("put shadow review record %q finding count: %w",
			record.InvocationID, domain.ErrParentKeyMismatch)
	}
	findingsByID := make(map[domain.FindingID]domain.Finding, len(findings))
	for _, finding := range findings {
		if finding.RunID != record.RunID {
			return fmt.Errorf("put shadow review record %q finding %q source binding: %w",
				record.InvocationID, finding.ID, domain.ErrParentKeyMismatch)
		}
		if err := domain.ValidateShadowReviewFinding(record.Source, finding); err != nil {
			return fmt.Errorf("put shadow review record %q finding %q schema: %w",
				record.InvocationID, finding.ID, err)
		}
		if _, duplicate := findingsByID[finding.ID]; duplicate {
			return fmt.Errorf("put shadow review record %q duplicate finding %q: %w",
				record.InvocationID, finding.ID, domain.ErrParentKeyMismatch)
		}
		findingsByID[finding.ID] = finding
	}
	for _, id := range record.FindingIDs {
		finding, ok := findingsByID[id]
		if !ok {
			return fmt.Errorf("put shadow review record %q missing finding %q: %w",
				record.InvocationID, id, domain.ErrParentKeyMismatch)
		}
		if err := tx.ensureFindingNotRoutedLinked(ctx, id); err != nil {
			return fmt.Errorf("put shadow review record %q routed finding %q: %w",
				record.InvocationID, id, err)
		}
		if err := tx.ensureFindingAvailableForShadow(ctx, id, record.InvocationID); err != nil {
			return fmt.Errorf("put shadow review record %q shadow finding %q parent: %w",
				record.InvocationID, id, err)
		}
		if err := tx.PutFinding(ctx, finding); err != nil {
			return err
		}
	}
	// A replay must reconstruct the existing row before byte comparison, so a
	// copied-column or source-binding corruption cannot hide behind an unchanged
	// canonical body.
	if _, err := tx.GetShadowReviewRecord(ctx, record.InvocationID); err != nil &&
		!errors.Is(err, ErrNotFound) {
		return fmt.Errorf("put shadow review record %q existing row: %w", record.InvocationID, err)
	}
	body, err := encode(record)
	if err != nil {
		return fmt.Errorf("put shadow review record %q: %w", record.InvocationID, err)
	}
	if err := tx.putImmutable(ctx, putShadowReviewRecordSQL,
		[]any{
			record.InvocationID, record.RunID, record.ShadowedRound, record.Source,
			record.Provider, record.BaseSHA, record.HeadSHA, record.Outcome,
			formatTime(record.CompletedAt), reviewBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM shadow_review_records WHERE invocation_id = ?`,
		[]any{record.InvocationID}, reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put shadow review record %q: %w", record.InvocationID, err)
	}
	for i, id := range record.FindingIDs {
		if _, err := tx.tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO shadow_review_record_findings
			 (invocation_id, finding_id, ordinal) VALUES (?, ?, ?)`,
			record.InvocationID, id, i); err != nil {
			return fmt.Errorf("put shadow review record %q finding %q: %w", record.InvocationID, id, err)
		}
	}
	return nil
}

// GetShadowReviewRecord reconstructs one shadow pass and re-derives its shadow
// distinction from the registered source value in the canonical body. It also
// cross-checks every copied column, ordinal finding join, and finding source.
func (tx *ReadTx) GetShadowReviewRecord(
	ctx context.Context, id domain.InvocationID,
) (domain.ShadowReviewRecord, error) {
	var (
		runID, source, provider, baseSHA, headSHA string
		outcome, completedAt, bodyDigest          string
		shadowedRound                             int
		body                                      []byte
	)
	err := tx.tx.QueryRowContext(ctx, `SELECT run_id, shadowed_round, source, provider,
		base_sha, head_sha, outcome, completed_at, body_digest, body
		FROM shadow_review_records WHERE invocation_id = ?`, id).Scan(
		&runID, &shadowedRound, &source, &provider, &baseSHA, &headSHA,
		&outcome, &completedAt, &bodyDigest, &body)
	if err != nil {
		return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q: %w", id, notFoundOr(err))
	}
	if bodyDigest != reviewBodyDigest(string(body)) {
		return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q: %w", id, errRowInconsistent)
	}
	record, err := decode[domain.ShadowReviewRecord](body)
	if err != nil {
		return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q: %w", id, err)
	}
	if record.InvocationID != id || record.RunID != domain.RunID(runID) ||
		record.ShadowedRound != shadowedRound || string(record.Source) != source ||
		record.Provider != provider || record.BaseSHA != baseSHA || record.HeadSHA != headSHA ||
		string(record.Outcome) != outcome || formatTime(record.CompletedAt) != completedAt {
		return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q: %w", id, errRowInconsistent)
	}
	if err := tx.validateShadowReviewRecordBinding(ctx, record); err != nil {
		return domain.ShadowReviewRecord{}, fmt.Errorf(
			"get shadow review record %q routed candidate binding: %w", id, err)
	}
	if err := tx.ensureShadowInvocationNotRoutedRecorded(ctx, record.InvocationID); err != nil {
		return domain.ShadowReviewRecord{}, fmt.Errorf(
			"get shadow review record %q routed invocation binding: %w", id, err)
	}
	rows, err := tx.tx.QueryContext(ctx, `SELECT finding_id FROM shadow_review_record_findings
		WHERE invocation_id = ? ORDER BY ordinal`, id)
	if err != nil {
		return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q findings: %w", id, err)
	}
	ids := make([]domain.FindingID, 0, len(record.FindingIDs))
	for rows.Next() {
		var findingID string
		if err := rows.Scan(&findingID); err != nil {
			_ = rows.Close()
			return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q findings: %w", id, err)
		}
		ids = append(ids, domain.FindingID(findingID))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q findings: %w", id, err)
	}
	if err := rows.Close(); err != nil {
		return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q findings: %w", id, err)
	}
	if !slices.Equal(ids, record.FindingIDs) {
		return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q findings: %w", id, errRowInconsistent)
	}
	for _, findingID := range ids {
		if err := tx.ensureFindingShadowParent(ctx, findingID, record.InvocationID); err != nil {
			return domain.ShadowReviewRecord{}, fmt.Errorf(
				"get shadow review record %q finding %q parent: %w", id, findingID, err)
		}
		if err := tx.ensureFindingNotRoutedLinked(ctx, findingID); err != nil {
			return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q routed finding %q: %w",
				id, findingID, err)
		}
		finding, err := tx.GetFinding(ctx, findingID)
		if err != nil {
			return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q finding %q: %w",
				id, findingID, err)
		}
		if finding.RunID != record.RunID {
			return domain.ShadowReviewRecord{}, fmt.Errorf("get shadow review record %q finding %q source binding: %w",
				id, findingID, domain.ErrParentKeyMismatch)
		}
		if err := domain.ValidateShadowReviewFinding(record.Source, finding); err != nil {
			return domain.ShadowReviewRecord{}, fmt.Errorf(
				"get shadow review record %q finding %q schema: %w", id, findingID, err)
		}
	}
	return record, nil
}

// ListShadowReviewRecords returns one run's complete shadow-review history.
// It reconstructs the entire table before filtering, so a corrupted copied run
// key cannot hide a row from all keyed reads.
func (tx *ReadTx) ListShadowReviewRecords(
	ctx context.Context, runID domain.RunID,
) ([]domain.ShadowReviewRecord, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT invocation_id FROM shadow_review_records
		ORDER BY run_id, shadowed_round, source, invocation_id`)
	if err != nil {
		return nil, fmt.Errorf("list shadow review records %q: %w", runID, err)
	}
	var ids []domain.InvocationID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list shadow review records %q row %d: %w", runID, len(ids)+1, err)
		}
		ids = append(ids, domain.InvocationID(id))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("list shadow review records %q: %w", runID, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list shadow review records %q: %w", runID, err)
	}
	records := make([]domain.ShadowReviewRecord, 0, len(ids))
	for _, id := range ids {
		record, err := tx.GetShadowReviewRecord(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("list shadow review records %q: %w", runID, err)
		}
		if record.RunID == runID {
			records = append(records, record)
		}
	}
	return records, nil
}

type classifierAccuracySampleRow struct {
	runID, findingID, shadowInvocationID string
	assessment, recordedAt, bodyDigest   string
	classificationVersion                int
	body                                 []byte
}

const selectClassifierAccuracySampleColumns = `run_id, finding_id,
    classification_version, shadow_invocation_id, assessment, recorded_at,
    body_digest, body`

func scanClassifierAccuracySampleRow(sc scanner) (classifierAccuracySampleRow, error) {
	var row classifierAccuracySampleRow
	err := sc.Scan(&row.runID, &row.findingID, &row.classificationVersion,
		&row.shadowInvocationID, &row.assessment, &row.recordedAt,
		&row.bodyDigest, &row.body)
	return row, err
}

func (tx *ReadTx) validateClassifierAccuracySampleBinding(
	ctx context.Context, sample domain.ClassifierAccuracySample,
) error {
	record, err := tx.GetShadowReviewRecord(ctx, sample.ShadowInvocationID)
	if err != nil {
		return err
	}
	if record.RunID != sample.RunID || !slices.Contains(record.FindingIDs, sample.FindingID) {
		return domain.ErrParentKeyMismatch
	}
	finding, err := tx.GetFinding(ctx, sample.FindingID)
	if err != nil {
		return err
	}
	if finding.RunID != sample.RunID || finding.Source != string(record.Source) {
		return domain.ErrParentKeyMismatch
	}
	classification, err := tx.GetClassification(ctx, sample.FindingID, sample.ClassificationVersion)
	if err != nil {
		return err
	}
	if classification.FindingID != sample.FindingID || classification.Version != sample.ClassificationVersion {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *ReadTx) reconstructClassifierAccuracySample(
	ctx context.Context, row classifierAccuracySampleRow,
) (domain.ClassifierAccuracySample, error) {
	if row.bodyDigest != reviewBodyDigest(string(row.body)) {
		return domain.ClassifierAccuracySample{}, errRowInconsistent
	}
	sample, err := decode[domain.ClassifierAccuracySample](row.body)
	if err != nil {
		return domain.ClassifierAccuracySample{}, err
	}
	if string(sample.RunID) != row.runID || string(sample.FindingID) != row.findingID ||
		sample.ClassificationVersion != row.classificationVersion ||
		string(sample.ShadowInvocationID) != row.shadowInvocationID ||
		string(sample.Assessment) != row.assessment || formatTime(sample.RecordedAt) != row.recordedAt {
		return domain.ClassifierAccuracySample{}, errRowInconsistent
	}
	if err := tx.validateClassifierAccuracySampleBinding(ctx, sample); err != nil {
		return domain.ClassifierAccuracySample{}, err
	}
	return sample, nil
}

func (tx *ReadTx) getClassifierAccuracySample(
	ctx context.Context, shadowInvocationID domain.InvocationID,
	findingID domain.FindingID, classificationVersion int,
) (domain.ClassifierAccuracySample, error) {
	row, err := scanClassifierAccuracySampleRow(tx.tx.QueryRowContext(ctx,
		`SELECT `+selectClassifierAccuracySampleColumns+` FROM classifier_accuracy_samples
		 WHERE shadow_invocation_id = ? AND finding_id = ? AND classification_version = ?`,
		shadowInvocationID, findingID, classificationVersion))
	if err != nil {
		return domain.ClassifierAccuracySample{}, notFoundOr(err)
	}
	return tx.reconstructClassifierAccuracySample(ctx, row)
}

const putClassifierAccuracySampleSQL = `
INSERT INTO classifier_accuracy_samples
    (run_id, finding_id, classification_version, shadow_invocation_id,
     assessment, recorded_at, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (shadow_invocation_id, finding_id, classification_version) DO NOTHING`

// PutClassifierAccuracySample persists one immutable adjudicated sample after
// re-gating its run, shadow result, shadow finding, and classification joins.
func (tx *WriteTx) PutClassifierAccuracySample(
	ctx context.Context, sample domain.ClassifierAccuracySample,
) error {
	if err := sample.Validate(); err != nil {
		return fmt.Errorf("put classifier accuracy sample %q/%q/v%d: %w",
			sample.ShadowInvocationID, sample.FindingID, sample.ClassificationVersion, err)
	}
	if err := tx.validateClassifierAccuracySampleBinding(ctx, sample); err != nil {
		return fmt.Errorf("put classifier accuracy sample %q/%q/v%d binding: %w",
			sample.ShadowInvocationID, sample.FindingID, sample.ClassificationVersion, err)
	}
	if _, err := tx.getClassifierAccuracySample(ctx, sample.ShadowInvocationID,
		sample.FindingID, sample.ClassificationVersion); err != nil && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("put classifier accuracy sample %q/%q/v%d existing row: %w",
			sample.ShadowInvocationID, sample.FindingID, sample.ClassificationVersion, err)
	}
	body, err := encode(sample)
	if err != nil {
		return fmt.Errorf("put classifier accuracy sample %q/%q/v%d: %w",
			sample.ShadowInvocationID, sample.FindingID, sample.ClassificationVersion, err)
	}
	if err := tx.putImmutable(ctx, putClassifierAccuracySampleSQL,
		[]any{
			sample.RunID, sample.FindingID, sample.ClassificationVersion,
			sample.ShadowInvocationID, sample.Assessment, formatTime(sample.RecordedAt),
			reviewBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM classifier_accuracy_samples
		 WHERE shadow_invocation_id = ? AND finding_id = ? AND classification_version = ?`,
		[]any{sample.ShadowInvocationID, sample.FindingID, sample.ClassificationVersion},
		reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put classifier accuracy sample %q/%q/v%d: %w",
			sample.ShadowInvocationID, sample.FindingID, sample.ClassificationVersion, err)
	}
	return nil
}

// ListClassifierAccuracySamples returns one run's sampled classifier history.
// Every row is reconstructed and re-bound before the run filter is applied.
func (tx *ReadTx) ListClassifierAccuracySamples(
	ctx context.Context, runID domain.RunID,
) ([]domain.ClassifierAccuracySample, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT `+selectClassifierAccuracySampleColumns+`
		FROM classifier_accuracy_samples
		ORDER BY run_id, recorded_at, shadow_invocation_id, finding_id, classification_version`)
	if err != nil {
		return nil, fmt.Errorf("list classifier accuracy samples %q: %w", runID, err)
	}
	var stored []classifierAccuracySampleRow
	for rows.Next() {
		row, err := scanClassifierAccuracySampleRow(rows)
		if err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("list classifier accuracy samples %q row %d: %w", runID, len(stored)+1, err)
		}
		stored = append(stored, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("list classifier accuracy samples %q: %w", runID, err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list classifier accuracy samples %q: %w", runID, err)
	}
	samples := make([]domain.ClassifierAccuracySample, 0, len(stored))
	for _, row := range stored {
		sample, err := tx.reconstructClassifierAccuracySample(ctx, row)
		if err != nil {
			return nil, fmt.Errorf("list classifier accuracy samples %q: %w", runID, err)
		}
		if sample.RunID == runID {
			samples = append(samples, sample)
		}
	}
	return samples, nil
}
