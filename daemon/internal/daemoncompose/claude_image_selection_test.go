package daemoncompose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	selectorRepo         = "owner/repo"
	selectorRepositoryID = int64(42)
)

var selectorBase = strings.Repeat("b", 40)

// selectorRevision is the run base the selector tests admit at.
var selectorRevision = domain.BaseRevision{
	Repo: selectorRepo, RepositoryID: selectorRepositoryID, BaseRef: "main", BaseSHA: selectorBase,
}

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
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
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
	return newBaseInputsMemo(func(_ context.Context, baseSHA string, _ domain.ProjectImageInputLayout) (domain.ProjectImageBaseInputs, error) {
		if failure != nil {
			return domain.ProjectImageBaseInputs{}, failure
		}
		return domain.ProjectImageBaseInputs{
			CommitSHA:         baseSHA,
			PackageJSONSHA256: strings.Repeat(manifest, 64), PackageLockSHA256: strings.Repeat(manifest, 64),
		}, nil
	}, slog.New(slog.DiscardHandler))
}

// selectAfterFetch is selectImage as the engine's passes see it once the
// base is fetched: a selection the memo answers as pending waits for its
// fetch and selects again, once per layout the selection reads.
func selectAfterFetch(
	t *testing.T, selector *claudeImageSelector, invocationID domain.InvocationID, base domain.BaseRevision,
	repair *domain.ImageUpkeepRefusal,
) (domain.ImageRef, error) {
	t.Helper()
	for range 4 {
		image, err := selector.selectImage(t.Context(), invocationID, base, repair)
		if !errors.Is(err, engine.ErrBaseInputsPending) {
			return image, err
		}
		selector.base.fetches.Wait()
	}
	t.Fatal("selection still pending after its fetches finished")
	return "", nil
}

// fakeWard holds a proof per writer image, as ward's backend does: proving
// or retiring one image leaves every other image's proof as it was. tracked
// maps each image ward holds proof state for to whether its proof is
// published.
type fakeWard struct {
	mu      sync.Mutex
	tracked map[string]bool
	retired []domain.ImageRef
}

// newFakeWard publishes a proof for each of proven.
func newFakeWard(proven ...domain.ImageRef) *fakeWard {
	w := &fakeWard{tracked: map[string]bool{}}
	for _, image := range proven {
		w.tracked[string(image)] = true
	}
	return w
}

func (w *fakeWard) publish(image domain.ImageRef, proved bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.tracked[string(image)] = proved
}

func (w *fakeWard) ProvenImages() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var images []string
	for image, published := range w.tracked {
		if published {
			images = append(images, image)
		}
	}
	slices.Sort(images)
	return images
}

func (w *fakeWard) TrackedImages() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Sorted(maps.Keys(w.tracked))
}

func (w *fakeWard) ConfigurationDigestFor(image string) domain.Digest {
	sum := sha256.Sum256([]byte(image))
	return domain.Digest("sha256:" + hex.EncodeToString(sum[:]))
}

func (w *fakeWard) RestoreConformance(image string, _ domain.BackendConformance) error {
	w.publish(domain.ImageRef(image), true)
	return nil
}

// retire is ward's RetireConformance: ward stops tracking image.
func (w *fakeWard) retire(_ context.Context, image domain.ImageRef) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.tracked, string(image))
	w.retired = append(w.retired, image)
	return nil
}

func (w *fakeWard) retiredImages() []domain.ImageRef {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.retired)
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
// A pass clears its image's published proof, as ward does when a pass
// begins, and publishes it when the test ends the pass with a nil result.
// Once results is closed, a pass the test did not expect fails at once
// instead of hanging.
type fakeSuite struct {
	ward    *fakeWard
	started chan domain.ImageRef
	results chan error

	mu     sync.Mutex
	proved []domain.ImageRef
}

func newFakeSuite(ward *fakeWard) *fakeSuite {
	return &fakeSuite{ward: ward, started: make(chan domain.ImageRef, 1), results: make(chan error)}
}

func (s *fakeSuite) prove(ctx context.Context, image domain.ImageRef) error {
	s.mu.Lock()
	s.proved = append(s.proved, image)
	s.mu.Unlock()
	s.ward.publish(image, false)
	s.started <- image
	select {
	case err, ok := <-s.results:
		if !ok {
			return errors.New("a pass the test did not expect")
		}
		s.ward.publish(image, err == nil)
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// end waits for a pass to start, ends it with err, and returns its image.
func (s *fakeSuite) end(err error) domain.ImageRef {
	image := <-s.started
	s.results <- err
	return image
}

// passes lists the images the suite's passes proved, in order.
func (s *fakeSuite) passes() []domain.ImageRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.proved)
}

// testFollower composes a follower over ward, closed at cleanup.
func testFollower(
	t *testing.T, ward *fakeWard, prove func(context.Context, domain.ImageRef) error, now func() time.Time,
	start domain.ImageRef,
) *conformanceFollower {
	t.Helper()
	follower := newConformanceFollower(followerConfig{
		Ledger: ward, Prove: prove, Retire: ward.retire, Start: start, Now: now,
		Logger: slog.New(slog.DiscardHandler),
	})
	t.Cleanup(func() { _ = follower.Close(context.Background()) })
	return follower
}

// refuseProof is a prove function for a test in which no pass may run.
func refuseProof(t *testing.T) func(context.Context, domain.ImageRef) error {
	return func(_ context.Context, image domain.ImageRef) error {
		t.Errorf("a conformance pass ran for %s", image)
		return errors.New("unexpected pass")
	}
}

func unproved(err error) bool {
	return errors.Is(err, engine.ErrImageNotProved) && errors.Is(err, domain.ErrAdmissionConfigurationMismatch)
}

