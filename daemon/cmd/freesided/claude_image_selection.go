package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The writer image comes from the admitted-image pointer (plan §5.7,
// Project-image upkeep), and ward's conformance follows it: ward proves one
// writer image at a time, so when selection picks another image the daemon
// repoints ward at it and runs the full suite once, and the attempt holds
// under backend_not_conformant until that pass is recorded. An attempt also
// holds there while ward names the selected image but has published no proof
// for it, after a failed pass or during one; a failed follower pass is retried
// once its backoff has passed.

// A failed follower pass may be retried after followerRetryBackoff, doubled
// for each consecutive failure up to followerRetryBackoffCap, so a suite that
// keeps failing does not run back to back.
const (
	followerRetryBackoff    = 5 * time.Minute
	followerRetryBackoffCap = time.Hour
)

// baseInputsMemo reads what the daemon's base holds for each input layout
// once. The base is fixed per daemon (-base-sha) and its tree cannot change,
// so an observation stays true for the process's life; a failed read is not
// kept and the next selection tries again.
type baseInputsMemo struct {
	mu      sync.Mutex
	observe func(context.Context, domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error)
	inputs  map[domain.ProjectImageInputLayout]domain.ProjectImageBaseInputs
}

func newBaseInputsMemo(
	observe func(context.Context, domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error),
) *baseInputsMemo {
	return &baseInputsMemo{observe: observe, inputs: map[domain.ProjectImageInputLayout]domain.ProjectImageBaseInputs{}}
}

func (m *baseInputsMemo) read(
	ctx context.Context, layout domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inputs, ok := m.inputs[layout]; ok {
		return inputs, nil
	}
	inputs, err := m.observe(ctx, layout)
	if err != nil {
		return domain.ProjectImageBaseInputs{}, err
	}
	m.inputs[layout] = inputs
	return inputs, nil
}

// imageRepointer is the part of ward's backend the follower drives.
type imageRepointer interface {
	RepointAgentImage(image string) error
}

// startupWardBackend is the part of ward's backend a start drives before
// any proof is restored or run.
type startupWardBackend interface {
	imageRepointer
	ConfigurationDigestFor(image string) domain.Digest
}

// conformanceFollower moves ward's proof to the image selection picks. It
// runs at most one conformance pass at a time, its own or the doctor's
// scheduled one, so a scheduled pass never proves an image the follower is
// about to leave. It remembers the image its last pass failed on, so a later
// selection of that image can retry it once the backoff has passed.
type conformanceFollower struct {
	backend imageRepointer
	// prove runs the full suite for the backend's current image.
	prove  func(context.Context) error
	now    func() time.Time
	logger *slog.Logger

	passMu sync.Mutex

	mu        sync.Mutex
	following domain.ImageRef
	// failed is the image the last follower pass failed on, failures the
	// consecutive failed follower passes, and retryAt when failed may be
	// retried. Any successful pass clears all three.
	failed   domain.ImageRef
	failures int
	retryAt  time.Time

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newConformanceFollower(
	backend imageRepointer, prove func(context.Context) error, now func() time.Time, logger *slog.Logger,
) *conformanceFollower {
	ctx, cancel := context.WithCancel(context.Background())
	return &conformanceFollower{backend: backend, prove: prove, now: now, logger: logger, ctx: ctx, cancel: cancel}
}

// follow starts repointing ward at image and proving it, unless a pass is
// already following an image; an attempt still waiting after that pass asks
// again. It never blocks the caller.
func (f *conformanceFollower) follow(image domain.ImageRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.following != "" || f.ctx.Err() != nil {
		return
	}
	f.startPass(image, false)
}

// retry proves image again when the follower's last pass failed on it and
// its backoff has passed, unless a pass is already following an image. It
// never blocks the caller.
func (f *conformanceFollower) retry(image domain.ImageRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.following != "" || f.ctx.Err() != nil || f.failed != image || f.now().Before(f.retryAt) {
		return
	}
	f.startPass(image, true)
}

// startPass runs one follower pass for image in the background; f.mu must be
// held. A retry that waited behind a scheduled pass runs no suite when that
// pass cleared the failure, because ward then already proves image.
func (f *conformanceFollower) startPass(image domain.ImageRef, retry bool) {
	f.following = image
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		defer func() {
			f.mu.Lock()
			f.following = ""
			f.mu.Unlock()
		}()
		f.passMu.Lock()
		defer f.passMu.Unlock()
		if retry && !f.stillFailed(image) {
			return
		}
		if err := f.backend.RepointAgentImage(string(image)); err != nil {
			f.logger.Error("repoint ward at the selected writer image", "image", image, "error", err)
			return
		}
		if retry {
			f.logger.Info("retrying conformance for the selected writer image", "image", image)
		} else {
			f.logger.Info("ward repointed at the selected writer image; running conformance", "image", image)
		}
		if err := f.prove(f.ctx); err != nil {
			// A pass Close cancelled did not fail; it says nothing of the image.
			if f.ctx.Err() == nil {
				retryAt := f.recordFailure(image)
				f.logger.Warn("conformance for the selected writer image failed",
					"image", image, "retry_at", retryAt, "error", err)
			}
			return
		}
		f.clearFailure()
		f.logger.Info("conformance recorded for the selected writer image", "image", image)
	}()
}

func (f *conformanceFollower) stillFailed(image domain.ImageRef) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failed == image
}

