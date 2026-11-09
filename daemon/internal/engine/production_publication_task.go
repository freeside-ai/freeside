package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const productionPublicationTaskKeyPrefix = "production-publication/"

func productionPublicationTaskKey(runID domain.RunID) string {
	return productionPublicationTaskKeyPrefix + string(runID)
}

// productionRunIDFromPublicationTaskKey inverts productionPublicationTaskKey,
// reporting false for a key this lane could not have filed. Like the marker's
// inversion it attributes a row by its key, never by the payload that has
// just failed to reconstruct.
func productionRunIDFromPublicationTaskKey(key string) (domain.RunID, bool) {
	runID, ok := strings.CutPrefix(key, productionPublicationTaskKeyPrefix)
	if !ok || runID == "" {
		return "", false
	}
	return domain.RunID(runID), true
}

func productionRunIDFromPendingTaskKey(key string) (domain.RunID, bool) {
	if runID, _, ok := domain.PublicationContinuationCoordinates(key); ok {
		return runID, true
	}
	if runID, ok := productionRunIDFromPublicationTaskKey(key); ok {
		return runID, true
	}
	// The last segment is a command ID, or for a re-entry the digest of its
	// predecessor item; either way only the run is read from the key.
	for _, prefix := range []string{
		"production-publication-successor/", "production-publication-continuation/",
		"production-publication-reentry/",
	} {
		if rest, ok := strings.CutPrefix(key, prefix); ok {
			encodedRun, commandID, found := strings.Cut(rest, "/")
			runID, err := url.PathUnescape(encodedRun)
			if err != nil || !found || runID == "" || commandID == "" || url.PathEscape(runID) != encodedRun {
				return "", false
			}
			return domain.RunID(runID), true
		}
	}
	runID, _, ok := signet.PublicationReevaluationCoordinates(key)
	return runID, ok
}

func productionVerificationCheckpointKey(runID domain.RunID, headSHA, commandID string) string {
	key := "production-verification/" + string(runID) + "/" + headSHA
	if commandID != "" {
		key += "/reevaluation/" + commandID
	}
	return key
}

func (task productionPublicationTask) intentKey() string {
	if task.continuation != nil {
		return task.continuation.Key()
	}
	if task.reevaluation != nil {
		return task.reevaluation.IntentKey
	}
	if task.Successor != nil {
		return task.Successor.TaskKey()
	}
	return productionPublicationTaskKey(task.RunID)
}

// productionPublicationRoute names the path reconcileTask sends a task down.
// The route is computed from the task on each pass and never stored. The
// zero value "" is invalid.
type productionPublicationRoute string

const (
	// productionPublicationRouteReentryInPlace re-earns readiness for an
	// already published pull request: reconcileReentryTask.
	productionPublicationRouteReentryInPlace productionPublicationRoute = "reentry_in_place"
	// productionPublicationRouteRetireUnpublishedReentry retires a legacy
	// re-entry of an unpublished head: retireReentryTask.
	productionPublicationRouteRetireUnpublishedReentry productionPublicationRoute = "retire_unpublished_reentry"
	// productionPublicationRouteContinuation starts a remediation
	// continuation after import: startPublicationContinuation.
	productionPublicationRouteContinuation productionPublicationRoute = "continuation"
	// productionPublicationRouteForward imports, verifies, reviews, and
	// publishes the task's head: the rest of reconcileTask.
	productionPublicationRouteForward productionPublicationRoute = "forward"
)

var allProductionPublicationRoutes = []productionPublicationRoute{
	productionPublicationRouteReentryInPlace,
	productionPublicationRouteRetireUnpublishedReentry,
	productionPublicationRouteContinuation,
	productionPublicationRouteForward,
}

func (r productionPublicationRoute) valid() bool {
	switch r {
	case productionPublicationRouteReentryInPlace, productionPublicationRouteRetireUnpublishedReentry,
		productionPublicationRouteContinuation, productionPublicationRouteForward:
		return true
	default:
		return false
	}
}

// route picks the task's path from its fields alone; the first match wins,
// in the order reconcileTask dispatches. Two checks there are not routes,
// because they read the store: the completed-publication check, which runs
// in front of every route, and scope-conflict recovery, which runs in front
// of every route but in-place re-entry and, once a conflict is answered,
// falls through to the route.
func (task productionPublicationTask) route() productionPublicationRoute {
	switch {
	case task.reentersInPlace():
		return productionPublicationRouteReentryInPlace
	case task.Successor != nil && task.Successor.ReentersUnpublishedHead():
		return productionPublicationRouteRetireUnpublishedReentry
	case task.continuation != nil:
		return productionPublicationRouteContinuation
	default:
		return productionPublicationRouteForward
	}
}