// recordConformance appends a conformance row with outcome for image's
// configuration on ward.
func recordConformance(t *testing.T, st *store.Store, ward *fakeWard, image domain.ImageRef, outcome domain.ConformanceOutcome) {
	t.Helper()
	in := domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: outcome,
		ConfigurationDigest: ward.ConfigurationDigestFor(string(image)), ProvedAt: time.Now().UTC(),
	}
	if outcome == domain.ConformancePassed {
		in.Capabilities = domain.NewCapabilitySnapshot(
			domain.CapDetachableWorkspace, domain.CapPostExitExport, domain.CapReadOnlyRemount,
			domain.CapNetworklessExport, domain.CapEnforcedProviderEgress)
	}
	record, err := domain.NewBackendConformance(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestStartClaudeConformance covers the restore-all start: ward restores
// every image whose configuration's newest row passed, the start's image is
// the one selection picks among them, and only -run-conformance proves it.
func TestStartClaudeConformance(t *testing.T) {
	t.Parallel()
	// pointed bakes manifest 2; rebuilt keeps what it binds and bakes 5 at
	// the base; stale is an earlier record that serves neither base.
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	rebuilt := selectorImage(t, selectorBase, "5", "e")
	stale := selectorImage(t, strings.Repeat("c", 40), "9", "f")
	type row struct {
		image   domain.ImageRef
		outcome domain.ConformanceOutcome
	}
	for name, tc := range map[string]struct {
		images         []domain.ProjectImage
		manifest       string
		rows           []row
		runConformance bool
		wantStart      domain.ImageRef
		wantProven     []domain.ImageRef
		wantErr        error
	}{
		// The start does not refuse: attempts hold until upkeep acts.
		"an unserved base starts on the pointed-to image": {
			images: []domain.ProjectImage{pointed, stale}, manifest: "5", runConformance: true,
			wantStart: pointed.ImageRef, wantProven: []domain.ImageRef{pointed.ImageRef},
		},
		"a recorded image that serves the base": {
			images: []domain.ProjectImage{pointed, rebuilt}, manifest: "5", runConformance: true,
			wantStart: rebuilt.ImageRef, wantProven: []domain.ImageRef{rebuilt.ImageRef},
		},
		// A restart that does not run the suite restores every image whose
		// configuration last passed, the one the tip selects or not.
		"a restoring start restores every passed configuration": {
			images: []domain.ProjectImage{pointed, stale}, manifest: "2",
			rows:      []row{{stale.ImageRef, domain.ConformancePassed}, {pointed.ImageRef, domain.ConformancePassed}},
			wantStart: pointed.ImageRef, wantProven: []domain.ImageRef{pointed.ImageRef, stale.ImageRef},
		},
		"a start that runs the suite proves the selected image beside the restored ones": {
			images: []domain.ProjectImage{pointed, stale}, manifest: "2", runConformance: true,
			rows:      []row{{stale.ImageRef, domain.ConformancePassed}},
			wantStart: pointed.ImageRef, wantProven: []domain.ImageRef{pointed.ImageRef, stale.ImageRef},
		},
		// A retired configuration's newest row is its supersession marker.
		"a failed or retired configuration is not restored": {
			images: []domain.ProjectImage{pointed, stale}, manifest: "2",
			rows: []row{
				{pointed.ImageRef, domain.ConformancePassed},
				{pointed.ImageRef, domain.ConformanceFailed},
				{stale.ImageRef, domain.ConformancePassed},
				{stale.ImageRef, domain.ConformanceSuperseded},
			},
			wantErr: errBackendConformanceUnavailable,
		},
		"an unattended start with no proof fails": {
			images: []domain.ProjectImage{pointed}, manifest: "2", wantErr: errBackendConformanceUnavailable,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := selectorStore(t, tc.images...)
			ward := newFakeWard()
			for _, r := range tc.rows {
				recordConformance(t, st, ward, r.image, r.outcome)
			}
			var proved []domain.ImageRef
			start, err := startClaudeConformance(t.Context(), st, ward, ClaudeDriverConfig{
				Repo: selectorRepo, RepositoryID: selectorRepositoryID, RunConformance: tc.runConformance,
				OperatingMode: domain.ModeUnattended,
			}, selectorBase, selectorBaseInputs(tc.manifest, nil), tc.images[0].ImageRef,
				func(_ context.Context, image domain.ImageRef) error {
					proved = append(proved, image)
					ward.publish(image, true)
					return nil
				}, slog.New(slog.DiscardHandler))
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("start = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("start = %v, want it to succeed", err)
			}
			if start != tc.wantStart {
				t.Fatalf("start image = %s, want %s", start, tc.wantStart)
			}
			if got := imageRefs(ward.ProvenImages()); !slices.Equal(got, tc.wantProven) {
				t.Fatalf("proven images = %v, want %v", got, tc.wantProven)
			}
			if wantProved := tc.runConformance; wantProved != (len(proved) == 1 && proved[0] == tc.wantStart) {
				t.Fatalf("start proved %v, want %v proved only with -run-conformance", proved, tc.wantStart)
			}
		})
	}

	t.Run("a base the start cannot read fails it", func(t *testing.T) {
		t.Parallel()
		st := selectorStore(t, pointed)
		failure := errors.New("fetch failed")
		_, err := startClaudeConformance(t.Context(), st, newFakeWard(), ClaudeDriverConfig{
			Repo: selectorRepo, RepositoryID: selectorRepositoryID, RunConformance: true,
			OperatingMode: domain.ModeUnattended,
		}, selectorBase, selectorBaseInputs("2", failure), pointed.ImageRef, refuseProof(t),
			slog.New(slog.DiscardHandler))
		if !errors.Is(err, failure) || !errors.Is(err, exec.ErrInputUnavailable) {
			t.Fatalf("start = %v, want the read failure as input_unavailable", err)
		}
	})

	t.Run("an attended start restores only its configured image", func(t *testing.T) {
		t.Parallel()
		st := selectorStore(t, pointed, stale)
		ward := newFakeWard()
		recordConformance(t, st, ward, stale.ImageRef, domain.ConformancePassed)
		cfg := ClaudeDriverConfig{
			Repo: selectorRepo, RepositoryID: selectorRepositoryID, OperatingMode: domain.ModeAttendedDev,
			AgentImage: pointed.ImageRef,
		}
		start, err := startClaudeConformance(t.Context(), st, ward, cfg, selectorBase,
			selectorBaseInputs("2", nil), pointed.ImageRef, refuseProof(t), slog.New(slog.DiscardHandler))
		if err != nil || start != pointed.ImageRef || len(ward.ProvenImages()) != 0 {
			t.Fatalf("attended start = (%s, %v) with proofs %v, want the configured image and no proof",
				start, err, ward.ProvenImages())
		}
		recordConformance(t, st, ward, pointed.ImageRef, domain.ConformancePassed)
		if _, err := startClaudeConformance(t.Context(), st, ward, cfg, selectorBase,
			selectorBaseInputs("2", nil), pointed.ImageRef, refuseProof(t), slog.New(slog.DiscardHandler)); err != nil {
			t.Fatal(err)
		}
		if got := imageRefs(ward.ProvenImages()); !slices.Equal(got, []domain.ImageRef{pointed.ImageRef}) {
			t.Fatalf("attended proofs = %v, want only the configured image's", got)
		}
	})
}

