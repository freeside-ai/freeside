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
// verified merge into it, recorded once. Since migration 0129 (#2073) the row
// names the workflow_audits row its tip was sealed from (workflow_audit_id),
// so the store, not the engine, proves the tip was observed on the run's
// target. The rows are ordinary state (Tier B): they decode with plain
// validation, and a legacy row with no audit decodes with a zero id. The
// authority they grant is re-checked where it is spent, and both checks are
// Tier A: the publication decision (publish.storePublicationDecision)
// re-reads the named audit and refuses a check that names none, and the v7
// handoff_conflict gate (validateHandoffConflictSuccessor) runs
// checkHandoffTipAudit before a remediation is admitted at the sealed tip.

const handoffCheckColumns = `run_id, publication_id, head_sha, evidence_base_sha, tip_sha,
    workflow_audit_id, merge_sha, verification_proof_digest`

// SealHandoffTip records the tip a publication's handoff resolved. It is the
// first write of the row and takes no merge. The check must name the audit
// its tip came from: a recorded audit that resolved the repository and ref
// the run's admissions target to TipSHA; anything else is
// domain.ErrParentKeyMismatch, so a wrong audit is refused before the run
// verifies, reviews, or authors against its tip. The publication decision
// re-checks the same audit against the candidate. Sealing the same values again is a no-op, so a retry converges; any other
// value, a different tip or audit above all, is ErrImmutableConflict: the
// tip is never re-resolved for the publication.
func (tx *InternalTx) SealHandoffTip(ctx context.Context, check domain.HandoffCheck, at time.Time) error {
	if err := check.Validate(); err != nil {
		return fmt.Errorf("seal handoff tip: %w", err)
	}
	if check.Merge != nil {
		return fmt.Errorf("seal handoff tip with a merge: %w", domain.ErrHandoffCheckInconsistent)
	}
	if err := tx.checkHandoffTipAudit(ctx, check); err != nil {
		return fmt.Errorf("seal handoff tip for %s: %w", check.PublicationID, err)
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
        (run_id, publication_id, head_sha, evidence_base_sha, tip_sha, workflow_audit_id, resolved_at)
        VALUES (?, ?, ?, ?, ?, ?, ?)`,
		check.RunID, check.PublicationID, check.HeadSHA, check.EvidenceBaseSHA, check.TipSHA,
		check.WorkflowAuditID, formatTime(at)); err != nil {
		return fmt.Errorf("seal handoff tip for %s: %w", check.PublicationID, err)
	}
	return nil
}

// checkHandoffTipAudit requires the audit a check names to be a recorded
// audit that resolved the run's target, the one repository and ref its
// admissions name, to the check's tip. The seal runs it, and so does the v7
// handoff_conflict gate, which admits a remediation at that tip.
func (tx *ReadTx) checkHandoffTipAudit(ctx context.Context, check domain.HandoffCheck) error {
	if check.WorkflowAuditID == 0 {
		return fmt.Errorf("handoff check names no workflow audit: %w", domain.ErrParentKeyMismatch)
	}
	admissions, err := tx.listRunExecutionAdmissions(ctx, check.RunID, false)
	if err != nil {
		return err
	}
	var repo, ref string
	for _, admission := range admissions {
		if repo != "" && (admission.Base.Repo != repo || admission.Base.BaseRef != ref) {
			return fmt.Errorf("run %s admissions name more than one target: %w", check.RunID, domain.ErrParentKeyMismatch)
		}
		repo, ref = admission.Base.Repo, admission.Base.BaseRef
	}
	if repo == "" {
		return fmt.Errorf("run %s has no admission naming its target: %w", check.RunID, domain.ErrParentKeyMismatch)
	}
	rec, err := tx.GetWorkflowAudit(ctx, check.WorkflowAuditID)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: %w", err, domain.ErrParentKeyMismatch)
	}
	if err != nil {
		return err
	}
	audit := rec.Audit
	if audit.Repo != repo || audit.AuditedRef != ref || audit.AuditedCommitSHA != check.TipSHA {
		return fmt.Errorf("workflow audit %d (%s@%q at %s) does not observe tip %s of %s@%s: %w",
			rec.ID, audit.Repo, audit.AuditedRef, audit.AuditedCommitSHA, check.TipSHA, repo, ref,
			domain.ErrParentKeyMismatch)
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
		auditID     sql.NullInt64
		mergeSHA    sql.NullString
		proofDigest sql.NullString
	)
	err := tx.tx.QueryRowContext(ctx, `SELECT `+handoffCheckColumns+`
        FROM publication_handoff_checks WHERE run_id = ? AND publication_id = ?`,
		runID, publicationID).Scan(
		&check.RunID, &check.PublicationID, &check.HeadSHA, &check.EvidenceBaseSHA, &check.TipSHA,
		&auditID, &mergeSHA, &proofDigest)
	if err != nil {
		return domain.HandoffCheck{}, fmt.Errorf("handoff check %s: %w", publicationID, notFoundOr(err))
	}
	check.WorkflowAuditID = auditID.Int64
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