func (task productionPublicationTask) readyItemID() domain.ItemID {
	if task.Successor != nil {
		return task.Successor.ReadyItemID()
	}
	return productionReadyItemID(task.RunID)
}

// reentryCheckpointKey names a re-entered cycle's verification checkpoint.
// Run plus head is not enough: a base-advance re-entry keeps its
// predecessor's head, so the key names the cycle.
func reentryCheckpointKey(runID domain.RunID, headSHA string, publicationID domain.InvocationID) string {
	return productionVerificationCheckpointKey(runID, headSHA, "") +
		"/reentry/" + string(publicationID)
}

// reentryCheckpointKey names the re-entry checkpoint of a task that
// re-enters in place or before publication. The second kind keeps its
// ordinary verificationCheckpointKey too: that names the checkpoint holding
// its candidate authorization, at the base its head was imported over.
func (task productionPublicationTask) reentryCheckpointKey() string {
	return reentryCheckpointKey(task.RunID, task.HeadSHA, task.Successor.PublicationID())
}

func (task productionPublicationTask) verificationCheckpointKey() string {
	if task.reentersInPlace() {
		return task.reentryCheckpointKey()
	}
	// A successor re-entry reads the checkpoint of the task it replaced,
	// which for a remediation continuation is keyed by its command.
	if task.reentersBeforePublication() {
		prefix := "production-publication-continuation/" + url.PathEscape(string(task.RunID)) + "/"
		if commandID, ok := strings.CutPrefix(task.AuthorizingTaskKey, prefix); ok {
			return productionVerificationCheckpointKey(task.RunID, task.HeadSHA, "") + "/continuation/" + commandID
		}
	}
	commandID := ""
	if task.reevaluation != nil {
		commandID = task.reevaluation.CommandID
	}
	if task.reevaluation == nil && task.Successor != nil && task.Successor.EffectiveOrigin() == domain.PublicationSuccessorRemediation {
		return productionVerificationCheckpointKey(task.RunID, task.HeadSHA, "") + "/continuation/" + task.Successor.CommandID
	}
	return productionVerificationCheckpointKey(task.RunID, task.HeadSHA, commandID)
}

func (task productionPublicationTask) verificationInvocationID() domain.InvocationID {
	if task.reevaluation != nil {
		return domain.InvocationID(string(task.VerificationID) + "/reevaluation/" + task.reevaluation.CommandID)
	}
	return task.VerificationID
}

// blockedMilestoneInvocationID gives each reevaluation's publication_blocked
// milestone its own identity; the store keeps one milestone per (run, kind,
// invocation), so a rerun on the shared publication invocation would record
// no block at all (signet.PublicationReevaluationBlockedMilestoneInvocationID).
func (task productionPublicationTask) blockedMilestoneInvocationID() domain.InvocationID {
	if task.reevaluation != nil {
		return signet.PublicationReevaluationBlockedMilestoneInvocationID(
			task.RunID, task.reevaluation.CommandID)
	}
	return task.PublicationID
}

func (task productionPublicationTask) blockedItemID() domain.ItemID {
	if task.reevaluation != nil {
		return signet.ReevaluatedBlockedItemID(task.RunID, task.reevaluation.CommandID)
	}
	if task.Successor != nil {
		return task.Successor.BlockedItemID()
	}
	return productionBlockedItemID(task.RunID)
}

func productionReadyItemID(runID domain.RunID) domain.ItemID {
	return domain.ProductionReadyItemID(runID)
}

func productionBlockedItemID(runID domain.RunID) domain.ItemID {
	return domain.ProductionBlockedItemID(runID)
}

func productionReviewItemID(runID domain.RunID, round int) domain.ItemID {
	return domain.ProductionFindingAdjudicationItemID(runID, round, 1)
}

func productionReviewHardLimitItemID(
	runID domain.RunID, hardLimit int, recoveredContradiction bool,
) domain.ItemID {
	if recoveredContradiction {
		// This namespace cannot collide with productionReviewItemID for any
		// legal RunID: their fixed prefixes diverge before the run coordinate.
		return domain.ItemID(fmt.Sprintf(
			"production-recovered-review-exhaustion-%s-%d", runID, hardLimit))
	}
	// Preserve the pre-recovery identity for every existing exhaustion path,
	// so an upgrade after its item write converges on the same durable row.
	return productionReviewItemID(runID, hardLimit)
}

