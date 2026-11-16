package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/keyedrun"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// The writer image comes from the admitted-image pointer (plan §5.7,
// Project-image upkeep). Ward holds one proof per writer image's
// configuration (#3211), so proving one image never retires another's: when
// selection picks an image ward has not proved, the daemon proves it once,
// and that attempt holds under backend_not_conformant until the pass is
// recorded while attempts on proven images keep starting. An attempt also
// holds there after its image's pass failed; the pass is retried once its
// backoff has passed. The scheduled pass re-proves the images attempts
// selected since the previous one and retires every other proof
// (runScheduled), so the live proofs stay bounded by the images in use.

// A failed follower pass may be retried after followerRetryBackoff, doubled
// for each consecutive failure up to followerRetryBackoffCap, so a suite that
// keeps failing does not run back to back.
const (
	followerRetryBackoff    = 5 * time.Minute
	followerRetryBackoffCap = time.Hour
)

// baseInputsMemoBases is how many base commits baseInputsMemo keeps. Runs
// admitted at different tips select at their own bases, but nearly all
// selections are for the latest tip or a few older runs, so a handful of
// bases keeps the fetches rare without growing as the tip advances.
const baseInputsMemoBases = 8

// A fetch the engine's pass starts (baseInputsMemo.lookup) is bounded by
// baseFetchTimeout, so a hung fetch fails and holds its attempt visibly as
// input_unavailable instead of under a pending hold that records nothing. A
// failed fetch is served for baseFetchRetryBackoff before the base is
// fetched again: the engine passes every 100 ms (cmd/freesided's reconcile
// interval), and a base that keeps failing would otherwise be refetched on
// nearly every pass.
const (
	baseFetchTimeout      = 5 * time.Minute
	baseFetchRetryBackoff = time.Minute
)

// baseInputsMemo reads what a base commit holds for each input layout once.
// A commit's tree cannot change, so an observation stays true for the
// process's life. It keeps the baseInputsMemoBases most recently first-read
// commits and forgets the oldest past that, so a long-running daemon whose
// tip keeps advancing holds a bounded set. It is per composition, which
// serves one repository, so a base commit and layout name a project's base.
//
// The engine's pass reads it with lookup, which never waits on the network:
// a miss starts the fetch on a worker and answers engine.ErrBaseInputsPending.
// The start reads it with read, which waits for the fetch. Neither holds the
// lock across a fetch.
type baseInputsMemo struct {
	mu      sync.Mutex
	observe func(context.Context, string, domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error)
	inputs  map[baseInputsKey]domain.ProjectImageBaseInputs
	// bases lists the commits inputs holds, oldest first.
	bases []string
	// failures holds each key whose last worker fetch failed, until its
	// retry time. Recording a failure drops the expired ones, so it holds
	// only the failures of the last baseFetchRetryBackoff. read keeps no
	// failure: the start fails on it.
	failures map[baseInputsKey]baseFetchFailure

	fetches keyedrun.Group[baseInputsKey]
	now     func() time.Time
	timeout time.Duration
	logger  *slog.Logger
	// ctx is what the worker fetches run under; Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc
}

type baseInputsKey struct {
	baseSHA string
	layout  domain.ProjectImageInputLayout
}

// baseFetchFailure is a worker fetch's error and when the base may be
// fetched again.
type baseFetchFailure struct {
	err     error
	retryAt time.Time
}

func newBaseInputsMemo(
	observe func(context.Context, string, domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error),
	logger *slog.Logger,
) *baseInputsMemo {
	ctx, cancel := context.WithCancel(context.Background())
	return &baseInputsMemo{
		observe: observe, inputs: map[baseInputsKey]domain.ProjectImageBaseInputs{},
		failures: map[baseInputsKey]baseFetchFailure{},
		now:      time.Now, timeout: baseFetchTimeout, logger: logger, ctx: ctx, cancel: cancel,
	}
}

