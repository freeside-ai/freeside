package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	rebuildBasePackageJSON = `{"name":"fixture","private":true,"dependencies":{"left":"^1.0.0"}}` + "\n"
	rebuildHeadPackageJSON = `{"name":"fixture","private":true,"dependencies":{"left":"^1.0.0","right":"^2.0.0"}}` + "\n"
	rebuildDeclaredSet     = `["registry.npmjs.org"]`
	verificationBlocked    = "Verification or current policy findings blocked production publication."
)

// rebuildLeftPackage is the base's one locked package. Its integrity value has
// the shape of a sha512 pin; the gate reads the shape and fetches nothing.
var rebuildLeftPackage = `"node_modules/left":{"version":"1.0.0","resolved":"https://registry.npmjs.org/left/-/left-1.0.0.tgz","integrity":"sha512-` +
	strings.Repeat("A", 86) + `=="}`

var rebuildBasePackageLock = `{"name":"fixture","lockfileVersion":3,"packages":{` +
	`"":{"name":"fixture","dependencies":{"left":"^1.0.0"}},` + rebuildLeftPackage + `}}` + "\n"

// rebuildHeadPackageLock is the candidate's lockfile: the base's, plus one
// package resolved from host.
func rebuildHeadPackageLock(host string) string {
	return `{"name":"fixture","lockfileVersion":3,"packages":{` +
		`"":{"name":"fixture","dependencies":{"left":"^1.0.0","right":"^2.0.0"}},` +
		rebuildLeftPackage + `,` +
		`"node_modules/right":{"version":"2.0.0","resolved":"https://` + host + `/right/-/right-2.0.0.tgz","integrity":"sha512-` +
		strings.Repeat("B", 86) + `=="}}}` + "\n"
}

// rebuildCandidate is a candidate that adds one dependency from host.
func rebuildCandidate(host string) map[string]string {
	return map[string]string{
		"package.json": rebuildHeadPackageJSON, "package-lock.json": rebuildHeadPackageLock(host),
	}
}

// recordingImageBuilder stands in for the project-image builder: it records
// an image for the requested commit as the real one does, from the manifests
// that commit holds in the checkout it was handed. A source that does not
// hold the commit fails the build.
type recordingImageBuilder struct {
	p        *productionPublicationHarness
	requests []projectimage.Request
	// fail, while set, is returned in place of a build.
	fail error
	// stall makes a build last until its context ends, as a proof running a
	// candidate's hanging test would.
	stall bool
	// afterRecord, when set, replaces a successful build's return: the image
	// is recorded and the process is lost before the engine hears of it.
	afterRecord error
}

func (b *recordingImageBuilder) Build(
	ctx context.Context, request projectimage.Request,
) (domain.ProjectImage, error) {
	b.requests = append(b.requests, request)
	if b.fail != nil {
		return domain.ProjectImage{}, b.fail
	}
	if b.stall {
		<-ctx.Done()
		return domain.ProjectImage{}, ctx.Err()
	}
	inputs, err := projectimage.ObserveBaseInputs(ctx, "git", request.SourceDir, request.CommitSHA)
	if err != nil {
		return domain.ProjectImage{}, err
	}
	sum := sha256.Sum256([]byte(request.CommitSHA))
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: request.Repository, RepositoryID: request.RepositoryID,
		CommitSHA: request.CommitSHA, RecipeDigest: verify.RecipeDigest(request.Recipe),
		PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef:       request.BaseImageRef,
		ImageRef: domain.ImageRef(fmt.Sprintf("%s/%s@sha256:%s",
			request.Registry, request.ImageName, hex.EncodeToString(sum[:]))),
		Environment: &domain.ProjectImageEnvironment{
			PackageJSONSHA256: inputs.PackageJSONSHA256, PackageLockSHA256: inputs.PackageLockSHA256,
			PreparationDigest: projectimage.PreparationDigest(),
		},
	})
	if err != nil {
		return domain.ProjectImage{}, err
	}
	if err := b.p.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordProjectImage(ctx, image)
	}); err != nil {
		return domain.ProjectImage{}, err
	}
	b.p.rebuiltImages[image.ID] = true
	if b.afterRecord != nil {
		return domain.ProjectImage{}, b.afterRecord
	}
	return image, nil
}

