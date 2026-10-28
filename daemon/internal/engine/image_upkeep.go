package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// ImageUpkeep keeps one repository's admitted image in service at its
// default-branch tip (plan §5.7, Project-image upkeep). A pass reads the
// admitted-image pointer, and when the image it names is not current at the
// tip, either rebuilds the image and advances the pointer, or records why it
// could not. Neither outcome raises an AttentionItem: a refusal is a record
// the adoption and maintenance units act on, and a host fault is an advisory
// system_health item the Health hook converges.
//
// The daemon serves one repository, so one ImageUpkeep covers it.
type ImageUpkeep struct {
	Store *store.Store
	// Rebuild is the daemon's one ProjectImageRebuild, shared with the
	// publication lane so their builds take one gate. Nil means the daemon
	// is not configured to rebuild, which a pass that needs a build records
	// as a host fault.
	Rebuild      *ProjectImageRebuild
	RepositoryID int64
	// ReadTip reads the repository's default-branch tip.
	ReadTip func(context.Context) (string, error)
	// Checkout fetches the default branch at tipSHA into dir, which must not
	// exist yet, and returns the repository ID the fetched checkout carries.
	// The checkout must hold the pointed-to image's build commit too.
	Checkout func(ctx context.Context, tipSHA, dir string) (int64, error)
	// NewRoom builds the networkless room the image's recipe is read through.
	NewRoom func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error)
	// RecipeReadTimeout bounds the recipe read. Zero means the publication
	// lane's default.
	RecipeReadTimeout time.Duration
	// Policy holds the project's policy keys: its registry set bounds a
	// dependency change, and its sizes bound the recipe-read room. Only
	// Keys is read.
	Policy            domain.ResolvedPolicy
	PreparationDigest domain.Digest
	// WorkDir holds each pass's scratch checkout.
	WorkDir string
	// Health converges the host-fault item: fault is empty once a pass
	// reaches a verdict, and says what failed when the builder could not.
	// Health, Now, and Logger are required.
	Health func(ctx context.Context, fault string) error
	Now    func() time.Time
	Logger *slog.Logger
	// AdoptionPending and MaintenancePending answer a settled refusal with
	// the adoption card or maintenance task that resolves it, when one does
	// (PendingResolution). Nil means that arm is not composed.
	AdoptionPending    func(context.Context, domain.ImageUpkeepRefusal) (domain.ItemID, bool, error)
	MaintenancePending func(context.Context, domain.ImageUpkeepRefusal) (domain.TaskID, bool, error)

	// settledMu guards settled, the last settled outcome this process
	// reached, which PendingResolution reads.
	settledMu sync.Mutex
	settled   *imageUpkeepOutcome
}

// imageUpkeepHarness is the agent base upkeep keeps. Claude Code is the only
// harness the daemon composes.
const imageUpkeepHarness = domain.HarnessClientClaudeCode

// imageUpkeepOutcome is what one pass decided. The zero value is no decision:
// no pointer yet, or another pass moved the pointer under this one.
type imageUpkeepOutcome struct {
	tipSHA   string
	current  bool
	advanced *domain.ProjectImage
	refusal  *domain.ImageUpkeepRefusal
	// hostFault says what failed, beside a host_fault refusal.
	hostFault string
}

// settled reports a verdict that stands until the tip or the binary
// changes. A host fault is not one: the next trigger tries again.
func (o imageUpkeepOutcome) settled() bool {
	return o.current || o.advanced != nil ||
		(o.refusal != nil && o.refusal.Class != domain.ImageUpkeepRefusalHostFault)
}

// TipAdvanced runs a pass at a tip the caller observed. It leaves the tip
// observation's UpkeepTipSHA to the caller, which owns that row.
func (u *ImageUpkeep) TipAdvanced(ctx context.Context, repositoryID int64, tipSHA string) error {
	if repositoryID != u.RepositoryID {
		return fmt.Errorf("image upkeep serves repository %d, not %d: %w",
			u.RepositoryID, repositoryID, domain.ErrRepositoryIdentityMismatch)
	}
	_, err := u.run(ctx, tipSHA, false)
	return err
}