// A start with an unserved base succeeds, and its attempts hold on what image
// upkeep has pending: a rebuild, or nothing, which holds unserved. No pass
// runs for an unserved base.
func TestClaudeImageSelectorHoldsAnUnservedBase(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	st := selectorStore(t, pointed)
	ward := newFakeWard(pointed.ImageRef)
	follower := testFollower(t, ward, refuseProof(t), time.Now, pointed.ImageRef)
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID,
		backend: ward, base: selectorBaseInputs("5", nil), follower: follower,
	}

	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !errors.Is(err, engine.ErrProjectImageUnserved) {
		t.Fatalf("select before upkeep is bound = %v, want unserved", err)
	}
	var pending *domain.ImageUpkeepHold
	lookupErr := error(nil)
	selector.bindImageUpkeep(func(context.Context) (*domain.ImageUpkeepHold, error) { return pending, lookupErr })

	pending = &domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingRebuild}
	var upkeep *engine.ImageUpkeepPendingError
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !errors.As(err, &upkeep) ||
		upkeep.Hold.Pending != domain.ImageUpkeepPendingRebuild {
		t.Fatalf("select with a rebuild pending = %v, want an image_upkeep_pending rebuild", err)
	}
	pending = nil
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !errors.Is(err, engine.ErrProjectImageUnserved) {
		t.Fatalf("select with nothing pending = %v, want unserved", err)
	}
	lookupErr = errors.New("tip read failed")
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !errors.Is(err, lookupErr) ||
		errors.Is(err, engine.ErrProjectImageUnserved) || errors.Is(err, engine.ErrImageUpkeepPending) {
		t.Fatalf("select with a failed lookup = %v, want the failure itself", err)
	}
}

// When selection picks an image ward has not proved, the follower proves it
// once, attempts hold as unproved until that pass ends, and the next attempt
// is admitted with the selected image. The other image's proof stands
// throughout.
func TestClaudeImageSelectorProvesTheSelectedImage(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	stale := selectorImage(t, strings.Repeat("c", 40), "9", "f")
	st := selectorStore(t, pointed, stale)
	ward := newFakeWard(stale.ImageRef)
	suite := newFakeSuite(ward)
	follower := testFollower(t, ward, suite.prove, time.Now, stale.ImageRef)
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID,
		backend: ward, base: selectorBaseInputs("2", nil), follower: follower,
	}

	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !unproved(err) {
		t.Fatalf("select with only another image proved = %v, want the image held as unproved", err)
	}
	image := <-suite.started
	if image != pointed.ImageRef {
		t.Fatalf("the follower proved %s, want the selected %s", image, pointed.ImageRef)
	}
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !unproved(err) {
		t.Fatalf("select during the pass = %v, want the image held as unproved", err)
	}
	if follower.passMu.TryLock() {
		t.Fatal("a scheduled pass could start beside the follower's")
	}
	suite.results <- nil
	follower.wg.Wait()
	got, err := selectAfterFetch(t, selector, "", selectorRevision, nil)
	if err != nil || got != pointed.ImageRef {
		t.Fatalf("select after the pass = (%s, %v), want %s", got, err, pointed.ImageRef)
	}
	if passes := suite.passes(); len(passes) != 1 {
		t.Fatalf("passes = %v, want one", passes)
	}
	if proven := imageRefs(ward.ProvenImages()); !slices.Contains(proven, stale.ImageRef) {
		t.Fatalf("proven images = %v, want the other image's proof to stand", proven)
	}
}

// A maintenance task's run takes the image its refusal was measured from
// while the pointer names it, even with no image serving the daemon's base
// and upkeep holding other attempts on that task; it proves that image like
// any selection, and holds unserved once the pointer moves.
func TestClaudeImageSelectorRunsARepairOnItsRefusalsImage(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	other := selectorImage(t, strings.Repeat("c", 40), "9", "f")
	st := selectorStore(t, pointed, other)
	ward := newFakeWard(other.ImageRef)
	passed := make(chan struct{})
	follower := testFollower(t, ward, func(_ context.Context, image domain.ImageRef) error {
		ward.publish(image, true)
		close(passed)
		return nil
	}, time.Now, other.ImageRef)
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID,
		backend: ward, base: selectorBaseInputs("5", nil), follower: follower,
	}
	task := domain.TaskID("task-repair")
	selector.bindImageUpkeep(func(context.Context) (*domain.ImageUpkeepHold, error) {
		return &domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingMaintenanceTask, MaintenanceTaskID: &task}, nil
	})
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !errors.Is(err, engine.ErrImageUpkeepPending) {
		t.Fatalf("an ordinary attempt = %v, want it held on the maintenance task", err)
	}
	refusal := &domain.ImageUpkeepRefusal{ID: domain.Digest("sha256:" + strings.Repeat("7", 64)), MeasuredFromImageID: pointed.ID}

	if _, err := selectAfterFetch(t, selector, "", selectorRevision, refusal); !errors.Is(err, engine.ErrImageNotProved) {
		t.Fatalf("repair with only another image proved = %v, want it held as unproved", err)
	}
	<-passed
	if err := follower.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := selectAfterFetch(t, selector, "", selectorRevision, refusal)
	if err != nil || got != pointed.ImageRef {
		t.Fatalf("repair = (%s, %v), want the pointed-to image %s", got, err, pointed.ImageRef)
	}

	moved := *refusal
	moved.MeasuredFromImageID = other.ID
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, &moved); !errors.Is(err, engine.ErrProjectImageUnserved) ||
		errors.Is(err, engine.ErrImageUpkeepPending) {
		t.Fatalf("repair after the pointer moved = %v, want unserved", err)
	}
}

// After a failed follower pass the selected image has no published proof:
// attempts hold as unproved, and once the backoff has passed the next one
// retries the pass, after which the image is admitted.
func TestClaudeImageSelectorRetriesAFailedPass(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	stale := selectorImage(t, strings.Repeat("c", 40), "9", "f")
	st := selectorStore(t, pointed, stale)
	ward := newFakeWard(stale.ImageRef)
	clock := newFakeClock()
	suite := newFakeSuite(ward)
	follower := testFollower(t, ward, suite.prove, clock.Now, stale.ImageRef)
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID,
		backend: ward, base: selectorBaseInputs("2", nil), follower: follower,
	}

	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !unproved(err) {
		t.Fatalf("select with only another image proved = %v, want the image held as unproved", err)
	}
	suite.end(errors.New("the VM did not start"))
	follower.wg.Wait()

	clock.advance(followerRetryBackoff - time.Nanosecond)
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !unproved(err) ||
		!strings.Contains(err.Error(), "no published conformance proof") {
		t.Fatalf("select after the failed pass = %v, want the image held as having no published proof", err)
	}
	if follower.pendingFor(pointed.ImageRef) {
		t.Fatal("the failed pass was retried before its backoff")
	}
	clock.advance(time.Nanosecond)
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !unproved(err) {
		t.Fatalf("select after the backoff = %v, want the image held as unproved", err)
	}
	if !follower.pendingFor(pointed.ImageRef) {
		t.Fatal("the failed pass was not retried after its backoff")
	}
	suite.end(nil)
	follower.wg.Wait()

	got, err := selectAfterFetch(t, selector, "", selectorRevision, nil)
	if err != nil || got != pointed.ImageRef {
		t.Fatalf("select after the retry = (%s, %v), want %s", got, err, pointed.ImageRef)
	}
	if passes := suite.passes(); len(passes) != 2 {
		t.Fatalf("passes = %v, want two", passes)
	}
}