// quarantineTaskRow holds the run named by a publication task row this daemon
// cannot reconstruct. The run comes from the row's key, so its project — which
// the notice needs — is read from the store rather than from the payload that
// just failed to decode. A run the store does not have is not quarantinable
// state and stays the caller's loud failure.
func (w *productionPublicationWorkflow) quarantineTaskRow(
	ctx context.Context, runID domain.RunID,
) (bool, error) {
	var run domain.Run
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, w.holdUnreadableTask(ctx, run, nil)
}

// releaseTaskQuarantineIfRowAbsent retires the task notice only when the task
// row itself is gone, which is how an operator repairs an unreadable one that
// the pending scan can no longer reach.
func (w *productionPublicationWorkflow) releaseTaskQuarantineIfRowAbsent(
	ctx context.Context, run domain.Run,
) error {
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := currentProductionTaskEntry(ctx, tx, run.ID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return releaseProductionQuarantine(
			ctx, w.store, w.attention, productionTaskQuarantinePrefix, run.ID)
	}
	return err
}

// holdUnreadableTask records one run's publication-task hold, keeping the
// cause with any store fault so the failure that triggered it is not dropped.
func (w *productionPublicationWorkflow) holdUnreadableTask(
	ctx context.Context, run domain.Run, cause error,
) error {
	if err := recordProductionQuarantine(
		ctx, w.store, w.attention, productionTaskQuarantinePrefix,
		run.ID, run.ProjectID, productionQuarantineUnreadableTask,
	); err != nil {
		return errors.Join(cause, err)
	}
	return nil
}