// recordFailure remembers a failed follower pass on image and returns when it
// may be retried.
func (f *conformanceFollower) recordFailure(image domain.ImageRef) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed = image
	f.failures++
	f.retryAt = f.now().Add(followerBackoff(f.failures))
	return f.retryAt
}

func (f *conformanceFollower) clearFailure() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failed, f.failures, f.retryAt = "", 0, time.Time{}
}

// followerBackoff is the wait after failures consecutive failed passes.
func followerBackoff(failures int) time.Duration {
	backoff := followerRetryBackoff
	for i := 1; i < failures && backoff < followerRetryBackoffCap; i++ {
		backoff *= 2
	}
	return min(backoff, followerRetryBackoffCap)
}

// pendingFor reports whether a pass is following image right now.
func (f *conformanceFollower) pendingFor(image domain.ImageRef) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.following == image
}

// runScheduled is the doctor's scheduled pass, serialized with the
// follower's. It proves ward's current image. A success clears the follower's
// failure; a failure is not remembered, so it arms no retry and waits for the
// next scheduled pass.
func (f *conformanceFollower) runScheduled(ctx context.Context) error {
	f.passMu.Lock()
	defer f.passMu.Unlock()
	if err := f.prove(ctx); err != nil {
		return err
	}
	f.clearFailure()
	return nil
}

// Close stops a pass in flight and waits for it.
func (f *conformanceFollower) Close(ctx context.Context) error {
	f.cancel()
	done := make(chan struct{})
	go func() {
		f.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop the conformance follower: %w", ctx.Err())
	}
}

// wardConformance reads the image ward proves and the capabilities it
// declares for it.
type wardConformance interface {
	ConformanceIdentity() (image string, digest domain.Digest)
	Capabilities() exec.CapabilitySet
}

// claudeImageSelector selects each writer attempt's image from the pointer
// and the repository's recorded images.
type claudeImageSelector struct {
	store        *store.Store
	repo         string
	repositoryID int64
	baseSHA      string
	backend      wardConformance
	base         *baseInputsMemo
	follower     *conformanceFollower

	// pendingMu guards pending, image upkeep's lookup. Upkeep is composed
	// after the driver, so it is bound late (bindImageUpkeep); unbound, a
	// base no image serves has nothing pending.
	pendingMu sync.Mutex
	pending   func(context.Context) (*domain.ImageUpkeepHold, error)
}

func (s *claudeImageSelector) bindImageUpkeep(pending func(context.Context) (*domain.ImageUpkeepHold, error)) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	s.pending = pending
}

// selectImage is the engine's ImageSelection. It returns the image to admit,
// or the error that holds the attempt: image_upkeep_pending or the unserved
// admission_policy_refused hold when no image serves the base,
// backend_not_conformant while ward does not yet prove the selected image,
// and input_unavailable when the base could not be read.
//
// Ward proves the selected image only once it also publishes the two
// capabilities a pass earns, supports_networkless_export and
// supports_enforced_provider_egress: a pass publishes both on success, and
// one that is running or failed leaves both cleared. Until then the attempt
// holds as unproved rather than reaching admission's capability check, and a
// failed follower pass is retried.
//
// A maintenance task's run (repair non-nil) skips that choice: it runs on the
// image its refusal was measured from while the pointer still names it, and
// holds as unserved once the pointer has moved on, never in another image.
// It never takes upkeep's hold, which names the task itself.
func (s *claudeImageSelector) selectImage(
	ctx context.Context, _ domain.InvocationID, repair *domain.ImageUpkeepRefusal,
) (domain.ImageRef, error) {
	proved, _ := s.backend.ConformanceIdentity()
	var (
		selected domain.ProjectImage
		found    bool
		err      error
	)
	if repair != nil {
		selected, found, err = s.repairImage(ctx, *repair)
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("admitted image pointer no longer names image %s, which refusal %s was measured from: %w",
				repair.MeasuredFromImageID, repair.ID, engine.ErrProjectImageUnserved)
		}
	} else {
		selected, found, err = selectRecordedImage(ctx, s.store, s.repo, s.repositoryID, s.baseSHA,
			domain.ImageRef(proved), s.base)
		if err != nil {
			return "", err
		}
	}
	if !found {
		s.pendingMu.Lock()
		pending := s.pending
		s.pendingMu.Unlock()
		if pending == nil {
			return "", engine.ErrProjectImageUnserved
		}
		hold, err := pending(ctx)
		if err != nil {
			return "", fmt.Errorf("read image upkeep's pending resolution: %w", err)
		}
		if hold == nil {
			return "", engine.ErrProjectImageUnserved
		}
		return "", &engine.ImageUpkeepPendingError{Hold: *hold}
	}
	notProved := errors.Join(engine.ErrImageNotProved, domain.ErrAdmissionConfigurationMismatch)
	switch {
	case string(selected.ImageRef) != proved:
		s.follower.follow(selected.ImageRef)
		return "", fmt.Errorf("image %s waits on its conformance pass: %w", selected.ImageRef, notProved)
	case s.follower.pendingFor(selected.ImageRef):
		return "", fmt.Errorf("image %s waits on its conformance pass: %w", selected.ImageRef, notProved)
	case !publishedProof(s.backend.Capabilities()):
		s.follower.retry(selected.ImageRef)
		return "", fmt.Errorf("image %s has no published conformance proof: %w", selected.ImageRef, notProved)
	}
	return selected.ImageRef, nil
}

