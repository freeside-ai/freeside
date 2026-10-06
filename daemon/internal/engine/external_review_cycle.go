package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// externalReviewFirstRound reports whether record is the round an external
// review cycle opened with: the one that adjudicates the cycle's external
// findings and the one their dispositions are keyed by. A cycle whose first
// review record landed on a later round (a review that failed first) has no
// such round, and ends on a person (issue #1767 decision 4).
func (t productionPublicationTask) externalReviewFirstRound(record domain.ReviewRecord) bool {
	return t.answersExternalReview() && record.Round == t.Successor.ReviewRound
}

// remediatesExternalFindings reports whether a remediation of round under
// successor is one an external review cycle's first round starts: the one
// round whose remediation may fix the cycle's admitted external findings,
// which no review record lists, and may answer a clean record when it fixes
// nothing of the record's own (issue #1767 decisions 1 and 2). The three
// producer gates (store, engine, signet) widen under this condition and no
// other.
func remediatesExternalFindings(successor domain.PublicationSuccessor, round int) bool {
	return successor.EffectiveOrigin() == domain.PublicationSuccessorExternalReview &&
		successor.Reentry != nil && round == successor.ReviewRound
}

// externalCycleFindings splits the external findings a cycle admits by whether
// an earlier cycle already gave them an outcome.
type externalCycleFindings struct {
	// open are the findings the cycle's first round adjudicates.
	open []domain.Finding
	// answered hold a disposition from an earlier cycle. They stay on the
	// head the cycle reviews, so the cycle admits them again, but one outcome
	// is final: they are named and not judged a second time (decision 6).
	answered []domain.Finding
}

// loadExternalCycleFindings reads the cycle's admitted findings, earliest
// first, and splits them. A disposition of the cycle's own first round does
// not move a finding to answered, so the split reads the same before and
// after the round writes its outcomes.
func loadExternalCycleFindings(
	ctx context.Context, tx *store.ReadTx, task productionPublicationTask,
) (externalCycleFindings, error) {
	admitted, err := tx.ExternalReviewCycleFindings(ctx, task.RunID, task.PublicationID)
	if err != nil {
		return externalCycleFindings{}, err
	}
	dispositions, err := tx.ListExternalFindingDispositions(ctx, task.RunID)
	if err != nil {
		return externalCycleFindings{}, err
	}
	var findings externalCycleFindings
	for _, finding := range admitted {
		if slices.ContainsFunc(dispositions, func(disposition domain.ExternalFindingDisposition) bool {
			return disposition.FindingID == finding.ID && disposition.Round < task.Successor.ReviewRound
		}) {
			findings.answered = append(findings.answered, finding)
			continue
		}
		findings.open = append(findings.open, finding)
	}
	return findings, nil
}

// openExternalFindings returns the external findings record's round has to
// give an outcome: the cycle's open findings in its first round, and none in
// any other round or task.
//
// The round's stored adjudication fixes that set. The cycle's admitted
// findings are read live, and a reviewer can leave one more between the
// cycle's ready item and the end of its task. That finding was never judged,
// so counting it would hold a finished round open; it has no outcome, so the
// trigger starts the next cycle for it.
func (w *productionPublicationWorkflow) openExternalFindings(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
) ([]domain.Finding, error) {
	if !task.externalReviewFirstRound(record) {
		return nil, nil
	}
	var (
		findings externalCycleFindings
		judged   *domain.FindingAdjudication
	)
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if findings, err = loadExternalCycleFindings(ctx, tx, task); err != nil {
			return err
		}
		artifact, err := tx.GetFindingAdjudicationForRound(ctx, task.RunID, record.Round)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		judged = &artifact
		return err
	}); err != nil {
		return nil, err
	}
	if judged == nil {
		return findings.open, nil
	}
	return slices.DeleteFunc(findings.open, func(finding domain.Finding) bool {
		return !slices.ContainsFunc(judged.Entries, func(entry domain.FindingAdjudicationEntry) bool {
			return entry.FindingID == finding.ID
		})
	}), nil
}

// externalRoundCard is what the adjudication card of an external review
// cycle's first round says that no other round's card does. That round acts
// on a card only when every route is a decline or a defer
// (externalReviewRoutesHandoff), so its card cannot promise what an ordinary
// round's does. Any other round or task has none.
type externalRoundCard struct {
	// answered are the admitted findings an earlier cycle already gave an
	// outcome. The card names them and the round does not judge them again
	// (issue #1767 decision 6).
	answered []domain.Finding
}

// loadExternalRoundCard returns what the card of record's round says as an
// external review cycle's first round, and nil for any other round or task.
func loadExternalRoundCard(
	ctx context.Context, tx *store.ReadTx, task productionPublicationTask, record domain.ReviewRecord,
) (*externalRoundCard, error) {
	if !task.externalReviewFirstRound(record) {
		return nil, nil
	}
	findings, err := loadExternalCycleFindings(ctx, tx, task)
	if err != nil {
		return nil, err
	}
	return &externalRoundCard{answered: findings.answered}, nil
}

// externalReviewMayAdjudicate reports whether the cycle's coordinates let a
// round go to adjudication (issue #1767 decisions 1 and 4): the record is the
// cycle's first round, and the cycle reviews the base its predecessor's
// producer was admitted at, the only base a remediation could be admitted at.
func (t productionPublicationTask) externalReviewMayAdjudicate(
	record domain.ReviewRecord, reentry *reentryCycle,
) bool {
	return t.externalReviewFirstRound(record) && reentry != nil &&
		reentry.admittedBaseSHA == t.Successor.Reentry.BaseSHA
}