// The wait before a retry starts at followerRetryBackoff, doubles for each
// consecutive failed pass up to followerRetryBackoffCap, and resets after a
// success. Backoff is per image: an image in backoff delays no other image's
// pass.
func TestConformanceFollowerBacksOffFailedPasses(t *testing.T) {
	t.Parallel()
	image := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("d", 64))
	other := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("e", 64))
	ward := newFakeWard()
	clock := newFakeClock()
	suite := newFakeSuite(ward)
	follower := testFollower(t, ward, suite.prove, clock.Now, "")
	failure := errors.New("the VM did not start")

	follower.request(image)
	suite.end(failure)
	follower.wg.Wait()
	for i, wait := range []time.Duration{
		5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 40 * time.Minute, time.Hour, time.Hour,
	} {
		clock.advance(wait - time.Nanosecond)
		follower.request(image)
		if follower.pendingFor(image) {
			t.Fatalf("after %d failures a retry started before %s", i+1, wait)
		}
		clock.advance(time.Nanosecond)
		follower.request(image)
		if !follower.pendingFor(image) {
			t.Fatalf("after %d failures no retry started at %s", i+1, wait)
		}
		suite.end(failure)
		follower.wg.Wait()
	}

	// image is in backoff; other has never failed, so its pass starts now.
	follower.request(other)
	if !follower.pendingFor(other) {
		t.Fatal("another image's backoff delayed this image's pass")
	}
	suite.end(failure)
	follower.wg.Wait()

	clock.advance(time.Hour)
	follower.request(image)
	suite.end(nil)
	follower.wg.Wait()
	clock.advance(24 * time.Hour)
	before := len(suite.passes())
	follower.request(image)
	follower.wg.Wait()
	if len(suite.passes()) != before {
		t.Fatal("a proved image was proved again")
	}
	if !follower.failedFor(other) || follower.failedFor(image) {
		t.Fatal("a success cleared another image's failure, or kept its own")
	}

	clock.advance(followerRetryBackoff)
	image2 := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("9", 64))
	follower.request(image2)
	suite.end(failure)
	follower.wg.Wait()
	follower.request(image2)
	if follower.pendingFor(image2) {
		t.Fatalf("no backoff after a first failure")
	}
}

// A pass that queued behind the doctor's scheduled re-proof of its image
// runs no suite once that pass has proved the image.
func TestConformanceFollowerPassSkipsAfterAScheduledProof(t *testing.T) {
	t.Parallel()
	image := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("d", 64))
	ward := newFakeWard(image)
	suite := newFakeSuite(ward)
	follower := testFollower(t, ward, suite.prove, time.Now, image)

	scheduled := make(chan error, 1)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	<-suite.started
	follower.request(image)
	if !follower.pendingFor(image) {
		t.Fatal("the pass did not queue behind the scheduled pass")
	}
	suite.results <- nil
	close(suite.results)
	if err := <-scheduled; err != nil {
		t.Fatalf("scheduled pass = %v, want it to succeed", err)
	}
	follower.wg.Wait()
	if passes := suite.passes(); len(passes) != 1 {
		t.Fatalf("passes = %v, want the scheduled one only", passes)
	}
}

// A failed scheduled re-proof leaves the image with no published proof:
// attempts hold as unproved, and the image is retried after its backoff like
// a failed follower pass.
func TestClaudeImageSelectorHoldsAfterAFailedScheduledPass(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	st := selectorStore(t, pointed)
	ward := newFakeWard(pointed.ImageRef)
	clock := newFakeClock()
	suite := newFakeSuite(ward)
	follower := testFollower(t, ward, suite.prove, clock.Now, pointed.ImageRef)
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID,
		backend: ward, base: selectorBaseInputs("2", nil), follower: follower,
	}

	if got, err := selectAfterFetch(t, selector, "", selectorRevision, nil); err != nil || got != pointed.ImageRef {
		t.Fatalf("select under the proof = (%s, %v), want %s", got, err, pointed.ImageRef)
	}
	scheduled := make(chan error, 1)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	suite.end(errors.New("the egress probe failed"))
	if err := <-scheduled; err == nil {
		t.Fatal("scheduled pass succeeded, want it to fail")
	}
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !unproved(err) {
		t.Fatalf("select after the failed scheduled pass = %v, want the image held as unproved", err)
	}
	if follower.pendingFor(pointed.ImageRef) {
		t.Fatal("a follower pass started before the failed image's backoff")
	}
	clock.advance(followerRetryBackoff)
	if _, err := selectAfterFetch(t, selector, "", selectorRevision, nil); !unproved(err) {
		t.Fatalf("select after the backoff = %v, want the image held as unproved", err)
	}
	if image := suite.end(nil); image != pointed.ImageRef {
		t.Fatalf("retried %s, want %s", image, pointed.ImageRef)
	}
	follower.wg.Wait()
	if passes := suite.passes(); len(passes) != 2 {
		t.Fatalf("passes = %v, want the scheduled one and the retry", passes)
	}
}

// Close cancels a pass in flight and waits for it; the cancelled pass is not
// a failure, and a closed follower starts nothing.
func TestConformanceFollowerStopsOnClose(t *testing.T) {
	t.Parallel()
	image := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("d", 64))
	started := make(chan struct{})
	follower := newConformanceFollower(followerConfig{
		Ledger: newFakeWard(),
		Prove: func(ctx context.Context, _ domain.ImageRef) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		},
		Now: time.Now, Logger: slog.New(slog.DiscardHandler),
	})
	follower.request(image)
	<-started
	if err := follower.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v, want the pass cancelled and awaited", err)
	}
	if follower.failedFor(image) {
		t.Fatal("the cancelled pass was remembered as a failure")
	}
	next := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("e", 64))
	follower.request(next)
	if follower.pendingFor(next) {
		t.Fatal("a closed follower started a pass")
	}
}