// RunAll reads the tip and measures it again, for the start trigger: a new
// binary can change a verdict an older one recorded at the same tip. A
// settled pass records the tip it ran at on the repository's tip
// observation, when one exists, so the tip trigger does not run again for
// the same tip.
func (u *ImageUpkeep) RunAll(ctx context.Context) error {
	return u.runAtTip(ctx, false)
}

// RunScheduled is RunAll for the doctor's schedule, a backstop for a missed
// tip advance. A tip this binary's pointed-to image already has a settled
// refusal for is not measured again: that verdict stands until the tip or
// the image changes, and measuring it again would repeat a failing build
// every fire.
func (u *ImageUpkeep) RunScheduled(ctx context.Context) error {
	return u.runAtTip(ctx, true)
}

func (u *ImageUpkeep) runAtTip(ctx context.Context, reuseVerdict bool) error {
	tipSHA, err := u.ReadTip(ctx)
	if err != nil {
		return fmt.Errorf("read default-branch tip for image upkeep: %w", err)
	}
	outcome, err := u.run(ctx, tipSHA, reuseVerdict)
	if err != nil || !outcome.settled() {
		return err
	}
	return u.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		observation, found, err := tx.GetRepositoryTipObservation(ctx, u.RepositoryID)
		if err != nil || !found {
			return err
		}
		observation.UpkeepTipSHA = outcome.tipSHA
		return tx.RecordRepositoryTipObservation(ctx, observation)
	})
}

// run is one pass with its log line and health convergence.
func (u *ImageUpkeep) run(ctx context.Context, tipSHA string, reuseVerdict bool) (imageUpkeepOutcome, error) {
	outcome, err := u.pass(ctx, tipSHA, reuseVerdict)
	if err != nil {
		return outcome, fmt.Errorf("image upkeep at %s: %w", tipSHA, err)
	}
	switch {
	case outcome.current:
		u.Logger.Info("image upkeep: admitted image is current", "tip", tipSHA)
	case outcome.advanced != nil:
		u.Logger.Info("image upkeep: advanced the admitted image",
			"tip", tipSHA, "image", outcome.advanced.ImageRef)
	case outcome.refusal != nil:
		u.Logger.Warn("image upkeep: recorded a refusal",
			"tip", tipSHA, "class", outcome.refusal.Class, "refusal", outcome.refusal.ID)
	default:
		return outcome, nil
	}
	if outcome.settled() {
		u.settledMu.Lock()
		u.settled = &outcome
		u.settledMu.Unlock()
	}
	if err := u.Health(ctx, outcome.hostFault); err != nil {
		return outcome, fmt.Errorf("converge image upkeep health: %w", err)
	}
	return outcome, nil
}

// PendingResolution reports what a writer attempt whose base no admitted
// image serves is waiting on, or nil when upkeep has nothing pending that
// could change that:
//
//   - A rebuild, while this process has no settled verdict, or the
//     repository's observed tip has moved past the one it settled at. A
//     host fault never settles, so it stays a rebuild.
//   - An adoption proposal or a maintenance task, when the settled verdict
//     is a refusal that AdoptionPending or MaintenancePending answers.
//   - Nothing otherwise: the image is current at the tip, or the refusal
//     has no answer yet.
func (u *ImageUpkeep) PendingResolution(ctx context.Context) (*domain.ImageUpkeepHold, error) {
	u.settledMu.Lock()
	settled := u.settled
	u.settledMu.Unlock()
	rebuild := &domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingRebuild}
	if settled == nil {
		return rebuild, nil
	}
	var (
		observation domain.RepositoryTipObservation
		found       bool
	)
	if err := u.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		observation, found, err = tx.GetRepositoryTipObservation(ctx, u.RepositoryID)
		return err
	}); err != nil {
		return nil, fmt.Errorf("read repository tip observation: %w", err)
	}
	if found && observation.TipSHA != settled.tipSHA {
		return rebuild, nil
	}
	refusal := settled.refusal
	if refusal == nil {
		return nil, nil
	}
	refusalID := refusal.ID
	if u.AdoptionPending != nil {
		item, ok, err := u.AdoptionPending(ctx, *refusal)
		if err != nil {
			return nil, fmt.Errorf("find the adoption proposal for refusal %s: %w", refusalID, err)
		}
		if ok {
			return &domain.ImageUpkeepHold{
				Pending: domain.ImageUpkeepPendingAdoptionProposal, RefusalRecordID: &refusalID,
				ProposalItemID: &item,
			}, nil
		}
	}
	if u.MaintenancePending != nil {
		task, ok, err := u.MaintenancePending(ctx, *refusal)
		if err != nil {
			return nil, fmt.Errorf("find the maintenance task for refusal %s: %w", refusalID, err)
		}
		if ok {
			return &domain.ImageUpkeepHold{
				Pending: domain.ImageUpkeepPendingMaintenanceTask, RefusalRecordID: &refusalID,
				MaintenanceTaskID: &task,
			}, nil
		}
	}
	return nil, nil
}

