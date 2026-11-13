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
	"github.com/freeside-ai/freeside/daemon/internal/macverify"
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

// verificationRooms is the one room choice of every verification path: a
// first cycle, a remediation round, a re-entry, and the handoff merge check.
// project_image always runs in imageRoom, the room the recipe was read
// through. The host executor is asked to admit the job only when the recipe
// names macos_host, so a project_image-only recipe never reaches its gate. A
// refusal returns *hostVerificationUnavailable and no room for the class:
// nothing has run, and the caller records the refusal on its own path. A
// recipe that does not parse returns only imageRoom, for verify to refuse.
// The host room records whether the executor refused a command for
// quarantined slots (hostSlotsRoom).
func (w *productionPublicationWorkflow) verificationRooms(
	ctx context.Context, task productionPublicationTask, imageRoom verify.Room, recipe []byte,
) (map[verify.ExecutionClass]verify.Room, error) {
	rooms := map[verify.ExecutionClass]verify.Room{verify.ClassProjectImage: imageRoom}
	parsed, err := verify.ParseRecipe(recipe)
	if err != nil || len(parsed.CommandsIn(verify.ClassMacOSHost)) == 0 {
		return rooms, nil
	}
	if w.hostVerificationRoom == nil {
		return nil, &hostVerificationUnavailable{refusal: &HostVerificationRefusal{
			InvocationID: task.verificationInvocationID(), Reason: "no host verification backend is configured",
		}}
	}
	room, admission, err := w.hostVerificationRoom(ctx, task)
	if err != nil {
		// A pass canceled while the gate read the store, or a transient
		// store or host failure under it, is not the gate's verdict; the
		// next pass asks again.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, productionPublicationRetryableError(ctxErr)
		}
		if productionPublicationRetryableFailure(err) {
			return nil, productionPublicationRetryableError(err)
		}
		return nil, &hostVerificationUnavailable{refusal: err}
	}
	if w.logger != nil {
		w.logger.Info("host verification admitted",
			"run_id", admission.RunID, "invocation_id", admission.InvocationID,
			"backend", admission.Backend, "configuration_digest", admission.BackendConfigurationDigest,
			"operating_mode", admission.OperatingMode)
	}
	rooms[verify.ClassMacOSHost] = &hostSlotsRoom{Room: room}
	return rooms, nil
}

// hostVerificationUnavailable is a recipe that names macos_host meeting a
// host executor that did not admit its job. No verification command ran. A
// first cycle or remediation round records it as a failed verification
// (hostVerificationRefusedVerification); a re-entry or handoff, which records
// no failed checkpoint without a verifier report, stops on it.
type hostVerificationUnavailable struct {
	refusal error
}

func (e *hostVerificationUnavailable) Error() string {
	return fmt.Sprintf("recipe names execution class %s: %v", verify.ClassMacOSHost, e.refusal)
}

func (e *hostVerificationUnavailable) Unwrap() error { return e.refusal }

// hostVerificationRefusedLine is the first line of every account of a
// refused host executor, so a reader finds the class and the refusal in one
// place on each path.
func hostVerificationRefusedLine(unavailable *hostVerificationUnavailable) string {
	return fmt.Sprintf("Host verification refused: the recipe names execution class %s, and no macOS verification executor was admitted for this job (%v).",
		verify.ClassMacOSHost, unavailable.refusal)
}