// With a pass for one image blocked, an attempt on another, proven image is
// admitted; and attempts that alternate between two proven images run no
// pass at all.
func TestClaudeImageSelectorAdmitsAProvenImageWhileAnotherProves(t *testing.T) {
	t.Parallel()
	olderBase, newerBase := strings.Repeat("a", 40), strings.Repeat("e", 40)
	older := selectorImage(t, olderBase, "2", "d")
	rebuilt := selectorImage(t, newerBase, "7", "8")
	st := selectorStore(t, older, rebuilt)
	inputs := &countedBaseInputs{
		manifests: map[string]string{olderBase: "2", newerBase: "7"},
		reads:     map[string]int{},
	}
	at := func(sha string) domain.BaseRevision {
		base := selectorRevision
		base.BaseSHA = sha
		return base
	}

	t.Run("blocked pass", func(t *testing.T) {
		t.Parallel()
		ward := newFakeWard(rebuilt.ImageRef)
		suite := newFakeSuite(ward)
		follower := testFollower(t, ward, suite.prove, time.Now, rebuilt.ImageRef)
		selector := &claudeImageSelector{
			store: st, repo: selectorRepo, repositoryID: selectorRepositoryID,
			backend: ward, base: newBaseInputsMemo(inputs.observe, slog.New(slog.DiscardHandler)), follower: follower,
		}
		if _, err := selectAfterFetch(t, selector, "", at(olderBase), nil); !unproved(err) {
			t.Fatalf("select the unproved image = %v, want it held", err)
		}
		if image := <-suite.started; image != older.ImageRef {
			t.Fatalf("pass on %s, want %s", image, older.ImageRef)
		}
		got, err := selectAfterFetch(t, selector, "", at(newerBase), nil)
		if err != nil || got != rebuilt.ImageRef {
			t.Fatalf("select the proven image while another's pass is blocked = (%s, %v), want %s",
				got, err, rebuilt.ImageRef)
		}
		suite.results <- nil
	})

	t.Run("alternating", func(t *testing.T) {
		t.Parallel()
		ward := newFakeWard(older.ImageRef, rebuilt.ImageRef)
		follower := testFollower(t, ward, refuseProof(t), time.Now, rebuilt.ImageRef)
		selector := &claudeImageSelector{
			store: st, repo: selectorRepo, repositoryID: selectorRepositoryID,
			backend: ward, base: newBaseInputsMemo(inputs.observe, slog.New(slog.DiscardHandler)), follower: follower,
		}
		for range 3 {
			for _, want := range []domain.ProjectImage{older, rebuilt} {
				got, err := selectAfterFetch(t, selector, "", at(want.CommitSHA), nil)
				if err != nil || got != want.ImageRef {
					t.Fatalf("select = (%s, %v), want %s", got, err, want.ImageRef)
				}
			}
		}
	})
}

// A scheduled round re-proves each kept image in its own pass, one after
// another, never two at once.
func TestConformanceFollowerScheduledRoundProvesOneImageAtATime(t *testing.T) {
	t.Parallel()
	a := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("a", 64))
	b := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("b", 64))
	ward := newFakeWard(a, b)
	var (
		mu       sync.Mutex
		running  int
		overlaps int
		proved   []domain.ImageRef
	)
	follower := testFollower(t, ward, func(_ context.Context, image domain.ImageRef) error {
		mu.Lock()
		running++
		if running > 1 {
			overlaps++
		}
		proved = append(proved, image)
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
		return nil
	}, time.Now, a)
	follower.noteSelected(b)
	follower.noteSelected(a)
	if err := follower.runScheduled(t.Context()); err != nil {
		t.Fatal(err)
	}
	if overlaps != 0 || !slices.Equal(proved, []domain.ImageRef{a, b}) {
		t.Fatalf("round proved %v with %d overlaps, want a then b, one at a time", proved, overlaps)
	}
}

// A failed scheduled pass closes only its own image: the round still
// succeeds while another proof stands, the failed image waits out its
// backoff, and the next round does not re-prove it before then even when it
// is the most recently selected image. Once no proof stands, the round fails.
func TestConformanceFollowerScheduledFailureClosesOnlyItsImage(t *testing.T) {
	t.Parallel()
	a := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("a", 64))
	b := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("b", 64))
	ward := newFakeWard(a, b)
	clock := newFakeClock()
	suite := newFakeSuite(ward)
	follower := testFollower(t, ward, suite.prove, clock.Now, a)
	follower.noteSelected(a)
	follower.noteSelected(b)

	scheduled := make(chan error, 1)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	if image := suite.end(errors.New("the egress probe failed")); image != b {
		t.Fatalf("first re-proof = %s, want the latest image %s", image, b)
	}
	suite.end(nil)
	if err := <-scheduled; err != nil {
		t.Fatalf("round with one failed image = %v, want it to pass while %s stands", err, a)
	}
	if proven := ward.ProvenImages(); !slices.Equal(proven, []string{string(a)}) {
		t.Fatalf("proven = %v, want only %s", proven, a)
	}

	// b is still the latest selection, but its backoff has not passed.
	follower.noteSelected(a)
	follower.noteSelected(b)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	suite.end(nil)
	if err := <-scheduled; err != nil {
		t.Fatal(err)
	}
	if passes := suite.passes(); !slices.Equal(passes, []domain.ImageRef{b, a, a}) {
		t.Fatalf("passes = %v, want b's re-proof held back by its backoff", passes)
	}

	// Once nothing stands, a failed round fails the doctor pass.
	clock.advance(followerRetryBackoff)
	follower.noteSelected(b)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	suite.end(errors.New("the egress probe failed again"))
	if err := <-scheduled; err == nil {
		t.Fatal("round that left no proof succeeded, want it to fail")
	}
	if retired := ward.retiredImages(); !slices.Equal(retired, []domain.ImageRef{a}) {
		t.Fatalf("retired %v, want the unselected %s", retired, a)
	}
}

// The scheduled pass retires every proof no attempt selected since the
// previous one, failed ones included, and keeps the most recently selected
// image's proof even when the window selected nothing.
func TestConformanceFollowerRetiresUnusedProofs(t *testing.T) {
	t.Parallel()
	a := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("a", 64))
	b := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("b", 64))
	c := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("c", 64))
	failed := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("f", 64))
	ward := newFakeWard(a, b, c)
	clock := newFakeClock()
	suite := newFakeSuite(ward)
	follower := testFollower(t, ward, suite.prove, clock.Now, a)
	follower.request(failed)
	suite.end(errors.New("the VM did not start"))
	follower.wg.Wait()
	follower.noteSelected(b)

	scheduled := make(chan error, 1)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	if image := suite.end(nil); image != b {
		t.Fatalf("re-proved %s, want the selected %s", image, b)
	}
	if err := <-scheduled; err != nil {
		t.Fatal(err)
	}
	if retired := ward.retiredImages(); !slices.Equal(retired, []domain.ImageRef{a, c, failed}) {
		t.Fatalf("retired %v, want every image no attempt selected", retired)
	}
	if follower.failedFor(failed) {
		t.Fatal("retiring an image kept its failure")
	}

	// Nothing selected since: b, the most recently selected, is re-proved.
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	if image := suite.end(nil); image != b {
		t.Fatalf("re-proved %s, want the most recently selected %s", image, b)
	}
	if err := <-scheduled; err != nil {
		t.Fatal(err)
	}
	if retired := ward.retiredImages(); len(retired) != 3 {
		t.Fatalf("retired %v, want the most recently selected image kept", retired)
	}
}