// read returns what baseSHA holds for layout, fetching it under ctx and
// waiting when it is not stored. A failed read is not kept.
func (m *baseInputsMemo) read(
	ctx context.Context, baseSHA string, layout domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error) {
	key := baseInputsKey{baseSHA: baseSHA, layout: layout}
	m.mu.Lock()
	inputs, ok := m.inputs[key]
	m.mu.Unlock()
	if ok {
		return inputs, nil
	}
	inputs, err := m.observe(ctx, baseSHA, layout)
	if err != nil {
		return domain.ProjectImageBaseInputs{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storeLocked(key, inputs)
	return inputs, nil
}

// lookup is the engine pass's read: it never waits on a fetch. It returns
// what baseSHA holds for layout when stored, and the error of a failed fetch
// until that fetch's backoff ends. Otherwise it starts the fetch on a worker,
// unless one for the key is running, and returns engine.ErrBaseInputsPending;
// a later pass finds the result.
func (m *baseInputsMemo) lookup(baseSHA string, layout domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
	key := baseInputsKey{baseSHA: baseSHA, layout: layout}
	m.mu.Lock()
	defer m.mu.Unlock()
	if inputs, ok := m.inputs[key]; ok {
		return inputs, nil
	}
	if failure, ok := m.failures[key]; ok {
		if m.now().Before(failure.retryAt) {
			return domain.ProjectImageBaseInputs{}, failure.err
		}
		delete(m.failures, key)
	}
	// Starting the fetch under the lock means a worker cannot store its
	// result between the checks above and the start, so no key is fetched
	// twice, and no fetch starts once Close has canceled the memo. Go only
	// starts a goroutine, so the lock never spans the fetch.
	if m.ctx.Err() == nil {
		m.fetches.Go(key, func() { m.fetch(key) })
	}
	return domain.ProjectImageBaseInputs{}, engine.ErrBaseInputsPending
}

// fetch is one worker fetch. A fetch Close canceled stores nothing: it says
// nothing of the base.
func (m *baseInputsMemo) fetch(key baseInputsKey) {
	ctx, cancel := context.WithTimeout(m.ctx, m.timeout)
	defer cancel()
	inputs, err := m.observe(ctx, key.baseSHA, key.layout)
	if m.ctx.Err() != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		now := m.now()
		maps.DeleteFunc(m.failures, func(_ baseInputsKey, failure baseFetchFailure) bool {
			return !now.Before(failure.retryAt)
		})
		retryAt := now.Add(baseFetchRetryBackoff)
		m.failures[key] = baseFetchFailure{err: err, retryAt: retryAt}
		m.logger.Warn("fetching a run's base for image selection failed",
			"base", key.baseSHA, "layout", key.layout, "retry_at", retryAt, "error", err)
		return
	}
	delete(m.failures, key)
	m.storeLocked(key, inputs)
}

// storeLocked stores an observation, forgetting the oldest base when a new
// one passes baseInputsMemoBases. The caller holds m.mu.
func (m *baseInputsMemo) storeLocked(key baseInputsKey, inputs domain.ProjectImageBaseInputs) {
	if !slices.Contains(m.bases, key.baseSHA) {
		if len(m.bases) == baseInputsMemoBases {
			oldest := m.bases[0]
			m.bases = m.bases[1:]
			maps.DeleteFunc(m.inputs, func(k baseInputsKey, _ domain.ProjectImageBaseInputs) bool {
				return k.baseSHA == oldest
			})
		}
		m.bases = append(m.bases, key.baseSHA)
	}
	m.inputs[key] = inputs
}

// Close cancels the worker fetches and waits for them, so none outlives the
// transport it fetches through. The daemon closes it while an engine pass
// may still run; a lookup after Close starts no fetch.
func (m *baseInputsMemo) Close(ctx context.Context) error {
	m.mu.Lock()
	m.cancel()
	m.mu.Unlock()
	done := make(chan struct{})
	go func() {
		m.fetches.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop the base fetches: %w", ctx.Err())
	}
}

// proofLedger is the part of ward's backend the follower reads: the images
// with a published proof, and every image ward holds proof state for.
type proofLedger interface {
	ProvenImages() []string
	TrackedImages() []string
}

// followerConfig composes a conformanceFollower.
type followerConfig struct {
	Ledger proofLedger
	// Prove runs the full suite for an image; Retire withdraws an image's
	// proof (ward.Backend.RetireConformance).
	Prove  func(context.Context, domain.ImageRef) error
	Retire func(context.Context, domain.ImageRef) error
	// Start is the image the start proved or would have, which the first
	// scheduled pass keeps when no attempt has selected an image yet.
	Start  domain.ImageRef
	Now    func() time.Time
	Logger *slog.Logger
}

// followerFailure is an image's consecutive failed passes and when it may
// be retried.
type followerFailure struct {
	failures int
	retryAt  time.Time
}

// conformanceFollower proves the images selection picks. It runs at most one
// conformance pass at a time, its own or the doctor's scheduled one, so
// passes never compete for the runtime. It remembers, per image, a pass that
// failed, so a later selection of that image retries it only once the
// backoff has passed.
type conformanceFollower struct {
	cfg followerConfig

	passMu sync.Mutex

	mu        sync.Mutex
	following domain.ImageRef
	// failed holds the images whose last pass failed. Any successful pass
	// of an image clears that image's entry only.
	failed map[domain.ImageRef]followerFailure
	// selected holds the images attempts selected since the last scheduled
	// pass, and latest the most recent of them, or the start's image before
	// any attempt selected one.
	selected map[domain.ImageRef]struct{}
	latest   domain.ImageRef

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newConformanceFollower(cfg followerConfig) *conformanceFollower {
	ctx, cancel := context.WithCancel(context.Background())
	return &conformanceFollower{
		cfg: cfg, failed: map[domain.ImageRef]followerFailure{}, selected: map[domain.ImageRef]struct{}{},
		latest: cfg.Start, ctx: ctx, cancel: cancel,
	}
}

// noteSelected records that an attempt selected image, which keeps image's
// proof through the next scheduled pass.
func (f *conformanceFollower) noteSelected(image domain.ImageRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.selected[image] = struct{}{}
	f.latest = image
}

// errImageNotProved holds an unattended attempt on an image ward holds no
// published proof of. The engine holds only that attempt, so attempts on
// other, proven images still start.
var errImageNotProved = errors.Join(engine.ErrImageNotProved, domain.ErrAdmissionConfigurationMismatch)

// use gates an unattended start on image. Like a selection, it keeps
// image's proof through the next scheduled pass. When image has no
// published proof it asks for one and refuses the start with
// errImageNotProved. A recorded attempt starts under its stored admission
// without selecting its image again, so its start is what asks to prove an
// image whose proof was retired, or not restored, while the attempt waited.
func (f *conformanceFollower) use(image domain.ImageRef) error {
	f.noteSelected(image)
	if slices.Contains(f.cfg.Ledger.ProvenImages(), string(image)) {
		return nil
	}
	f.request(image)
	return fmt.Errorf("image %s has no published conformance proof: %w", image, errImageNotProved)
}

// request starts proving image in the background, unless a pass is already
// following an image or image's last pass failed and its backoff has not
// passed; an attempt still waiting asks again on the next admission pass.
// It never blocks the caller.
func (f *conformanceFollower) request(image domain.ImageRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.following != "" || f.ctx.Err() != nil {
		return
	}
	if !f.retryDueLocked(image) {
		return
	}
	f.following = image
	f.wg.Add(1)
	go f.pass(image)
}

// pass is one follower pass for image. A pass that waited behind a
// scheduled one runs no suite when image is proven by then.
func (f *conformanceFollower) pass(image domain.ImageRef) {
	defer f.wg.Done()
	defer func() {
		f.mu.Lock()
		f.following = ""
		f.mu.Unlock()
	}()
	f.passMu.Lock()
	defer f.passMu.Unlock()
	if slices.Contains(f.cfg.Ledger.ProvenImages(), string(image)) {
		return
	}
	if f.failedFor(image) {
		f.cfg.Logger.Info("retrying conformance for the selected writer image", "image", image)
	} else {
		f.cfg.Logger.Info("running conformance for the selected writer image", "image", image)
	}
	if err := f.cfg.Prove(f.ctx, image); err != nil {
		// A pass Close cancelled did not fail; it says nothing of the image.
		if f.ctx.Err() == nil {
			retryAt := f.recordFailure(image)
			f.cfg.Logger.Warn("conformance for the selected writer image failed",
				"image", image, "retry_at", retryAt, "error", err)
		}
		return
	}
	f.clearFailure(image)
	f.cfg.Logger.Info("conformance recorded for the selected writer image", "image", image)
}

// failedFor reports whether image's last pass failed.
func (f *conformanceFollower) failedFor(image domain.ImageRef) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.failed[image]
	return ok
}

// recordFailure remembers a failed pass on image and returns when it may be
// retried.
func (f *conformanceFollower) recordFailure(image domain.ImageRef) time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	failure := f.failed[image]
	failure.failures++
	failure.retryAt = f.cfg.Now().Add(followerBackoff(failure.failures))
	f.failed[image] = failure
	return failure.retryAt
}

