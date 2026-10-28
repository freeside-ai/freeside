package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	selectorRepo         = "owner/repo"
	selectorRepositoryID = int64(42)
)

var selectorBase = strings.Repeat("b", 40)

// selectorImage is a project image built at commit that bakes the npm
// manifests whose hashes repeat manifest, under image reference fill.
func selectorImage(t *testing.T, commit, manifest, fill string) domain.ProjectImage {
	t.Helper()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: selectorRepo, RepositoryID: selectorRepositoryID, CommitSHA: commit,
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("1", 64)),
		PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef:       domain.ImageRef("ghcr.io/owner/base@sha256:" + strings.Repeat("c", 64)),
		ImageRef:           domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat(fill, 64)),
		Environment: &domain.ProjectImageEnvironment{
			PackageJSONSHA256: strings.Repeat(manifest, 64), PackageLockSHA256: strings.Repeat(manifest, 64),
			PreparationDigest: projectimage.PreparationDigest(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// selectorStore records images in order and seeds the pointer at the first.
func selectorStore(t *testing.T, images ...domain.ProjectImage) *store.Store {
	t.Helper()
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		for _, image := range images {
			if err := tx.RecordProjectImage(t.Context(), image); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := seedAdmittedImagePointer(t.Context(), st, images[0], time.Now().UTC(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	return st
}

// selectorBaseInputs is a base holding the manifests selectorImage bakes for
// manifest; a non-nil failure is returned instead.
func selectorBaseInputs(manifest string, failure error) *baseInputsMemo {
	return newBaseInputsMemo(func(context.Context, domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
		if failure != nil {
			return domain.ProjectImageBaseInputs{}, failure
		}
		return domain.ProjectImageBaseInputs{
			CommitSHA:         selectorBase,
			PackageJSONSHA256: strings.Repeat(manifest, 64), PackageLockSHA256: strings.Repeat(manifest, 64),
		}, nil
	})
}

// fakeWard proves one writer image at a time, as ward's backend does:
// repointing at another image clears its published proof.
type fakeWard struct {
	mu       sync.Mutex
	image    string
	proved   bool
	repoints []string
}

func (w *fakeWard) RepointAgentImage(image string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if image != w.image {
		w.image = image
		w.proved = false
		w.repoints = append(w.repoints, image)
	}
	return nil
}

// Capabilities declares the suite-earned capabilities only while a proof is
// published, as ward's backend does.
func (w *fakeWard) Capabilities() exec.CapabilitySet {
	w.mu.Lock()
	defer w.mu.Unlock()
	caps := exec.NewCapabilitySet(exec.CapDetachableWorkspace, exec.CapPostExitExport, exec.CapReadOnlyRemount)
	if w.proved {
		caps[exec.CapNetworklessExport] = struct{}{}
		caps[exec.CapEnforcedProviderEgress] = struct{}{}
	}
	return caps
}

func (w *fakeWard) publish(proved bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.proved = proved
}

func (w *fakeWard) ConfigurationDigestFor(image string) domain.Digest {
	sum := sha256.Sum256([]byte(image))
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func (w *fakeWard) ConformanceIdentity() (string, domain.Digest) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.image, w.ConfigurationDigestFor(w.image)
}

func (w *fakeWard) repointed() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.repoints...)
}

// fakeClock is a follower's clock that moves only when the test advances it.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fakeSuite is a conformance suite whose passes the test ends one at a time.
// A pass clears ward's published proof, as ward does when a pass begins, and
// publishes it when the test ends the pass with a nil result. Once results is
// closed, a pass the test did not expect fails at once instead of hanging.
type fakeSuite struct {
	ward    *fakeWard
	started chan struct{}
	results chan error

	mu     sync.Mutex
	passes int
}

func newFakeSuite(ward *fakeWard) *fakeSuite {
	return &fakeSuite{ward: ward, started: make(chan struct{}, 1), results: make(chan error)}
}

func (s *fakeSuite) prove(ctx context.Context) error {
	s.mu.Lock()
	s.passes++
	s.mu.Unlock()
	s.ward.publish(false)
	s.started <- struct{}{}
	select {
	case err, ok := <-s.results:
		if !ok {
			return errors.New("a pass the test did not expect")
		}
		s.ward.publish(err == nil)
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// end waits for a pass to start and ends it with err.
func (s *fakeSuite) end(err error) {
	<-s.started
	s.results <- err
}

func (s *fakeSuite) ran() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.passes
}

func unproved(err error) bool {
	return errors.Is(err, engine.ErrImageNotProved) && errors.Is(err, domain.ErrAdmissionConfigurationMismatch)
}

func TestStartWardOnSelectedImage(t *testing.T) {
	t.Parallel()
	// pointed bakes manifest 2; rebuilt keeps what it binds and bakes 5 at
	// the base; stale is an earlier record that serves neither base.
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	rebuilt := selectorImage(t, selectorBase, "5", "e")
	stale := selectorImage(t, strings.Repeat("c", 40), "9", "f")
	ward := &fakeWard{}
	passed := func(image domain.ProjectImage) domain.BackendConformance {
		return domain.BackendConformance{
			Outcome: domain.ConformancePassed, ConfigurationDigest: ward.ConfigurationDigestFor(string(image.ImageRef)),
		}
	}
	for name, tc := range map[string]struct {
		images         []domain.ProjectImage
		manifest       string
		conformance    *domain.BackendConformance
		runConformance bool
		want           domain.ImageRef
	}{
		// The start does not refuse: attempts hold until upkeep acts.
		"an unserved base starts on the pointed-to image": {
			images: []domain.ProjectImage{pointed, stale}, manifest: "5", runConformance: true, want: pointed.ImageRef,
		},
		"a recorded image that serves the base": {
			images: []domain.ProjectImage{pointed, rebuilt}, manifest: "5", runConformance: true, want: rebuilt.ImageRef,
		},
		// A restart that does not run the suite restores the image the
		// latest record proves; the first attempt then moves ward.
		"a restoring start keeps the proved image": {
			images: []domain.ProjectImage{pointed, stale}, manifest: "2",
			conformance: new(passed(stale)), want: stale.ImageRef,
		},
		"a start that runs the suite takes the selected image": {
			images: []domain.ProjectImage{pointed, stale}, manifest: "2",
			conformance: new(passed(stale)), runConformance: true, want: pointed.ImageRef,
		},
		"a failed record proves nothing": {
			images: []domain.ProjectImage{pointed, stale}, manifest: "2",
			conformance: &domain.BackendConformance{
				Outcome: domain.ConformanceFailed, ConfigurationDigest: passed(stale).ConfigurationDigest,
			},
			want: pointed.ImageRef,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := selectorStore(t, tc.images...)
			backend := &fakeWard{}
			var conformance domain.BackendConformance
			if tc.conformance != nil {
				conformance = *tc.conformance
			}
			if err := startWardOnSelectedImage(t.Context(), st, backend, claudeDriverConfig{
				Repo: selectorRepo, RepositoryID: selectorRepositoryID, BaseSHA: selectorBase,
				RunConformance: tc.runConformance,
			}, selectorBaseInputs(tc.manifest, nil), tc.images[0].ImageRef, conformance, tc.conformance != nil,
				slog.New(slog.DiscardHandler)); err != nil {
				t.Fatalf("start = %v, want it to succeed", err)
			}
			if got, _ := backend.ConformanceIdentity(); got != string(tc.want) {
				t.Fatalf("ward starts on %s, want %s", got, tc.want)
			}
		})
	}

	t.Run("a base the start cannot read fails it", func(t *testing.T) {
		t.Parallel()
		st := selectorStore(t, pointed)
		failure := errors.New("fetch failed")
		err := startWardOnSelectedImage(t.Context(), st, &fakeWard{}, claudeDriverConfig{
			Repo: selectorRepo, RepositoryID: selectorRepositoryID, BaseSHA: selectorBase, RunConformance: true,
		}, selectorBaseInputs("2", failure), pointed.ImageRef, domain.BackendConformance{}, false,
			slog.New(slog.DiscardHandler))
		if !errors.Is(err, failure) || !errors.Is(err, exec.ErrInputUnavailable) {
			t.Fatalf("start = %v, want the read failure as input_unavailable", err)
		}
	})
}

// A start with an unserved base succeeds, and its attempts hold on what image
// upkeep has pending: a rebuild, or nothing, which holds unserved.
func TestClaudeImageSelectorHoldsAnUnservedBase(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	st := selectorStore(t, pointed)
	backend := &fakeWard{}
	base := selectorBaseInputs("5", nil)
	// The start itself repoints ward from no image to the pointed-to one;
	// only later repoints would follow an attempt.
	if err := startWardOnSelectedImage(t.Context(), st, backend, claudeDriverConfig{
		Repo: selectorRepo, RepositoryID: selectorRepositoryID, BaseSHA: selectorBase, RunConformance: true,
	}, base, pointed.ImageRef, domain.BackendConformance{}, false, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("start = %v, want an unserved base to start", err)
	}
	follower := newConformanceFollower(backend, func(context.Context) error {
		t.Error("an unserved base ran a conformance pass")
		return nil
	}, time.Now, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = follower.Close(context.Background()) })
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID, baseSHA: selectorBase,
		backend: backend, base: base, follower: follower,
	}

	if _, err := selector.selectImage(t.Context(), "", nil); !errors.Is(err, engine.ErrProjectImageUnserved) {
		t.Fatalf("select before upkeep is bound = %v, want unserved", err)
	}
	var pending *domain.ImageUpkeepHold
	lookupErr := error(nil)
	selector.bindImageUpkeep(func(context.Context) (*domain.ImageUpkeepHold, error) { return pending, lookupErr })

	pending = &domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingRebuild}
	var upkeep *engine.ImageUpkeepPendingError
	if _, err := selector.selectImage(t.Context(), "", nil); !errors.As(err, &upkeep) ||
		upkeep.Hold.Pending != domain.ImageUpkeepPendingRebuild {
		t.Fatalf("select with a rebuild pending = %v, want an image_upkeep_pending rebuild", err)
	}
	pending = nil
	if _, err := selector.selectImage(t.Context(), "", nil); !errors.Is(err, engine.ErrProjectImageUnserved) {
		t.Fatalf("select with nothing pending = %v, want unserved", err)
	}
	lookupErr = errors.New("tip read failed")
	if _, err := selector.selectImage(t.Context(), "", nil); !errors.Is(err, lookupErr) ||
		errors.Is(err, engine.ErrProjectImageUnserved) || errors.Is(err, engine.ErrImageUpkeepPending) {
		t.Fatalf("select with a failed lookup = %v, want the failure itself", err)
	}
	if repoints := backend.repointed(); len(repoints) != 1 || repoints[0] != string(pointed.ImageRef) {
		t.Fatalf("ward was repointed at %v, want only the start's repoint to the pointed-to image", repoints)
	}
}