// A start on an image with no proof asks for one, as a recorded attempt
// waiting past its image's retirement must, and keeps the image's proof
// through the next scheduled pass; a start on a proven image runs no pass.
func TestConformanceFollowerStartAsksForItsImagesProof(t *testing.T) {
	t.Parallel()
	retired := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("a", 64))
	proven := "ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("b", 64))
	ward := newFakeWard(proven)
	suite := newFakeSuite(ward)
	follower := testFollower(t, ward, suite.prove, time.Now, proven)

	if err := follower.use(proven); err != nil {
		t.Fatalf("start on a proven image = %v", err)
	}
	follower.wg.Wait()
	if err := follower.use(retired); !unproved(err) {
		t.Fatalf("start on an unproven image = %v, want it held as unproved", err)
	}
	if image := suite.end(nil); image != retired {
		t.Fatalf("start proved %s, want its image %s", image, retired)
	}
	follower.wg.Wait()
	if passes := suite.passes(); len(passes) != 1 {
		t.Fatalf("passes = %v, want only the unproven image's", passes)
	}
	scheduled := make(chan error, 1)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	for range 2 {
		suite.end(nil)
	}
	if err := <-scheduled; err != nil {
		t.Fatal(err)
	}
	if retiredImages := ward.retiredImages(); len(retiredImages) != 0 {
		t.Fatalf("retired %v, want both started images kept", retiredImages)
	}
}

// A retired image is proved again when an attempt next selects it.
func TestClaudeImageSelectorProvesARetiredImageAgain(t *testing.T) {
	t.Parallel()
	olderBase, newerBase := strings.Repeat("a", 40), strings.Repeat("e", 40)
	older := selectorImage(t, olderBase, "2", "d")
	rebuilt := selectorImage(t, newerBase, "7", "8")
	st := selectorStore(t, older, rebuilt)
	inputs := &countedBaseInputs{
		manifests: map[string]string{olderBase: "2", newerBase: "7"},
		reads:     map[string]int{},
	}
	at := func(sha string) domain.BaseRevision {
		base := selectorRevision
		base.BaseSHA = sha
		return base
	}
	ward := newFakeWard(older.ImageRef, rebuilt.ImageRef)
	suite := newFakeSuite(ward)
	follower := testFollower(t, ward, suite.prove, time.Now, rebuilt.ImageRef)
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID,
		backend: ward, base: newBaseInputsMemo(inputs.observe, slog.New(slog.DiscardHandler)), follower: follower,
	}
	if _, err := selectAfterFetch(t, selector, "", at(newerBase), nil); err != nil {
		t.Fatal(err)
	}
	scheduled := make(chan error, 1)
	go func() { scheduled <- follower.runScheduled(t.Context()) }()
	suite.end(nil)
	if err := <-scheduled; err != nil {
		t.Fatal(err)
	}
	if retired := ward.retiredImages(); !slices.Equal(retired, []domain.ImageRef{older.ImageRef}) {
		t.Fatalf("retired %v, want the unselected image", retired)
	}
	if _, err := selectAfterFetch(t, selector, "", at(olderBase), nil); !unproved(err) {
		t.Fatalf("select the retired image = %v, want it held as unproved", err)
	}
	if image := suite.end(nil); image != older.ImageRef {
		t.Fatalf("proved %s, want the retired image %s", image, older.ImageRef)
	}
	follower.wg.Wait()
	if got, err := selectAfterFetch(t, selector, "", at(olderBase), nil); err != nil || got != older.ImageRef {
		t.Fatalf("select after the new proof = (%s, %v), want %s", got, err, older.ImageRef)
	}
}

// countedBaseInputs observes manifests[baseSHA] at each base and counts the
// reads per base, so a test sees which commit each selection read.
type countedBaseInputs struct {
	mu        sync.Mutex
	manifests map[string]string
	reads     map[string]int
}

func (c *countedBaseInputs) observe(
	_ context.Context, baseSHA string, _ domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads[baseSHA]++
	manifest, ok := c.manifests[baseSHA]
	if !ok {
		return domain.ProjectImageBaseInputs{}, fmt.Errorf("base %s absent: %w", baseSHA, exec.ErrInputUnavailable)
	}
	return domain.ProjectImageBaseInputs{
		CommitSHA:         baseSHA,
		PackageJSONSHA256: strings.Repeat(manifest, 64), PackageLockSHA256: strings.Repeat(manifest, 64),
	}, nil
}

func (c *countedBaseInputs) readsOf(baseSHA string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads[baseSHA]
}

// After a tip advance that changes the image inputs, an attempt selects the
// image that serves its own run's base: a new run's base selects the rebuilt
// image and an older run's base still selects the image built for it, each
// base read once however often it is selected at.
func TestClaudeImageSelectorSelectsAtEachRunsBase(t *testing.T) {
	t.Parallel()
	olderBase, newerBase := strings.Repeat("a", 40), strings.Repeat("e", 40)
	older := selectorImage(t, olderBase, "2", "d")
	rebuilt := selectorImage(t, newerBase, "7", "8")
	st := selectorStore(t, older, rebuilt)
	inputs := &countedBaseInputs{
		manifests: map[string]string{olderBase: "2", newerBase: "7"},
		reads:     map[string]int{},
	}
	ward := newFakeWard(rebuilt.ImageRef)
	var (
		mu     sync.Mutex
		proved []domain.ImageRef
	)
	follower := testFollower(t, ward, func(_ context.Context, image domain.ImageRef) error {
		mu.Lock()
		defer mu.Unlock()
		proved = append(proved, image)
		return nil
	}, time.Now, rebuilt.ImageRef)
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID,
		backend: ward, base: newBaseInputsMemo(inputs.observe, slog.New(slog.DiscardHandler)), follower: follower,
	}
	at := func(sha string) domain.BaseRevision {
		base := selectorRevision
		base.BaseSHA = sha
		return base
	}

	for range 2 {
		got, err := selectAfterFetch(t, selector, "", at(newerBase), nil)
		if err != nil || got != rebuilt.ImageRef {
			t.Fatalf("select at the new run's base = (%s, %v), want %s", got, err, rebuilt.ImageRef)
		}
	}
	// The older run's image has no proof, so its attempt holds while the
	// follower proves the image its base selects.
	if _, err := selectAfterFetch(t, selector, "", at(olderBase), nil); !unproved(err) {
		t.Fatalf("select at the older run's base = %v, want its own image held as unproved", err)
	}
	if err := follower.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(proved, []domain.ImageRef{older.ImageRef}) {
		t.Fatalf("proved %v, want the older run's image %s", proved, older.ImageRef)
	}
	if inputs.readsOf(newerBase) != 1 || inputs.readsOf(olderBase) != 1 {
		t.Fatalf("reads = %v, want each base read once", inputs.reads)
	}
}

