package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// The policy-gated rebuild (plan §5.7) keeps two engine-private, write-once
// inbox rows per run and verified commit. The intent row is durable before a
// build starts, so a restart knows a build may have published. The image row
// is the run's binding to the image that verifies that commit: the binding
// loaders read it, and nothing else decides which image a checkpoint names.
const (
	productionRebuildIntentKind = "production_rebuild_intent"
	productionRebuildImageKind  = "production_rebuild_image"
	productionRebuildVersion    = "1"
)

// ProjectImageBuilder is the part of *projectimage.Builder a rebuild uses.
type ProjectImageBuilder interface {
	Build(context.Context, projectimage.Request) (domain.ProjectImage, error)
}

// ProjectImageRebuild lets the publication lane rebuild a project image for a
// candidate whose dependency change stays within the project's policy. The
// builder must record what it builds in the store this workflow reads.
type ProjectImageRebuild struct {
	Builder ProjectImageBuilder
	Inputs  projectimage.BuildInputs
	// Timeout bounds one build with its proofs. Zero means
	// defaultProjectImageRebuildTimeout.
	Timeout time.Duration
}

// defaultProjectImageRebuildTimeout is far above an install and three proof
// runs of a project whose verification the same lane already bounds.
const defaultProjectImageRebuildTimeout = time.Hour

