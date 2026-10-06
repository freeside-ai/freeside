package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// A re-entered cycle (plan §7, issue #502) re-earns readiness for a pull
// request that is already published, after its ready item was superseded
// because the base advanced or the head changed. It imports nothing, pushes
// nothing, and publishes nothing: it fetches the base its authority names,
// fetches the pull request's head, verifies and reviews that pair (through
// the prospective merge for a base advance), and writes a fresh ready item
// bound in place to the same pull request.

const (
	productionReentryCheckpointVersion = "freeside.production-reentry-verification/v1"
	// A re-entered cycle's checkpoint has its own kind because it carries no
	// import account and no stored authorization, so a reader of the original
	// kind can never decode one as a publishable candidate.
	productionReentryCheckpointKind = "production_reentry_verification_checkpoint"
	// reentryConflictPathLimit bounds the conflicting paths a stop item names.
	reentryConflictPathLimit = 20
	// reentryQuotedTextLimit bounds, in bytes, one piece of quoted
	// tree-derived text in a stop item's reason.
	reentryQuotedTextLimit = 256
)

// reentryStopActions are the decisions on the item that ends a re-entered
// cycle. Rerunning trust evaluation is not offered: the cycle has no stored
// candidate authorization to re-evaluate.
var reentryStopActions = []domain.Action{
	domain.ActionInspectTrustFailure, domain.ActionOpenPR, domain.ActionStop,
}

// reentryCycle is the store-proven context of one re-entered cycle.
type reentryCycle struct {
	// predecessor is the superseded ready item's pull-request binding. The
	// cycle's own bindings restate its repository, pull request, producing
	// invocation, and publication identity.
	predecessor domain.ReadyItemPRBinding
	// branch is the pull request's head branch, from the publication record.
	branch string
	// createdAt is when the cycle's task row was recorded. It stands in for
	// the export time a first cycle stamps its evidence with, so a re-run
	// reproduces the same evidence bytes.
	createdAt time.Time
}

// productionReentryCheckpoint is the durable verification result of one
// re-entered cycle. It is engine-private and carries what the review gate and
// the ready item read from a first cycle's candidate authorization, which a
// re-entered cycle does not record: the store keeps one authorization per
// repository, head, and trust profile, and a base-advance re-entry keeps its
// predecessor's head.
type productionReentryCheckpoint struct {
	Version string `json:"version"`
	TaskKey string `json:"task_key"`
	BaseSHA string `json:"base_sha"`
	HeadSHA string `json:"head_sha"`
	// EvaluatedSHA is the prospective merge a base advance verified in place
	// of its head, and empty for a head change.
	EvaluatedSHA           string                     `json:"evaluated_sha"`
	ProjectImage           domain.Digest              `json:"project_image"`
	Outcome                domain.VerificationOutcome `json:"outcome"`
	RecipeDigest           domain.Digest              `json:"recipe_digest"`
	EvidenceSnapshotDigest domain.Digest              `json:"evidence_snapshot_digest"`
	Findings               []domain.CandidateFinding  `json:"findings"`
	Artifacts              []domain.Artifact          `json:"artifacts"`
	DiffStats              *domain.DiffStats          `json:"diff_stats"`
}

// clean reports whether verification passed with nothing flagged, the same
// rule a candidate authorization applies before it authorizes publication.
func (c productionReentryCheckpoint) clean() bool {
	return c.Outcome == domain.VerificationPassed && len(c.Findings) == 0
}

// view presents the checkpoint in the shape the shared review gate and ready
// item read. It exists only in memory: the authorization it carries has no ID
// and is never stored, compared with a stored one, or offered to the
// publication gate.
func (c productionReentryCheckpoint) view(repo string) productionVerificationCheckpoint {
	return productionVerificationCheckpoint{
		Version: c.Version, TaskKey: c.TaskKey, HeadSHA: c.HeadSHA,
		ProjectImage: c.ProjectImage,
		Imported:     importer.Result{CommitSHA: c.HeadSHA},
		Authorization: domain.CandidateAuthorization{
			Repo: repo, BaseSHA: c.BaseSHA, HeadSHA: c.HeadSHA,
			VerificationRecipeDigest: c.RecipeDigest,
			EvidenceSnapshotDigest:   c.EvidenceSnapshotDigest,
			VerificationOutcome:      c.Outcome,
		},
		Artifacts: c.Artifacts, DiffStats: c.DiffStats,
	}
}

// reentersInPlace reports whether the task re-earns readiness for an already
// published pull request. Such a task has no producing invocation: nothing
// was exported for it.
func (t productionPublicationTask) reentersInPlace() bool {
	return t.Successor != nil && t.Successor.Reentry != nil && t.ProducingInvocationID == ""
}

func reentryVerificationInvocationID(successor domain.PublicationSuccessor) domain.InvocationID {
	return domain.InvocationID(
		"verify-" + strings.TrimPrefix(string(successor.PublicationID()), "publish-"))
}

// newReentryTask builds the task row of one re-entered cycle from its sealed
// authority. Every coordinate derives from the authority, so validateReentry
// can re-derive and compare each one on decode.
func newReentryTask(
	projectID domain.ProjectID, successor domain.PublicationSuccessor,
) productionPublicationTask {
	return productionPublicationTask{
		Version: productionPublicationTaskVersion,
		RunID:   successor.RunID, ProjectID: projectID,
		VerificationID: reentryVerificationInvocationID(successor),
		PublicationID:  successor.PublicationID(),
		HeadSHA:        successor.Reentry.HeadSHA,
		Successor:      &successor,
	}
}