// quarantineTaskMarkers reports whether this task's run is held out of the
// lane by either ownership marker in its production transition. The active
// remediation transition comes from the run's durable stages, not from the
// producer currently carried by the publication task: before remediation
// export that task deliberately still names the prior producer. A task whose
// complete marker chain reads again retires each marker's notice.
func (w *productionPublicationWorkflow) quarantineTaskMarkers(
	ctx context.Context, task productionPublicationTask,
) (bool, error) {
	var (
		run        domain.Run
		transition authenticatedProductionRunTransition
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		transition, err = authenticateProductionRunTransition(ctx, tx, task.RunID)
		run = transition.run
		// A re-entered cycle carries no replay or publication to compare
		// with the remediation that produced its head; its authority is
		// re-gated against the store in loadBinding.
		if err != nil || transition.remediation == nil || task.reentersInPlace() {
			return err
		}
		verified := transition.remediation
		if verified.request.BaseSHA != task.Replay.ObservedBaseSHA ||
			!reflect.DeepEqual(verified.publication, task.Publication) {
			return taskMarkerMismatch(domain.ErrParentKeyMismatch)
		}
		if task.ProducingInvocationID == verified.request.InvocationID {
			return nil
		}
		producerRound, priorRemediation := remediationRoundForInvocation(
			task.RunID, task.ProducingInvocationID)
		// A successor re-entry keeps the producer of the cycle it replaced
		// (issue #2111), so the feedback that produced its head belongs to
		// the first successor behind it that is not one.
		producing := task.Successor
		for producing != nil && producing.ReentersUnpublishedHead() && !producing.RepublishesUnpublished() {
			parent, err := tx.PublicationSuccessorForReadyItem(ctx, task.RunID, producing.PredecessorItemID)
			if err != nil {
				return taskMarkerMismatch(errors.Join(err, domain.ErrParentKeyMismatch))
			}
			producing = &parent
		}
		feedback := producing != nil &&
			task.ProducingInvocationID == producing.FeedbackInvocationID
		priorFeedback := feedback &&
			verified.request.SuccessorPublicationID == producing.PublicationID()
		// Feedback returned on a head reviewed after this remediation is a
		// later producer: a round-N remediation answers review N, so its
		// head is reviewed at N+1 or later, and the successor's review
		// round, sealed one past the review covering the returned head,
		// exceeds N+1. Its head is the feedback's own, not the head the
		// remediation was asked to fix.
		laterFeedback := feedback && !priorFeedback &&
			producing.ReviewRound > verified.request.Round+1
		if priorFeedback || laterFeedback {
			if err := authenticateTaskSuccessor(ctx, tx, task); err != nil {
				return taskMarkerMismatch(err)
			}
		}
		if laterFeedback {
			return nil
		}
		priorProducer := task.ProducingInvocationID == productionInvocationID(task.RunID) ||
			priorFeedback || priorRemediation && producerRound < verified.request.Round
		if !priorProducer || task.HeadSHA != verified.request.HeadSHA {
			return taskMarkerMismatch(domain.ErrParentKeyMismatch)
		}
		return nil
	})
	if err == nil {
		err = authenticateProductionRunInput(w.artifacts, transition)
	}
	if _, markerFailure := productionQuarantineReason(err); markerFailure {
		quarantined, quarantineErr := quarantineProductionMarker(
			ctx, w.store, w.attention, run.ID, run.ProjectID, err)
		if quarantineErr != nil {
			return false, errors.Join(err, quarantineErr)
		}
		if quarantined {
			return true, nil
		}
		return false, err
	}
	if errors.Is(err, errTaskMarkerMismatch) {
		if quarantineErr := recordProductionQuarantine(
			ctx, w.store, w.attention, productionTaskMarkerQuarantinePrefix,
			run.ID, run.ProjectID, productionQuarantineTaskMarker,
		); quarantineErr != nil {
			return false, errors.Join(err, quarantineErr)
		}
		return true, nil
	}
	if errors.Is(err, errRemediationMarkerUnreadable) {
		if quarantineErr := recordProductionQuarantine(
			ctx, w.store, w.attention, remediationMarkerQuarantinePrefix,
			run.ID, run.ProjectID, remediationQuarantineUnreadable,
		); quarantineErr != nil {
			return false, errors.Join(err, quarantineErr)
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	for _, prefix := range []string{
		productionMarkerQuarantinePrefix,
		remediationMarkerQuarantinePrefix,
		productionTaskMarkerQuarantinePrefix,
	} {
		if err := releaseProductionQuarantine(
			ctx, w.store, w.attention, prefix, task.RunID); err != nil {
			return false, err
		}
	}
	return false, nil
}

// errTaskMarkerMismatch marks a publication task that disagrees with its
// run's remediation record although the record itself authenticates. Only the
// publication lane makes that comparison, so it holds the run under its own
// notice: under the shared remediation notice, the production lane, which
// authenticates the record alone, retired it every pass and this lane raised
// a new one, notifying every device each time (#2102).
var errTaskMarkerMismatch = errors.New("publication task disagrees with its remediation marker")

func taskMarkerMismatch(err error) error {
	err = classifyRemediationMarkerError(err)
	if errors.Is(err, errRemediationMarkerUnreadable) {
		return errors.Join(errTaskMarkerMismatch, err)
	}
	return err
}

func (w *productionPublicationWorkflow) loadProductionRequest(
	ctx context.Context,
	run domain.Run,
) (productionInvocationRequest, error) {
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(productionInvocationID(run.ID)))
		return err
	})
	if err != nil {
		return productionInvocationRequest{}, fmt.Errorf("load production submission metadata: %w", err)
	}
	request, err := authenticateProductionMarker(entry, run.ID)
	if err != nil {
		return productionInvocationRequest{}, fmt.Errorf("authenticate production submission metadata: %w", err)
	}
	return request, nil
}

func (t productionPublicationTask) validate() error {
	return t.validateWithPublication(ProductionPublication.Validate)
}

