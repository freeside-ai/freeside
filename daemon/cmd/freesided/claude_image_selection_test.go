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

// fakeWard proves one writer image at a time, as ward's backend does.
type fakeWard struct {
	mu       sync.Mutex
	image    string
	repoints []string
}

func (w *fakeWard) RepointAgentImage(image string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if image != w.image {
		w.image = image
		w.repoints = append(w.repoints, image)
	}
	return nil
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
	}, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = follower.Close(context.Background()) })
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID, baseSHA: selectorBase,
		backend: backend, base: base, follower: follower,
	}

	if _, err := selector.selectImage(t.Context(), ""); !errors.Is(err, engine.ErrProjectImageUnserved) {
		t.Fatalf("select before upkeep is bound = %v, want unserved", err)
	}
	var pending *domain.ImageUpkeepHold
	lookupErr := error(nil)
	selector.bindImageUpkeep(func(context.Context) (*domain.ImageUpkeepHold, error) { return pending, lookupErr })

	pending = &domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingRebuild}
	var upkeep *engine.ImageUpkeepPendingError
	if _, err := selector.selectImage(t.Context(), ""); !errors.As(err, &upkeep) ||
		upkeep.Hold.Pending != domain.ImageUpkeepPendingRebuild {
		t.Fatalf("select with a rebuild pending = %v, want an image_upkeep_pending rebuild", err)
	}
	pending = nil
	if _, err := selector.selectImage(t.Context(), ""); !errors.Is(err, engine.ErrProjectImageUnserved) {
		t.Fatalf("select with nothing pending = %v, want unserved", err)
	}
	lookupErr = errors.New("tip read failed")
	if _, err := selector.selectImage(t.Context(), ""); !errors.Is(err, lookupErr) ||
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
		return nil
	}, slog.New(slog.DiscardHandler))
	selector := &claudeImageSelector{
		store: st, repo: selectorRepo, repositoryID: selectorRepositoryID, baseSHA: selectorBase,
		backend: backend, base: selectorBaseInputs("2", nil), follower: follower,
	}
	unproved := func(err error) bool {
		return errors.Is(err, engine.ErrImageNotProved) && errors.Is(err, domain.ErrAdmissionConfigurationMismatch)
	}

	if _, err := selector.selectImage(t.Context(), ""); !unproved(err) {
		t.Fatalf("select under the stale proof = %v, want the image held as unproved", err)
	}
	<-started
	// Ward already names the selected image, but its pass has not ended.
	if _, err := selector.selectImage(t.Context(), ""); !unproved(err) {
		t.Fatalf("select during the pass = %v, want the image held as unproved", err)
	}
	if follower.passMu.TryLock() {
		t.Fatal("a scheduled pass could start beside the follower's")
	}
	close(release)
	if err := follower.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := selector.selectImage(t.Context(), "")
	if err != nil || got != pointed.ImageRef {
		t.Fatalf("select after the pass = (%s, %v), want %s", got, err, pointed.ImageRef)
	}
	if repoints := backend.repointed(); proves != 1 || len(repoints) != 1 || repoints[0] != string(pointed.ImageRef) {
		t.Fatalf("ward repointed %v with %d passes, want one repoint and one pass", repoints, proves)
	}
}

// Close cancels a pass in flight and waits for it; a closed follower starts
// nothing.
func TestConformanceFollowerStopsOnClose(t *testing.T) {
	t.Parallel()
	backend := &fakeWard{}
	started := make(chan struct{})
	follower := newConformanceFollower(backend, func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}, slog.New(slog.DiscardHandler))
	follower.follow("ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("d", 64)))
	<-started
	if err := follower.Close(t.Context()); err != nil {
		t.Fatalf("Close = %v, want the pass cancelled and awaited", err)
	}
	follower.follow("ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("e", 64)))
	if repoints := backend.repointed(); len(repoints) != 1 {
		t.Fatalf("ward repointed %v, want nothing after Close", repoints)
	}
	if follower.pendingFor("ghcr.io/owner/project@sha256:" + domain.ImageRef(strings.Repeat("e", 64))) {
		t.Fatal("a closed follower reports a pass")
	}
}
