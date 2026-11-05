package integration_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
	if refs, prs := p.forge.Counts(); refs != 1 || prs != 1 {
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
			if refs, prs := p.forge.Counts(); refs != 0 || prs != 0 {
				t.Fatalf("refused image caused effects: %d refs, %d PRs", refs, prs)
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
		pushed := p.pushToPullRequest(t, "FOLLOWUP.md", "pushed after readiness\n")
		if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: pushed,
		}); !started {
			t.Fatal("a head change started no re-entry")
		}
		p.scriptCleanReview(2, p.baseSHA, pushed)
		p.workflow = p.newEngine(t, crashAfterVerification, true)
		if _, err := p.reconcileLanes(); err == nil {
			t.Fatal("the crash seam after verification did not interrupt the cycle")
		}
		runs := p.room.runs
		p.restartDurableState(t)
		p.scriptCleanReview(2, p.baseSHA, pushed)
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