// pass decides the pointed-to image at tipSHA.
func (u *ImageUpkeep) pass(ctx context.Context, tipSHA string, reuseVerdict bool) (imageUpkeepOutcome, error) {
	pointer, image, found, err := u.readPointer(ctx)
	if err != nil {
		return imageUpkeepOutcome{}, err
	}
	if !found {
		u.Logger.Info("image upkeep: no admitted image to keep yet", "repository_id", u.RepositoryID)
		return imageUpkeepOutcome{}, nil
	}
	if reuseVerdict {
		recorded, found, err := u.settledRefusal(ctx, image.ID, tipSHA)
		if err != nil || found {
			return imageUpkeepOutcome{tipSHA: tipSHA, refusal: &recorded}, err
		}
	}
	scratch, err := os.MkdirTemp(u.WorkDir, "image-upkeep-")
	if err != nil {
		return imageUpkeepOutcome{}, fmt.Errorf("create upkeep scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort cleanup of pass-owned scratch
	dir := filepath.Join(scratch, "checkout")
	repositoryID, err := u.Checkout(ctx, tipSHA, dir)
	if err != nil {
		return imageUpkeepOutcome{}, fmt.Errorf("fetch default-branch tip: %w", err)
	}
	if repositoryID != u.RepositoryID {
		return imageUpkeepOutcome{}, fmt.Errorf(
			"fetched tip carries repository id %d, not the configured %d: %w",
			repositoryID, u.RepositoryID, domain.ErrRepositoryIdentityMismatch)
	}
	if u.current(ctx, dir, image, tipSHA) {
		return imageUpkeepOutcome{tipSHA: tipSHA, current: true}, nil
	}
	decision, err := projectimage.EvaluateUpkeep(ctx, "git", dir, image, tipSHA, u.Policy)
	if err != nil {
		if ctx.Err() != nil {
			return imageUpkeepOutcome{}, err
		}
		// Reading the checkout failed, most often because the image's build
		// commit is not on the default branch, so no delta can be measured
		// from it. That is the host's to fix, not a verdict on the tip.
		return u.refuse(ctx, image, tipSHA, domain.ImageUpkeepRefusalHostFault, nil,
			"the admitted image could not be measured against the tip: "+boundedRebuildText(err.Error()))
	}
	if decision.Refusal != "" {
		class, err := projectimage.ClassifyUpkeepFindings(decision.Findings)
		if err != nil {
			return imageUpkeepOutcome{}, err
		}
		return u.refuse(ctx, image, tipSHA, class, decision.Findings, "")
	}
	target, found, err := u.recordedServing(ctx, dir, image, decision.Head)
	if err != nil {
		return imageUpkeepOutcome{}, err
	}
	if !found {
		if u.Rebuild == nil {
			const detail = "start the daemon with -base-build-ref to let it rebuild project images"
			return u.refuse(ctx, image, tipSHA, domain.ImageUpkeepRefusalHostFault,
				[]domain.ImageUpkeepClauseFinding{{Clause: projectimage.RebuildRefusalNotConfigured, Detail: detail}},
				"this daemon is not configured to rebuild project images: "+detail)
		}
		built, outcome, err := u.rebuild(ctx, pointer, image, dir, tipSHA, decision.Head)
		if err != nil || built == nil {
			return outcome, err
		}
		target = *built
	}
	return u.advance(ctx, pointer, target, tipSHA)
}

func (u *ImageUpkeep) readPointer(
	ctx context.Context,
) (pointer domain.AdmittedImagePointer, image domain.ProjectImage, found bool, err error) {
	err = u.Store.Read(ctx, func(tx *store.ReadTx) error {
		pointer, image, found, err = tx.GetAdmittedImagePointer(ctx, u.RepositoryID, imageUpkeepHarness)
		return err
	})
	if err != nil {
		return domain.AdmittedImagePointer{}, domain.ProjectImage{}, false,
			fmt.Errorf("read admitted image pointer: %w", err)
	}
	return pointer, image, found, nil
}

// current is the contract's verdict: AdmissibleAt passes at the tip, and the
// image was prepared with this binary's preparation sources. AdmissibleAt
// alone takes an image at its own build commit without comparing them, so
// after an upgrade an image built at the tip would never be rebuilt. A tip
// whose inputs cannot be read is not current; the gate says why.
func (u *ImageUpkeep) current(ctx context.Context, dir string, image domain.ProjectImage, tipSHA string) bool {
	if image.Environment == nil || image.Environment.PreparationDigest != u.PreparationDigest {
		return false
	}
	tip, err := projectimage.ObserveBaseInputs(ctx, "git", dir, tipSHA, image.InputLayout())
	return err == nil && image.AdmissibleAt(tip, u.PreparationDigest) == nil
}

// recordedServing finds the first recorded image of the repository that
// serves the tip and keeps what the pointed-to image binds, as the
// publication lane's recordedRebuild does. Unlike that lane, upkeep takes
// only an image built at a commit the checkout holds: a later pass measures
// the next dependency change from the pointed-to image's build commit, and
// an image a publication rebuild made at a pull request head that never
// reached the branch would leave nothing to measure from.
func (u *ImageUpkeep) recordedServing(
	ctx context.Context, dir string, pointed domain.ProjectImage, tip domain.ProjectImageBaseInputs,
) (domain.ProjectImage, bool, error) {
	var images []domain.ProjectImage
	if err := u.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		images, err = tx.ListProjectImages(ctx, u.RepositoryID)
		return err
	}); err != nil {
		return domain.ProjectImage{}, false, err
	}
	for _, image := range images {
		if rebuiltImageServes(image, pointed, tip, u.PreparationDigest) != nil {
			continue
		}
		if _, err := projectimage.ObserveBaseInputs(
			ctx, "git", dir, image.CommitSHA, image.InputLayout(),
		); err == nil {
			return image, true, nil
		}
	}
	return domain.ProjectImage{}, false, nil
}