// hostVerificationRefusedVerification is the verification account of a
// candidate whose recipe needs the host executor and did not get it: failed,
// with one daemon-produced evidence artifact naming the class and the
// refusal, and no command run. It takes the place of verify's result, so the
// refusal blocks and recovers as any failed verification does.
func hostVerificationRefusedVerification(
	task productionPublicationTask,
	binding productionBinding,
	headSHA string,
	unavailable *hostVerificationUnavailable,
	approvedRecipes map[domain.Digest]bool,
) (verify.Result, error) {
	content := fmt.Sprintf(
		"%s\n\n"+
			"Freeside did not verify candidate %s. No verification command ran.\n\n"+
			"Configure and prove the executor (freesided -macverify-root and -run-conformance), then rerun the work.\n",
		hostVerificationRefusedLine(unavailable), headSHA)
	recipe := binding.image.RecipeDigest
	// The stable per-verification time a room's evidence carries, so a
	// replayed refusal converges on the same artifact.
	evidence, err := daemonVerificationEvidence(task.verificationInvocationID(), headSHA, recipe,
		content, binding.export.RecordedAt, approvedRecipes)
	if err != nil {
		return verify.Result{}, err
	}
	return verify.Result{
		HeadSHA: headSHA, RecipeDigest: recipe, Outcome: verify.OutcomeFailed,
		Evidence: []verify.Evidence{evidence},
	}, nil
}

// reentryHostVerificationRefusedReason reports a re-entry whose recipe needs
// the host executor and did not get it, with the reason the cycle stops on.
func reentryHostVerificationRefusedReason(
	task productionPublicationTask, baseSHA string, err error,
) (string, bool) {
	var unavailable *hostVerificationUnavailable
	if !errors.As(err, &unavailable) {
		return "", false
	}
	return fmt.Sprintf(
		"Readiness re-entry stopped because pull request head %s could not be verified against base %s: %s No verification command ran. Configure and prove the executor, then rerun the work.",
		task.HeadSHA, baseSHA, hostVerificationRefusedLine(unavailable)), true
}

// reserveVerification reserves the verification job's memory before its
// first container, the recipe read (plan §5.4). A job takes no pool slot, so
// the memory gate is its whole scheduling decision. One that does not fit yet
// records its wait and hold and returns a retryable error, so the task stays
// pending and a later pass asks again; a grant ends that hold. One the budget
// refuses for good is retryable too, so the job waits under the notice
// recordRetryableHold raises.
//
// It also starts the job's count as running verification_job work and
// refuses the job while an update drain is in force, budget or not, with a
// retryable error recordRetryableHold records as update_drain (issue #3104).
// The caller ends the count with the returned end once the job is done; a
// refusal ends it before returning.
func (w *productionPublicationWorkflow) reserveVerification(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) (end func(), err error) {
	id := task.verificationInvocationID()
	// began is held apart from the named end, which every refusal below
	// returns as nil before the deferred call reads it.
	began, err := w.running.BeginUnlessDraining(ctx, w.store, domain.RestartBlockerVerificationJob)
	if errors.Is(err, store.ErrUpdateDrainInForce) {
		return nil, &verificationReservationRefused{invocationID: id, err: productionPublicationRetryableError(
			fmt.Errorf("start verification: %w", err))}
	}
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			began()
		}
	}()
	// Read before the gate's write, so an unbudgeted job writes nothing. No
	// command clears a recorded budget, so a budget read here still applies
	// in the gate. The gate reads the budget again in its own transaction, so
	// one lowered between the two reads still bounds this reservation.
	bound, err := currentLaunchBound(ctx, w.store, w.launchBound)
	if err != nil {
		return nil, err
	}
	if !bound.Budgeted() {
		return began, nil
	}
	sizes, err := ward.ResolveLaunchSizes(binding.resolvedPolicy.Keys, ward.LaunchBound{})
	if err != nil {
		return nil, fmt.Errorf("resolve verification size: %w", err)
	}
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
			return nil, errors.Join(err, orderErr)
		}
		return nil, &verificationReservationRefused{invocationID: id, err: productionPublicationRetryableError(
			fmt.Errorf("reserve verification memory: %w", err))}
	}
	if launchMemoryRefusal(err) {
		return nil, &verificationReservationRefused{invocationID: id, err: productionPublicationRetryableError(
			fmt.Errorf("reserve verification memory: %w", err))}
	}
	if err != nil {
		return nil, err
	}
	if waited {
		w.holdPace.forget("hold:" + string(task.RunID))
	}
	if err := releaseProductionQuarantine(ctx, w.store, w.attention, launchMemoryRefusedPrefix, task.RunID); err != nil {
		return nil, err
	}
	return began, nil
}