func publishedProof(caps exec.CapabilitySet) bool {
	return caps.Has(exec.CapNetworklessExport) && caps.Has(exec.CapEnforcedProviderEgress)
}

// repairImage returns the pointed-to image when it is the one refusal was
// measured from, and found false otherwise.
func (s *claudeImageSelector) repairImage(
	ctx context.Context, refusal domain.ImageUpkeepRefusal,
) (domain.ProjectImage, bool, error) {
	var (
		pointed domain.ProjectImage
		found   bool
	)
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		_, pointed, found, err = tx.GetAdmittedImagePointer(ctx, s.repositoryID, domain.HarnessClientClaudeCode)
		return err
	}); err != nil {
		return domain.ProjectImage{}, false, fmt.Errorf("read admitted image pointer: %w", err)
	}
	if !found || pointed.ID != refusal.MeasuredFromImageID {
		return domain.ProjectImage{}, false, nil
	}
	if pointed.Repository != s.repo {
		return domain.ProjectImage{}, false, fmt.Errorf(
			"admitted image repository %q disagrees with configured %q: %w",
			pointed.Repository, s.repo, domain.ErrRepositoryIdentityMismatch)
	}
	return pointed, true, nil
}

// selectRecordedImage reads the pointer and the repository's recorded images
// and selects among them (engine.SelectProjectImage). found is false when no
// image serves the base, including when no pointer exists. A base that
// cannot be read is exec.ErrInputUnavailable, which holds the attempt
// instead of failing the dispatch pass.
func selectRecordedImage(
	ctx context.Context, st *store.Store, repo string, repositoryID int64, baseSHA string,
	proved domain.ImageRef, base *baseInputsMemo,
) (domain.ProjectImage, bool, error) {
	var (
		pointed domain.ProjectImage
		images  []domain.ProjectImage
		found   bool
	)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		_, pointed, found, err = tx.GetAdmittedImagePointer(ctx, repositoryID, domain.HarnessClientClaudeCode)
		if err != nil || !found {
			return err
		}
		images, err = tx.ListProjectImages(ctx, repositoryID)
		return err
	}); err != nil {
		return domain.ProjectImage{}, false, fmt.Errorf("read admitted images: %w", err)
	}
	if !found {
		return domain.ProjectImage{}, false, nil
	}
	if pointed.Repository != repo {
		return domain.ProjectImage{}, false, fmt.Errorf(
			"admitted image repository %q disagrees with configured %q: %w",
			pointed.Repository, repo, domain.ErrRepositoryIdentityMismatch)
	}
	selected, found, err := engine.SelectProjectImage(engine.ProjectImageSelection{
		Proved: proved, Pointed: pointed, Images: images, BaseSHA: baseSHA,
		BaseInputs: func(layout domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
			return base.read(ctx, layout)
		},
		Preparation: projectimage.PreparationDigest(),
	})
	if err != nil {
		return domain.ProjectImage{}, false, fmt.Errorf("select project image: %w",
			errors.Join(exec.ErrInputUnavailable, err))
	}
	return selected, found, nil
}

// startupProvedImage names the recorded image whose configuration digest,
// on this backend, is the latest passed conformance record's: the image a
// restart can restore a proof for without running the suite again.
func startupProvedImage(
	ctx context.Context, st *store.Store, repositoryID int64,
	digestFor func(image string) domain.Digest, conformance domain.BackendConformance, found bool,
) (domain.ImageRef, error) {
	if !found || conformance.Outcome != domain.ConformancePassed {
		return "", nil
	}
	var images []domain.ProjectImage
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		images, err = tx.ListProjectImages(ctx, repositoryID)
		return err
	}); err != nil {
		return "", fmt.Errorf("list project images: %w", err)
	}
	for _, image := range images {
		if digestFor(string(image.ImageRef)) == conformance.ConfigurationDigest {
			return image.ImageRef, nil
		}
	}
	return "", nil
}

// startupWriterImage is the image ward starts on. It is the selected image,
// or the pointed-to image when none serves the base, so attempts hold until
// upkeep acts. A start that will not run the suite starts ward on the image
// the latest record proves instead, when that is another one, so the proof
// restores; the first attempt then moves ward to the selected image through
// the follower rather than the start failing for want of a proof.
func startupWriterImage(
	selected domain.ProjectImage, found bool, pointed domain.ImageRef, proved domain.ImageRef, runConformance bool,
) domain.ImageRef {
	target := pointed
	if found {
		target = selected.ImageRef
	}
	if !runConformance && proved != "" {
		return proved
	}
	return target
}