// build runs one build under the rebuild's time bound. The builder's proofs
// run the candidate's own verification commands with no bound of their own,
// and the lane handles one task at a time, so an unbounded build would let one
// candidate stop publication for every run.
//
// The bound ends the build's work. The builder then cleans up under a context
// it detaches from this one, so a container runtime that stalls there is not
// bounded here (#1806).
//
// timedOut reports that the bound ended the build. A build the caller's own
// context ended is not one: err is then that context's error.
func (r *ProjectImageRebuild) build(
	ctx context.Context, request projectimage.Request,
) (image domain.ProjectImage, timedOut bool, err error) {
	timeout := r.Timeout
	if timeout == 0 {
		timeout = defaultProjectImageRebuildTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	image, err = r.Builder.Build(bounded, request)
	if err == nil {
		return image, false, nil
	}
	// A cancelled pass fails whatever the build was doing, a proof included,
	// so nothing the builder reports about it is a verdict on the candidate.
	if cause := ctx.Err(); cause != nil {
		return domain.ProjectImage{}, false, cause
	}
	if errors.Is(bounded.Err(), context.DeadlineExceeded) {
		return domain.ProjectImage{}, true, fmt.Errorf(
			"the build and its proofs did not finish within %s", timeout)
	}
	return domain.ProjectImage{}, false, err
}

type productionRebuildIntent struct {
	Version       string                     `json:"version"`
	RunID         domain.RunID               `json:"run_id"`
	VerifiedSHA   string                     `json:"verified_sha"`
	AdmittedImage domain.Digest              `json:"admitted_image"`
	Delta         projectimage.LockfileDelta `json:"delta"`
}

type productionRebuildRecord struct {
	Version       string        `json:"version"`
	RunID         domain.RunID  `json:"run_id"`
	VerifiedSHA   string        `json:"verified_sha"`
	AdmittedImage domain.Digest `json:"admitted_image"`
	ProjectImage  domain.Digest `json:"project_image"`
}

func productionRebuildKey(kind string, runID domain.RunID, verifiedSHA string) string {
	return strings.ReplaceAll(kind, "_", "-") + "/" + string(runID) + "/" + verifiedSHA
}

// rebuildRefusal is a gate or build verdict that keeps a candidate on the
// fail-loud path. Each verification path decides what the refusal ends.
type rebuildRefusal struct {
	clause projectimage.RebuildRefusal
	detail string
}

// account is the refusal as a person reads it: the clause token, what the
// clause means, and what failed it. Detail can quote candidate-written text,
// which the gate already bounded and quoted.
func (r rebuildRefusal) account() string {
	return fmt.Sprintf("%s. %s (%s)", r.clause, rebuildRefusalMeaning(r.clause), r.detail)
}

func rebuildRefusalMeaning(clause projectimage.RebuildRefusal) string {
	switch clause {
	case projectimage.RebuildRefusalUndeclaredAuthority:
		return "A changed dependency resolves from a registry host the project's policy does not declare."
	case projectimage.RebuildRefusalUnpinnedSource:
		return "A changed dependency is not a registry package declared by version and pinned by an https URL and a sha512 integrity value."
	case projectimage.RebuildRefusalRecipeChanged:
		return "The candidate changes the verification recipe along with its dependencies."
	case projectimage.RebuildRefusalLockfileInconsistent:
		return "The candidate's package-lock.json is not a readable npm lockfile that agrees with its package.json and holds what its packages depend on."
	case projectimage.RebuildRefusalUnsupportedInput:
		return "The candidate holds an npm input no project image supports, or changes one a rebuild cannot bound."
	case projectimage.RebuildRefusalNoRegistrySet:
		return "The run's policy declares no valid registry set to check the dependency change against."
	case projectimage.RebuildRefusalNotConfigured:
		return "The dependency change is within policy, but this daemon is not configured to rebuild project images."
	case projectimage.RebuildRefusalProofFailed:
		return "The dependency change is within policy, but the rebuilt image failed its proofs or did not finish building in time."
	}
	return "The project image could not be rebuilt for this candidate."
}

// boundedRebuildText bounds and quotes text a build produced. A builder error
// can carry megabytes of command output written by the candidate's own tree.
func boundedRebuildText(text string) string {
	const limit = 1000
	if len(text) > limit {
		text = strings.ToValidUTF8(text[:limit], "") + "..."
	}
	return strconv.Quote(text)
}

// rebuiltImageServes is the trust gate every image other than the admitted
// one passes before it verifies a candidate, whether a build just returned
// it, a restart found it recorded, or a binding row named it. The row and the
// record are decoded state, so nothing they say about the image is taken on
// trust: the image must keep what the admitted image binds
// (rebuiltImageKeepsAdmitted), record the environment the commit it will
// verify holds, and be admissible there.
//
// The environment is compared here and not left to AdmissibleAt, which takes
// an image at its own build commit from the record alone. A rebuilt image is
// built at the commit it verifies, so that rule would compare nothing: not
// the manifests the record says it baked, and not the preparation sources,
// which follow the running binary and so change under a recorded image.
func rebuiltImageServes(
	image, admitted domain.ProjectImage, head domain.ProjectImageBaseInputs, preparation domain.Digest,
) error {
	if err := rebuiltImageKeepsAdmitted(image, admitted); err != nil {
		return err
	}
	if environment := image.Environment; environment == nil ||
		environment.PackageJSONSHA256 != head.PackageJSONSHA256 ||
		environment.PackageLockSHA256 != head.PackageLockSHA256 ||
		environment.PreparationDigest != preparation {
		return fmt.Errorf(
			"%w: image %s does not record the dependency manifests at %s, baked with this binary's toolchain and preparation sources",
			domain.ErrProjectImageIncompatible, image.ImageRef, head.CommitSHA)
	}
	return image.AdmissibleAt(head, preparation)
}

// rebuiltImageKeepsAdmitted is the part of the gate that needs no checkout:
// the image must be another image of the admitted image's repository that
// keeps its recipe, its base image, and the fixed preparation helper the room
// runs as its first command.
func rebuiltImageKeepsAdmitted(image, admitted domain.ProjectImage) error {
	if err := image.Validate(); err != nil {
		return err
	}
	if image.ID == admitted.ID || image.Repository != admitted.Repository ||
		image.RepositoryID != admitted.RepositoryID ||
		image.RecipeDigest != admitted.RecipeDigest ||
		image.BaseImageRef != admitted.BaseImageRef ||
		!slices.Equal(image.PreparationCommand, []string{projectimage.PreparationPath}) {
		return fmt.Errorf("%w: image %s does not keep what admitted image %s binds",
			domain.ErrProjectImageIncompatible, image.ImageRef, admitted.ImageRef)
	}
	return nil
}

// verificationImage is the image whose room verifies the candidate and that
// the checkpoint names.
func (b productionBinding) verificationImage() domain.ProjectImage {
	if b.rebuilt != nil {
		return *b.rebuilt
	}
	return b.image
}

// resolveVerificationImage sets the image that verifies verifiedSHA for this
// run: the rebuilt image a binding row names, the admitted image otherwise.
// The image a row names is held here to everything the admitted image binds,
// and to the commit's own inputs where the checkout is at hand
// (bindVerificationImage). A row that fails here contradicts the gate that
// wrote it, so it is a fault and never a stop a person is asked to act on.
func (w *productionPublicationWorkflow) resolveVerificationImage(
	ctx context.Context, binding *productionBinding, verifiedSHA string,
) error {
	binding.rebuilt = nil
	key := productionRebuildKey(productionRebuildImageKind, binding.run.ID, verifiedSHA)
	var rebuilt domain.ProjectImage
	found := false
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(ctx, key)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		var record productionRebuildRecord
		if entry.Kind != productionRebuildImageKind {
			return domain.ErrParentKeyMismatch
		}
		if err := strictjson.Decode(
			entry.Payload, &record, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
		); err != nil {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		if record != (productionRebuildRecord{
			Version: productionRebuildVersion, RunID: binding.run.ID, VerifiedSHA: verifiedSHA,
			AdmittedImage: binding.image.ID, ProjectImage: record.ProjectImage,
		}) {
			return domain.ErrParentKeyMismatch
		}
		rebuilt, err = tx.GetProjectImage(ctx, record.ProjectImage)
		if errors.Is(err, store.ErrNotFound) {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		found = err == nil
		return err
	})
	if err != nil {
		return fmt.Errorf("load rebuilt project-image binding for %s: %w", verifiedSHA, err)
	}
	if !found {
		return nil
	}
	if err := rebuiltImageKeepsAdmitted(rebuilt, binding.image); err != nil {
		// Not the incompatibility a re-entry stops its cycle on: that one is
		// an image the running daemon can no longer admit, and this is a row
		// naming an image the gate could never have bound.
		return fmt.Errorf("rebuilt project-image binding for %s names %s, which does not keep what the admitted image binds: %w",
			verifiedSHA, rebuilt.ImageRef, domain.ErrParentKeyMismatch)
	}
	binding.rebuilt = &rebuilt
	return nil
}

// bindVerificationImage applies the policy-gated rebuild to one verification
// immediately before its room is built, and returns the binding whose
// verification image the room and the checkpoint use.
//
// A candidate that keeps the dependency manifests the admitted image baked
// takes no gate: the binding comes back unchanged. A candidate that changes
// them either stays within the project's policy, and is verified in an image
// rebuilt at verifiedSHA from the daemon's own checkout, or is refused. A
// refusal is returned as a value, never as an error, and no room is built for
// it. An error is a fault: reading the checkout or the store, a builder
// failure that is not a proof failure (retryable), or state that contradicts
// the durable binding.
//
// The caller has proved checkoutDir is the run's repository and that the
// admitted image serves baseSHA.
func (w *productionPublicationWorkflow) bindVerificationImage(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkoutDir, baseSHA, verifiedSHA string,
) (productionBinding, *rebuildRefusal, error) {
	decision, err := projectimage.EvaluateRebuild(
		ctx, "git", checkoutDir, binding.image, baseSHA, verifiedSHA, binding.resolvedPolicy)
	if err != nil {
		return productionBinding{}, nil, err
	}
	if !decision.Needed {
		if binding.rebuilt != nil {
			return productionBinding{}, nil, fmt.Errorf(
				"run is bound to a rebuilt project image for %s, whose manifests the admitted image already serves: %w",
				verifiedSHA, domain.ErrParentKeyMismatch)
		}
		return binding, nil, nil
	}
	if decision.Refusal != "" {
		return binding, &rebuildRefusal{clause: decision.Refusal, detail: decision.Detail}, nil
	}
	if binding.rebuilt != nil {
		// A binding outlives the pass that wrote it, and the preparation
		// sources an image must record follow the running binary.
		if err := rebuiltImageServes(
			*binding.rebuilt, binding.image, decision.Head, w.preparationDigest,
		); err != nil {
			return productionBinding{}, nil, fmt.Errorf("rebuilt project image bound to %s: %w", verifiedSHA, err)
		}
		return binding, nil, nil
	}
	if w.rebuild == nil {
		return binding, &rebuildRefusal{
			clause: projectimage.RebuildRefusalNotConfigured,
			detail: "start the daemon with -base-build-ref to let it rebuild, or rebuild the project image and rerun",
		}, nil
	}
	if err := w.recordRebuildRow(ctx, productionRebuildIntentKind, binding.run.ID, verifiedSHA,
		productionRebuildIntent{
			Version: productionRebuildVersion, RunID: binding.run.ID, VerifiedSHA: verifiedSHA,
			AdmittedImage: binding.image.ID, Delta: decision.Delta,
		}); err != nil {
		return productionBinding{}, nil, err
	}
	// A build that finished before a restart already recorded its image, and
	// an earlier candidate of this repository may have built this environment.
	// Either is used as it stands; building again would record a second image
	// for one environment.
	rebuilt, found, err := w.recordedRebuild(ctx, binding.image, decision.Head)
	if err != nil {
		return productionBinding{}, nil, err
	}
	if !found {
		var refusal *rebuildRefusal
		rebuilt, refusal, err = w.rebuildProjectImage(ctx, task, binding, checkoutDir, verifiedSHA, decision.Head)
		if err != nil || refusal != nil {
			return binding, refusal, err
		}
	}
	if err := w.recordRebuildRow(ctx, productionRebuildImageKind, binding.run.ID, verifiedSHA,
		productionRebuildRecord{
			Version: productionRebuildVersion, RunID: binding.run.ID, VerifiedSHA: verifiedSHA,
			AdmittedImage: binding.image.ID, ProjectImage: rebuilt.ID,
		}); err != nil {
		return productionBinding{}, nil, err
	}
	binding.rebuilt = &rebuilt
	return binding, nil, nil
}

// recordRebuildRow writes one of the two rebuild rows and requires the stored
// row to be exactly this one. Both rows are functions of the run, the
// verified commit, and durable state, so a differing row is a contradiction.
func (w *productionPublicationWorkflow) recordRebuildRow(
	ctx context.Context, kind string, runID domain.RunID, verifiedSHA string, row any,
) error {
	payload, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return w.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		entry, _, err := tx.RecordInbox(ctx, productionRebuildKey(kind, runID, verifiedSHA), kind, payload)
		if err != nil {
			return err
		}
		if entry.Kind != kind || !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("project-image rebuild row %s disagrees with stored row: %w",
				kind, domain.ErrImmutableTransition)
		}
		return nil
	})
}