// The memo reads each base once, keeps baseInputsMemoBases commits, forgets
// the oldest first-read commit past that, and keeps no failed read.
func TestBaseInputsMemoIsBounded(t *testing.T) {
	t.Parallel()
	layout := domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM}
	inputs := &countedBaseInputs{manifests: map[string]string{}, reads: map[string]int{}}
	commit := func(i int) string { return fmt.Sprintf("%040x", i+1) }
	for i := range baseInputsMemoBases + 1 {
		inputs.manifests[commit(i)] = "2"
	}
	memo := newBaseInputsMemo(inputs.observe, slog.New(slog.DiscardHandler))
	read := func(sha string) {
		t.Helper()
		got, err := memo.read(t.Context(), sha, layout)
		if err != nil || got.CommitSHA != sha {
			t.Fatalf("read %s = (%+v, %v), want its inputs", sha, got, err)
		}
	}

	for i := range baseInputsMemoBases {
		read(commit(i))
		read(commit(i))
	}
	read(commit(0))
	if len(memo.bases) != baseInputsMemoBases || inputs.readsOf(commit(0)) != 1 {
		t.Fatalf("memo holds %d bases after %d first reads of the oldest, want %d bases and one read",
			len(memo.bases), inputs.readsOf(commit(0)), baseInputsMemoBases)
	}
	read(commit(baseInputsMemoBases))
	if len(memo.bases) != baseInputsMemoBases || len(memo.inputs) != baseInputsMemoBases {
		t.Fatalf("memo holds %d bases and %d entries, want %d of each",
			len(memo.bases), len(memo.inputs), baseInputsMemoBases)
	}
	read(commit(0))
	if inputs.readsOf(commit(0)) != 2 || inputs.readsOf(commit(1)) != 1 {
		t.Fatalf("reads = %v, want the forgotten oldest base read again and no other", inputs.reads)
	}

	absent := strings.Repeat("f", 40)
	for range 2 {
		if _, err := memo.read(t.Context(), absent, layout); !errors.Is(err, exec.ErrInputUnavailable) {
			t.Fatalf("read of an absent base = %v, want ErrInputUnavailable", err)
		}
	}
	if inputs.readsOf(absent) != 2 {
		t.Fatalf("absent base read %d times, want a failed read retried", inputs.readsOf(absent))
	}
}

// stallingBaseInputs counts each fetch as it starts, signals started, and
// holds the fetch until release closes or its context ends; ended receives
// the error each fetch returned.
type stallingBaseInputs struct {
	inputs  *countedBaseInputs
	started chan struct{}
	release chan struct{}
	ended   chan error
}

func newStallingBaseInputs(manifests map[string]string) *stallingBaseInputs {
	return &stallingBaseInputs{
		inputs:  &countedBaseInputs{manifests: manifests, reads: map[string]int{}},
		started: make(chan struct{}, 4), release: make(chan struct{}), ended: make(chan error, 4),
	}
}

func (s *stallingBaseInputs) observe(
	ctx context.Context, baseSHA string, layout domain.ProjectImageInputLayout,
) (domain.ProjectImageBaseInputs, error) {
	inputs, err := s.inputs.observe(ctx, baseSHA, layout)
	s.started <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		inputs, err = domain.ProjectImageBaseInputs{}, fmt.Errorf("fetch base %s: %w", baseSHA, ctx.Err())
	}
	s.ended <- err
	return inputs, err
}

// closeAtCleanup closes memo when the test ends, so no fetch outlives it.
func closeAtCleanup(t *testing.T, memo *baseInputsMemo) *baseInputsMemo {
	t.Helper()
	t.Cleanup(func() { _ = memo.Close(context.Background()) })
	return memo
}

// While a base's fetch stalls, a selection answers pending at once and a
// second selection starts no second fetch. Once the fetch lands, later
// selections select from the memo without fetching again. A selection that
// ran the fetch itself would wait on the stall and fail the deadline.
func TestClaudeImageSelectorFetchesTheBaseOutsideTheSelection(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	st := selectorStore(t, pointed)
	ward := newFakeWard(pointed.ImageRef)
	follower := testFollower(t, ward, refuseProof(t), time.Now, pointed.ImageRef)
	stall := newStallingBaseInputs(map[string]string{selectorBase: "2"})
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID, backend: ward, follower: follower,
		base: closeAtCleanup(t, newBaseInputsMemo(stall.observe, slog.New(slog.DiscardHandler))),
	}
	selectWithin := func() (domain.ImageRef, error) {
		t.Helper()
		type result struct {
			image domain.ImageRef
			err   error
		}
		done := make(chan result, 1)
		go func() {
			image, err := selector.selectImage(t.Context(), "", selectorRevision, nil)
			done <- result{image, err}
		}()
		select {
		case got := <-done:
			return got.image, got.err
		case <-time.After(10 * time.Second):
			t.Fatal("the selection waited on the stalled fetch")
			return "", nil
		}
	}
	pending := func(err error) bool {
		return errors.Is(err, engine.ErrBaseInputsPending) && !errors.Is(err, exec.ErrInputUnavailable)
	}

	if _, err := selectWithin(); !pending(err) {
		t.Fatalf("select while the base is unread = %v, want pending and not input_unavailable", err)
	}
	<-stall.started
	if _, err := selectWithin(); !pending(err) {
		t.Fatalf("select while the fetch stalls = %v, want pending", err)
	}
	if reads := stall.inputs.readsOf(selectorBase); reads != 1 {
		t.Fatalf("base fetched %d times while stalled, want one fetch", reads)
	}
	close(stall.release)
	selector.base.fetches.Wait()
	for range 2 {
		if got, err := selectWithin(); err != nil || got != pointed.ImageRef {
			t.Fatalf("select after the fetch = (%s, %v), want %s", got, err, pointed.ImageRef)
		}
	}
	if reads := stall.inputs.readsOf(selectorBase); reads != 1 {
		t.Fatalf("base fetched %d times, want later selections served from the memo", reads)
	}
}