func (t productionPublicationTask) validateReentry() error {
	successor := *t.Successor
	if err := successor.Validate(); err != nil || successor.RunID != t.RunID {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	if t.Version != productionPublicationTaskVersion || t.RunID == "" || t.ProjectID == "" ||
		t.LegacyRemediationNoop ||
		t.VerificationID != reentryVerificationInvocationID(successor) ||
		t.PublicationID != successor.PublicationID() ||
		!validCommitSHA(t.HeadSHA) || t.HeadSHA != successor.Reentry.HeadSHA ||
		!validCommitSHA(successor.Reentry.BaseSHA) ||
		len(t.Artifacts) != 0 || t.Summary != "" ||
		!reflect.DeepEqual(t.Replay, ProductionReplay{}) ||
		!reflect.DeepEqual(t.Publication, ProductionPublication{}) {
		return fmt.Errorf("invalid production re-entry task: %w", domain.ErrParentKeyMismatch)
	}
	return nil
}

// loadReentryBinding rebuilds a re-entered cycle's authority from the store.
// The image, trust profile, and policy are the predecessor producer's; the
// base is the one the sealed authority names, which is why the returned
// admission carries it in place of the base the producer was admitted at.
func (w *productionPublicationWorkflow) loadReentryBinding(
	ctx context.Context, task productionPublicationTask,
) (productionBinding, error) {
	var (
		binding productionBinding
		cycle   reentryCycle
		target  struct {
			headSHA  string
			prNumber int
		}
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		binding.run, err = tx.GetRun(ctx, task.RunID)
		if err != nil {
			return err
		}
		if err := authenticateTaskSuccessor(ctx, tx, task); err != nil {
			return err
		}
		entry, err := tx.GetOutbox(ctx, task.intentKey())
		if err != nil {
			return err
		}
		cycle.createdAt = entry.CreatedAt.UTC()
		cycle.predecessor, err = tx.GetReadyItemPRBinding(ctx, task.Successor.PredecessorItemID)
		if err != nil {
			return err
		}
		published, err := tx.PublicationSuccessorTarget(ctx, task.RunID, task.PublicationID)
		if err != nil {
			return err
		}
		cycle.branch, target.headSHA, target.prNumber = published.Branch, published.HeadSHA, published.PRNumber
		binding.admission, err = tx.GetExecutionAdmissionRecord(
			ctx, cycle.predecessor.ProducingInvocationID)
		if err != nil {
			return err
		}
		binding.resolvedPolicy, err = tx.GetResolvedPolicy(ctx, task.RunID)
		if err != nil {
			return err
		}
		declaration, declarationErr := tx.GetWorkUnitDeclarationByRun(ctx, task.RunID)
		switch {
		case declarationErr == nil:
			binding.declaration = &declaration
		case errors.Is(declarationErr, store.ErrNotFound):
		default:
			return declarationErr
		}
		if binding.admission.TrustProfileDigest == nil {
			binding.profile, err = tx.LatestTrustProfile(ctx, binding.admission.Base.Repo)
		} else {
			binding.profile, err = tx.GetTrustProfile(ctx, *binding.admission.TrustProfileDigest)
		}
		if err != nil {
			return err
		}
		images, err := tx.ListProjectImages(ctx, binding.admission.Base.RepositoryID)
		if err != nil {
			return err
		}
		for _, image := range images {
			if image.ImageRef == binding.admission.ImageRef {
				if binding.image.ID != "" {
					return fmt.Errorf("multiple project-image records name the admitted image: %w",
						domain.ErrParentKeyMismatch)
				}
				binding.image = image
			}
		}
		if binding.image.ID == "" {
			return fmt.Errorf("admitted image has no project-image record: %w",
				domain.ErrParentKeyMismatch)
		}
		return nil
	})
	if err != nil {
		return productionBinding{}, fmt.Errorf("load production re-entry authority: %w", err)
	}
	stage, stageFound := productionStageForInvocation(
		binding.run, cycle.predecessor.ProducingInvocationID)
	base := binding.admission.Base
	if binding.run.ID != task.RunID || binding.run.ProjectID != task.ProjectID ||
		binding.run.SpecDigest != binding.admission.SpecDigest ||
		binding.admission.RunID != task.RunID ||
		!stageFound || binding.admission.StageID != stage.ID ||
		cycle.predecessor.RunID != task.RunID ||
		cycle.predecessor.Repo != base.Repo ||
		cycle.predecessor.RepositoryID != base.RepositoryID ||
		cycle.predecessor.BaseRef != base.BaseRef ||
		cycle.createdAt.IsZero() || cycle.branch == "" ||
		target.prNumber != cycle.predecessor.PRNumber || target.headSHA != task.HeadSHA ||
		binding.resolvedPolicy.RunID != binding.run.ID ||
		binding.resolvedPolicy.Digest != binding.admission.PolicyDigest ||
		(binding.admission.TrustProfileDigest != nil &&
			binding.profile.ProfileDigest != *binding.admission.TrustProfileDigest) ||
		binding.profile.Repo != base.Repo ||
		binding.profile.RepositoryID != base.RepositoryID ||
		binding.image.Repository != base.Repo ||
		binding.image.RepositoryID != base.RepositoryID ||
		// The image is checked against the base its producer was admitted at.
		// The cycle keeps that image on its newer base (issue #502 non-goals).
		binding.image.CommitSHA != base.BaseSHA {
		return productionBinding{}, fmt.Errorf(
			"production re-entry binding disagrees with durable authority: %w",
			domain.ErrParentKeyMismatch)
	}
	binding.admission.Base.BaseSHA = task.Successor.Reentry.BaseSHA
	binding.reentry = &cycle
	return binding, nil
}

// reentryPullRequest is the pull request a re-entered cycle's holds carry: the
// one its predecessor's ready binding already proves.
func (w *productionPublicationWorkflow) reentryPullRequest(
	ctx context.Context, task productionPublicationTask,
) (*recordedPullRequest, error) {
	var predecessor domain.ReadyItemPRBinding
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		predecessor, err = tx.GetReadyItemPRBinding(ctx, task.Successor.PredecessorItemID)
		return err
	}); err != nil {
		return nil, fmt.Errorf("load re-entry pull request: %w", err)
	}
	if predecessor.RunID != task.RunID {
		return nil, fmt.Errorf("re-entry predecessor binding names another run: %w",
			domain.ErrParentKeyMismatch)
	}
	return &recordedPullRequest{
		identity: predecessor.PublicationIdentity,
		base: domain.BaseRevision{
			Repo: predecessor.Repo, RepositoryID: predecessor.RepositoryID,
			BaseRef: predecessor.BaseRef,
		},
		number:   predecessor.PRNumber,
		producer: predecessor.ProducingInvocationID,
	}, nil
}