// adjudicatesExternalReview reports whether an external review cycle's round
// goes to adjudication instead of straight to a person. It does when the
// cycle's coordinates allow it and an admitted finding still has no outcome
// from an earlier cycle. Both are durable, so the answer is the same on every
// pass. Whether an adjudicator is configured is not asked here: adjudication
// itself ends the round on a person when it has no one to ask, and a round
// already judged, waiting on its card, must not change course because the
// daemon restarted without one.
func (w *productionPublicationWorkflow) adjudicatesExternalReview(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
	record domain.ReviewRecord,
) (bool, error) {
	if !task.externalReviewMayAdjudicate(record, binding.reentry) {
		return false, nil
	}
	open, err := w.openExternalFindings(ctx, task, record)
	return len(open) > 0, err
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

// externalReviewFindingList names external findings for an item: who left
// each, on which thread, and their words. Those words come from outside
// Freeside, so each is quoted and cut, and the list is capped.
func externalReviewFindingList(findings []domain.Finding) string {
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
	return listing
}

// externalReviewAnsweredNote names the admitted findings an earlier cycle
// already gave an outcome, or is empty when there are none. They are still on
// the head, so a reader would otherwise take them for open.
func externalReviewAnsweredNote(answered []domain.Finding) string {
	if len(answered) == 0 {
		return ""
	}
	return fmt.Sprintf(" Already answered in an earlier cycle and not judged again: %s.",
		externalReviewFindingList(answered))
}

// externalReviewFindingsNote names the external findings a cycle answers.
func externalReviewFindingsNote(head string, findings externalCycleFindings) string {
	return fmt.Sprintf(
		"An external reviewer's findings on %s started this cycle, and a person must decide what to do with them. External findings: %s.",
		head, externalReviewFindingList(findings.open)) + externalReviewAnsweredNote(findings.answered)
}

// withExternalReviewFindings appends the cycle's external findings to the
// reason of an item an external review cycle ends on. The cycle withdrew
// readiness for those findings and nothing automatic answered them, so every
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
	var findings externalCycleFindings
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		findings, err = loadExternalCycleFindings(ctx, tx, task)
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

// externalReviewHandoffItemID is the item an external review cycle's first
// round ends on when adjudication leaves work the cycle cannot do itself. The
// round's review identity may already hold the adjudication card a person
// answered, and an item's type is bound when it is first written, so this
// ending takes an identity of its own, as the exhaustion item does.
func externalReviewHandoffItemID(runID domain.RunID, round int) domain.ItemID {
	return domain.ItemID(fmt.Sprintf("production-external-review-handoff-%s-%d", runID, round))
}

// externalReviewRoutesHandoff says why the routes of an external review
// cycle's adjudicated first round end the cycle on a person, and is empty
// when they do not. They do when a finding routes to a fix, because the cycle
// starts no remediation, and when any finding keeps a route other than
// decline or defer, because those two are the only outcomes the cycle can
// record itself. The adjudication card asks the same question of the routes
// it recommends, so what it says accepting does is what accepting does.
func externalReviewRoutesHandoff(
	artifact domain.FindingAdjudication, routes map[domain.FindingID]domain.AdjudicationRoute,
) string {
	if len(remediationFindingIDs(artifact, routes)) > 0 {
		return "Adjudication judged that a finding needs a fix in this pull request. An external review cycle starts no remediation, so a person must decide."
	}
	for _, entry := range artifact.Entries {
		if route := routes[entry.FindingID]; route != domain.RouteDecline && route != domain.RouteDefer {
			return "Adjudication left a finding that is neither declined nor deferred. An external review cycle records no other outcome, so a person must take the finding from here."
		}
	}
	return ""
}

// externalReviewRoundEndsOnPerson reports whether an adjudicated first round
// of an external review cycle still has to end on a person, and why. The
// caller has already raised the adjudication card when a route needs a
// person's choice, so the routes here are final. The round ends on a person
// when its routes do (externalReviewRoutesHandoff), and when the convergence
// policy stops the run at this round, because the item that stop raises
// answers only the review record's findings.
func (w *productionPublicationWorkflow) externalReviewRoundEndsOnPerson(
	ctx context.Context, record domain.ReviewRecord, artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
) (string, error) {
	if reason := externalReviewRoutesHandoff(artifact, routes); reason != "" {
		return reason, nil
	}
	convergence, err := w.reviewConvergenceState(ctx, record)
	if err != nil {
		return "", err
	}
	if _, stop, err := store.EvaluateReviewConvergence(convergence, record); err != nil || !stop {
		return "", err
	}
	return "The review policy stops this run at this round, so a person must decide what to do with the findings.", nil
}

// escalateExternalReviewHandoff ends the cycle on a person with reason. The
// review item writer names the external findings on the item.
func (w *productionPublicationWorkflow) escalateExternalReviewHandoff(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord, reason string,
) (productionReviewGateState, error) {
	if err := w.putReviewAttentionWithID(
		ctx, task, record, reason, domain.AttentionReviewDispute,
		externalReviewHandoffItemID(task.RunID, record.Round),
	); err != nil {
		return productionReviewPending, err
	}
	return productionReviewEscalated, nil
}
