package publish

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// AuditHandoffTip runs the workflow audit a publication's handoff resolves
// its target tip from (plan §6, revision 110). The caller records the audit
// in the transaction that seals the tip, so the run's audit history holds
// every tip a handoff relied on.
func (p *Publisher) AuditHandoffTip(ctx context.Context, repo, baseRef string) (domain.WorkflowAudit, error) {
	if p.auditor == nil {
		return domain.WorkflowAudit{}, errors.New("handoff audit: no workflow auditor")
	}
	audit, err := p.auditor.Audit(ctx, repo, baseRef)
	if err != nil {
		return domain.WorkflowAudit{}, fmt.Errorf("handoff audit: %w", err)
	}
	if audit.Repo != repo || !validCommitSHA(audit.AuditedCommitSHA) {
		return domain.WorkflowAudit{}, fmt.Errorf("handoff audit of %s@%s observed %s at %q: %w",
			repo, baseRef, audit.Repo, audit.AuditedCommitSHA, ErrUnauthorizedPublication)
	}
	return audit, nil
}

// validateHandoffCheck is the handoff gate of a new execution publication.
// It re-checks, inside the decision transaction, the sealed handoff check of
// the candidate's own run and publication, never the candidate's copy:
//
//   - the copy equals the sealed row, whose head is the candidate's, whose
//     tip is the candidate's BaseSHA, and whose evidence base is the
//     authorization's base;
//   - every readiness proof binds the head at that evidence base, or the tip
//     counts as moved (plan §7, revision 110);
//   - a moved tip has a recorded merge whose CheckProof proves the merge of
//     the head into the tip under the readiness's own clean-verification
//     resolution and the candidate's recipe.
//
// Any miss is ErrHandoffCheckUnsatisfied.
func validateHandoffCheck(
	ctx context.Context, tx *store.ReadTx, c Candidate, auth domain.CandidateAuthorization,
) error {
	unsatisfied := func(format string, args ...any) error {
		return fmt.Errorf("handoff check of %s: %s: %w",
			c.InvocationID, fmt.Sprintf(format, args...), ErrHandoffCheckUnsatisfied)
	}
	if c.HandoffCheck == nil || c.DispositionHistory == nil || c.RecipeDigest == nil {
		return unsatisfied("candidate names no handoff check")
	}
	sealed, err := tx.GetHandoffCheck(ctx, c.RunID, c.InvocationID)
	if errors.Is(err, store.ErrNotFound) {
		return unsatisfied("no tip was sealed")
	}
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(*c.HandoffCheck, sealed) {
		return unsatisfied("candidate's copy is not the sealed check")
	}
	if sealed.HeadSHA != c.HeadSHA || sealed.TipSHA != c.BaseSHA || sealed.EvidenceBaseSHA != auth.BaseSHA {
		return unsatisfied("sealed check names head %s at tip %s over %s, candidate publishes %s at %s over %s",
			sealed.HeadSHA, sealed.TipSHA, sealed.EvidenceBaseSHA, c.HeadSHA, c.BaseSHA, auth.BaseSHA)
	}
	moved := sealed.Moved()
	var cleanVerification domain.Digest
	for _, expected := range c.DispositionHistory.readinessProofs {
		proof, err := tx.GetCheckProof(ctx, expected.proofDigest)
		if err != nil {
			return err
		}
		if proof.Base == nil || proof.Base.BaseSHA != sealed.EvidenceBaseSHA ||
			proof.CandidateHead != c.HeadSHA || proof.MergeSHA != "" {
			moved = true
		}
		resolution, err := tx.GetRequirementResolution(ctx, expected.resolutionDigest)
		if err != nil {
			return err
		}
		if resolution.CheckClass == domain.CheckClassCleanVerification {
			cleanVerification = resolution.Digest
		}
	}
	if !moved {
		return nil
	}
	if sealed.Merge == nil {
		return unsatisfied("tip %s moved past evidence base %s and no merge was verified",
			sealed.TipSHA, sealed.EvidenceBaseSHA)
	}
	if cleanVerification == "" {
		return unsatisfied("readiness names no clean-verification resolution")
	}
	proof, err := tx.GetCheckProof(ctx, sealed.Merge.VerificationProofDigest)
	if err != nil {
		return errors.Join(err, unsatisfied("merge proof unreadable"))
	}
	if proof.CandidateHead != sealed.HeadSHA || proof.Base == nil ||
		proof.Base.BaseSHA != sealed.TipSHA || proof.Base.Repo != c.Repo || proof.Base.BaseRef != c.BaseRef ||
		proof.MergeSHA != sealed.Merge.MergeSHA ||
		proof.RequirementResolutionDigest != cleanVerification || proof.RecipeDigest != *c.RecipeDigest {
		return unsatisfied("merge proof %s does not prove %s merged into %s under the run's verification",
			proof.Digest, sealed.HeadSHA, sealed.TipSHA)
	}
	return nil
}
