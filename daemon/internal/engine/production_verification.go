package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func (w *productionPublicationWorkflow) materializeReplay(
	replay ProductionReplay, dir string,
) error {
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o700); err != nil {
		return err
	}
	manifest, err := replay.Manifest.Encode()
	if err != nil || digestProductionBytes(manifest) != replay.ManifestDigest {
		return fmt.Errorf("replay manifest disagrees with export: %w", errors.Join(err, domain.ErrParentKeyMismatch))
	}
	if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), manifest, 0o600); err != nil {
		return err
	}
	for _, entry := range replay.Manifest.Entries {
		if entry.Kind != export.EntryRegular {
			continue
		}
		if entry.BlobOmitted || entry.Digest == nil {
			return fmt.Errorf("replay manifest entry %q has no durable blob", entry.Path)
		}
		if err := w.materializeBlob(domain.Digest(*entry.Digest), filepath.Join(dir, "blobs", "sha256")); err != nil {
			return fmt.Errorf("materialize replay blob for %q: %w", entry.Path, err)
		}
	}
	if replay.EvidenceManifestDigest != nil {
		evidence, err := replay.Evidence.Encode()
		if err != nil || digestProductionBytes(evidence) != *replay.EvidenceManifestDigest {
			return fmt.Errorf("replay evidence manifest disagrees with export: %w",
				errors.Join(err, domain.ErrParentKeyMismatch))
		}
		if err := os.WriteFile(filepath.Join(dir, export.EvidenceFilename), evidence, 0o600); err != nil {
			return err
		}
		evidenceDir := filepath.Join(dir, export.EvidenceBlobsDirname, "sha256")
		if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
			return err
		}
		for _, entry := range replay.Evidence.Entries {
			if err := w.materializeBlob(domain.Digest(entry.Digest), evidenceDir); err != nil {
				return fmt.Errorf("materialize replay evidence %q: %w", entry.Label, err)
			}
		}
	} else if len(replay.Evidence.Entries) != 0 {
		return fmt.Errorf("replay carries evidence without a manifest digest: %w", domain.ErrParentKeyMismatch)
	}
	if replay.CommitPlanDigest != nil {
		body, err := loadArtifactBlob(w.artifacts, *replay.CommitPlanDigest)
		if err != nil {
			return fmt.Errorf("load durable replay commit plan: %w",
				errors.Join(err, domain.ErrParentKeyMismatch))
		}
		if err := os.WriteFile(filepath.Join(dir, export.CommitPlanFilename), body, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func (w *productionPublicationWorkflow) materializeBlob(digest domain.Digest, dir string) error {
	hexDigits, ok := contentaddr.Parse(string(digest))
	if !ok {
		return fmt.Errorf("invalid blob digest %q", digest)
	}
	body, err := loadArtifactBlob(w.artifacts, digest)
	if err != nil {
		return fmt.Errorf("load durable replay blob %s: %w", digest,
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	return os.WriteFile(filepath.Join(dir, hexDigits), body, 0o600)
}

func digestProductionBytes(body []byte) domain.Digest {
	sum := sha256.Sum256(body)
	return domain.Digest(contentaddr.Format(sum[:]))
}

// verificationRoomAndRecipe constructs the networkless room for one cycle's
// verification from the image the binding verifies in, and reads that image's
// recipe through it.
func (w *productionPublicationWorkflow) verificationRoomAndRecipe(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) (ProductionVerificationRoom, []byte, error) {
	return w.roomAndRecipe(ctx, task, binding, binding.verificationImage())
}

// reserveVerification reserves the verification job's memory before its
// first container, the recipe read (plan §5.4). A job takes no pool slot, so
// the memory gate is its whole scheduling decision. One that does not fit yet
// records its wait and hold and returns a retryable error, so the task stays
// pending and a later pass asks again; a grant ends that hold. One the budget
// refuses for good is retryable too, so the job waits under the notice
// recordRetryableHold raises.
func (w *productionPublicationWorkflow) reserveVerification(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) error {
	// Read before the gate's write, so an unbudgeted job writes nothing. No
	// command clears a recorded budget, so a budget read here still applies
	// in the gate. The gate reads the budget again in its own transaction, so
	// one lowered between the two reads still bounds this reservation.
	bound, err := currentLaunchBound(ctx, w.store, w.launchBound)
	if err != nil || !bound.Budgeted() {
		return err
	}
	sizes, err := ward.ResolveLaunchSizes(binding.resolvedPolicy.Keys, ward.LaunchBound{})
	if err != nil {
		return fmt.Errorf("resolve verification size: %w", err)
	}
	id := task.verificationInvocationID()
	now := w.now().UTC()
	reservation, _, err := bound.LaunchReservation(
		id, task.RunID, domain.ReservationVerification, sizes.Verification)
	var waited bool
	if err == nil {
		err = w.store.Write(ctx, func(tx *store.WriteTx) error {
			if err := requireTaskExecutionOpen(ctx, &tx.ReadTx, task.RunID); err != nil {
				return err
			}
			gate, err := launchBoundAt(ctx, &tx.ReadTx, w.launchBound)
			if err != nil {
				return err
			}
			if err := tx.RequireMachineCapacity(ctx, gate.BudgetMiB, reservation, now); err != nil {
				return err
			}
			hold, present, err := tx.GetRunHold(ctx, task.RunID)
			if err != nil {
				return err
			}
			waited = present && hold.Reason == domain.HoldMachineCapacity
			return tx.ClearRunHoldCause(ctx, task.RunID, domain.HoldMachineCapacity)
		})
	}
	if errors.Is(err, domain.ErrMachineCapacity) {
		if orderErr := recordLaunchOrder(ctx, w.store, w.attention, task.RunID, id, err, now); orderErr != nil {
			return errors.Join(err, orderErr)
		}
		return productionPublicationRetryableError(fmt.Errorf("reserve verification memory: %w", err))
	}
	if launchMemoryRefusal(err) {
		return productionPublicationRetryableError(fmt.Errorf("reserve verification memory: %w", err))
	}
	if err != nil {
		return err
	}
	if waited {
		w.holdPace.forget("hold:" + string(task.RunID))
	}
	return releaseProductionQuarantine(ctx, w.store, w.attention, launchMemoryRefusedPrefix, task.RunID)
}

// releaseVerification ends the verification job's reservation once its room
// returned with every container proved absent, which is any return but one
// marked ward.ErrVerificationAbsenceUnproven. An unproven one keeps the
// reservation until a later pass's room, or a confirmed task stop, proves it.
func (w *productionPublicationWorkflow) releaseVerification(
	ctx context.Context, task productionPublicationTask, roomErr error,
) error {
	if errors.Is(roomErr, ward.ErrVerificationAbsenceUnproven) {
		return nil
	}
	bound, err := currentLaunchBound(ctx, w.store, w.launchBound)
	if err != nil || !bound.Budgeted() {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ReleaseVerificationReservation(ctx, task.verificationInvocationID(), w.now().UTC())
	})
}

// roomAndRecipe constructs a networkless room from image and reads the
// image's recipe through it, refusing a recipe that is not the one the
// admitted image was built with.
func (w *productionPublicationWorkflow) roomAndRecipe(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
	image domain.ProjectImage,
) (ProductionVerificationRoom, []byte, error) {
	// The writer start already resolved this durable policy, so a failure
	// here means the stored policy changed under the run.
	sizes, err := ward.ResolveLaunchSizes(binding.resolvedPolicy.Keys, ward.LaunchBound{})
	if err != nil {
		return nil, nil, fmt.Errorf("resolve verification size: %w", err)
	}
	var room ProductionVerificationRoom
	if w.newBoundRoom != nil {
		room, err = w.newBoundRoom(image, sizes.Verification, binding.run, task.verificationInvocationID())
	} else {
		room, err = w.newRoom(image, sizes.Verification)
	}
	if err != nil {
		return nil, nil, fmt.Errorf("construct networkless verification room: %w", err)
	}
	readCtx, cancelRead := context.WithTimeout(ctx, w.recipeReadTimeout)
	recipe, err := room.ReadRecipe(readCtx)
	cancelRead()
	if err != nil {
		return nil, nil, fmt.Errorf(
			"load project-image verification recipe: %w",
			productionPublicationRetryableError(err),
		)
	}
	if len(recipe) > verify.DefaultMaxRecipeBytes {
		return nil, nil, fmt.Errorf(
			"project-image verification recipe exceeds the %d-byte cap",
			verify.DefaultMaxRecipeBytes,
		)
	}
	if got := verify.RecipeDigest(recipe); got != binding.image.RecipeDigest {
		return nil, nil, fmt.Errorf(
			"project-image verification recipe digest %s, want %s: %w",
			got, binding.image.RecipeDigest, domain.ErrParentKeyMismatch,
		)
	}
	return room, recipe, nil
}

// storeVerificationEvidence writes each evidence blob to the artifact store
// and reads it back, so a checkpoint never names evidence the store lacks.
func (w *productionPublicationWorkflow) storeVerificationEvidence(
	evidence []verify.Evidence,
) ([]domain.Artifact, error) {
	artifacts := make([]domain.Artifact, len(evidence))
	for index, item := range evidence {
		artifacts[index] = item.Artifact
		if _, err := w.artifacts.Put(item.Artifact.Digest, bytes.NewReader(item.Content)); err != nil {
			return nil, err
		}
		if err := verifyArtifactBlob(w.artifacts, item.Artifact); err != nil {
			return nil, err
		}
	}
	return artifacts, nil
}

func (w *productionPublicationWorkflow) verifyAndCheckpoint(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	imported importer.Result,
	checkoutDir string,
) (productionVerificationCheckpoint, error) {
	repair, err := w.repairVerification(ctx, binding, binding.admission.Base.BaseSHA)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	var refusal *rebuildRefusal
	if repair {
		// A maintenance task's image is the one its refusal says does not
		// serve the base; the candidate is verified only in an image rebuilt
		// for its head.
		binding, refusal, err = w.bindRepairVerificationImage(ctx, task, binding, checkoutDir, imported.CommitSHA)
	} else {
		// Composition admitted this image at this base before the run started,
		// so a refusal here is a disagreement with durable authority, not a
		// hold.
		if err := requireProjectImageServesBase(
			ctx, binding.image, checkoutDir, binding.admission.Base.BaseSHA, w.preparationDigest,
		); err != nil {
			if errors.Is(err, domain.ErrProjectImageIncompatible) {
				err = fmt.Errorf("production publication binding disagrees with durable authority: %w",
					errors.Join(domain.ErrParentKeyMismatch, err))
			}
			return productionVerificationCheckpoint{}, err
		}
		binding, refusal, err = w.bindVerificationImage(
			ctx, task, binding, checkoutDir, binding.admission.Base.BaseSHA, imported.CommitSHA)
	}
	if err != nil {
		// A rebuilt image the run is bound to is durable authority as the
		// admitted one is, so its refusal is the same disagreement.
		if errors.Is(err, domain.ErrProjectImageIncompatible) {
			err = fmt.Errorf("production publication binding disagrees with durable authority: %w",
				errors.Join(domain.ErrParentKeyMismatch, err))
		}
		return productionVerificationCheckpoint{}, err
	}
	recipes, err := w.recipes(ctx)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	var verified verify.Result
	if refusal != nil {
		// No room is built for a refused gate. The refusal is recorded as this
		// candidate's failed verification, so the block, its recovery, and a
		// rerun are the ones every failed verification already has.
		verified, err = refusedRebuildVerification(task, binding, imported.CommitSHA, *refusal, recipes)
		if err != nil {
			return productionVerificationCheckpoint{}, err
		}
	} else {
		if err := w.reserveVerification(ctx, task, binding); err != nil {
			return productionVerificationCheckpoint{}, err
		}
		room, recipe, err := w.verificationRoomAndRecipe(ctx, task, binding)
		if err != nil {
			return productionVerificationCheckpoint{}, errors.Join(err, w.releaseVerification(ctx, task, err))
		}
		verified, err = verify.Verify(ctx, checkoutDir, verify.Options{
			HeadSHA: imported.CommitSHA, BaseSHA: binding.admission.Base.BaseSHA,
			InvocationID: task.verificationInvocationID(), RecipeSource: verify.ConfigRecipe(recipe),
			RecipePath: verify.DefaultRecipePath, Room: room,
			ApprovedRecipes: recipes, Changes: imported.Changes,
			// A stable per-verification time, not a wall clock: the emitted evidence
			// is content-addressed and write-once, and this checkpoint can re-run, so
			// the evidence metadata must converge byte-identically on replay (mirrors
			// the checkpoint's CandidateAuthorization CreatedAt).
			Now: binding.export.RecordedAt,
			Policy: verify.Policy{ExtraVerificationControlPatterns: slices.Clone(
				binding.profile.ProtectedPaths.ExtraVerificationControlPatterns,
			)},
		})
		if releaseErr := w.releaseVerification(ctx, task, err); releaseErr != nil {
			return productionVerificationCheckpoint{}, errors.Join(err, releaseErr)
		}
		if err != nil {
			if !productionVerificationStateContradiction(err) && !verificationMemoryLimit(err) {
				err = productionPublicationRetryableError(err)
			}
			return productionVerificationCheckpoint{}, fmt.Errorf("clean production verification: %w", err)
		}
	}
	if verified.HeadSHA != imported.CommitSHA || verified.RecipeDigest != binding.image.RecipeDigest {
		return productionVerificationCheckpoint{}, fmt.Errorf("verification disagrees with project-image binding: %w",
			domain.ErrParentKeyMismatch)
	}
	artifacts, err := w.storeVerificationEvidence(verified.Evidence)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	importDigest, err := digestJSON(imported)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(artifacts)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	outcome := domain.VerificationFailed
	if verified.Outcome == verify.OutcomePassed {
		outcome = domain.VerificationPassed
	}
	authorization, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: binding.admission.Base.Repo, BaseSHA: binding.admission.Base.BaseSHA,
		HeadSHA: imported.CommitSHA, ImportResultDigest: importDigest,
		VerificationRecipeDigest: verified.RecipeDigest,
		EvidenceSnapshotDigest:   evidenceDigest, VerificationOutcome: outcome,
		Findings:           candidateFindings(imported.Findings, verified.Findings),
		TrustProfileDigest: binding.profile.ProfileDigest,
		InvocationID:       task.verificationInvocationID(), CreatedAt: binding.export.RecordedAt,
	})
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	diffStats, err := deriveDiffStats(
		ctx, w.workDir, checkoutDir, authorization.BaseSHA, authorization.HeadSHA,
	)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	reviewGuide, err := resolveReviewGuide(
		ctx, w.workDir, checkoutDir, w.artifacts, imported.Claims, task.ProducingInvocationID,
		authorization.BaseSHA, authorization.HeadSHA,
	)
	if err != nil {
		return productionVerificationCheckpoint{}, err
	}
	checkpoint := productionVerificationCheckpoint{
		Version: productionVerificationVersion,
		TaskKey: task.intentKey(), HeadSHA: task.HeadSHA,
		ProjectImage: binding.verificationImage().ID,
		Imported:     imported, Authorization: authorization, Artifacts: artifacts,
		DiffStats: diffStats, ReviewGuide: reviewGuide,
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionVerificationEvidence, DurableTransitionBefore); err != nil {
		return productionVerificationCheckpoint{}, err
	}
	if err := w.persistCheckpoint(ctx, task, checkpoint); err != nil {
		return productionVerificationCheckpoint{}, err
	}
	if err := runDurableTransitionHook(w.transitionHook,
		DurableTransitionVerificationEvidence, DurableTransitionAfter); err != nil {
		return productionVerificationCheckpoint{}, err
	}
	return checkpoint, nil
}

// verificationMemoryLimit reports a verification room killed at its declared
// memory limit. The room runs at the size the run's durable policy resolved,
// so a retry meets the same limit: the caller ends the cycle on it instead of
// holding for a retry. A larger size is the capacity card's (#1598).
func verificationMemoryLimit(err error) bool {
	var limit *ward.MemoryLimitError
	return errors.As(err, &limit) && limit.Class == ward.LaunchVerification
}

func productionVerificationStateContradiction(err error) bool {
	return errors.Is(err, verify.ErrRecipeInvalid) ||
		errors.Is(err, verify.ErrRecipeUnreadable) ||
		errors.Is(err, verify.ErrGitPlumbing) ||
		errors.Is(err, verify.ErrUnsupportedRepo) ||
		errors.Is(err, verify.ErrHeadMismatch) ||
		errors.Is(err, verify.ErrBaseMismatch) ||
		errors.Is(err, verify.ErrWorkspaceMismatch) ||
		errors.Is(err, verify.ErrMalformedTree) ||
		errors.Is(err, verify.ErrInvalidOptions) ||
		errors.Is(err, verify.ErrSymlinkEntrypoint)
}

func (w *productionPublicationWorkflow) persistCheckpoint(
	ctx context.Context,
	task productionPublicationTask,
	checkpoint productionVerificationCheckpoint,
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
		if err := tx.RecordCandidateAuthorization(ctx, checkpoint.Authorization); err != nil {
			return err
		}
		entry, _, err := tx.RecordInbox(
			ctx, task.verificationCheckpointKey(),
			productionVerificationCheckpointKind, payload,
		)
		if err != nil {
			return err
		}
		if entry.Kind != productionVerificationCheckpointKind || !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("production verification checkpoint disagrees with stored row: %w",
				domain.ErrImmutableTransition)
		}
		return nil
	})
}