// newProjectImageRebuildHarness is a run whose candidate carries files over a
// base with real npm manifests, under a policy that declares registrySet
// (none when empty). The engine can rebuild unless the returned builder is
// detached with p.rebuild = nil.
func newProjectImageRebuildHarness(
	t *testing.T, files map[string]string, registrySet string,
) (*productionPublicationHarness, *recordingImageBuilder) {
	t.Helper()
	h := newPublicationHarnessWithBaseFiles(
		t, []byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`),
		map[string]string{"package.json": rebuildBasePackageJSON, "package-lock.json": rebuildBasePackageLock})
	var keys []domain.PolicyKey
	if registrySet != "" {
		keys = append(keys, domain.PolicyKey{
			Key: domain.RegistrySetPolicyKey, Value: registrySet,
			Provenance: domain.KeyProvenance{
				Source: domain.ProvenanceOverride,
				Digest: submissionDigest("run-production-publication", "registry-set-source"),
			},
		})
	}
	p := newProductionPublicationHarnessFromBase(t, h, "", keys, nil, files)
	builder := &recordingImageBuilder{p: p}
	p.rebuiltImages = map[domain.Digest]bool{}
	p.rebuild = &engine.ProjectImageRebuild{
		Builder: builder,
		Inputs:  projectimage.BuildInputs{BaseBuildRef: "freeside-agent-claude:local"},
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	return p, builder
}

// recordedImages lists the repository's project-image records.
func (p *productionPublicationHarness) recordedImages(t *testing.T) []domain.ProjectImage {
	t.Helper()
	var images []domain.ProjectImage
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		images, err = tx.ListProjectImages(p.ctx, p.image.RepositoryID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return images
}

// verificationCheckpointImage is the image the first cycle's verification
// checkpoint names.
func (p *productionPublicationHarness) verificationCheckpointImage(t *testing.T) domain.Digest {
	t.Helper()
	var payload []byte
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetInbox(p.ctx, "production-verification/"+string(p.runID)+"/"+p.replay.HeadSHA)
		payload = entry.Payload
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var checkpoint struct {
		ProjectImage domain.Digest `json:"project_image"`
	}
	if err := json.Unmarshal(payload, &checkpoint); err != nil {
		t.Fatal(err)
	}
	return checkpoint.ProjectImage
}

// blockedEvidenceText returns the blocked item and the text of its one
// evidence artifact.
func (p *productionPublicationHarness) blockedEvidenceText(
	t *testing.T, itemID domain.ItemID,
) (domain.AttentionItem, string) {
	t.Helper()
	blocked, err := p.attention.GetAttentionItem(p.ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if len(blocked.Item.EvidenceSnapshot) != 1 {
		t.Fatalf("blocked item evidence = %#v, want one artifact", blocked.Item.EvidenceSnapshot)
	}
	reader, err := p.blobs.Open(blocked.Item.EvidenceSnapshot[0].Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close() //nolint:errcheck // test read
	text, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return blocked.Item, string(text)
}

// A dependency change inside the declared registry set is rebuilt from the
// daemon's own checkout at the verified commit, verified in the rebuilt image,
// and published with no item for a person.
func TestProductionPublicationRebuildsProjectImageForDeclaredDependencyChange(t *testing.T) {
	t.Parallel()
	p, builder := newProjectImageRebuildHarness(t, rebuildCandidate("registry.npmjs.org"), rebuildDeclaredSet)
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil || result.ReadyItemsCreated != 1 || result.BlockedItemsCreated != 0 {
		t.Fatalf("rebuilt cycle = %#v, %v", result, err)
	}
	if len(builder.requests) != 1 {
		t.Fatalf("builds = %d, want 1", len(builder.requests))
	}
	request := builder.requests[0]
	if request.CommitSHA != p.replay.HeadSHA || request.SourceDir == "" ||
		request.Repository != p.image.Repository || request.RepositoryID != p.image.RepositoryID ||
		request.BaseImageRef != p.image.BaseImageRef ||
		verify.RecipeDigest(request.Recipe) != p.image.RecipeDigest ||
		request.BaseBuildRef != "freeside-agent-claude:local" {
		t.Fatalf("build request = %#v", request)
	}
	images := p.recordedImages(t)
	if len(images) != 2 || images[1].CommitSHA != p.replay.HeadSHA || images[1].ID == p.image.ID {
		t.Fatalf("recorded images = %#v, want the admitted one and one built at the head", images)
	}
	rebuilt := images[1]
	// The admitted image served only to read the recipe a rebuild keeps; the
	// one room that ran commands was the rebuilt image's.
	if p.room.runs != 1 || !slices.Equal(p.room.images, []domain.Digest{p.image.ID, rebuilt.ID}) {
		t.Fatalf("room runs = %d over images %v, want one run in %s", p.room.runs, p.room.images, rebuilt.ID)
	}
	if got := p.verificationCheckpointImage(t); got != rebuilt.ID {
		t.Fatalf("checkpoint names image %s, want the rebuilt %s", got, rebuilt.ID)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d PRs, want 1 and 1", refs, prs)
	}
	// A later pass holds its evidence: it neither builds nor verifies again.
	if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
		t.Fatalf("pass after publication = %#v, %v", result, err)
	}
	if len(builder.requests) != 1 || p.room.runs != 1 {
		t.Fatalf("a settled cycle built %d times and ran %d commands", len(builder.requests), p.room.runs)
	}
}

// A head that keeps the manifests the image baked takes no gate: nothing is
// built, no rebuild row is written, and the run needs no registry set.
func TestProductionPublicationTakesNoRebuildGateForUnchangedManifests(t *testing.T) {
	t.Parallel()
	p, builder := newProjectImageRebuildHarness(t, nil, "")
	p.publishReady(t)
	if len(builder.requests) != 0 || !slices.Equal(p.room.images, []domain.Digest{p.image.ID}) {
		t.Fatalf("an unchanged head built %d times and used rooms %v", len(builder.requests), p.room.images)
	}
	if got := p.verificationCheckpointImage(t); got != p.image.ID {
		t.Fatalf("checkpoint names image %s, want the admitted %s", got, p.image.ID)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		for _, kind := range []string{"production-rebuild-intent", "production-rebuild-image"} {
			if _, err := tx.GetInbox(p.ctx, kind+"/"+string(p.runID)+"/"+p.replay.HeadSHA); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("an unchanged head wrote a %s row: %v", kind, err)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Every clause of the gate blocks publication on the existing fail-loud item,
// names itself in the item's evidence, and builds no room.
func TestProductionPublicationBlocksDependencyChangeOutsidePolicy(t *testing.T) {
	t.Parallel()
	declared := rebuildCandidate("registry.npmjs.org")
	with := func(extra map[string]string) map[string]string {
		files := rebuildCandidate("registry.npmjs.org")
		for name, body := range extra {
			files[name] = body
		}
		return files
	}
	for name, tc := range map[string]struct {
		files       map[string]string
		registrySet string
		configure   func(*productionPublicationHarness, *recordingImageBuilder)
		clause      projectimage.RebuildRefusal
		builds      int
		// reads counts recipe reads: a build needs the admitted image's recipe.
		reads int
	}{
		"undeclared registry host": {
			files: rebuildCandidate("registry.example.com"), registrySet: rebuildDeclaredSet,
			clause: projectimage.RebuildRefusalUndeclaredAuthority,
		},
		"git source": {
			files: map[string]string{
				"package.json": rebuildHeadPackageJSON,
				"package-lock.json": strings.Replace(rebuildHeadPackageLock("registry.npmjs.org"),
					"https://registry.npmjs.org/right/-/right-2.0.0.tgz",
					"git+https://registry.npmjs.org/right.git#0123456", 1),
			},
			registrySet: rebuildDeclaredSet, clause: projectimage.RebuildRefusalUnpinnedSource,
		},
		// npm satisfies a dependency declared by URL only from that URL, so it
		// would ignore the declared host this lockfile pins the package to.
		"dependency declared by URL": {
			files: map[string]string{
				"package.json": strings.Replace(rebuildHeadPackageJSON,
					`"right":"^2.0.0"`, `"right":"https://registry.example.com/right.tgz"`, 1),
				"package-lock.json": strings.Replace(rebuildHeadPackageLock("registry.npmjs.org"),
					`"right":"^2.0.0"`, `"right":"https://registry.example.com/right.tgz"`, 1),
			},
			registrySet: rebuildDeclaredSet, clause: projectimage.RebuildRefusalUnpinnedSource,
		},
		"recipe changed": {
			files:       with(map[string]string{".freeside/verify.json": `{"commands":[["/usr/bin/false"]],"capture":"none"}`}),
			registrySet: rebuildDeclaredSet, clause: projectimage.RebuildRefusalRecipeChanged,
		},
		"lockfile out of step with package.json": {
			files:       map[string]string{"package.json": rebuildHeadPackageJSON},
			registrySet: rebuildDeclaredSet, clause: projectimage.RebuildRefusalLockfileInconsistent,
		},
		"installation configuration": {
			files:       with(map[string]string{".npmrc": "registry=https://registry.example.com/\n"}),
			registrySet: rebuildDeclaredSet, clause: projectimage.RebuildRefusalUnsupportedInput,
		},
		"no registry set": {
			files: declared, clause: projectimage.RebuildRefusalNoRegistrySet,
		},
		"rebuild not configured": {
			files: declared, registrySet: rebuildDeclaredSet,
			configure: func(p *productionPublicationHarness, _ *recordingImageBuilder) { p.rebuild = nil },
			clause:    projectimage.RebuildRefusalNotConfigured,
		},
		"proof failed": {
			files: declared, registrySet: rebuildDeclaredSet,
			configure: func(_ *productionPublicationHarness, builder *recordingImageBuilder) {
				builder.fail = fmt.Errorf("offline preflight exited 1: %w", projectimage.ErrProofFailed)
			},
			clause: projectimage.RebuildRefusalProofFailed, builds: 1, reads: 1,
		},
		// The stalled build returns only when the bound ends it, so the short
		// bound sets how long the case takes and never which way it goes.
		"build did not finish": {
			files: declared, registrySet: rebuildDeclaredSet,
			configure: func(p *productionPublicationHarness, builder *recordingImageBuilder) {
				builder.stall = true
				p.rebuild.Timeout = time.Millisecond
			},
			clause: projectimage.RebuildRefusalProofFailed, builds: 1, reads: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, builder := newProjectImageRebuildHarness(t, tc.files, tc.registrySet)
			if tc.configure != nil {
				tc.configure(p, builder)
				p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			}
			p.startAndRecordExport(t)
			result, err := p.reconcileLanes()
			if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 ||
				result.PublicationTasksCompleted != 1 {
				t.Fatalf("refused cycle = %#v, %v", result, err)
			}
			blocked, text := p.blockedEvidenceText(t, domain.ProductionBlockedItemID(p.runID))
			if blocked.Type != domain.AttentionPublishBlocked || blocked.Reason != verificationBlocked ||
				blocked.PublishBlock == nil || blocked.PublishBlock.TrustRule == nil ||
				*blocked.PublishBlock.TrustRule != domain.TrustRuleVerificationFailed {
				t.Fatalf("blocked item = %#v", blocked)
			}
			if !strings.HasPrefix(text, "Project image rebuild refused: "+string(tc.clause)+". ") {
				t.Fatalf("evidence does not name clause %s:\n%s", tc.clause, text)
			}
			if p.room.runs != 0 || p.room.reads != tc.reads || len(builder.requests) != tc.builds {
				t.Fatalf("refused gate ran %d commands, read %d recipes, built %d times",
					p.room.runs, p.room.reads, len(builder.requests))
			}
			if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
				t.Fatalf("refused gate caused effects: %d refs, %d PRs", refs, prs)
			}
			if images := p.recordedImages(t); len(images) != 1 {
				t.Fatalf("refused gate recorded images: %#v", images)
			}
			// The block is terminal for the task, and it survives a restart.
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if result, err := p.reconcileLanes(); err != nil || result != (engine.ReconcileResult{}) {
				t.Fatalf("pass after the block = %#v, %v", result, err)
			}
			if len(builder.requests) != tc.builds {
				t.Fatalf("a blocked task built again: %d builds", len(builder.requests))
			}
		})
	}
}

// A builder failure that is not a proof failure is the build's environment,
// not a verdict: the task stays queued and the next pass builds again.
func TestProductionPublicationRetriesRebuildAfterBuilderFault(t *testing.T) {
	t.Parallel()
	p, builder := newProjectImageRebuildHarness(t, rebuildCandidate("registry.npmjs.org"), rebuildDeclaredSet)
	builder.fail = errors.New("registry connection reset")
	p.startAndRecordExport(t)
	result, err := p.reconcileLanes()
	if err != nil || result.BlockedItemsCreated != 0 || result.ReadyItemsCreated != 0 ||
		result.PublicationTasksCompleted != 0 {
		t.Fatalf("faulted build = %#v, %v, want the task left queued", result, err)
	}
	if _, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionBlockedItemID(p.runID)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a builder fault raised an item: %v", err)
	}
	if p.room.runs != 0 {
		t.Fatalf("a faulted build ran %d verification commands", p.room.runs)
	}
	builder.fail = nil
	p.now = p.now.Add(time.Hour)
	p.reconcileUntilReady(t, 4)
	if len(builder.requests) != 2 || len(p.recordedImages(t)) != 2 {
		t.Fatalf("builds = %d, recorded images = %d, want 2 and 2",
			len(builder.requests), len(p.recordedImages(t)))
	}
}

// A process lost after the build recorded its image and before the run was
// bound to it finds that image on restart. It is not built a second time.
func TestProductionPublicationRebuildConvergesAfterRestart(t *testing.T) {
	t.Parallel()
	p, builder := newProjectImageRebuildHarness(t, rebuildCandidate("registry.npmjs.org"), rebuildDeclaredSet)
	builder.afterRecord = errors.New("injected process loss")
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("interrupted build = %#v, %v", result, err)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		key := string(p.runID) + "/" + p.replay.HeadSHA
		if _, err := tx.GetInbox(p.ctx, "production-rebuild-intent/"+key); err != nil {
			t.Errorf("the build started with no durable intent: %v", err)
		}
		if _, err := tx.GetInbox(p.ctx, "production-rebuild-image/"+key); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("an interrupted build bound the run: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p.restartDurableState(t)
	builder.afterRecord = nil
	p.now = p.now.Add(time.Hour)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.reconcileUntilReady(t, 4)
	images := p.recordedImages(t)
	if len(builder.requests) != 1 || len(images) != 2 {
		t.Fatalf("builds = %d, recorded images = %d, want 1 and 2", len(builder.requests), len(images))
	}
	if got := p.verificationCheckpointImage(t); got != images[1].ID {
		t.Fatalf("checkpoint names image %s, want the recorded %s", got, images[1].ID)
	}
}

// A candidate blocked only because the daemon could not rebuild is rerun once
// it can. The block is an ordinary failed verification, so it reruns as one
// does, after a revised trust profile; the rerun of the same run and head then
// rebuilds, verifies, and publishes.
func TestProductionPublicationRerunRebuildsOnceConfigured(t *testing.T) {
	t.Parallel()
	p, builder := newProjectImageRebuildHarness(t, rebuildCandidate("registry.npmjs.org"), rebuildDeclaredSet)
	rebuild := p.rebuild
	p.rebuild = nil
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.startAndRecordExport(t)
	if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 1 {
		t.Fatalf("unconfigured cycle = %#v, %v", result, err)
	}
	original, err := p.attention.GetAttentionItem(p.ctx, domain.ProductionBlockedItemID(p.runID))
	if err != nil {
		t.Fatal(err)
	}
	p.rebuild = rebuild
	repairPublicationTrustProfile(t, p)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	submitPublicationRerun(t, p, original, "rerun-after-rebuild-configured")
	p.reconcileUntilReady(t, 4)
	images := p.recordedImages(t)
	if len(builder.requests) != 1 || len(images) != 2 || p.room.runs != 1 ||
		p.room.images[len(p.room.images)-1] != images[1].ID {
		t.Fatalf("rerun built %d times, recorded %d images, ran %d commands in rooms %v",
			len(builder.requests), len(images), p.room.runs, p.room.images)
	}
	if refs, prs := p.forge.counts(); refs != 1 || prs != 1 {
		t.Fatalf("publication effects = %d refs, %d PRs, want 1 and 1", refs, prs)
	}
}

// pushDependencyChange pushes a commit to the pull request that adds one
// dependency resolved from host.
func (p *productionPublicationHarness) pushDependencyChange(t *testing.T, host string) string {
	t.Helper()
	return p.pushCommitToPullRequest(t, func(dir string) {
		for name, body := range rebuildCandidate(host) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	})
}

// A re-entered cycle takes the same gate at the commit it verifies.
func TestReadinessReentryAppliesRebuildGate(t *testing.T) {
	t.Parallel()

	t.Run("a pushed dependency change within policy is rebuilt", func(t *testing.T) {
		t.Parallel()
		p, builder := newProjectImageRebuildHarness(t, nil, rebuildDeclaredSet)
		ready := p.publishReady(t)
		head := p.pushDependencyChange(t, "registry.npmjs.org")
		superseded, started := p.invalidateReady(t, domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationHeadChanged, Bound: ready.PRHeadSHA, Observed: head,
		})
		if !started {
			t.Fatal("a head change started no re-entry")
		}
		p.scriptCleanReview(2, p.baseSHA, head)
		if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
			t.Fatalf("re-entry result = %#v, %v", result, err)
		}
		p.assertReentered(t, superseded, 2, p.baseSHA, head)
		images := p.recordedImages(t)
		if len(builder.requests) != 1 || builder.requests[0].CommitSHA != head ||
			len(images) != 2 || images[1].CommitSHA != head {
			t.Fatalf("re-entry builds = %#v, recorded images = %#v", builder.requests, images)
		}
		if p.room.runs != 2 || p.room.images[len(p.room.images)-1] != images[1].ID {
			t.Fatalf("re-entry ran %d commands in rooms %v, want the last in %s",
				p.room.runs, p.room.images, images[1].ID)
		}

		// The base then advances under the rebuilt head. The merge keeps the
		// head's manifests, so the image already recorded for them serves it.
		advanced := p.advanceBase(t, "UPSTREAM.md", "upstream change\n")
		if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationBaseAdvanced, Bound: p.baseSHA, Observed: advanced,
		}); !started {
			t.Fatal("a base advance started no re-entry")
		}
		p.scriptCleanReview(3, advanced, head)
		if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
			t.Fatalf("base-advance re-entry result = %#v, %v", result, err)
		}
		if len(builder.requests) != 1 || len(p.recordedImages(t)) != 2 ||
			p.room.runs != 3 || p.room.images[len(p.room.images)-1] != images[1].ID {
			t.Fatalf("base-advance re-entry built %d times, recorded %d images, ran %d commands in rooms %v",
				len(builder.requests), len(p.recordedImages(t)), p.room.runs, p.room.images)
		}
	})

	t.Run("a pushed dependency change outside policy stops the cycle", func(t *testing.T) {
		t.Parallel()
		p, builder := newProjectImageRebuildHarness(t, nil, rebuildDeclaredSet)
		ready := p.publishReady(t)
		head := p.pushDependencyChange(t, "registry.example.com")
		if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationHeadChanged, Bound: ready.PRHeadSHA, Observed: head,
		}); !started {
			t.Fatal("a head change started no re-entry")
		}
		reads, runs := p.room.reads, p.room.runs
		result, err := p.reconcileLanes()
		if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
			t.Fatalf("refused re-entry = %#v, %v", result, err)
		}
		stop := p.reentryStopItem(t)
		if !strings.Contains(stop.Reason, string(projectimage.RebuildRefusalUndeclaredAuthority)) ||
			!strings.Contains(stop.Reason, head) || !strings.Contains(stop.Reason, "registry.example.com") {
			t.Fatalf("stop item = %#v", stop)
		}
		if p.room.reads != reads || p.room.runs != runs || len(builder.requests) != 0 {
			t.Fatalf("a refused gate read %d recipes, ran %d commands, built %d times",
				p.room.reads-reads, p.room.runs-runs, len(builder.requests))
		}
		if result, err := p.reconcileLanes(); err != nil || result.BlockedItemsCreated != 0 {
			t.Fatalf("pass after the stop = %#v, %v", result, err)
		}
	})
}