func (w *productionPublicationWorkflow) reconcileReentryTask(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) (productionTaskOutcome, error) {
	cycle := binding.reentry
	base := binding.admission.Base
	held := importer.Result{CommitSHA: task.HeadSHA}
	scratch, err := os.MkdirTemp(w.workDir, ".production-publication-")
	if err != nil {
		return productionTaskOutcome{}, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // run-owned scratch
	parentInfo, err := os.Stat(scratch)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	checkout, err := w.transport.FetchBase(
		ctx, base.Repo, base.BaseRef, base.BaseSHA, filepath.Join(scratch, "checkout"))
	if err != nil {
		return w.reentryTransportFailure(ctx, task, "fetch exact re-entry base", err)
	}
	checkoutDir, err := validatePublicationCheckoutBinding(
		checkout, base.Repo, base.BaseRef, base.BaseSHA,
		filepath.Join(scratch, "checkout"), parentInfo,
	)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	// The head is a commit Freeside did not produce. The transport proves the
	// branch holds exactly the commit the authority names, and nothing below
	// evaluates any other commit of that branch.
	fetched, err := w.transport.FetchHead(ctx, checkout, cycle.branch, task.HeadSHA)
	if err != nil {
		// A missing head also matches the transport class, so it is read first.
		if errors.Is(err, publish.ErrRemoteMissingHead) {
			return w.stopReentryCycle(ctx, task, reentryMissingHeadReason(task, cycle.branch), nil)
		}
		return w.reentryTransportFailure(ctx, task, "fetch pull request head", err)
	}
	if task.Successor.Reentry.Reason == domain.ReadinessInvalidationBaseAdvanced {
		if task.HeadSHA == base.BaseSHA {
			return w.stopReentryCycle(ctx, task, fmt.Sprintf(
				"Readiness re-entry stopped because the pull request head %s is the base commit itself, so there is nothing to merge. Inspect the pull request.",
				task.HeadSHA), nil)
		}
		merge, err := w.transport.ProspectiveMerge(ctx, checkout, task.HeadSHA)
		if err != nil {
			var conflict *publish.MergeConflictError
			if errors.As(err, &conflict) {
				return w.stopReentryCycle(ctx, task, reentryConflictReason(task, base.BaseSHA, conflict), nil)
			}
			return w.reentryTransportFailure(ctx, task, "build prospective merge", err)
		}
		// The merge is evidence only for the pair the authority sealed. Nothing
		// verifies, reviews, or proves against it before this gate admits it.
		if !task.Successor.AllowsProspectiveMerge(domain.ProspectiveMergeIdentity{
			BaseSHA: base.BaseSHA, HeadSHA: task.HeadSHA, MergeSHA: merge,
		}) {
			return productionTaskOutcome{}, fmt.Errorf(
				"re-entry authority does not admit prospective merge %q: %w",
				merge, domain.ErrParentKeyMismatch)
		}
		binding.evaluatedSHA = merge
	} else if !fetched.DescendsFromBase {
		if task.answersExternalReview() {
			// The last review covered the head's merge into an advanced base.
			// The external review authority admits no prospective merge, so
			// this cycle has nothing it may review.
			return w.stopReentryCycle(ctx, task, fmt.Sprintf(
				"Freeside did not review pull request head %s again because it does not descend from the reviewed base %s. That is expected after the base advanced: the last review covered their merge, and this cycle reviews only the head.",
				task.HeadSHA, base.BaseSHA), nil)
		}
		return w.stopReentryCycle(ctx, task, fmt.Sprintf(
			"Readiness re-entry stopped because the pull request head %s does not descend from the reviewed base %s. Inspect the pull request.",
			task.HeadSHA, base.BaseSHA), nil)
	}
	reviewInstructions, heldOutcome, err := w.exactBaseReviewInstructions(
		ctx, task, checkout, filepath.Join(scratch, "base-instructions"), base.BaseSHA)
	if err != nil || heldOutcome != nil {
		if heldOutcome != nil {
			return *heldOutcome, nil
		}
		return productionTaskOutcome{}, err
	}
	stored, found, err := w.loadReentryCheckpoint(ctx, task, binding)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	readyExists, err := w.hasReadyItemRecord(ctx, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if readyExists && !found {
		return productionTaskOutcome{}, fmt.Errorf(
			"ready item %q has no verification checkpoint: %w",
			task.readyItemID(), domain.ErrParentKeyMismatch)
	}
	// New readiness needs a still-approved recipe; an item that is already
	// durably ready finishes without re-deriving its proofs (issue #527).
	if !readyExists && !w.approvedRecipes[binding.image.RecipeDigest] {
		return w.holdBlockedTask(ctx, task, held,
			"Readiness re-entry is durably held because current trust no longer approves the admitted project-image recipe. Restore that approval to resume.",
			domain.HoldRecipeRevoked)
	}
	if !found {
		stored, err = w.verifyReentry(ctx, task, binding, checkoutDir)
		if reason, stop := reentryUnverifiableTreeReason(task, base.BaseSHA, err); stop {
			return w.stopReentryCycle(ctx, task, reason, nil)
		}
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if w.afterVerification != nil {
			if err := w.afterVerification(); err != nil {
				return productionTaskOutcome{}, fmt.Errorf("after production verification: %w",
					errors.Join(err, errProductionCrashSeam))
			}
		}
	}
	if !stored.clean() {
		return w.stopReentryCycle(ctx, task, fmt.Sprintf(
			"Readiness re-entry stopped because verification of pull request head %s against base %s did not pass cleanly. Inspect the verification evidence.",
			task.HeadSHA, base.BaseSHA), stored.Artifacts)
	}
	checkpoint := stored.view(base.Repo)
	reviewState, err := w.reconcileReviewGate(
		ctx, task, binding, checkpoint, checkout, reviewInstructions)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	switch reviewState {
	case productionReviewPending:
		return productionTaskOutcome{}, nil
	case productionReviewEscalated:
		// The review item is the durable surface for a person. No terminal is
		// recorded: the run already published, and a re-entered cycle starts no
		// remediation. A hold an earlier pass recorded describes a retry that
		// will not happen, so it ends with the task.
		if err := w.supersedeBlockedHold(ctx, task); err != nil {
			return productionTaskOutcome{}, err
		}
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.ClearRunHold(ctx, task.RunID)
		}); err != nil {
			return productionTaskOutcome{}, err
		}
		return productionTaskOutcome{}, w.finishTask(ctx, task)
	case productionReviewPassed:
	case productionReviewContinue:
		return productionTaskOutcome{}, fmt.Errorf(
			"review continuation escaped the review gate: %w", domain.ErrParentKeyMismatch)
	}
	return w.completeReentryTask(ctx, task, binding, checkpoint, checkout, reviewInstructions)
}