func (w *productionPublicationWorkflow) loadCheckpoint(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
) (productionVerificationCheckpoint, bool, error) {
	var (
		entry  store.QueueEntry
		legacy bool
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetInbox(ctx, task.verificationCheckpointKey())
		if errors.Is(err, store.ErrNotFound) &&
			task.reevaluation == nil &&
			task.ProducingInvocationID == productionInvocationID(task.RunID) {
			entry, err = tx.GetInbox(ctx, "production-verification/"+string(task.RunID))
			legacy = err == nil
		}
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return productionVerificationCheckpoint{}, false, nil
	}
	if err != nil {
		return productionVerificationCheckpoint{}, false, err
	}
	if entry.Kind != productionVerificationCheckpointKind {
		return productionVerificationCheckpoint{}, false, domain.ErrParentKeyMismatch
	}
	var checkpoint productionVerificationCheckpoint
	if err := strictjson.Decode(
		entry.Payload, &checkpoint, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return productionVerificationCheckpoint{}, false, fmt.Errorf(
				"production verification checkpoint has trailing content: %w",
				domain.ErrParentKeyMismatch,
			)
		}
		return productionVerificationCheckpoint{}, false, fmt.Errorf(
			"decode durable production verification checkpoint: %w",
			errors.Join(err, domain.ErrParentKeyMismatch),
		)
	}
	importDigest, err := digestJSON(checkpoint.Imported)
	if err != nil {
		return productionVerificationCheckpoint{}, false, err
	}
	evidenceDigest, err := domain.ComputeEvidenceSnapshotDigest(checkpoint.Artifacts)
	if err != nil {
		return productionVerificationCheckpoint{}, false, err
	}
	authorization := checkpoint.Authorization
	versionMatches := !legacy && checkpoint.Version == productionVerificationVersion &&
		checkpoint.HeadSHA == task.HeadSHA
	if legacy {
		versionMatches = checkpoint.Version == productionVerificationVersionV1 && checkpoint.HeadSHA == ""
	}
	authorizingTaskKey := task.intentKey()
	if task.AuthorizingTaskKey != "" {
		authorizingTaskKey = task.AuthorizingTaskKey
	}
	if !versionMatches ||
		checkpoint.TaskKey != authorizingTaskKey ||
		checkpoint.ProjectImage != binding.verificationImage().ID || authorization.Validate() != nil ||
		authorization.Repo != binding.admission.Base.Repo ||
		authorization.BaseSHA != binding.admission.Base.BaseSHA ||
		authorization.HeadSHA != task.HeadSHA ||
		authorization.ImportResultDigest != importDigest ||
		authorization.VerificationRecipeDigest != binding.image.RecipeDigest ||
		authorization.EvidenceSnapshotDigest != evidenceDigest ||
		authorization.TrustProfileDigest != binding.profile.ProfileDigest ||
		authorization.InvocationID != task.verificationInvocationID() {
		return productionVerificationCheckpoint{}, false, fmt.Errorf("production verification checkpoint disagrees with task: %w",
			domain.ErrParentKeyMismatch)
	}
	if checkpoint.DiffStats != nil {
		if err := checkpoint.DiffStats.Validate(); err != nil ||
			checkpoint.DiffStats.BaseSHA != authorization.BaseSHA ||
			checkpoint.DiffStats.HeadSHA != authorization.HeadSHA {
			return productionVerificationCheckpoint{}, false, fmt.Errorf(
				"production verification checkpoint diff stats disagree with authorization: %w",
				domain.ErrParentKeyMismatch,
			)
		}
	}
	if checkpoint.ReviewGuide != nil && !reviewGuideMatchesImport(
		*checkpoint.ReviewGuide, checkpoint.Imported.Claims, authorization.HeadSHA,
	) {
		return productionVerificationCheckpoint{}, false, fmt.Errorf(
			"production verification checkpoint review guide disagrees with authorization: %w",
			domain.ErrParentKeyMismatch,
		)
	}
	for _, artifact := range checkpoint.Artifacts {
		if err := verifyArtifactBlob(w.artifacts, artifact); err != nil {
			// A transient blob fault stays retryable; only a digest mismatch or
			// an absent blob is a durable checkpoint contradiction. Forcing every
			// fault terminal would stop a valid run on a transient I/O blip.
			return productionVerificationCheckpoint{}, false, fmt.Errorf(
				"verify durable production checkpoint artifact: %w",
				retryableOrTerminal(ctx, err, domain.ErrParentKeyMismatch),
			)
		}
	}
	var stored domain.CandidateAuthorization
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		stored, err = tx.GetCandidateAuthorization(ctx, authorization.ID)
		return err
	}); err != nil {
		return productionVerificationCheckpoint{}, false, err
	}
	if !reflect.DeepEqual(stored, authorization) {
		return productionVerificationCheckpoint{}, false, fmt.Errorf("stored production authorization disagrees with checkpoint: %w",
			domain.ErrParentKeyMismatch)
	}
	return checkpoint, true, nil
}