// When selection picks an image ward does not prove, ward is repointed and
// proved once, attempts hold as unproved until that pass ends, and the next
// attempt is admitted with the selected image.
func TestClaudeImageSelectorFollowsTheSelectedImage(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	stale := selectorImage(t, strings.Repeat("c", 40), "9", "f")
	st := selectorStore(t, pointed, stale)
	backend := &fakeWard{image: string(stale.ImageRef)}
	started, release := make(chan struct{}), make(chan struct{})
	proves := 0
	follower := newConformanceFollower(backend, func(context.Context) error {
		proves++
		close(started)
		<-release
		backend.publish(true)
		return nil
	}, time.Now, slog.New(slog.DiscardHandler))
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID, baseSHA: selectorBase,
		backend: backend, base: selectorBaseInputs("2", nil), follower: follower,
	}

	if _, err := selector.selectImage(t.Context(), "", nil); !unproved(err) {
		t.Fatalf("select under the stale proof = %v, want the image held as unproved", err)
	}
	<-started
	// Ward already names the selected image, but its pass has not ended.
	if _, err := selector.selectImage(t.Context(), "", nil); !unproved(err) {
		t.Fatalf("select during the pass = %v, want the image held as unproved", err)
	}
	if follower.passMu.TryLock() {
		t.Fatal("a scheduled pass could start beside the follower's")
	}
	close(release)
	if err := follower.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := selector.selectImage(t.Context(), "", nil)
	if err != nil || got != pointed.ImageRef {
		t.Fatalf("select after the pass = (%s, %v), want %s", got, err, pointed.ImageRef)
	}
	if repoints := backend.repointed(); proves != 1 || len(repoints) != 1 || repoints[0] != string(pointed.ImageRef) {
		t.Fatalf("ward repointed %v with %d passes, want one repoint and one pass", repoints, proves)
	}
}