// reentryTransportFailure classifies a transport failure the way the first
// cycle classifies a failed base fetch: a definitive refusal holds the run,
// and anything else is retried or surfaced.
func (w *productionPublicationWorkflow) reentryTransportFailure(
	ctx context.Context, task productionPublicationTask, step string, err error,
) (productionTaskOutcome, error) {
	held := importer.Result{CommitSHA: task.HeadSHA}
	if isDefinitiveTrustRefusal(err) {
		return w.holdBlockedTask(ctx, task, held, productionBlockTrust, domain.HoldTrustBlocked)
	}
	if productionPublicationPermanentExternalFailure(err) {
		return w.holdBlockedTask(ctx, task, held, productionBlockExternal, domain.HoldExternalConflict)
	}
	if productionPublicationRetryableFailure(err) {
		err = productionPublicationRetryableError(err)
	}
	return productionTaskOutcome{}, fmt.Errorf("%s: %w", step, err)
}

func reentryMissingHeadReason(task productionPublicationTask, branch string) string {
	return fmt.Sprintf(
		"Readiness re-entry stopped because pull request branch %s does not contain the head commit %s it must evaluate. Inspect the pull request.",
		strconv.Quote(branch), task.HeadSHA)
}

// reentryQuoted quotes text that carries tree paths from a branch anyone with
// push access can write, cut to a bounded length first: a stop item's reason
// is stored and synced, and git bounds neither a path nor a tree's size.
func reentryQuoted(text string) string {
	return quotedWithin(text, reentryQuotedTextLimit)
}

// quotedWithin quotes text nobody at Freeside wrote, cut to limit bytes first.
// The cut can split a character, so the tail is repaired before quoting.
func quotedWithin(text string, limit int) string {
	if len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "") + "..."
	}
	return strconv.Quote(text)
}

// reentryUnverifiableTreeReason reports a verification refusal that the
// evaluated tree itself causes, with the reason the cycle stops on. On a
// first cycle the importer refuses such a tree before verification, so there
// these classes contradict stored state and stop the lane. A re-entry
// evaluates a commit the importer never saw: returned, the error would stop
// the lane for every run, on every pass, for as long as the push stands.
func reentryUnverifiableTreeReason(
	task productionPublicationTask, baseSHA string, err error,
) (string, bool) {
	var cause string
	switch {
	case errors.Is(err, verify.ErrSymlinkEntrypoint):
		cause = "a verification command's entrypoint is a symbolic link in its tree"
	case errors.Is(err, verify.ErrMalformedTree):
		cause = "its tree holds paths that cannot be materialized safely"
	case errors.Is(err, verify.ErrWorkspaceMismatch):
		cause = "its tree did not materialize faithfully on this host"
	default:
		return "", false
	}
	return fmt.Sprintf(
		"Readiness re-entry stopped because pull request head %s cannot be verified against base %s: %s (%s). Change the pull request so its tree can be verified.",
		task.HeadSHA, baseSHA, cause, reentryQuoted(err.Error())), true
}

// reentryConflictReason names both commits and the conflicting paths. The
// paths are tree paths from a branch anyone with push access can write, so
// each is quoted and bounded, and so is the list.
func reentryConflictReason(
	task productionPublicationTask, baseSHA string, conflict *publish.MergeConflictError,
) string {
	paths := conflict.Paths
	more := conflict.Truncated
	if len(paths) > reentryConflictPathLimit {
		paths, more = paths[:reentryConflictPathLimit], true
	}
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = reentryQuoted(path)
	}
	listing := strings.Join(quoted, ", ")
	if more {
		listing += ", and more"
	}
	return fmt.Sprintf(
		"Readiness re-entry stopped because pull request head %s conflicts with base %s in: %s. Resolve the conflict on the pull request.",
		task.HeadSHA, baseSHA, listing)
}