// recordedRebuild finds a recorded image of the admitted image's repository
// that already serves head. The first recorded one wins, so every pass and
// every run picks the same image.
func (w *productionPublicationWorkflow) recordedRebuild(
	ctx context.Context, admitted domain.ProjectImage, head domain.ProjectImageBaseInputs,
) (domain.ProjectImage, bool, error) {
	var images []domain.ProjectImage
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		images, err = tx.ListProjectImages(ctx, admitted.RepositoryID)
		return err
	}); err != nil {
		return domain.ProjectImage{}, false, err
	}
	for _, image := range images {
		if rebuiltImageServes(image, admitted, head, w.preparationDigest) == nil {
			return image, true, nil
		}
	}
	return domain.ProjectImage{}, false, nil
}

// rebuildProjectImage builds the admitted image again at verifiedSHA from the
// daemon's checkout and returns the image the store recorded. The recipe is
// the admitted image's own, read out of it: a rebuild changes dependencies,
// never what verification runs.
func (w *productionPublicationWorkflow) rebuildProjectImage(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkoutDir, verifiedSHA string,
	head domain.ProjectImageBaseInputs,
) (domain.ProjectImage, *rebuildRefusal, error) {
	refuse := func(err error) (domain.ProjectImage, *rebuildRefusal, error) {
		return domain.ProjectImage{}, &rebuildRefusal{
			clause: projectimage.RebuildRefusalProofFailed, detail: boundedRebuildText(err.Error()),
		}, nil
	}
	_, recipe, err := w.roomAndRecipe(ctx, task, binding, binding.image)
	if err != nil {
		return domain.ProjectImage{}, nil, err
	}
	request, err := projectimage.RebuildRequest(binding.image, verifiedSHA, checkoutDir, recipe, w.rebuild.Inputs)
	if err != nil {
		return refuse(err)
	}
	built, timedOut, err := w.rebuild.build(ctx, request)
	if timedOut || errors.Is(err, projectimage.ErrProofFailed) || errors.Is(err, projectimage.ErrInvalidRequest) {
		return refuse(err)
	}
	if err != nil {
		// Anything else is the build's environment (the registry, the proxy,
		// the container runtime, a cancelled pass), not a verdict on the
		// candidate, so the task stays queued and the next pass builds again.
		return domain.ProjectImage{}, nil, fmt.Errorf("rebuild project image at %s: %w",
			verifiedSHA, productionPublicationRetryableError(err))
	}
	// The builder's return is not the record: read back what the store holds
	// under the returned identity, and gate that.
	var recorded domain.ProjectImage
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		recorded, err = tx.GetProjectImage(ctx, built.ID)
		return err
	}); err != nil {
		return domain.ProjectImage{}, nil, fmt.Errorf("load rebuilt project image at %s: %w", verifiedSHA, err)
	}
	if recorded.CommitSHA != verifiedSHA {
		return domain.ProjectImage{}, nil, fmt.Errorf(
			"rebuilt project image was built at %s, not %s: %w",
			recorded.CommitSHA, verifiedSHA, domain.ErrParentKeyMismatch)
	}
	if err := rebuiltImageServes(recorded, binding.image, head, w.preparationDigest); err != nil {
		return domain.ProjectImage{}, nil, fmt.Errorf("rebuilt project image at %s: %w",
			verifiedSHA, errors.Join(domain.ErrParentKeyMismatch, err))
	}
	return recorded, nil, nil
}