// A maintenance task's run takes the image its refusal was measured from
// while the pointer names it, even with no image serving the daemon's base
// and upkeep holding other attempts on that task; it follows ward to that
// image like any selection, and holds unserved once the pointer moves.
func TestClaudeImageSelectorRunsARepairOnItsRefusalsImage(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	other := selectorImage(t, strings.Repeat("c", 40), "9", "f")
	st := selectorStore(t, pointed, other)
	backend := &fakeWard{image: string(other.ImageRef)}
	passed := make(chan struct{})
	follower := newConformanceFollower(backend, func(context.Context) error {
		backend.publish(true)
		close(passed)
		return nil
	}, time.Now, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = follower.Close(context.Background()) })
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID, baseSHA: selectorBase,
		backend: backend, base: selectorBaseInputs("5", nil), follower: follower,
	}
	task := domain.TaskID("task-repair")
	selector.bindImageUpkeep(func(context.Context) (*domain.ImageUpkeepHold, error) {
		return &domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingMaintenanceTask, MaintenanceTaskID: &task}, nil
	})
	if _, err := selector.selectImage(t.Context(), "", nil); !errors.Is(err, engine.ErrImageUpkeepPending) {
		t.Fatalf("an ordinary attempt = %v, want it held on the maintenance task", err)
	}
	refusal := &domain.ImageUpkeepRefusal{ID: domain.Digest("sha256:" + strings.Repeat("7", 64)), MeasuredFromImageID: pointed.ID}

	if _, err := selector.selectImage(t.Context(), "", refusal); !errors.Is(err, engine.ErrImageNotProved) {
		t.Fatalf("repair under another image's proof = %v, want it held as unproved", err)
	}
	<-passed
	if err := follower.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := selector.selectImage(t.Context(), "", refusal)
	if err != nil || got != pointed.ImageRef {
		t.Fatalf("repair = (%s, %v), want the pointed-to image %s", got, err, pointed.ImageRef)
	}

	moved := *refusal
	moved.MeasuredFromImageID = other.ID
	if _, err := selector.selectImage(t.Context(), "", &moved); !errors.Is(err, engine.ErrProjectImageUnserved) ||
		errors.Is(err, engine.ErrImageUpkeepPending) {
		t.Fatalf("repair after the pointer moved = %v, want unserved", err)
	}
}