// stopReentryCycle ends a re-entered cycle on a condition no retry can clear:
// it raises a durable item that carries the pull request and retires the task.
// Unlike a hold it records no run hold and is not retried.
func (w *productionPublicationWorkflow) stopReentryCycle(
	ctx context.Context,
	task productionPublicationTask,
	reason string,
	artifacts []domain.Artifact,
) (productionTaskOutcome, error) {
	reason, err := w.withExternalReviewFindings(ctx, task, reason)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	pull, err := w.reentryPullRequest(ctx, task)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	reference := pull.reference()
	item, err := w.newBlockedItem(
		ctx, task, importer.Result{CommitSHA: task.HeadSHA}, artifacts, reason,
		reentryStopActions, w.approvedRecipes, nil, &reference,
	)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	var current domain.AttentionItem
	currentErr := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		current, err = tx.GetAttentionItemRecord(ctx, item.ID)
		return err
	})
	if currentErr != nil && !errors.Is(currentErr, store.ErrNotFound) {
		return productionTaskOutcome{}, currentErr
	}
	found := currentErr == nil
	if found {
		// An earlier hold of this cycle owns the identity. The stop is its
		// next version, never a second item.
		if current.ProjectID != item.ProjectID || current.Subject.Type != item.Subject.Type ||
			current.Subject.ID != item.Subject.ID || current.Subject.RunID == nil ||
			*current.Subject.RunID != task.RunID || current.Type != item.Type ||
			current.PRHeadSHA != item.PRHeadSHA || current.Status != domain.StatusOpen {
			return productionTaskOutcome{}, fmt.Errorf(
				"production re-entry stop %q disagrees with task: %w",
				item.ID, domain.ErrParentKeyMismatch)
		}
		unchanged := current.Reason == item.Reason &&
			slices.Equal(current.RequestedDecision, item.RequestedDecision) &&
			reflect.DeepEqual(current.PRReference, item.PRReference)
		if !unchanged {
			item.ItemVersion = current.ItemVersion + 1
			item.Timing = current.Timing
			item.ConversationID = current.ConversationID
			item.CreatedAt = current.CreatedAt
			item.ExpiresWhen = current.ExpiresWhen
			if err := w.attention.PutItem(ctx, item); err != nil {
				return productionTaskOutcome{}, err
			}
		}
	} else if err := w.attention.PutItem(ctx, item); err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.recordHeldItemPRBinding(ctx, task, *pull); err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.recordWorkUnitPRBinding(ctx, task, pull.base, pull.number); err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		// A pass that lost its process after writing the cycle's ready item
		// can reach a stop only because the pull request changed before the
		// next pass. The item then claims a readiness the stop denies.
		ready, err := tx.GetAttentionItem(ctx, task.readyItemID())
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		case ready.Status == domain.StatusOpen:
			ready.Status = domain.StatusSuperseded
			ready.ItemVersion++
			if err := tx.PutAttentionItem(ctx, ready); err != nil {
				return err
			}
		}
		// A hold an earlier pass recorded describes a retry that will not happen.
		return tx.ClearRunHold(ctx, task.RunID)
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.finishTask(ctx, task); err != nil {
		return productionTaskOutcome{}, err
	}
	return productionTaskOutcome{blocked: !found}, nil
}

// escalateReentryFindings sends a re-entered cycle's blocking findings to a
// person (issue #502, decision 3). No adjudicator or remediation runs, so the
// item is the review-dispute escalation the gate already raises when no
// automatic path remains, and the cycle ends there.
func (w *productionPublicationWorkflow) escalateReentryFindings(
	ctx context.Context, task productionPublicationTask, record domain.ReviewRecord,
) (productionReviewGateState, error) {
	if err := w.removeReviewWorkspace(record.InvocationID); err != nil {
		return productionReviewPending, err
	}
	if err := w.putReviewAttention(
		ctx, task, record,
		"Review found blocking findings after the pull request's readiness was invalidated. A re-entered cycle starts no remediation, so a person must decide.",
		domain.AttentionReviewDispute,
	); err != nil {
		return productionReviewPending, err
	}
	return productionReviewEscalated, nil
}

