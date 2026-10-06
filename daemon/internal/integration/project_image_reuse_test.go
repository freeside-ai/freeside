package integration_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// earlierBuildCommit stands for a commit the run's base has moved on from. The
// publication lane never reads the build commit's tree: an image built there
// serves another base only through the environment its record carries.
const earlierBuildCommit = "abababababababababababababababababababab"

func newProjectImageReuseHarness(
	t *testing.T, baseFiles map[string]string, edit func(*domain.ProjectImageInput),
) *productionPublicationHarness {
	t.Helper()
	files := productionBaseManifests()
	for path, body := range baseFiles {
		files[path] = body
	}
	h := newPublicationHarnessWithBaseFiles(
		t, []byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`), files)
	h.projectImageInput = edit
	// The candidate's export is its whole tree, so it carries the extra base
	// files unchanged.
	return newProductionPublicationHarnessFromBase(t, h, "", nil, nil, baseFiles)
}

func builtAtEarlierCommit(input *domain.ProjectImageInput) {
	input.CommitSHA = earlierBuildCommit
}

func requireIncompatibleImageRefusal(t *testing.T, err error, names string) {
	t.Helper()
	if !errors.Is(err, domain.ErrParentKeyMismatch) ||
		!errors.Is(err, domain.ErrProjectImageIncompatible) {
		t.Fatalf("error = %v, want an incompatible-image disagreement with durable authority", err)
	}
	if !strings.Contains(err.Error(), names) {
		t.Fatalf("error %q does not name %q", err, names)
	}
}

// An image built at an earlier commit serves a base whose environment inputs
// are unchanged, and the run is still verified on its own base: the record
// keeps the build commit, the admission keeps the run's base.
func TestProductionPublicationReusesProjectImageAtCompatibleBase(t *testing.T) {
	t.Parallel()
	p := newProjectImageReuseHarness(t, nil, builtAtEarlierCommit)
	p.publishReady(t)
	if p.room.runs != 1 {
		t.Fatalf("verification runs = %d, want 1 on the run's own base", p.room.runs)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d PRs, want 1 and 1", refs, prs)
	}
	var recorded domain.ProjectImage
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		recorded, _, err = tx.GetProjectImageByRef(p.ctx, p.image.ImageRef)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if recorded.CommitSHA != earlierBuildCommit || recorded.CommitSHA == p.baseSHA {
		t.Fatalf("recorded build commit = %s, want %s and not the run base %s",
			recorded.CommitSHA, earlierBuildCommit, p.baseSHA)
	}
}

// Every input the image baked must be unchanged at the base, and the refusal
// lands before a verification room exists or anything is published.
func TestProductionPublicationRefusesIncompatibleProjectImage(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		baseFiles map[string]string
		edit      func(*domain.ProjectImageInput)
		names     string
	}{
		"legacy record": {nil, func(input *domain.ProjectImageInput) {
			builtAtEarlierCommit(input)
			input.Environment = nil
		}, "rebuild the image"},
		"changed lockfile": {nil, func(input *domain.ProjectImageInput) {
			builtAtEarlierCommit(input)
			input.Environment.PackageLockSHA256 = strings.Repeat("ab", 32)
		}, "package-lock.json"},
		"changed manifest": {nil, func(input *domain.ProjectImageInput) {
			builtAtEarlierCommit(input)
			input.Environment.PackageJSONSHA256 = strings.Repeat("ab", 32)
		}, "package.json"},
		"another builder's preparation": {nil, func(input *domain.ProjectImageInput) {
			builtAtEarlierCommit(input)
			input.Environment.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("ab", 32))
		}, "preparation"},
		"installation configuration at the base": {
			map[string]string{".npmrc": "registry=https://registry.example.test/\n"},
			builtAtEarlierCommit, ".npmrc",
		},
		"shrinkwrap at the base": {
			map[string]string{"npm-shrinkwrap.json": `{"lockfileVersion":3}` + "\n"},
			builtAtEarlierCommit, "npm-shrinkwrap.json",
		},
		"base declares another recipe": {
			map[string]string{".freeside/verify.json": `{"commands":[["different-check"]],"capture":"none"}`},
			builtAtEarlierCommit, "recipe",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newProjectImageReuseHarness(t, tc.baseFiles, tc.edit)
			p.startAndRecordExport(t)
			_, err := p.reconcileLanes()
			requireIncompatibleImageRefusal(t, err, tc.names)
			if p.room.reads != 0 || p.room.runs != 0 {
				t.Fatalf("room reads/runs = %d/%d, want none before the refusal",
					p.room.reads, p.room.runs)
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
				t.Fatalf("refused image caused effects: %d refs, %d PRs", refs, prs)
			}
		})
	}
}

// A re-entered cycle keeps its producer's image and runs it on the re-entry
// base, so that base is the one the image must serve. Before this rule the
// cycle checked the image only against the base its producer was admitted at.
// A base advance is ordinary, so the refusal stops this one cycle on an item a
// person can read: as a lane error it would stop publication for every run.
func TestReadinessReentryStopsOnBaseTheImageCannotServe(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		edit    func(*domain.ProjectImageInput)
		advance func(dir string) error
		names   string
	}{
		"dependency drift on the advanced base": {
			nil,
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "package-lock.json"),
					[]byte(`{"name":"fixture","lockfileVersion":2}`+"\n"), 0o600)
			},
			"package-lock.json",
		},
		"legacy record on a source-only advance": {
			func(input *domain.ProjectImageInput) { input.Environment = nil },
			func(dir string) error {
				return os.WriteFile(filepath.Join(dir, "UPSTREAM.md"), []byte("upstream change\n"), 0o600)
			},
			"rebuild the image",
		},
		// No image can have baked a manifest that is not a regular file, so
		// the base is one the image cannot serve, not a fault of the lane.
		"manifest replaced by a symbolic link on the advanced base": {
			nil,
			func(dir string) error {
				manifest := filepath.Join(dir, "package.json")
				if err := os.Rename(manifest, filepath.Join(dir, "real.json")); err != nil {
					return err
				}
				return os.Symlink("real.json", manifest)
			},
			"package.json",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newProjectImageReuseHarness(t, nil, tc.edit)
			// The first cycle runs at the image's own build commit.
			p.publishReady(t)
			if err := tc.advance(p.baseDir); err != nil {
				t.Fatal(err)
			}
			runGit(t, p.baseDir, "add", "-A")
			runGit(t, p.baseDir, "commit", "-q", "-m", "advance the base")
			advanced := runGit(t, p.baseDir, "rev-parse", "HEAD")
			if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
				Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
			}); !started {
				t.Fatal("a base advance started no re-entry")
			}
			reads, runs := p.room.reads, p.room.runs
			result, err := p.reconcileLanes()
			if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
				t.Fatalf("unserved base result = %#v, %v", result, err)
			}
			stop := p.reentryStopItem(t)
			if !strings.Contains(stop.Reason, advanced) || !strings.Contains(stop.Reason, tc.names) ||
				!strings.Contains(stop.Reason, "project image") {
				t.Fatalf("stop item = %#v", stop)
			}
			if p.room.reads != reads || p.room.runs != runs {
				t.Fatalf("a refused image served %d room reads and %d verification commands",
					p.room.reads-reads, p.room.runs-runs)
			}
			// The task is retired: later passes neither fail nor repeat the stop.
			if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 0 {
				t.Fatalf("pass after the stop = %#v, %v", result, err)
			}
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				pending, err := tx.ListPendingOutbox(p.ctx, engine.KindProductionPublicationRequested)
				if err != nil || len(pending) != 0 {
					t.Errorf("a stopped cycle left %d pending tasks, %v", len(pending), err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The preparation clause follows the running binary, so the verdict is taken
// where a verification room is about to be built and nowhere else. A cycle
// that lost its process after verification finishes on the evidence it holds,
// even under a daemon whose builder sources have since changed; judging it
// again would refuse finished work after every such upgrade.
func TestProjectImageVerdictIsNotTakenAgainAfterVerification(t *testing.T) {
	t.Parallel()
	upgraded := domain.Digest("sha256:" + strings.Repeat("cd", 32))
	crashAfterVerification := productionCrashSeams{
		afterVerification: func() error { return errors.New("injected process loss") },
	}

	t.Run("first publication", func(t *testing.T) {
		t.Parallel()
		p := newProjectImageReuseHarness(t, nil, builtAtEarlierCommit)
		p.startAndRecordExport(t)
		p.workflow = p.newEngine(t, crashAfterVerification, true)
		if _, err := p.reconcileLanes(); err == nil {
			t.Fatal("the crash seam after verification did not interrupt the cycle")
		}
		runs := p.room.runs
		p.restartDurableState(t)
		p.preparationDigest = upgraded
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
			t.Fatalf("cycle resumed under an upgraded daemon = %#v, %v", result, err)
		}
		if p.room.runs != runs {
			t.Fatalf("the resumed cycle verified again: %d more runs", p.room.runs-runs)
		}
	})

	t.Run("re-entry", func(t *testing.T) {
		t.Parallel()
		p := newProjectImageReuseHarness(t, nil, nil)
		p.publishReady(t)
		advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
		if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
		}); !started {
			t.Fatal("a base advance started no re-entry")
		}
		p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
		p.workflow = p.newEngine(t, crashAfterVerification, true)
		if _, err := p.reconcileLanes(); err == nil {
			t.Fatal("the crash seam after verification did not interrupt the cycle")
		}
		runs := p.room.runs
		p.restartDurableState(t)
		p.scriptCleanReview(2, advanced, p.replay.HeadSHA)
		p.preparationDigest = upgraded
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
			t.Fatalf("re-entry resumed under an upgraded daemon = %#v, %v", result, err)
		}
		if p.room.runs != runs {
			t.Fatalf("the resumed re-entry verified again: %d more runs", p.room.runs-runs)
		}
	})

	// The same upgrade, met before verification, is refused: the control that
	// shows the digest above is one the image was not built with.
	t.Run("an unverified cycle is still judged", func(t *testing.T) {
		t.Parallel()
		p := newProjectImageReuseHarness(t, nil, builtAtEarlierCommit)
		p.startAndRecordExport(t)
		p.preparationDigest = upgraded
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		_, err := p.reconcileLanes()
		requireIncompatibleImageRefusal(t, err, "rebuild the image")
		if p.room.reads != 0 || p.room.runs != 0 {
			t.Fatalf("room reads/runs = %d/%d, want none before the refusal", p.room.reads, p.room.runs)
		}
	})
}