// rebuild builds the pointed-to image again at the tip under the shared
// gate. It returns the image to advance to, or, with a nil image, the
// outcome that ends the pass: a refusal, or none when another pass moved the
// pointer.
func (u *ImageUpkeep) rebuild(
	ctx context.Context,
	pointer domain.AdmittedImagePointer,
	image domain.ProjectImage,
	dir, tipSHA string,
	tip domain.ProjectImageBaseInputs,
) (*domain.ProjectImage, imageUpkeepOutcome, error) {
	if err := u.Rebuild.gate.acquire(ctx); err != nil {
		return nil, imageUpkeepOutcome{}, err
	}
	defer u.Rebuild.gate.release()
	// While this pass waited, another may have advanced the pointer, or a
	// publication rebuild may have recorded an image serving the tip.
	again, _, found, err := u.readPointer(ctx)
	if err != nil || !found || again.ImageID != pointer.ImageID {
		return nil, imageUpkeepOutcome{}, err
	}
	if recorded, found, err := u.recordedServing(ctx, dir, image, tip); err != nil || found {
		return &recorded, imageUpkeepOutcome{}, err
	}
	refuse := func(class domain.ImageUpkeepRefusalClass, cause error) (*domain.ProjectImage, imageUpkeepOutcome, error) {
		var findings []domain.ImageUpkeepClauseFinding
		fault := ""
		if class == domain.ImageUpkeepRefusalHostFault {
			fault = "the project image could not be rebuilt at the tip: " + boundedRebuildText(cause.Error())
		} else {
			findings = []domain.ImageUpkeepClauseFinding{{
				Clause: projectimage.RebuildRefusalProofFailed, Detail: boundedRebuildText(cause.Error()),
			}}
		}
		outcome, err := u.refuse(ctx, image, tipSHA, class, findings, fault)
		return nil, outcome, err
	}
	recipe, err := u.readRecipe(ctx, image)
	if errors.Is(err, errImageUpkeepHostFault) {
		return refuse(domain.ImageUpkeepRefusalHostFault, err)
	}
	if err != nil {
		return nil, imageUpkeepOutcome{}, err
	}
	request, err := projectimage.RebuildRequest(image, tipSHA, dir, recipe, u.Rebuild.Inputs)
	if err != nil {
		return refuse(projectimage.ClassifyUpkeepBuildFailure(err), err)
	}
	built, timedOut, err := u.Rebuild.build(ctx, request)
	if timedOut {
		return refuse(domain.ImageUpkeepRefusalHostFault, err)
	}
	if err != nil {
		// A cancelled pass is no verdict on the tip (build returns the cause).
		if ctx.Err() != nil {
			return nil, imageUpkeepOutcome{}, err
		}
		return refuse(projectimage.ClassifyUpkeepBuildFailure(err), err)
	}
	// The builder's return is not the record: read back what the store holds
	// under the returned identity, and gate that.
	var recorded domain.ProjectImage
	if err := u.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		recorded, err = tx.GetProjectImage(ctx, built.ID)
		return err
	}); err != nil {
		return nil, imageUpkeepOutcome{}, fmt.Errorf("load rebuilt project image at %s: %w", tipSHA, err)
	}
	// A record that disagrees with the build is the builder's fault, not the
	// tip's, so it is a host fault and the pointer stays.
	if recorded.CommitSHA != tipSHA {
		return refuse(domain.ImageUpkeepRefusalHostFault, fmt.Errorf(
			"rebuilt project image was built at %s, not %s: %w",
			recorded.CommitSHA, tipSHA, domain.ErrParentKeyMismatch))
	}
	if err := rebuiltImageServes(recorded, image, tip, u.PreparationDigest); err != nil {
		return refuse(domain.ImageUpkeepRefusalHostFault, fmt.Errorf("rebuilt project image at %s: %w",
			tipSHA, errors.Join(domain.ErrParentKeyMismatch, err)))
	}
	return &recorded, imageUpkeepOutcome{}, nil
}

