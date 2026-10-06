package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const putExternalFindingDispositionSQL = `
INSERT INTO external_finding_dispositions
    (finding_id, run_id, round, disposition, reason, remediation_invocation_id,
     created_at, body_digest, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (finding_id, round) DO NOTHING`

// validateExternalFindingDispositionBinding re-runs every authoritative join
// instead of trusting the record's copied keys or anything stored with the
// finding: the round is the first round of an external_review cycle of the
// run, reviewed on that cycle's base and head; the cycle admits the finding
// (an external finding of the run on the cycle's head, whose reviewer the
// profile the authority names lists); and the outcome carries its authority. For
// fixed that is a later review record of the run on the same base and another
// head. For declined or deferred it is an adjudication of the same round
// whose entry for the finding authorizes that outcome.
func (tx *ReadTx) validateExternalFindingDispositionBinding(
	ctx context.Context, disposition domain.ExternalFindingDisposition,
) error {
	finding, err := tx.GetFinding(ctx, disposition.FindingID)
	if err != nil {
		return err
	}
	record, err := tx.reviewRecordForRound(ctx, disposition.RunID, disposition.Round)
	if err != nil {
		return err
	}
	authority, found, err := tx.externalReviewCycleForRound(ctx, record)
	if err != nil {
		return err
	}
	if !found {
		return domain.ErrParentKeyMismatch
	}
	admission, err := tx.externalReviewCycleAdmission(ctx, authority)
	if err != nil {
		return err
	}
	if err := admission.admits(finding); err != nil {
		return err
	}
	if disposition.Disposition == domain.ReviewDispositionFixed {
		remediation, err := tx.GetReviewRecord(ctx, disposition.Remediation())
		if err != nil {
			return err
		}
		if remediation.RunID != record.RunID || remediation.Round <= record.Round ||
			remediation.BaseSHA != record.BaseSHA || remediation.HeadSHA == record.HeadSHA {
			return domain.ErrParentKeyMismatch
		}
		return nil
	}
	adjudication, err := tx.GetFindingAdjudication(ctx, disposition.Adjudication())
	if err != nil {
		return err
	}
	if adjudication.RunID != disposition.RunID || adjudication.Round != disposition.Round {
		return domain.ErrParentKeyMismatch
	}
	return adjudication.AuthorizesFinalDisposition(disposition.FindingID, disposition.Disposition)
}

// PutExternalFindingDisposition persists the immutable outcome for one
// admitted external finding (plan §5.19, §7). It is the only write door for
// that record, and the only disposition an external finding can have:
// PutFindingDisposition refuses one, because no review record lists it.
// Replays converge only when the canonical bytes agree.
func (tx *WriteTx) PutExternalFindingDisposition(
	ctx context.Context, disposition domain.ExternalFindingDisposition,
) error {
	if err := disposition.Validate(); err != nil {
		return fmt.Errorf("put external finding disposition %q round %d: %w",
			disposition.FindingID, disposition.Round, err)
	}
	if err := tx.validateExternalFindingDispositionBinding(ctx, disposition); err != nil {
		return fmt.Errorf("put external finding disposition %q round %d binding: %w",
			disposition.FindingID, disposition.Round, err)
	}
	// A replay must validate the already-persisted row before putImmutable's
	// byte comparison. Otherwise corruption limited to a copied lookup column
	// could be hidden by an unchanged canonical body.
	if _, err := tx.GetExternalFindingDisposition(ctx, disposition.FindingID, disposition.Round); err != nil &&
		!errors.Is(err, ErrNotFound) {
		return fmt.Errorf("put external finding disposition %q round %d existing row: %w",
			disposition.FindingID, disposition.Round, err)
	}
	body, err := encode(disposition)
	if err != nil {
		return fmt.Errorf("put external finding disposition %q round %d: %w",
			disposition.FindingID, disposition.Round, err)
	}
	if err := tx.putImmutable(ctx, putExternalFindingDispositionSQL,
		[]any{
			disposition.FindingID, disposition.RunID, disposition.Round,
			disposition.Disposition, disposition.Reason, disposition.Remediation(),
			formatTime(disposition.CreatedAt),
			reviewBodyDigest(body), body,
		},
		`SELECT body_digest || body FROM external_finding_dispositions WHERE finding_id = ? AND round = ?`,
		[]any{disposition.FindingID, disposition.Round}, reviewBodyAuthority(body)); err != nil {
		return fmt.Errorf("put external finding disposition %q round %d: %w",
			disposition.FindingID, disposition.Round, err)
	}
	return nil
}

