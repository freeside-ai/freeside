package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// checkFailureReentryPlan is the check_failure cycle one trigger pass
// decided to start, derived from reads alone. successor names no failure
// record yet: the caller reads the failed jobs' logs only once a cycle is
// due, and StartCheckFailureReentry records it.
type checkFailureReentryPlan struct {
	item      domain.AttentionItem
	binding   domain.ReadyItemPRBinding
	successor domain.PublicationSuccessor
}

// planCheckFailureReentry decides, from reads, whether a ready item's failed
// required check starts a cycle that reworks the head (issue #3598), and
// returns nil when none should. The probe and the writer both run it.
//
// A cycle starts only when the item is the run's open, current ready item;
// its required-check reading is a code failure on its own head; the pull
// request's last recorded fact is open and unmerged on that head; the task
// that wrote the item has ended; the item's latest review covers that head;
// the head is one Freeside pushed; an earlier check_failure cycle that ended
// on a person has had that person's item closed; and the review round limit
// leaves room for the cycle's review and the review of its fix. Every fact
// the authority carries is proved again by the store when it seals it.
func planCheckFailureReentry(
	ctx context.Context, tx *store.ReadTx, itemID domain.ItemID,
) (*checkFailureReentryPlan, error) {
	item, err := tx.GetAttentionItem(ctx, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Most passes end here: the check did not fail on code.
	check := item.RequiredCheck
	if item.Type != domain.AttentionReadyForFinalReview || item.Status != domain.StatusOpen ||
		item.Subject.Type != domain.SubjectRun || item.Subject.RunID == nil || check == nil ||
		check.State != domain.RequiredCheckFailed || check.FailureClass == nil ||
		*check.FailureClass != domain.CheckFailureCode || check.HeadSHA != item.PRHeadSHA {
		return nil, nil
	}
	runID := *item.Subject.RunID
	binding, err := tx.GetReadyItemPRBinding(ctx, item.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if binding.HeadSHA != check.HeadSHA {
		return nil, nil
	}
	pull, err := tx.LatestPullMergeFact(ctx, binding.RepositoryID, binding.PRNumber)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if pull.State != domain.PullRequestOpen || pull.Merged || pull.HeadSHA != binding.HeadSHA ||
		pull.BaseRef != binding.BaseRef {
		return nil, nil
	}
	if err := tx.RequireIncompletePublication(ctx, runID); err != nil {
		if errors.Is(err, store.ErrPublicationCompleted) {
			return nil, nil
		}
		return nil, err
	}
	if err := requireTaskExecutionOpen(ctx, tx, runID); err != nil {
		if workflowAdvancementFenced(err) {
			return nil, nil
		}
		return nil, err
	}
	chain, err := tx.PublicationSuccessorChain(ctx, runID)
	if err != nil {
		return nil, err
	}
	current := domain.ProductionReadyItemID(runID)
	taskKey := productionPublicationTaskKey(runID)
	reworked := false
	for _, sealed := range chain {
		current, taskKey = sealed.ReadyItemID(), sealed.TaskKey()
		reworked = reworked || sealed.ReworksCheckFailure()
	}
	if current != item.ID {
		return nil, nil
	}
	// The task that wrote the item still acts for the run until it ends
	// (planExternalReviewReentry).
	owner, err := tx.GetOutbox(ctx, taskKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err == nil && !owner.Dispatched() {
		return nil, nil
	}
	// The store refuses a new cycle over an earlier one's open ending.
	// Decided here, the refusal costs no write transaction on every pass.
	if reworked {
		open, err := tx.ListOpenAttentionItemRecordsForRun(ctx, runID)
		if err != nil {
			return nil, err
		}
		if slices.ContainsFunc(open, store.ReworkCycleEnding) {
			return nil, nil
		}
	}
	latest, err := tx.LatestReviewRecord(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// The cycle reviews the published head against the base its last review
	// used. A record from before reviews carried a base has none to reuse.
	if latest.HeadSHA != binding.HeadSHA || latest.BaseSHA == "" {
		return nil, nil
	}
	failure, err := tx.LatestReviewFailure(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err == nil && failure.Round > latest.Round {
		return nil, nil
	}
	// The cycle reviews the head on the next round and its fix on the round
	// after. Without room for both, the card shows the failure and a person
	// decides.
	policy, err := tx.ReviewConvergencePolicy(ctx, runID)
	if err != nil {
		return nil, err
	}
	if latest.Round+2 > policy.HardRoundLimit {
		return nil, nil
	}
	foreign, err := tx.ReadyHeadIsForeign(ctx, item.ID)
	if err != nil || foreign {
		return nil, err
	}
	successor := domain.PublicationSuccessor{
		Version:                 domain.PublicationCheckFailureVersion,
		RunID:                   runID,
		PredecessorItemID:       item.ID,
		PriorReviewInvocationID: latest.InvocationID,
		ReviewRound:             latest.Round + 1,
		Origin:                  domain.PublicationSuccessorCheckFailure,
		Reentry: &domain.PublicationSuccessorReentry{
			BaseSHA: latest.BaseSHA, HeadSHA: binding.HeadSHA,
		},
	}
	return &checkFailureReentryPlan{item: item, binding: binding, successor: successor}, nil
}

// CheckFailureReentryDue reports whether StartCheckFailureReentry would start
// a cycle for the item now, so a caller that runs on every pass reads the
// failed jobs' logs and opens a write transaction only when one is due. A
// project gate refuses here as it would there, before any log is read.
func CheckFailureReentryDue(
	ctx context.Context, tx *store.ReadTx, itemID domain.ItemID, gate ProjectReadinessGate,
) (bool, error) {
	plan, err := planCheckFailureReentry(ctx, tx, itemID)
	if err != nil || plan == nil {
		return false, err
	}
	if err := requireProjectReady(ctx, gate, tx, plan.item.ProjectID); err != nil {
		return false, fmt.Errorf("check failure re-entry: %w", err)
	}
	return true, nil
}

// RequiredCheckFailureBackupPayloadDigests returns the log excerpts a
// failure record names, which the remediation's input is rebuilt from, so a
// checkpoint that keeps the record keeps them too.
func RequiredCheckFailureBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	failure, err := domain.DecodeRequiredCheckFailure(entry.Payload)
	if err != nil {
		return nil, err
	}
	return failure.ExcerptDigests(), nil
}

// StartCheckFailureReentry starts the cycle that reworks a published head
// whose required check failed on code (issue #3598): it records failure,
// supersedes the ready item, seals the check_failure authority over the
// record, and queues the cycle's task, all in the caller's transaction. It
// reports whether a cycle started. The decision is made again from this
// transaction's reads before its first write; any failure after that write
// must fail the caller's transaction.
//
// failure is the record the caller built from the check's failed jobs, whose
// log excerpts it already stored. It must name the item's run, pull request,
// and head, and jobs the card names; any other record is an error.
//
// gate is the project readiness gate. A project that is not ready returns
// its refusal before any write; the item stays open and the caller offers
// the cycle again on its next pass.
func StartCheckFailureReentry(
	ctx context.Context, tx *store.WriteTx, itemID domain.ItemID, failure domain.RequiredCheckFailure,
	gate ProjectReadinessGate,
) (bool, error) {
	plan, err := planCheckFailureReentry(ctx, &tx.ReadTx, itemID)
	if err != nil || plan == nil {
		return false, err
	}
	if failure.RunID != plan.successor.RunID || failure.HeadSHA != plan.binding.HeadSHA ||
		failure.Repo != plan.binding.Repo || failure.PRNumber != plan.binding.PRNumber ||
		!namesCardJobs(failure, plan.item.RequiredCheck.FailedJobs) {
		return false, fmt.Errorf("check failure record for %s does not match its ready item: %w",
			itemID, domain.ErrParentKeyMismatch)
	}
	if err := requireProjectReady(ctx, gate, &tx.ReadTx, plan.item.ProjectID); err != nil {
		return false, fmt.Errorf("check failure re-entry: %w", err)
	}
	digest, err := tx.RecordRequiredCheckFailure(ctx, failure)
	if err != nil {
		return false, fmt.Errorf("record required check failure: %w", err)
	}
	plan.successor.CheckFailureDigest = digest
	task := newReentryTask(plan.item.ProjectID, plan.successor)
	if err := task.validate(); err != nil {
		return false, fmt.Errorf("check failure task: %w", err)
	}
	item := plan.item
	item.ItemVersion++
	item, err = item.ConcludeByDaemon(domain.ConcludedBySupersession, time.Now().UTC(),
		"Superseded when the pull request's required check failed on code and a re-entry cycle started to fix it.", nil)
	if err != nil {
		return false, fmt.Errorf("withdraw ready item for check failure: %w", err)
	}
	if err := tx.PutAttentionItem(ctx, item); err != nil {
		return false, fmt.Errorf("withdraw ready item for check failure: %w", err)
	}
	if err := tx.RecordPublicationSuccessor(ctx, plan.successor); err != nil {
		return false, fmt.Errorf("record check failure authority: %w", err)
	}
	payload, err := json.Marshal(task)
	if err != nil {
		return false, err
	}
	entry, _, err := tx.EnqueueOutbox(ctx, plan.successor.TaskKey(), KindProductionPublicationRequested, payload)
	if err != nil {
		return false, err
	}
	if entry.Kind != KindProductionPublicationRequested || !bytes.Equal(entry.Payload, payload) {
		return false, fmt.Errorf("check failure task disagrees with stored row: %w", domain.ErrImmutableTransition)
	}
	return true, nil
}

// namesCardJobs reports whether the record's jobs are the first jobs the
// card names, in its order: the record keeps at most MaxCheckFailureJobs of
// them.
func namesCardJobs(failure domain.RequiredCheckFailure, card []string) bool {
	if len(failure.Jobs) == 0 || len(failure.Jobs) > len(card) {
		return false
	}
	for i, job := range failure.Jobs {
		if job.Name != card[i] {
			return false
		}
	}
	return true
}

// remediateCheckFailure starts the remediation a check_failure cycle exists
// for, once the cycle's review of its head came back clean and the head
// handed off cleanly onto the target's tip (issue #3598). It leaves the
// cycle's task pending: the remediation's export replaces that task with the
// one that verifies, reviews, hands off, and publishes the fix. A later pass
// that finds the remediation already committed waits for that export.
//
// No remediation starts, and the cycle stops on its blocked item naming the
// check's failed jobs, when the review bound has no room for the fix's
// review or the input would pass the delivery cap. A review record that is
// not the cycle's clean review of its head is a daemon fault, returned.
func (w *productionPublicationWorkflow) remediateCheckFailure(
	ctx context.Context, task productionPublicationTask, checkoutDir string,
) (productionTaskOutcome, error) {
	successor := *task.Successor
	invocationID := remediationInvocationID(task.RunID, successor.ReviewRound)
	var (
		record  domain.RequiredCheckFailure
		review  domain.ReviewRecord
		initial domain.AgentInvocation
		started bool
	)
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if record, err = tx.GetRequiredCheckFailure(ctx, task.RunID, successor.CheckFailureDigest); err != nil {
			return err
		}
		if review, err = tx.LatestReviewRecord(ctx, task.RunID); err != nil {
			return err
		}
		if initial, err = tx.GetAgentInvocation(ctx, productionInvocationID(task.RunID)); err != nil {
			return err
		}
		entry, err := tx.GetOutbox(ctx, string(invocationID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		request, err := decodeRemediationRequest(entry)
		if err != nil || !successor.AdmitsCheckFailureRemediation(domain.RemediationInvocationIntent(request)) {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		started = true
		return nil
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	if started {
		return productionTaskOutcome{}, nil
	}
	if review.Round != successor.ReviewRound || review.Outcome != domain.ReviewClean ||
		review.HeadSHA != successor.Reentry.HeadSHA || review.BaseSHA != successor.Reentry.BaseSHA {
		return productionTaskOutcome{}, fmt.Errorf(
			"check failure cycle %s: latest review is round %d (%s) of %s at %s: %w",
			successor.PublicationID(), review.Round, review.Outcome, review.HeadSHA, review.BaseSHA,
			domain.ErrParentKeyMismatch)
	}
	limit, err := w.reviewHardRoundLimit(ctx, task.RunID)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if review.Round+1 > limit {
		return w.stopReentryCycle(ctx, task, checkFailureStopReason(record, fmt.Sprintf(
			"the resolved hard limit of %d review rounds leaves no round to review a fix", limit)), nil)
	}
	intent, err := w.prepareCheckFailureRemediation(ctx, task, initial, review, record, checkoutDir)
	if errors.Is(err, ErrRemediationInputUndeliverable) {
		return w.stopReentryCycle(ctx, task, checkFailureStopReason(record,
			"the failure's account is too large to deliver to a remediator"), nil)
	}
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return intent.persistCheckFailure(ctx, tx, task)
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	return productionTaskOutcome{}, nil
}

// prepareCheckFailureRemediation builds the cycle's remediation: on the
// cycle's round, answering its clean review, at the base the authority names
// with the published head's patch against it, and carrying the failure's
// account rebuilt from the record the authority sealed.
func (w *productionPublicationWorkflow) prepareCheckFailureRemediation(
	ctx context.Context,
	task productionPublicationTask,
	initial domain.AgentInvocation,
	review domain.ReviewRecord,
	record domain.RequiredCheckFailure,
	checkoutDir string,
) (*preparedRemediationIntent, error) {
	successor := *task.Successor
	if len(initial.InputIDs) != 1 {
		return nil, domain.ErrParentKeyMismatch
	}
	account, err := checkFailureAccount(w.artifacts, successor.CheckFailureDigest, record)
	if err != nil {
		return nil, err
	}
	base, head := successor.Reentry.BaseSHA, successor.Reentry.HeadSHA
	patch, err := remediationCandidatePatch(ctx, w.workDir, checkoutDir, base, head)
	if err != nil {
		return nil, err
	}
	input := remediationInput{
		Version: remediationInputVersion, RunID: task.RunID, Round: review.Round,
		BaseSHA: base, HeadSHA: head,
		Instruction:          checkFailureInstruction,
		CandidatePatchBase64: patch,
		Findings:             []domain.Finding{},
		CheckFailure:         &account,
	}
	intent, err := w.prepareUnadjudicatedRemediation(task, initial, review, input, successor, "check failure")
	if err != nil {
		return nil, err
	}
	if !successor.AdmitsCheckFailureRemediation(domain.RemediationInvocationIntent(intent.request)) {
		return nil, fmt.Errorf("check failure remediation request outside its authority: %w",
			domain.ErrParentKeyMismatch)
	}
	return intent, nil
}

// persistCheckFailure commits the cycle's remediation. The cycle's task
// must be the pending row the intent was prepared from, and the review it
// answers must still be the run's latest; the task stays pending for the
// remediation's export to replace.
func (intent *preparedRemediationIntent) persistCheckFailure(
	ctx context.Context, tx *store.WriteTx, task productionPublicationTask,
) error {
	if err := requireTaskExecutionOpen(ctx, &tx.ReadTx, intent.request.RunID); err != nil {
		return err
	}
	latest, err := tx.LatestReviewRecord(ctx, intent.request.RunID)
	if err != nil || !reflect.DeepEqual(latest, intent.reviewRecord) {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	taskEntry, err := tx.GetOutbox(ctx, task.intentKey())
	if err != nil {
		return err
	}
	current, err := decodeProductionPublicationTask(taskEntry)
	if err != nil {
		return err
	}
	task.scopeDecision = nil
	if taskEntry.Dispatched() || !reflect.DeepEqual(current, task) {
		return domain.ErrImmutableTransition
	}
	return intent.commitFirstRound(ctx, tx)
}

// checkFailureStopReason names the failed check's jobs on the item a
// check_failure cycle stops on, and why no fix started. Job names were
// screened when the record was written.
func checkFailureStopReason(record domain.RequiredCheckFailure, why string) string {
	names := make([]string, 0, len(record.Jobs))
	for _, job := range record.Jobs {
		names = append(names, strconv.Quote(job.Name))
	}
	return fmt.Sprintf(
		"Freeside did not start a fix for the required check that failed on pull request head %s (failed jobs: %s) because %s. Read the check's logs and fix the head by hand.",
		record.HeadSHA, strings.Join(names, ", "), why)
}