// verifyReentry verifies the cycle's workspace commit (the prospective merge
// for a base advance, the head otherwise) in the networkless room and
// persists the result. The checkout already holds every commit involved.
func (w *productionPublicationWorkflow) verifyReentry(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkoutDir string,
) (productionReentryCheckpoint, error) {
	baseSHA := binding.admission.Base.BaseSHA
	workspaceSHA := task.HeadSHA
	if binding.evaluatedSHA != "" {
		workspaceSHA = binding.evaluatedSHA
	}
	changes, diffStats, err := w.reentryDiff(ctx, checkoutDir, baseSHA, workspaceSHA, task.HeadSHA)
	if err != nil {
		return productionReentryCheckpoint{}, err
	}
	room, recipe, err := w.verificationRoomAndRecipe(ctx, task, binding)
	if err != nil {
		return productionReentryCheckpoint{}, err
	}
	verified, err := verify.Verify(ctx, checkoutDir, verify.Options{
		HeadSHA: task.HeadSHA, BaseSHA: baseSHA, EvaluatedSHA: binding.evaluatedSHA,
		InvocationID: task.verificationInvocationID(), RecipeSource: verify.ConfigRecipe(recipe),
		RecipePath: verify.DefaultRecipePath, Room: room,
		ApprovedRecipes: w.approvedRecipes, Changes: changes,
		// A stable per-cycle time, as on a first cycle: the evidence is
		// content-addressed and write-once, so a re-run must reproduce it.
		Now: binding.reentry.createdAt,
		Policy: verify.Policy{ExtraVerificationControlPatterns: slices.Clone(
			binding.profile.ProtectedPaths.ExtraVerificationControlPatterns,
		)},
	})
	if err != nil {
		if !productionVerificationStateContradiction(err) {
			err = productionPublicationRetryableError(err)
		}
		return productionReentryCheckpoint{}, fmt.Errorf("clean re-entry verification: %w", err)
	}
	if verified.HeadSHA != task.HeadSHA || verified.RecipeDigest != binding.image.RecipeDigest {
		return productionReentryCheckpoint{}, fmt.Errorf(
			"re-entry verification disagrees with project-image binding: %w",
			domain.ErrParentKeyMismatch)
	}
	artifacts, err := w.storeVerificationEvidence(verified.Evidence)
	if err != nil {
		return productionReentryCheckpoint{}, err
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(artifacts)
	if err != nil {
		return productionReentryCheckpoint{}, err
	}
	checkpoint := productionReentryCheckpoint{
		Version: productionReentryCheckpointVersion,
		TaskKey: task.intentKey(),
		BaseSHA: baseSHA, HeadSHA: task.HeadSHA, EvaluatedSHA: binding.evaluatedSHA,
		ProjectImage: binding.image.ID,
		Outcome:      reentryOutcome(verified.Outcome), RecipeDigest: verified.RecipeDigest,
		EvidenceSnapshotDigest: evidenceDigest,
		Findings:               candidateFindings(nil, verified.Findings),
		Artifacts:              artifacts, DiffStats: diffStats,
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionVerificationEvidence, DurableTransitionBefore); err != nil {
		return productionReentryCheckpoint{}, err
	}
	if err := w.persistReentryCheckpoint(ctx, task, checkpoint); err != nil {
		return productionReentryCheckpoint{}, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionVerificationEvidence, DurableTransitionAfter); err != nil {
		return productionReentryCheckpoint{}, err
	}
	return checkpoint, nil
}

func reentryOutcome(outcome verify.Outcome) domain.VerificationOutcome {
	if outcome == verify.OutcomePassed {
		return domain.VerificationPassed
	}
	return domain.VerificationFailed
}

// reentryDiff derives what the workspace commit changes against the cycle's
// base: the change list the verification-control check reads, and the diff
// stats the ready item shows. For a base advance the workspace is the merge,
// so the count is what the pull request would change on the new base; the
// stats still name the pull request's head, which is the commit a person
// sees.
func (w *productionPublicationWorkflow) reentryDiff(
	ctx context.Context, checkoutDir, baseSHA, workspaceSHA, headSHA string,
) ([]importer.Change, *domain.DiffStats, error) {
	scratch, err := os.MkdirTemp(w.workDir, ".diff-stats-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	runner, err := gitrun.New(gitrun.Options{Scratch: scratch})
	if err != nil {
		return nil, nil, err
	}
	if _, err := runner.PinCheckout(ctx, checkoutDir); err != nil {
		return nil, nil, fmt.Errorf("bind re-entry diff checkout: %w", err)
	}
	out, err := runner.Run(
		ctx, nil, "diff", "--name-status", "--no-renames", "-z", "--no-ext-diff", "--no-textconv",
		baseSHA, workspaceSHA, "--",
	)
	if err != nil {
		return nil, nil, fmt.Errorf("derive re-entry changes: %w", err)
	}
	changes, err := parseNameStatus(out)
	if err != nil {
		return nil, nil, err
	}
	stats, err := numstatDiffStats(ctx, runner, baseSHA, baseSHA, workspaceSHA)
	if err != nil {
		return nil, nil, err
	}
	stats.HeadSHA = headSHA
	if err := stats.Validate(); err != nil {
		return nil, nil, err
	}
	return changes, stats, nil
}

// parseNameStatus reads `git diff --name-status --no-renames -z` output: a
// status and a path per change, each NUL-terminated. Without rename
// detection only added, modified, type-changed, and deleted appear; anything
// else is refused rather than guessed at.
func parseNameStatus(out []byte) ([]importer.Change, error) {
	fields := bytes.Split(out, []byte{0})
	if len(fields) > 0 && len(fields[len(fields)-1]) == 0 {
		fields = fields[:len(fields)-1]
	}
	if len(fields)%2 != 0 {
		return nil, fmt.Errorf("parse git diff --name-status output: %w", domain.ErrCardFactInconsistent)
	}
	changes := make([]importer.Change, 0, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		var change importer.Change
		switch string(fields[i]) {
		case "A":
			change.Kind = importer.ChangeAdded
		case "M", "T":
			change.Kind = importer.ChangeModified
		case "D":
			change.Kind = importer.ChangeDeleted
		default:
			return nil, fmt.Errorf("parse git diff --name-status status %q: %w",
				fields[i], domain.ErrCardFactInconsistent)
		}
		path := fields[i+1]
		if len(path) == 0 {
			return nil, fmt.Errorf("parse git diff --name-status path: %w",
				domain.ErrCardFactInconsistent)
		}
		if utf8.Valid(path) {
			change.Path = string(path)
		} else {
			change.PathHex = hex.EncodeToString(path)
		}
		changes = append(changes, change)
	}
	return changes, nil
}

func (w *productionPublicationWorkflow) persistReentryCheckpoint(
	ctx context.Context,
	task productionPublicationTask,
	checkpoint productionReentryCheckpoint,
) error {
	payload, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		for _, artifact := range checkpoint.Artifacts {
			if err := tx.PutArtifact(ctx, artifact); err != nil {
				return err
			}
		}
		entry, _, err := tx.RecordInbox(
			ctx, task.verificationCheckpointKey(), productionReentryCheckpointKind, payload)
		if err != nil {
			return err
		}
		if entry.Kind != productionReentryCheckpointKind || !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("production re-entry checkpoint disagrees with stored row: %w",
				domain.ErrImmutableTransition)
		}
		return nil
	})
}