// verificationReservationRefused is a verification job's reservation the
// budget refused, for now or for good, naming the invocation that asked.
// recordRetryableHold records the hold and the notice under that invocation,
// because it can differ from the task's own: a handoff verifies its merge
// under verify-handoff-<publication>, and only that invocation's wait leaves
// the memory order when the budget is lowered below it.
type verificationReservationRefused struct {
	invocationID domain.InvocationID
	err          error
}

func (e *verificationReservationRefused) Error() string { return e.err.Error() }
func (e *verificationReservationRefused) Unwrap() error { return e.err }

// releaseVerification ends the verification job's reservation once every
// container and host slot process the job started is proved absent. With
// bound rooms the container proof is ward's, from the invocation's ownership
// records, so it covers every room of every pass: a clean retry cannot release
// while an earlier pass's room is unproven. It retries owned cleanup, which
// gives a room whose own cleanup failed, a memory-limit kill included, a
// second attempt before the task blocks. A reservation still unproven then
// stays held until a later pass's proof or a confirmed task stop: an unproven
// container may still hold its memory, and releasing would let the budget
// overcommit. A task that ends with no later pass and no confirmed stop keeps
// it until the lane's end-of-task proof, which alerts the operator when it
// fails too, or the operator's release (verification_memory_held.go). Unbound
// rooms write no records, so for them the room's own return is the
// proof: any return but one marked ward.ErrVerificationAbsenceUnproven.
//
// The host executor writes no ownership records either, so for its slots the
// pass's own return is the only proof: a return marked
// macverify.ErrAbsenceUnproven keeps the reservation, after ward's proof has
// run its cleanup. That proof covers this pass alone. The executor never
// re-inspects a quarantined slot, so a later pass whose own proofs pass
// releases while that slot's processes may still run. They are uncapped
// either way, and the pool never leases the slot again (#3000). The job's
// one reservation covers whichever room runs: recipe commands run one at a
// time, so a container and a slot command are never live together.
//
// It runs detached from the pass, so a pass canceled at shutdown does not
// turn a provable release into a held one.
func (w *productionPublicationWorkflow) releaseVerification(
	ctx context.Context, task productionPublicationTask, run domain.Run, roomErr error,
) error {
	ctx = context.WithoutCancel(ctx)
	bound, err := currentLaunchBound(ctx, w.store, w.launchBound)
	if err != nil || !bound.Budgeted() {
		return err
	}
	invocation := task.verificationInvocationID()
	if w.proveVerificationAbsent != nil {
		// Bounded like the room's own cleanup.
		proofCtx, cancel := context.WithTimeout(ctx, ward.VerificationCleanupTimeout)
		err := w.proveVerificationAbsent(proofCtx, run, invocation)
		cancel()
		if errors.Is(err, ward.ErrVerificationAbsenceUnproven) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("prove verification absent: %w", err)
		}
	} else if errors.Is(roomErr, ward.ErrVerificationAbsenceUnproven) {
		return nil
	}
	if errors.Is(roomErr, macverify.ErrAbsenceUnproven) {
		return nil
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.ReleaseVerificationReservation(ctx, invocation, w.now().UTC())
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
		verified, err = w.runVerification(ctx, task, binding, imported, checkoutDir, recipes)
		if err != nil {
			return productionVerificationCheckpoint{}, err
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
		Findings:           candidateFindings(imported.Findings, verified.Findings, binding.profile.CandidateAutomationChanges),
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

// runVerification runs a first cycle's or remediation round's verification
// under the job's one memory reservation, taken before the recipe read and
// released once every room it used proved its work absent. A recipe that
// needs the host executor and did not get it releases the reservation, since
// nothing ran, and becomes a failed verification naming the class. A job whose
// host command the executor did not refuse for quarantined slots retires the
// run's quarantined-executor notice (retireHostSlotsNotice).
func (w *productionPublicationWorkflow) runVerification(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	imported importer.Result,
	checkoutDir string,
	recipes map[domain.Digest]bool,
) (verify.Result, error) {
	end, err := w.reserveVerification(ctx, task, binding)
	if err != nil {
		return verify.Result{}, err
	}
	defer end()
	room, recipe, err := w.verificationRoomAndRecipe(ctx, task, binding)
	if err != nil {
		return verify.Result{}, errors.Join(err, w.releaseVerification(ctx, task, binding.run, err))
	}
	rooms, err := w.verificationRooms(ctx, task, room, recipe)
	if err != nil {
		if releaseErr := w.releaseVerification(ctx, task, binding.run, nil); releaseErr != nil {
			return verify.Result{}, errors.Join(err, releaseErr)
		}
		var unavailable *hostVerificationUnavailable
		if errors.As(err, &unavailable) {
			return hostVerificationRefusedVerification(task, binding, imported.CommitSHA, unavailable, recipes)
		}
		return verify.Result{}, err
	}
	verified, err := verify.Verify(ctx, checkoutDir, verify.Options{
		HeadSHA: imported.CommitSHA, BaseSHA: binding.admission.Base.BaseSHA,
		InvocationID: task.verificationInvocationID(), RecipeSource: verify.ConfigRecipe(recipe),
		RecipePath:      verify.DefaultRecipePath,
		Rooms:           rooms,
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
	if releaseErr := w.releaseVerification(ctx, task, binding.run, err); releaseErr != nil {
		return verify.Result{}, errors.Join(err, releaseErr)
	}
	if retireErr := w.retireHostSlotsNotice(ctx, task, rooms, err); retireErr != nil {
		return verify.Result{}, errors.Join(err, retireErr)
	}
	if err != nil {
		if !productionVerificationStateContradiction(err) && !verificationMemoryLimit(err) {
			err = productionPublicationRetryableError(err)
		}
		return verify.Result{}, fmt.Errorf("clean production verification: %w", err)
	}
	return verified, nil
}

// verificationMemoryLimit reports a verification room killed at its declared
// memory limit. The room runs at the size the run's durable policy resolved,
// so a retry meets the same limit: the caller ends the cycle on it instead of
// holding for a retry. A larger size is the capacity card's (#1598).
func verificationMemoryLimit(err error) bool {
	var limit *ward.MemoryLimitError
	return errors.As(err, &limit) && limit.Class == ward.LaunchVerification
}

// productionVerificationStateContradiction reports a verify refusal that a
// retry would meet again because Freeside's own state causes it. A recipe that
// names an execution class with no room is one: the recipe is approved and the
// rooms are the engine's choice, so retrying cannot supply the missing room.
// verificationRooms supplies every class or refuses before verify runs, so
// this case is a backstop.
func productionVerificationStateContradiction(err error) bool {
	return errors.Is(err, verify.ErrRecipeInvalid) ||
		errors.Is(err, verify.ErrRoomUnavailable) ||
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
		authorization.ValidateAdvisoryStance(binding.profile.CandidateAutomationChanges) != nil ||
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
	// The import's notice rides in the authorized import result; the guide
	// beside it must be the one it describes: none for an omission, and the
	// trimmed guide for trims.
	if notice := checkpoint.Imported.ReviewGuideNotice; notice != nil &&
		(notice.Validate() != nil || (notice.Omission != nil) == (checkpoint.ReviewGuide != nil)) {
		return productionVerificationCheckpoint{}, false, fmt.Errorf(
			"production verification checkpoint review guide notice disagrees with its guide: %w",
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