// refusedRebuildVerification is the verification account of a candidate the
// rebuild gate refused: failed, with one daemon-produced evidence artifact
// that names the clause, and no room built. It takes the place of a room's
// result so the refusal blocks publication as any failed verification does.
func refusedRebuildVerification(
	task productionPublicationTask,
	binding productionBinding,
	headSHA string,
	refusal rebuildRefusal,
	approvedRecipes map[domain.Digest]bool,
) (verify.Result, error) {
	content := []byte(fmt.Sprintf(
		"Project image rebuild refused: %s\n\n"+
			"Freeside did not verify candidate %s. Its dependency manifests differ from the ones project image %s was built with, and Freeside rebuilds an image without a person only for a change inside the project's policy. No verification command ran.\n",
		refusal.account(), headSHA, binding.image.ImageRef))
	digest := domain.Digest(contentaddr.Sum(content))
	recipe := binding.image.RecipeDigest
	invocation := task.verificationInvocationID()
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID:     domain.ArtifactID(string(domain.ArtifactKindEvidence) + ":" + string(invocation) + ":" + string(digest)),
		Type:   domain.ArtifactKindEvidence,
		Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: invocation,
			HeadBinding: domain.HeadBound, SourceHeadSHA: headSHA,
			VerificationRecipeDigest: &recipe, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaTextPlain, SizeBytes: int64(len(content)),
			// The stable per-verification time a room's evidence carries, so a
			// replayed refusal converges on the same artifact.
			CreatedAt: binding.export.RecordedAt,
			Source:    domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
	}, approvedRecipes)
	if err != nil {
		return verify.Result{}, err
	}
	return verify.Result{
		HeadSHA: headSHA, RecipeDigest: recipe, Outcome: verify.OutcomeFailed,
		Evidence: []verify.Evidence{{Artifact: artifact, Content: content}},
	}, nil
}

// reentryRebuildRefusal carries a refused gate out of a re-entry's
// verification. A re-entry records no failed checkpoint without a verifier
// report, so its refusal ends the cycle with the clause in the reason.
type reentryRebuildRefusal struct {
	refusal rebuildRefusal
}

func (e *reentryRebuildRefusal) Error() string {
	return "project image rebuild refused: " + string(e.refusal.clause)
}

// reentryRebuildRefusedReason reports a refused rebuild gate with the reason
// the cycle stops on.
func reentryRebuildRefusedReason(
	task productionPublicationTask, baseSHA string, err error,
) (string, bool) {
	var refused *reentryRebuildRefusal
	if !errors.As(err, &refused) {
		return "", false
	}
	return fmt.Sprintf(
		"Readiness re-entry stopped because pull request head %s changes its dependency manifests against base %s and Freeside could not rebuild the project image for it: %s No verification ran. Rerun the work once the change is within the project's policy, or rebuild the project image.",
		task.HeadSHA, baseSHA, refused.refusal.account()), true
}