func (f *conformanceFollower) clearFailure(image domain.ImageRef) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.failed, image)
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
// follower's. It keeps the images attempts selected since the previous
// scheduled pass, and always the most recently selected one, or the start's
// image when none was selected yet. It re-proves each kept image with a
// published proof, and the most recent one whether or not it has one unless
// its last pass failed and its backoff has not passed, one pass after
// another, each under its own reservation. It retires every other image
// ward tracks, published or failed, so a proof nobody used stops admitting
// and is proved again only when an attempt next selects or starts on its
// image.
//
// A failed pass closes only its own image's configuration: it is
// remembered like a follower pass's, so its image retries after the
// backoff, and it fails the doctor pass, which stops the daemon, only when
// the round leaves ward no published proof at all. A failed retirement
// always fails it, since the durable marker did not land. A pass whose
// memory the budget refuses for good runs nothing and leaves the standing
// proof as it was, so it is skipped, logged.
func (f *conformanceFollower) runScheduled(ctx context.Context) error {
	f.passMu.Lock()
	defer f.passMu.Unlock()
	f.mu.Lock()
	keep, latest := f.selected, f.latest
	if latest != "" {
		keep[latest] = struct{}{}
	}
	f.selected = map[domain.ImageRef]struct{}{}
	f.mu.Unlock()

	proven := f.cfg.Ledger.ProvenImages()
	reprove := []domain.ImageRef{}
	if latest != "" && (slices.Contains(proven, string(latest)) || f.retryDue(latest)) {
		reprove = append(reprove, latest)
	}
	var errs, failedPasses []error
	for _, tracked := range f.cfg.Ledger.TrackedImages() {
		image := domain.ImageRef(tracked)
		if _, ok := keep[image]; !ok {
			if err := f.cfg.Retire(ctx, image); err != nil {
				errs = append(errs, fmt.Errorf("retire image %s's proof: %w", image, err))
				continue
			}
			f.clearFailure(image)
			f.cfg.Logger.Info("retired the proof of a writer image no attempt selected", "image", image)
			continue
		}
		// Another kept image whose pass failed is retried when an attempt
		// selects it, after its backoff.
		if image != latest && slices.Contains(proven, tracked) {
			reprove = append(reprove, image)
		}
	}
	for _, image := range reprove {
		if err := f.cfg.Prove(ctx, image); err != nil {
			if reservationRefusedForGood(err) {
				f.cfg.Logger.Warn("scheduled conformance pass skipped: the memory budget can never grant it",
					"image", image, "error", err)
				continue
			}
			if ctx.Err() == nil {
				f.recordFailure(image)
			}
			failedPasses = append(failedPasses, fmt.Errorf("re-prove image %s: %w", image, err))
			continue
		}
		f.clearFailure(image)
	}
	if len(failedPasses) > 0 {
		if len(f.cfg.Ledger.ProvenImages()) == 0 {
			errs = append(errs, failedPasses...)
		} else {
			f.cfg.Logger.Warn("scheduled conformance pass failed; its configuration holds until a pass succeeds",
				"error", errors.Join(failedPasses...))
		}
	}
	return errors.Join(errs...)
}