// After a failed follower pass, ward names the selected image with no proof
// published: attempts hold as unproved, and once the backoff has passed the
// next one retries the pass, after which the image is admitted.
func TestClaudeImageSelectorRetriesAFailedPass(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	stale := selectorImage(t, strings.Repeat("c", 40), "9", "f")
	st := selectorStore(t, pointed, stale)
	backend := &fakeWard{image: string(stale.ImageRef), proved: true}
	clock := newFakeClock()
	suite := newFakeSuite(backend)
	follower := newConformanceFollower(backend, suite.prove, clock.Now, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = follower.Close(context.Background()) })
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID, baseSHA: selectorBase,
		backend: backend, base: selectorBaseInputs("2", nil), follower: follower,
	}

	if _, err := selector.selectImage(t.Context(), "", nil); !unproved(err) {
		t.Fatalf("select under the stale proof = %v, want the image held as unproved", err)
	}
	suite.end(errors.New("the VM did not start"))
	follower.wg.Wait()

	clock.advance(followerRetryBackoff - time.Nanosecond)
	if _, err := selector.selectImage(t.Context(), "", nil); !unproved(err) ||
		!strings.Contains(err.Error(), "no published conformance proof") {
		t.Fatalf("select after the failed pass = %v, want the image held as having no published proof", err)
	}
	if follower.pendingFor(pointed.ImageRef) {
		t.Fatal("the failed pass was retried before its backoff")
	}
	clock.advance(time.Nanosecond)
	if _, err := selector.selectImage(t.Context(), "", nil); !unproved(err) {
		t.Fatalf("select after the backoff = %v, want the image held as unproved", err)
	}
	if !follower.pendingFor(pointed.ImageRef) {
		t.Fatal("the failed pass was not retried after its backoff")
	}
	suite.end(nil)
	follower.wg.Wait()

	got, err := selector.selectImage(t.Context(), "", nil)
	if err != nil || got != pointed.ImageRef {
		t.Fatalf("select after the retry = (%s, %v), want %s", got, err, pointed.ImageRef)
	}
	if repoints, passes := backend.repointed(), suite.ran(); passes != 2 || len(repoints) != 1 {
		t.Fatalf("ward repointed %v with %d passes, want one repoint and two passes", repoints, passes)
	}
}

// The wait before a retry starts at followerRetryBackoff, doubles for each
// consecutive failed pass up to followerRetryBackoffCap, and resets after a
// success; only the image the last pass failed on is retried.
func TestConformanceFollowerBacksOffFailedPasses(t *testing.T) {
	t.Parallel()
	image := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("d", 64))
	other := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("e", 64))
	backend := &fakeWard{}
	clock := newFakeClock()
	suite := newFakeSuite(backend)
	follower := newConformanceFollower(backend, suite.prove, clock.Now, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = follower.Close(context.Background()) })
	failure := errors.New("the VM did not start")

	follower.follow(image)
	suite.end(failure)
	follower.wg.Wait()
	for i, wait := range []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour,
	} {
		clock.advance(wait - time.Nanosecond)
		follower.retry(image)
		if follower.pendingFor(image) {
			t.Fatalf("after %d failures a retry started before %s", i+1, wait)
		}
		clock.advance(time.Nanosecond)
		follower.retry(image)
		if !follower.pendingFor(image) {
			t.Fatalf("after %d failures no retry started at %s", i+1, wait)
		}
		suite.end(failure)
		follower.wg.Wait()
	}

	clock.advance(time.Hour)
	follower.retry(image)
	suite.end(nil)
	follower.wg.Wait()
	clock.advance(24 * time.Hour)
	follower.retry(image)
	if follower.pendingFor(image) {
		t.Fatal("a proved image was retried")
	}

	follower.follow(other)
	suite.end(failure)
	follower.wg.Wait()
	clock.advance(followerRetryBackoff)
	follower.retry(image)
	if follower.pendingFor(image) {
		t.Fatal("an image the last pass did not fail on was retried")
	}
	follower.retry(other)
	if !follower.pendingFor(other) {
		t.Fatalf("no retry started %s after a failure that followed a success", followerRetryBackoff)
	}
	suite.end(nil)
	follower.wg.Wait()
}

