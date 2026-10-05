package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// externalReviewItemFindingLimit bounds the external findings one item names.
// Each is a reviewer's own words, quoted and cut, in a stored and synced item.
const externalReviewItemFindingLimit = 5

// answersExternalReview reports whether the task is the cycle an external
// reviewer's finding started.
func (t productionPublicationTask) answersExternalReview() bool {
	return t.reentersInPlace() &&
		t.Successor.EffectiveOrigin() == domain.PublicationSuccessorExternalReview
}

// externalReviewExhaustionItemID is the item an external review cycle past
// the hard round limit ends on. The round-limit identity the first cycle
// uses may already hold an item that cycle resolved on its way to ready (an
// adjudication a person answered in the last allowed round), and writing the
// exhaustion there would be refused or silently dropped with readiness
// already withdrawn. The cycle's own round keys a namespace no review item
// shares: the fixed prefixes diverge before the run coordinate.
func externalReviewExhaustionItemID(runID domain.RunID, round int) domain.ItemID {
	return domain.ItemID(fmt.Sprintf("production-external-review-exhaustion-%s-%d", runID, round))
}

// externalReviewVerdict says what Freeside's own review of an external review
// cycle's round found. It leads the item the cycle ends on.
func externalReviewVerdict(record domain.ReviewRecord) string {
	if record.Outcome == domain.ReviewFindings {
		return "Freeside reviewed the published pull request again and found blocking findings of its own."
	}
	return "Freeside reviewed the published pull request again and found nothing blocking."
}

// externalReviewFindingsNote names the external findings a cycle answers: who
// left each, on which thread, and their words. Those words come from outside
// Freeside, so each is quoted and cut, and the list is capped.
func externalReviewFindingsNote(head string, findings []domain.Finding) string {
	shown := findings
	if len(shown) > externalReviewItemFindingLimit {
		shown = shown[:externalReviewItemFindingLimit]
	}
	lines := make([]string, len(shown))
	for i, finding := range shown {
		lines[i] = fmt.Sprintf("%s on %s: %s",
			finding.External.ReviewerLogin, finding.External.ThreadID, reentryQuoted(finding.Message))
	}
	listing := strings.Join(lines, "; ")
	if more := len(findings) - len(shown); more > 0 {
		listing += fmt.Sprintf("; and %d more", more)
	}
	return fmt.Sprintf(
		"An external reviewer's findings on %s started this cycle, and a person must decide what to do with them. External findings: %s.",
		head, listing)
}

// withExternalReviewFindings appends the cycle's external findings to the
// reason of an item an external review cycle ends on. The cycle withdrew
// readiness for those findings and nothing automatic answers them, so every
// ending names them, not only the one that follows a review record: a review
// that fails, a round past the limit, and a stopped cycle would otherwise
// leave a person with no sign that a reviewer objected. Any other task's
// reason is returned unchanged.
func (w *productionPublicationWorkflow) withExternalReviewFindings(
	ctx context.Context, task productionPublicationTask, reason string,
) (string, error) {
	if !task.answersExternalReview() {
		return reason, nil
	}
	var findings []domain.Finding
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		findings, err = tx.ExternalReviewCycleFindings(ctx, task.RunID, task.PublicationID)
		return err
	}); err != nil {
		return "", err
	}
	return reason + " " + externalReviewFindingsNote(task.HeadSHA, findings), nil
}

// escalateExternalReviewFindings ends an external review cycle on a person
// once the round's review record exists, whatever that review found: an
// admitted external finding is still open, and the cycle has no automatic
// path for it. The item is the review-dispute escalation the gate raises
// when no automatic path remains, and the review item writer names the
// external findings on it.
func (w *productionPublicationWorkflow) escalateExternalReviewFindings(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
) (productionReviewGateState, error) {
	if err := w.removeReviewWorkspace(record.InvocationID); err != nil {
		return productionReviewPending, err
	}
	if err := w.putReviewAttention(
		ctx, task, record, externalReviewVerdict(record), domain.AttentionReviewDispute,
	); err != nil {
		return productionReviewPending, err
	}
	return productionReviewEscalated, nil
}