// retryDue reports whether image may be proved again: its last pass did
// not fail, or its backoff has passed.
func (f *conformanceFollower) retryDue(image domain.ImageRef) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.retryDueLocked(image)
}

func (f *conformanceFollower) retryDueLocked(image domain.ImageRef) bool {
	failure, ok := f.failed[image]
	return !ok || !f.cfg.Now().Before(failure.retryAt)
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

// provenImages reads the images ward holds a published proof for.
type provenImages interface {
	ProvenImages() []string
}

// claudeImageSelector selects each writer attempt's image from the pointer
// and the repository's recorded images.
type claudeImageSelector struct {
	store        *store.Store
	repo         string
	repositoryID int64
	backend      provenImages
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
// admission_policy_refused hold when no image serves base, the run's base,
// backend_not_conformant while ward holds no published proof of the
// selected image, input_unavailable when the base could not be read, and
// engine.ErrBaseInputsPending, which records no reason, while the base is
// fetched off the pass.
//
// Selection prefers an image ward has proved (engine.SelectProjectImage).
// Ward publishes an image's proof, the two capabilities a pass earns
// (supports_networkless_export and supports_enforced_provider_egress), only
// once its pass is recorded; a pass that is running or failed leaves them
// cleared. Until then the attempt holds as unproved rather than reaching
// admission's capability check, and the follower proves the image, or
// retries its failed pass after the backoff. Every selected image is noted,
// which keeps its proof through the next scheduled pass.
//
// A maintenance task's run (repair non-nil) skips that choice: it runs on the
// image its refusal was measured from while the pointer still names it, and
// holds as unserved once the pointer has moved on, never in another image.
// It never takes upkeep's hold, which names the task itself.
func (s *claudeImageSelector) selectImage(
	ctx context.Context, _ domain.InvocationID, base domain.BaseRevision, repair *domain.ImageUpkeepRefusal,
) (domain.ImageRef, error) {
	proven := s.backend.ProvenImages()
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
		selected, found, err = selectRecordedImage(ctx, s.store, s.repo, s.repositoryID, base.BaseSHA,
			imageRefs(proven), s.base)
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
	s.follower.noteSelected(selected.ImageRef)
	switch {
	case slices.Contains(proven, string(selected.ImageRef)):
		return selected.ImageRef, nil
	case s.follower.pendingFor(selected.ImageRef):
		return "", fmt.Errorf("image %s waits on its conformance pass: %w", selected.ImageRef, errImageNotProved)
	case s.follower.failedFor(selected.ImageRef):
		s.follower.request(selected.ImageRef)
		return "", fmt.Errorf("image %s has no published conformance proof: %w", selected.ImageRef, errImageNotProved)
	default:
		s.follower.request(selected.ImageRef)
		return "", fmt.Errorf("image %s waits on its conformance pass: %w", selected.ImageRef, errImageNotProved)
	}
}

func imageRefs(images []string) []domain.ImageRef {
	refs := make([]domain.ImageRef, len(images))
	for i, image := range images {
		refs[i] = domain.ImageRef(image)
	}
	return refs
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
// instead of failing the dispatch pass. A base not yet read is fetched off
// the pass (baseInputsMemo.lookup) and is engine.ErrBaseInputsPending.
func selectRecordedImage(
	ctx context.Context, st *store.Store, repo string, repositoryID int64, baseSHA string,
	proved []domain.ImageRef, base *baseInputsMemo,
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
	return SelectAmongRecordedImages(pointed, images, repo, baseSHA, proved,
		func(layout domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
			return base.lookup(baseSHA, layout)
		})
}

// SelectAmongRecordedImages selects among a pointer and record list already
// read. The daemon (selectRecordedImage) and preflight
// (selectPreflightWriterImage) share it so both answer the same question. A
// pointer whose repository disagrees with repo is
// domain.ErrRepositoryIdentityMismatch; a base that cannot be read is
// exec.ErrInputUnavailable. A base still being fetched is
// engine.ErrBaseInputsPending, returned without that join: joined, its
// hold would match input_unavailable first and record that reason.
func SelectAmongRecordedImages(
	pointed domain.ProjectImage, images []domain.ProjectImage, repo, baseSHA string, proved []domain.ImageRef,
	baseInputs func(domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error),
) (domain.ProjectImage, bool, error) {
	if pointed.Repository != repo {
		return domain.ProjectImage{}, false, fmt.Errorf(
			"admitted image repository %q disagrees with configured %q: %w",
			pointed.Repository, repo, domain.ErrRepositoryIdentityMismatch)
	}
	selected, found, err := engine.SelectProjectImage(engine.ProjectImageSelection{
		Proved: proved, Pointed: pointed, Images: images, BaseSHA: baseSHA,
		BaseInputs: baseInputs, Preparation: projectimage.PreparationDigest(),
	})
	if errors.Is(err, engine.ErrBaseInputsPending) {
		return domain.ProjectImage{}, false, fmt.Errorf("select project image: %w", err)
	}
	if err != nil {
		return domain.ProjectImage{}, false, fmt.Errorf("select project image: %w",
			errors.Join(exec.ErrInputUnavailable, err))
	}
	return selected, found, nil
}

// WriterImageCheck is what the writer-image rule reads: the record for the
// configured agent image (the pointer's seed), the pointer, the repository's
// recorded images, the base, and the approved recipe.
type WriterImageCheck struct {
	Seed           domain.ProjectImage
	SeedFound      bool
	AgentImage     domain.ImageRef
	Pointer        domain.ProjectImage
	PointerFound   bool
	Images         []domain.ProjectImage
	Repo           string
	RepositoryID   int64
	BaseSHA        string
	ApprovedRecipe domain.Digest
	BaseInputs     func(domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error)
}

// CheckWriterImage is the writer-image rule the offline preflight and the
// daemon's project readiness check share. It first runs the record-only
// refusals the daemon's start runs on the seed
// (projectimage.ResolvePreparation), which are projectimage.ErrComposition.
// With no pointer recorded the seed stands in for it, as an unattended start
// seeds the pointer with it (seedAdmittedImagePointer). It returns the image
// a run at BaseSHA starts on: the selected image when one serves the base
// (serves true), else the pointed-to image ward starts on while writer
// attempts hold (startClaudeConformance). An image built from a recipe other
// than the approved one is projectimage.ErrComposition. A base no image
// serves is not an error, since attempts already hold on it; a base the rule
// cannot read is exec.ErrInputUnavailable.
//
// Selection runs with no proved image. A restored proof can change which
// serving image the daemon picks, never whether one serves: every image the
// proved-image rule can pick, the pointer or record-order rule can pick too.
func CheckWriterImage(in WriterImageCheck) (image domain.ProjectImage, serves bool, err error) {
	if _, err := projectimage.ResolvePreparation(in.Seed, in.SeedFound, in.AgentImage,
		in.Repo, in.RepositoryID); err != nil {
		return domain.ProjectImage{}, false, err
	}
	pointed := in.Seed
	if in.PointerFound {
		pointed = in.Pointer
	}
	selected, serves, err := SelectAmongRecordedImages(pointed, in.Images, in.Repo, in.BaseSHA, nil, in.BaseInputs)
	if err != nil {
		return domain.ProjectImage{}, false, err
	}
	image = pointed
	if serves {
		image = selected
	}
	if image.RecipeDigest != in.ApprovedRecipe {
		return domain.ProjectImage{}, false, fmt.Errorf("project-image recipe %s disagrees with approved recipe %s: %w",
			image.RecipeDigest, in.ApprovedRecipe, projectimage.ErrComposition)
	}
	return image, serves, nil
}

// proofRestorer is the part of ward's backend a start restores proofs into.
type proofRestorer interface {
	ConfigurationDigestFor(image string) domain.Digest
	RestoreConformance(image string, record domain.BackendConformance) error
}

// restoreRecordedProofs restores ward's proof of each of images whose
// configuration's newest conformance row passed, and returns those images.
// A configuration whose newest row is a failure or a supersession marker,
// including one a scheduled pass retired, is not restored.
func restoreRecordedProofs(
	ctx context.Context, st *store.Store, backend proofRestorer, images []domain.ImageRef,
) ([]domain.ImageRef, error) {
	var restored []domain.ImageRef
	for _, image := range images {
		if slices.Contains(restored, image) {
			continue
		}
		var (
			record domain.BackendConformance
			found  bool
		)
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			record, found, err = tx.LatestConfigurationConformance(ctx,
				domain.BackendFreshVMReadOnlyVolumeHandoff, backend.ConfigurationDigestFor(string(image)))
			return err
		}); err != nil {
			return nil, fmt.Errorf("read image %s's conformance: %w", image, err)
		}
		if !found || record.Outcome != domain.ConformancePassed {
			continue
		}
		if err := backend.RestoreConformance(string(image), record); err != nil {
			return nil, fmt.Errorf("restore image %s's conformance: %w", image, err)
		}
		restored = append(restored, image)
	}
	return restored, nil
}