// errImageUpkeepHostFault marks a failure of the host rather than of the
// tip: a recipe-read room that could not start or read, or an image record
// that disagrees with the image or the build. A person fixes it, so it is a
// host fault that files an item, not a verdict.
var errImageUpkeepHostFault = errors.New("the image-upkeep host failed")

// readRecipe reads the pointed-to image's recipe out of the image, as the
// publication lane's roomAndRecipe does: a rebuild changes dependencies,
// never what verification runs. A recipe the record does not name means
// the record disagrees with its image, a host fault.
func (u *ImageUpkeep) readRecipe(ctx context.Context, image domain.ProjectImage) ([]byte, error) {
	sizes, err := ward.ResolveLaunchSizes(u.Policy.Keys)
	if err != nil {
		return nil, fmt.Errorf("resolve verification size: %w", err)
	}
	room, err := u.NewRoom(image, sizes.Verification)
	if err != nil {
		return nil, fmt.Errorf("construct networkless verification room: %w",
			errors.Join(errImageUpkeepHostFault, err))
	}
	timeout := u.RecipeReadTimeout
	if timeout == 0 {
		timeout = defaultProductionRecipeReadTimeout
	}
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	recipe, err := room.ReadRecipe(readCtx)
	cancel()
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		return nil, fmt.Errorf("load project-image verification recipe: %w",
			errors.Join(errImageUpkeepHostFault, err))
	}
	if len(recipe) > verify.DefaultMaxRecipeBytes {
		return nil, fmt.Errorf("project-image verification recipe exceeds the %d-byte cap: %w",
			verify.DefaultMaxRecipeBytes, errImageUpkeepHostFault)
	}
	if got := verify.RecipeDigest(recipe); got != image.RecipeDigest {
		return nil, fmt.Errorf("project-image verification recipe digest %s, want %s: %w",
			got, image.RecipeDigest, errors.Join(errImageUpkeepHostFault, domain.ErrParentKeyMismatch))
	}
	return recipe, nil
}