// A retry that queued behind the doctor's scheduled pass runs no suite when
// that pass proved ward's image.
func TestConformanceFollowerRetrySkipsAfterAScheduledProof(t *testing.T) {
	t.Parallel()
	image := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("d", 64))
	backend := &fakeWard{}
	clock := newFakeClock()
	suite := newFakeSuite(backend)
	follower := newConformanceFollower(backend, suite.prove, clock.Now, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = follower.Close(context.Background()) })

	follower.follow(image)
	suite.end(errors.New("the VM did not start"))
	follower.wg.Wait()
	clock.advance(followerRetryBackoff)

	scheduled := make(chan error, 1)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	<-suite.started
	follower.retry(image)
	if !follower.pendingFor(image) {
		t.Fatal("the retry did not queue behind the scheduled pass")
	}
	suite.results <- nil
	close(suite.results)
	if err := <-scheduled; err != nil {
		t.Fatalf("scheduled pass = %v, want it to succeed", err)
	}
	follower.wg.Wait()
	if passes := suite.ran(); passes != 2 {
		t.Fatalf("%d passes ran, want the failed one and the scheduled one only", passes)
	}
}

// A failed scheduled pass leaves ward with no published proof: attempts hold
// as unproved until the next scheduled pass, and no follower pass starts.
func TestClaudeImageSelectorHoldsAfterAFailedScheduledPass(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	st := selectorStore(t, pointed)
	backend := &fakeWard{image: string(pointed.ImageRef), proved: true}
	clock := newFakeClock()
	suite := newFakeSuite(backend)
	follower := newConformanceFollower(backend, suite.prove, clock.Now, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = follower.Close(context.Background()) })
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID, baseSHA: selectorBase,
		backend: backend, base: selectorBaseInputs("2", nil), follower: follower,
	}

	if got, err := selector.selectImage(t.Context(), "", nil); err != nil || got != pointed.ImageRef {
		t.Fatalf("select under the proof = (%s, %v), want %s", got, err, pointed.ImageRef)
	}
	scheduled := make(chan error, 1)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	suite.end(errors.New("the egress probe failed"))
	if err := <-scheduled; err == nil {
		t.Fatal("scheduled pass succeeded, want it to fail")
	}
	for _, wait := range []time.Duration{0, 24 * time.Hour} {
		clock.advance(wait)
		if _, err := selector.selectImage(t.Context(), "", nil); !unproved(err) {
			t.Fatalf("select %s after the failed scheduled pass = %v, want the image held as unproved", wait, err)
		}
		if follower.pendingFor(pointed.ImageRef) {
			t.Fatalf("a follower pass started %s after the failed scheduled pass", wait)
		}
	}
	if passes := suite.ran(); passes != 1 {
		t.Fatalf("%d passes ran, want the scheduled one only", passes)
	}
}

// Close cancels a pass in flight and waits for it; the cancelled pass is not
// a failure, and a closed follower starts nothing.
func TestConformanceFollowerStopsOnClose(t *testing.T) {
	t.Parallel()
	backend := &fakeWard{}
	started := make(chan struct{})
	follower := newConformanceFollower(backend, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, time.Now, slog.New(slog.DiscardHandler))
	follower.follow("ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("d", 64)))
	<-started
	if err := follower.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v, want the pass cancelled and awaited", err)
	}
	if follower.stillFailed("ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("d", 64))) {
		t.Fatal("the cancelled pass was remembered as a failure")
	}
	follower.follow("ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("e", 64)))
	if repoints := backend.repointed(); len(repoints) != 1 {
		t.Fatalf("ward repointed %v, want nothing after Close", repoints)
	}
	if follower.pendingFor("ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("e", 64))) {
		t.Fatal("a closed follower reports a pass")
	}
}