// loadReentryCheckpoint reads the cycle's checkpoint and re-binds it to the
// task and to the commits this pass resolved. The evaluated commit is the
// merge the caller just rebuilt, so a recorded merge that the same base and
// head no longer reproduce is a contradiction, never reused evidence.
func (w *productionPublicationWorkflow) loadReentryCheckpoint(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
) (productionReentryCheckpoint, bool, error) {
	var entry store.QueueEntry
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, task.verificationCheckpointKey())
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return productionReentryCheckpoint{}, false, nil
	}
	if err != nil {
		return productionReentryCheckpoint{}, false, err
	}
	if entry.Kind != productionReentryCheckpointKind {
		return productionReentryCheckpoint{}, false, domain.ErrParentKeyMismatch
	}
	var checkpoint productionReentryCheckpoint
	if err := strictjson.Decode(
		entry.Payload, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return productionReentryCheckpoint{}, false, fmt.Errorf(
				"production re-entry checkpoint has trailing content: %w",
				domain.ErrParentKeyMismatch)
		}
		return productionReentryCheckpoint{}, false, fmt.Errorf(
			"decode durable production re-entry checkpoint: %w",
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(checkpoint.Artifacts)
	if err != nil {
		return productionReentryCheckpoint{}, false, err
	}
	baseSHA := binding.admission.Base.BaseSHA
	if checkpoint.Version != productionReentryCheckpointVersion ||
		checkpoint.TaskKey != task.intentKey() ||
		checkpoint.ProjectImage != binding.image.ID ||
		checkpoint.BaseSHA != baseSHA || checkpoint.HeadSHA != task.HeadSHA ||
		checkpoint.EvaluatedSHA != binding.evaluatedSHA ||
		(checkpoint.Outcome != domain.VerificationPassed &&
			checkpoint.Outcome != domain.VerificationFailed) ||
		checkpoint.RecipeDigest != binding.image.RecipeDigest ||
		checkpoint.EvidenceSnapshotDigest != evidenceDigest {
		return productionReentryCheckpoint{}, false, fmt.Errorf(
			"production re-entry checkpoint disagrees with task: %w", domain.ErrParentKeyMismatch)
	}
	if checkpoint.DiffStats != nil {
		if err := checkpoint.DiffStats.Validate(); err != nil ||
			checkpoint.DiffStats.BaseSHA != baseSHA ||
			checkpoint.DiffStats.HeadSHA != task.HeadSHA {
			return productionReentryCheckpoint{}, false, fmt.Errorf(
				"production re-entry checkpoint diff stats disagree with task: %w",
				domain.ErrParentKeyMismatch)
		}
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := verifyFakePublicationBlob(w.artifacts, artifact); err != nil {
			return productionReentryCheckpoint{}, false, fmt.Errorf(
				"verify durable production re-entry checkpoint artifact: %w",
				retryableOrTerminal(ctx, err, domain.ErrParentKeyMismatch))
		}
	}
	// The outcome and findings decide readiness, so they are read back from
	// the verifier's own report, never trusted as the row decoded them. A
	// first cycle compares its checkpoint with the stored candidate
	// authorization; this cycle records none, so the report is its witness.
	raw, err := loadVerificationReport(w.artifacts, checkpoint.Artifacts, task.verificationInvocationID())
	if err != nil {
		return productionReentryCheckpoint{}, false, productionPublicationRetryableError(err)
	}
	report, err := verify.ParseReport(raw)
	if err != nil {
		return productionReentryCheckpoint{}, false, fmt.Errorf(
			"production re-entry checkpoint report: %w", errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if report.HeadSHA != task.HeadSHA || report.BaseSHA != baseSHA ||
		report.EvaluatedSHA != binding.evaluatedSHA ||
		report.RecipeDigest != checkpoint.RecipeDigest ||
		reentryOutcome(report.Outcome) != checkpoint.Outcome ||
		!slices.EqualFunc(candidateFindings(nil, report.Findings), checkpoint.Findings,
			func(reported, stored domain.CandidateFinding) bool {
				return reflect.DeepEqual(reported, stored)
			}) {
		return productionReentryCheckpoint{}, false, fmt.Errorf(
			"production re-entry checkpoint disagrees with its verification report: %w",
			domain.ErrParentKeyMismatch)
	}
	return checkpoint, true, nil
}

// completeReentryTask re-earns readiness in place. It mirrors the tail of a
// first cycle's completion, without a terminal record or a publication: the
// run already published, and this cycle's durable result is the ready item.
func (w *productionPublicationWorkflow) completeReentryTask(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	checkout PublicationCheckout,
	reviewInstructions exec.ReviewInstructionBinding,
) (productionTaskOutcome, error) {
	cycle := binding.reentry
	readiness, persistReadiness, err := w.assertReviewedCandidate(
		ctx, task, binding, checkpoint, reviewInstructions)
	if err != nil {
		if errors.Is(err, errShadowReviewStopped) {
			return w.stopReentryCycle(ctx, task,
				"Readiness re-entry stopped because an operator stopped the run on a shadow review finding.",
				nil)
		}
		if errors.Is(err, errShadowReviewBlocksReady) {
			return productionTaskOutcome{}, nil
		}
		if errors.Is(err, domain.ErrReviewConfigurationUnapproved) {
			return w.holdReviewConfigurationMismatch(ctx, task, checkpoint.Imported, err)
		}
		if errors.Is(err, domain.ErrParentKeyMismatch) {
			return w.holdBlockedTask(
				ctx, task, checkpoint.Imported,
				"Readiness re-entry is durably held because the complete current verification requirement set is not ready under its exact policy, base, and registry bindings.",
				domain.HoldTrustBlocked,
			)
		}
		return productionTaskOutcome{}, err
	}
	yieldHistory, err := w.reviewYieldHistory(ctx, task.RunID)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	published := publish.Result{PRNumber: cycle.predecessor.PRNumber}
	readyExists, err := w.hasCompatibleReadyItem(
		ctx, task, binding, checkpoint, published, readiness, yieldHistory)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	if !readyExists {
		if err := persistReadiness(ctx); err != nil {
			return productionTaskOutcome{}, err
		}
		ready, err := w.readyItem(ctx, task, checkpoint, published, readiness, yieldHistory)
		if err != nil {
			return productionTaskOutcome{}, err
		}
		// The cycle evaluated the head its authority sealed. A head pushed
		// while it ran makes that evidence stale before the item exists.
		live, err := w.transport.FetchHead(ctx, checkout, cycle.branch, task.HeadSHA)
		if err != nil {
			if errors.Is(err, publish.ErrRemoteMissingHead) {
				return w.stopReentryCycle(ctx, task, reentryMissingHeadReason(task, cycle.branch), nil)
			}
			return w.reentryTransportFailure(ctx, task, "observe pull request head before readiness", err)
		}
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionReadyItem, DurableTransitionBefore); err != nil {
			return productionTaskOutcome{}, err
		}
		moved := live.TipSHA != task.HeadSHA
		if moved {
			err = w.supersedeMovedReentry(ctx, task, cycle, ready, live.TipSHA)
		} else {
			err = w.attention.PutItem(ctx, ready)
		}
		if err != nil {
			return productionTaskOutcome{}, err
		}
		if err := runDurableTransitionHook(w.transitionHook,
			DurableTransitionReadyItem, DurableTransitionAfter); err != nil {
			return productionTaskOutcome{}, err
		}
		if moved {
			// The cycle produced no readiness, and its task row is already
			// retired with the item.
			return productionTaskOutcome{}, nil
		}
	}
	// Everything below converges on every pass through here, so a crash
	// between the item and any later record heals on the next pass.
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return w.recordReentryReadyBinding(ctx, tx, task, cycle)
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.recordWorkUnitPRBinding(
		ctx, task, binding.admission.Base, cycle.predecessor.PRNumber); err != nil {
		return productionTaskOutcome{}, err
	}
	// The binding's admission carries the cycle's base, so the base watch is
	// armed with the base this cycle evaluated and does not fire until that
	// base moves again.
	if err := w.armReadyItemWatches(ctx, task, binding); err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.appendPublicationMilestone(ctx, task, domain.MilestonePublicationReady, nil); err != nil {
		return productionTaskOutcome{}, err
	}
	if w.afterReady != nil {
		if err := w.afterReady(); err != nil {
			return productionTaskOutcome{}, fmt.Errorf("after production ready item: %w",
				errors.Join(err, errProductionCrashSeam))
		}
	}
	if err := w.supersedeBlockedHold(ctx, task); err != nil {
		return productionTaskOutcome{}, err
	}
	if err := w.finishTask(ctx, task); err != nil {
		return productionTaskOutcome{}, err
	}
	return productionTaskOutcome{
		completed: true, readiness: &readiness.verdict, prNumber: cycle.predecessor.PRNumber,
	}, nil
}

// recordReentryReadyBinding binds the cycle's ready item in place to the pull
// request its predecessor published. It is write-once like every ready
// binding: a repeated pass restates the same record.
func (w *productionPublicationWorkflow) recordReentryReadyBinding(
	ctx context.Context,
	tx *store.WriteTx,
	task productionPublicationTask,
	cycle *reentryCycle,
) error {
	record := domain.ReadyItemPRBinding{
		ItemID:                  task.readyItemID(),
		RunID:                   task.RunID,
		ProducingInvocationID:   cycle.predecessor.ProducingInvocationID,
		PublicationInvocationID: task.PublicationID,
		PublicationIdentity:     cycle.predecessor.PublicationIdentity,
		Repo:                    cycle.predecessor.Repo,
		RepositoryID:            cycle.predecessor.RepositoryID,
		PRNumber:                cycle.predecessor.PRNumber,
		BaseRef:                 cycle.predecessor.BaseRef,
		HeadSHA:                 task.HeadSHA,
		RecordedAt:              w.now().UTC(),
	}
	existing, err := tx.GetReadyItemPRBinding(ctx, record.ItemID)
	switch {
	case err == nil:
		want := record
		want.RecordedAt = existing.RecordedAt
		if want != existing {
			return fmt.Errorf("stored ready-item pr binding disagrees with the re-entered cycle: %w",
				store.ErrImmutableConflict)
		}
		return nil
	case errors.Is(err, store.ErrNotFound):
		return tx.RecordReadyItemPRBinding(ctx, record)
	default:
		return err
	}
}

// supersedeMovedReentry ends a cycle whose head moved while it ran. The
// cycle's evidence is for a head the pull request no longer has, so its ready
// item must never be open; but the store seals the next re-entry only from a
// superseded ready item whose head the latest review covered. So one
// transaction writes the item already superseded with the head_changed fact
// (its only version; it is never open), records its binding, starts the next
// cycle, and retires this task, so no later pass re-runs this cycle beside
// the next one.
func (w *productionPublicationWorkflow) supersedeMovedReentry(
	ctx context.Context,
	task productionPublicationTask,
	cycle *reentryCycle,
	ready domain.AttentionItem,
	tipSHA string,
) error {
	if err := signet.ValidateItemIntake(ready); err != nil {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		superseded := ready
		superseded.Status = domain.StatusSuperseded
		superseded.ReadinessInvalidation = &domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationHeadChanged,
			Bound:  task.HeadSHA, Observed: tipSHA,
			ObservedAt: w.now().UTC(),
		}
		if err := tx.PutAttentionItem(ctx, superseded); err != nil {
			return err
		}
		if err := w.recordReentryReadyBinding(ctx, tx, task, cycle); err != nil {
			return err
		}
		if _, err := StartReadinessReentry(ctx, tx, superseded); err != nil {
			return err
		}
		if err := supersedeOpenBlockedHold(ctx, tx, task); err != nil {
			return err
		}
		// No ready milestone is appended for an item that was never open, so
		// the hold that milestone would clear is cleared here.
		if err := tx.ClearRunHold(ctx, task.RunID); err != nil {
			return err
		}
		return tx.MarkOutboxDispatched(ctx, task.intentKey())
	})
}

// supersedeOpenBlockedHold is supersedeBlockedHold inside the caller's
// transaction.
func supersedeOpenBlockedHold(
	ctx context.Context, tx *store.WriteTx, task productionPublicationTask,
) error {
	item, err := tx.GetAttentionItemRecord(ctx, task.blockedItemID())
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if item.ProjectID != task.ProjectID || item.Subject.Type != domain.SubjectRun ||
		item.Subject.ID != domain.SubjectID(task.RunID) || item.Subject.RunID == nil ||
		*item.Subject.RunID != task.RunID || item.Type != domain.AttentionPublishBlocked {
		return fmt.Errorf(
			"production publication hold %q disagrees with task: %w",
			item.ID, domain.ErrParentKeyMismatch)
	}
	if item.Status != domain.StatusOpen {
		return nil
	}
	item.Status = domain.StatusSuperseded
	item.ItemVersion++
	if err := signet.ValidateItemIntake(item); err != nil {
		return err
	}
	return tx.PutAttentionItem(ctx, item)
}