// settledRefusal finds a refusal other than a host fault already recorded
// for imageID at tipSHA.
func (u *ImageUpkeep) settledRefusal(
	ctx context.Context, imageID domain.Digest, tipSHA string,
) (domain.ImageUpkeepRefusal, bool, error) {
	var refusals []domain.ImageUpkeepRefusal
	if err := u.Store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		refusals, err = tx.ListImageUpkeepRefusals(ctx, u.RepositoryID)
		return err
	}); err != nil {
		return domain.ImageUpkeepRefusal{}, false, fmt.Errorf("list image upkeep refusals: %w", err)
	}
	for _, refusal := range refusals {
		if refusal.MeasuredFromImageID == imageID && refusal.TipSHA == tipSHA &&
			refusal.Class != domain.ImageUpkeepRefusalHostFault {
			return refusal, true, nil
		}
	}
	return domain.ImageUpkeepRefusal{}, false, nil
}

// refuse records one classified refusal. A rerun that refuses the same way
// at the same tip converges on the first record.
func (u *ImageUpkeep) refuse(
	ctx context.Context,
	image domain.ProjectImage,
	tipSHA string,
	class domain.ImageUpkeepRefusalClass,
	findings []domain.ImageUpkeepClauseFinding,
	hostFault string,
) (imageUpkeepOutcome, error) {
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: u.RepositoryID, Repository: image.Repository, Harness: imageUpkeepHarness,
		TipSHA: tipSHA, MeasuredFromImageID: image.ID, Class: class, Findings: findings,
		RecordedAt: u.Now().UTC(),
	})
	if err != nil {
		return imageUpkeepOutcome{}, err
	}
	if err := u.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		refusal, err = tx.RecordImageUpkeepRefusal(ctx, refusal)
		return err
	}); err != nil {
		return imageUpkeepOutcome{}, err
	}
	return imageUpkeepOutcome{tipSHA: tipSHA, refusal: &refusal, hostFault: hostFault}, nil
}

// advance points the repository at target, unless another pass moved the
// pointer since this one read it.
func (u *ImageUpkeep) advance(
	ctx context.Context, pointer domain.AdmittedImagePointer, target domain.ProjectImage, tipSHA string,
) (imageUpkeepOutcome, error) {
	moved := false
	if err := u.Store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		again, _, found, err := tx.GetAdmittedImagePointer(ctx, u.RepositoryID, imageUpkeepHarness)
		if err != nil {
			return err
		}
		if !found || again.ImageID != pointer.ImageID {
			moved = true
			return nil
		}
		return tx.AdvanceAdmittedImagePointer(ctx, domain.AdmittedImagePointer{
			RepositoryID: u.RepositoryID, Repository: target.Repository, Harness: imageUpkeepHarness,
			ImageID: target.ID, ImageRef: target.ImageRef, AdvancedAt: u.Now().UTC(),
		})
	}); err != nil {
		return imageUpkeepOutcome{}, fmt.Errorf("advance admitted image pointer: %w", err)
	}
	if moved {
		return imageUpkeepOutcome{}, nil
	}
	return imageUpkeepOutcome{tipSHA: tipSHA, advanced: &target}, nil
}