// A failed fetch holds the selection with its own error, joined with
// input_unavailable as a failed read always was, and is served without a new
// fetch until baseFetchRetryBackoff has passed. The fetch after that reports
// its own error, so a repository identity mismatch still reaches the caller.
func TestClaudeImageSelectorBacksOffAFailedFetch(t *testing.T) {
	t.Parallel()
	pointed := selectorImage(t, strings.Repeat("a", 40), "2", "d")
	st := selectorStore(t, pointed)
	ward := newFakeWard(pointed.ImageRef)
	follower := testFollower(t, ward, refuseProof(t), time.Now, pointed.ImageRef)
	var (
		mu       sync.Mutex
		fetches  int
		fetchErr = errors.New("fetch refused")
	)
	memo := closeAtCleanup(t, newBaseInputsMemo(func(
		context.Context, string, domain.ProjectImageInputLayout,
	) (domain.ProjectImageBaseInputs, error) {
		mu.Lock()
		defer mu.Unlock()
		fetches++
		return domain.ProjectImageBaseInputs{}, fetchErr
	}, slog.New(slog.DiscardHandler)))
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	memo.now = func() time.Time { return now }
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID, backend: ward, follower: follower,
		base: memo,
	}
	selectAt := func() error {
		t.Helper()
		_, err := selector.selectImage(t.Context(), "", selectorRevision, nil)
		return err
	}
	fetchCount := func() int {
		mu.Lock()
		defer mu.Unlock()
		return fetches
	}

	if err := selectAt(); !errors.Is(err, engine.ErrBaseInputsPending) {
		t.Fatalf("first select = %v, want pending", err)
	}
	memo.fetches.Wait()
	for _, elapsed := range []time.Duration{0, baseFetchRetryBackoff - time.Nanosecond} {
		now = now.Add(elapsed)
		err := selectAt()
		if !errors.Is(err, fetchErr) || !errors.Is(err, exec.ErrInputUnavailable) || errors.Is(err, engine.ErrBaseInputsPending) {
			t.Fatalf("select %s after the failure = %v, want the failure as input_unavailable", elapsed, err)
		}
	}
	if got := fetchCount(); got != 1 {
		t.Fatalf("fetched %d times within the backoff, want 1", got)
	}

	fetchErr = fmt.Errorf("checkout names another repository: %w", domain.ErrRepositoryIdentityMismatch)
	now = now.Add(time.Nanosecond)
	if err := selectAt(); !errors.Is(err, engine.ErrBaseInputsPending) {
		t.Fatalf("select once the backoff passed = %v, want a new fetch pending", err)
	}
	memo.fetches.Wait()
	if err := selectAt(); !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("select after a mismatched fetch = %v, want the identity mismatch", err)
	}
	if got := fetchCount(); got != 2 {
		t.Fatalf("fetched %d times, want one retry after the backoff", got)
	}

	// A failure recorded after another's backoff has passed drops that one,
	// so failures of bases no run looks up again don't accumulate.
	now = now.Add(baseFetchRetryBackoff)
	other := selectorRevision
	other.BaseSHA = strings.Repeat("c", 40)
	if _, err := selector.selectImage(t.Context(), "", other, nil); !errors.Is(err, engine.ErrBaseInputsPending) {
		t.Fatalf("select at another base = %v, want pending", err)
	}
	memo.fetches.Wait()
	memo.mu.Lock()
	failed := slices.Collect(maps.Keys(memo.failures))
	memo.mu.Unlock()
	if len(failed) != 1 || failed[0].baseSHA != other.BaseSHA {
		t.Fatalf("failures = %v, want only the newest base's", failed)
	}
}

// A fetch that outlasts baseFetchTimeout fails with its deadline and is
// served like any failed fetch, so a hung fetch becomes an input_unavailable
// hold rather than a pending one that never ends.
func TestBaseInputsMemoFetchDeadline(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		layout := domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM}
		stall := newStallingBaseInputs(map[string]string{selectorBase: "2"})
		memo := newBaseInputsMemo(stall.observe, slog.New(slog.DiscardHandler))
		defer func() { _ = memo.Close(context.Background()) }()
		start := time.Now()

		if _, err := memo.lookup(selectorBase, layout); !errors.Is(err, engine.ErrBaseInputsPending) {
			t.Fatalf("lookup = %v, want pending", err)
		}
		memo.fetches.Wait()
		if elapsed := time.Since(start); elapsed != baseFetchTimeout {
			t.Fatalf("the fetch ended after %s, want its %s deadline", elapsed, baseFetchTimeout)
		}
		if _, err := memo.lookup(selectorBase, layout); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("lookup after the deadline = %v, want the deadline served", err)
		}
		if reads := stall.inputs.readsOf(selectorBase); reads != 1 {
			t.Fatalf("base fetched %d times, want the failure served without a new fetch", reads)
		}
	})
}

// Close cancels a fetch in flight and returns only once the fetch has, and a
// canceled fetch stores nothing. A new memo, as after a restart, fetches the
// base again.
func TestBaseInputsMemoCloseJoinsItsFetch(t *testing.T) {
	t.Parallel()
	layout := domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM}
	stall := newStallingBaseInputs(map[string]string{selectorBase: "2"})
	memo := newBaseInputsMemo(stall.observe, slog.New(slog.DiscardHandler))
	if _, err := memo.lookup(selectorBase, layout); !errors.Is(err, engine.ErrBaseInputsPending) {
		t.Fatalf("lookup = %v, want pending", err)
	}
	<-stall.started
	if err := memo.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-stall.ended:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the fetch ended with %v, want it canceled", err)
		}
	default:
		t.Fatal("Close returned before the fetch did")
	}
	memo.mu.Lock()
	stored, failed := len(memo.inputs), len(memo.failures)
	memo.mu.Unlock()
	if stored != 0 || failed != 0 {
		t.Fatalf("memo holds %d inputs and %d failures after Close, want a canceled fetch to store nothing", stored, failed)
	}
	if _, err := memo.lookup(selectorBase, layout); !errors.Is(err, engine.ErrBaseInputsPending) {
		t.Fatalf("lookup after Close = %v, want pending", err)
	}
	memo.fetches.Wait()
	if reads := stall.inputs.readsOf(selectorBase); reads != 1 {
		t.Fatalf("base fetched %d times, want no fetch started after Close", reads)
	}

	restarted := closeAtCleanup(t, newBaseInputsMemo(stall.observe, slog.New(slog.DiscardHandler)))
	if _, err := restarted.lookup(selectorBase, layout); !errors.Is(err, engine.ErrBaseInputsPending) {
		t.Fatalf("lookup after a restart = %v, want a new fetch pending", err)
	}
	<-stall.started
	if reads := stall.inputs.readsOf(selectorBase); reads != 2 {
		t.Fatalf("base fetched %d times, want it fetched again after the restart", reads)
	}
}