func (t productionPublicationTask) validateWithPublication(validatePublication func(ProductionPublication) error) error {
	if t.reentersInPlace() {
		return t.validateReentry()
	}
	_, remediationProducer := remediationRoundForInvocation(t.RunID, t.ProducingInvocationID)
	validProducer := t.ProducingInvocationID == productionInvocationID(t.RunID) || remediationProducer
	wantPublication := domain.ProductionPublicationInvocationID(t.RunID)
	if t.Successor != nil {
		if err := t.Successor.Validate(); err != nil || t.Successor.RunID != t.RunID {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		switch {
		case t.Successor.RepublishesUnpublished():
		case t.Successor.ReentersUnpublishedHead():
			// A successor re-entry keeps the head its predecessor's producer
			// exported: an operator feedback return or a remediation.
			validProducer = remediationProducer ||
				strings.HasPrefix(string(t.ProducingInvocationID), "inv-operator-feedback-")
		default:
			validProducer = t.ProducingInvocationID == t.Successor.FeedbackInvocationID || remediationProducer
		}
		wantPublication = t.Successor.PublicationID()
	}
	if t.Version != productionPublicationTaskVersion || t.RunID == "" || t.ProjectID == "" ||
		!validProducer ||
		(t.AuthorizingTaskKey != "") != t.reentersBeforePublication() ||
		(t.LegacyRemediationNoop && !remediationProducer) ||
		t.VerificationID != productionVerificationInvocationIDForProducer(t.RunID, t.ProducingInvocationID) ||
		t.PublicationID != wantPublication ||
		!validCommitSHA(t.HeadSHA) {
		return fmt.Errorf("invalid production publication task: %w", domain.ErrParentKeyMismatch)
	}
	for _, artifact := range t.Artifacts {
		if !contentaddr.Valid(string(artifact)) {
			return fmt.Errorf("production publication task artifact %q is invalid", artifact)
		}
	}
	if t.Replay.InvocationID != t.ProducingInvocationID ||
		!validCommitSHA(t.Replay.ObservedBaseSHA) || t.Replay.HeadSHA != t.HeadSHA {
		return fmt.Errorf("production publication replay disagrees with task: %w", domain.ErrParentKeyMismatch)
	}
	if err := validateProductionReplayRecord(t.Replay); err != nil {
		return err
	}
	if err := validatePublication(t.Publication); err != nil {
		return err
	}
	replayDigests := productionReplayDigests(t.Replay)
	if len(replayDigests) == 0 {
		return errors.New("production publication replay digests are absent")
	}
	for _, digest := range replayDigests {
		if !contentaddr.Valid(string(digest)) {
			return fmt.Errorf("production publication replay digest %q is invalid", digest)
		}
	}
	return nil
}

func validateProductionReplayRecord(replay ProductionReplay) error {
	manifest, err := replay.Manifest.Encode()
	if err != nil || digestProductionBytes(manifest) != replay.ManifestDigest {
		return fmt.Errorf("production replay manifest disagrees with its role digest: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if replay.EvidenceManifestDigest == nil {
		if replay.Evidence.Version != "" || len(replay.Evidence.Entries) != 0 {
			return fmt.Errorf("production replay carries evidence without a role digest: %w",
				domain.ErrParentKeyMismatch)
		}
	} else {
		evidence, err := replay.Evidence.Encode()
		if err != nil || digestProductionBytes(evidence) != *replay.EvidenceManifestDigest {
			return fmt.Errorf("production replay evidence disagrees with its role digest: %w",
				errors.Join(err, domain.ErrParentKeyMismatch))
		}
	}
	if replay.CommitPlanDigest != nil && !contentaddr.Valid(string(*replay.CommitPlanDigest)) {
		return fmt.Errorf("production replay commit-plan digest %q is invalid", *replay.CommitPlanDigest)
	}
	if !validCommitSHA(replay.ImportOptions.BaseSHA) || replay.ImportOptions.CommitDate.IsZero() {
		return fmt.Errorf("production replay import coordinates are invalid: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

func productionReplayDigests(replay ProductionReplay) []domain.Digest {
	set := map[domain.Digest]struct{}{replay.ManifestDigest: {}}
	if replay.EvidenceManifestDigest != nil {
		set[*replay.EvidenceManifestDigest] = struct{}{}
	}
	if replay.CommitPlanDigest != nil {
		set[*replay.CommitPlanDigest] = struct{}{}
	}
	for _, entry := range replay.Manifest.Entries {
		if entry.Kind == export.EntryRegular && entry.Digest != nil {
			set[domain.Digest(*entry.Digest)] = struct{}{}
		}
	}
	for _, entry := range replay.Evidence.Entries {
		set[domain.Digest(entry.Digest)] = struct{}{}
	}
	out := make([]domain.Digest, 0, len(set))
	for digest := range set {
		out = append(out, digest)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func decodeProductionPublicationTask(entry store.QueueEntry) (productionPublicationTask, error) {
	return decodeProductionPublicationTaskWithPublication(entry, ProductionPublication.Validate)
}

func decodeProductionPublicationTaskWithPublication(
	entry store.QueueEntry, validatePublication func(ProductionPublication) error,
) (productionPublicationTask, error) {
	if entry.Kind != KindProductionPublicationRequested {
		return productionPublicationTask{}, fmt.Errorf("task %q has kind %q: %w",
			entry.IdempotencyKey, entry.Kind, domain.ErrParentKeyMismatch)
	}
	var task productionPublicationTask
	if err := strictjson.Decode(entry.Payload, &task, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return productionPublicationTask{}, errors.New("production publication task has trailing content")
		}
		return productionPublicationTask{}, err
	}
	if err := task.validateWithPublication(validatePublication); err != nil {
		return productionPublicationTask{}, err
	}
	if entry.IdempotencyKey != task.intentKey() {
		return productionPublicationTask{}, fmt.Errorf("production publication task key disagrees with run: %w",
			domain.ErrParentKeyMismatch)
	}
	return task, nil
}

func (w *productionPublicationWorkflow) reconstructProductionReevaluationTask(
	ctx context.Context, entry store.QueueEntry,
) (productionPublicationTask, error) {
	if entry.Kind != signet.PublicationReevaluationRequestedKind {
		return productionPublicationTask{}, domain.ErrParentKeyMismatch
	}
	request, err := signet.DecodePublicationReevaluationRequest(entry.Payload)
	if err != nil {
		return productionPublicationTask{}, err
	}
	keyRunID, keyCommandID, ok := signet.PublicationReevaluationCoordinates(entry.IdempotencyKey)
	if !ok || keyRunID != request.RunID || keyCommandID != request.CommandID ||
		entry.IdempotencyKey != signet.PublicationReevaluationKey(request.RunID, request.CommandID) {
		return productionPublicationTask{}, domain.ErrParentKeyMismatch
	}
	var (
		command         domain.Command
		item            domain.AttentionItem
		taskRow         store.QueueEntry
		project         domain.Project
		acceptedProfile domain.AutomationTrustProfile
	)
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		command, _, err = tx.GetCommandSnapshot(ctx, request.CommandID)
		if err != nil {
			return err
		}
		item, err = tx.GetAttentionItemRecord(ctx, request.ItemID)
		if err != nil {
			return err
		}
		project, err = tx.GetProject(ctx, item.ProjectID)
		if err != nil {
			return err
		}
		acceptedProfile, err = tx.GetTrustProfile(ctx, request.TrustProfileDigest)
		if err != nil {
			return err
		}
		taskRow, err = productionTaskForBlockedItem(ctx, tx, request.RunID, request.ItemID)
		return err
	})
	if err != nil {
		return productionPublicationTask{}, err
	}
	task, err := decodeProductionPublicationTask(taskRow)
	if err != nil {
		return productionPublicationTask{}, err
	}
	itemRunID := item.Subject.RunID
	identityValid := item.ID == domain.ProductionBlockedItemID(request.RunID)
	if !identityValid {
		err = w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			identityValid, err = signet.AuthenticateReevaluatedBlockedItemIdentity(
				ctx, tx, item.ID, request.RunID, task.ProjectID,
			)
			return err
		})
		if err != nil {
			return productionPublicationTask{}, err
		}
	}
	if command.CommandID != request.CommandID || command.Action != domain.ActionRerunTrustEvaluation ||
		command.ItemID != request.ItemID || command.ItemVersion != request.ItemVersion ||
		command.PRHeadSHA != request.PRHeadSHA || itemRunID == nil || *itemRunID != request.RunID ||
		item.Subject.Type != domain.SubjectRun || item.Subject.ID != domain.SubjectID(request.RunID) ||
		item.ProjectID != task.ProjectID || item.Type != domain.AttentionPublishBlocked ||
		project.ID != item.ProjectID || project.RepositoryID != acceptedProfile.RepositoryID ||
		project.Repo != acceptedProfile.Repo ||
		request.TrustProfileDigest != acceptedProfile.ProfileDigest ||
		item.Status != domain.StatusResolved ||
		item.DecidedAt == nil || item.ItemVersion != request.ItemVersion+1 ||
		item.PRHeadSHA != request.PRHeadSHA ||
		!slices.Equal(command.ArtifactDigests, item.ArtifactDigests) || !identityValid ||
		!slices.Equal(item.RequestedDecision, productionRerunnableBlockActions) ||
		!taskRow.Dispatched() {
		return productionPublicationTask{}, domain.ErrParentKeyMismatch
	}
	if _, definitive := domain.DefinitivePublicationBlockReason(item.Reason); !definitive {
		return productionPublicationTask{}, domain.ErrParentKeyMismatch
	}
	if task.RunID != request.RunID || task.HeadSHA != request.PRHeadSHA {
		return productionPublicationTask{}, domain.ErrParentKeyMismatch
	}
	task.reevaluation = &productionReevaluation{
		CommandID: request.CommandID, ItemVersion: request.ItemVersion,
		IntentKey: entry.IdempotencyKey, TrustProfileDigest: request.TrustProfileDigest,
		ReviewRound: request.ReviewRound,
	}
	return task, nil
}