// GetExternalFindingDisposition reconstructs one disposition and re-runs its
// whole binding. Copied columns and decoded body must agree.
func (tx *ReadTx) GetExternalFindingDisposition(
	ctx context.Context, findingID domain.FindingID, round int,
) (domain.ExternalFindingDisposition, error) {
	dispositions, err := tx.loadExternalFindingDispositions(ctx)
	if err != nil {
		return domain.ExternalFindingDisposition{}, fmt.Errorf(
			"get external finding disposition %q round %d: %w", findingID, round, err)
	}
	for _, disposition := range dispositions {
		if disposition.FindingID == findingID && disposition.Round == round {
			return disposition, nil
		}
	}
	return domain.ExternalFindingDisposition{}, fmt.Errorf(
		"get external finding disposition %q round %d: %w", findingID, round, ErrNotFound)
}

// ListExternalFindingDispositions returns one run's external finding
// dispositions in round then finding-id order. An inconsistent row fails the
// whole list.
func (tx *ReadTx) ListExternalFindingDispositions(
	ctx context.Context, runID domain.RunID,
) ([]domain.ExternalFindingDisposition, error) {
	dispositions, err := tx.loadExternalFindingDispositions(ctx)
	if err != nil {
		return nil, fmt.Errorf("list external finding dispositions %q: %w", runID, err)
	}
	out := make([]domain.ExternalFindingDisposition, 0, len(dispositions))
	for _, disposition := range dispositions {
		if disposition.RunID == runID {
			out = append(out, disposition)
		}
	}
	return out, nil
}

// loadExternalFindingDispositions reconstructs the complete table before any
// lookup or run filter is applied. A copied key is untrusted too: selecting
// by it first would let corruption move an immutable record out of every
// keyed read.
func (tx *ReadTx) loadExternalFindingDispositions(
	ctx context.Context,
) ([]domain.ExternalFindingDisposition, error) {
	type rawDisposition struct {
		findingID, runID, class, reason, remediationInvocationID, bodyDigest string
		createdAt                                                            string
		round                                                                int
		body                                                                 []byte
	}
	rows, err := tx.tx.QueryContext(ctx, `SELECT finding_id, run_id, round, disposition,
        reason, remediation_invocation_id, created_at, body_digest, body FROM external_finding_dispositions
        ORDER BY run_id, round, finding_id`)
	if err != nil {
		return nil, err
	}
	raw := []rawDisposition{}
	for rows.Next() {
		var item rawDisposition
		if err := rows.Scan(&item.findingID, &item.runID, &item.round, &item.class,
			&item.reason, &item.remediationInvocationID, &item.createdAt,
			&item.bodyDigest, &item.body); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("row %d: %w", len(raw)+1, err)
		}
		raw = append(raw, item)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}

	out := make([]domain.ExternalFindingDisposition, 0, len(raw))
	for index, item := range raw {
		if item.bodyDigest != reviewBodyDigest(string(item.body)) {
			return nil, fmt.Errorf("row %d: %w", index+1, errRowInconsistent)
		}
		disposition, err := decode[domain.ExternalFindingDisposition](item.body)
		if err != nil {
			return nil, fmt.Errorf("row %d: %w", index+1, err)
		}
		if string(disposition.FindingID) != item.findingID || string(disposition.RunID) != item.runID ||
			disposition.Round != item.round || string(disposition.Disposition) != item.class ||
			disposition.Reason != item.reason ||
			string(disposition.Remediation()) != item.remediationInvocationID ||
			formatTime(disposition.CreatedAt) != item.createdAt {
			return nil, fmt.Errorf("row %d: %w", index+1, errRowInconsistent)
		}
		if err := tx.validateExternalFindingDispositionBinding(ctx, disposition); err != nil {
			return nil, fmt.Errorf("row %d binding: %w", index+1, err)
		}
		out = append(out, disposition)
	}
	return out, nil
}
