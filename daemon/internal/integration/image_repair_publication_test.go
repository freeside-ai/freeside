package integration_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// submitImageRepairForPublication records the refusal h.repairRefusal names
// for image and submits its maintenance task as image upkeep does. It
// returns the task's specification run.
func submitImageRepairForPublication(
	t *testing.T, h *publicationHarness, service *signet.Service, image domain.ProjectImage, project domain.ProjectID,
) domain.RunID {
	t.Helper()
	refusal, err := domain.NewImageUpkeepRefusal(h.repairRefusal(image))
	if err != nil {
		t.Fatal(err)
	}
	// Upkeep measures only a project's served repository, so the project
	// is bound before its maintenance task exists.
	if err := h.store.Write(h.ctx, func(tx *store.WriteTx) error {
		return tx.RegisterProject(h.ctx, domain.Project{
			ID: project, Repo: h.profile.Repo, RepositoryID: h.profile.RepositoryID,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.WriteInternal(h.ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordImageUpkeepRefusal(h.ctx, refusal)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	submission, err := service.SubmitImageRepairTask(h.ctx, project, refusal.ID)
	if err != nil {
		t.Fatal(err)
	}
	return submission.SpecificationRunID
}

// repairRefusalOf is the repair refusal image upkeep records for image at the
// harness's base, the run's admitted base.
func repairRefusalOf(h *publicationHarness, image domain.ProjectImage) domain.ImageUpkeepRefusalInput {
	return domain.ImageUpkeepRefusalInput{
		RepositoryID: h.profile.RepositoryID, Repository: h.profile.Repo, Harness: domain.HarnessClientClaudeCode,
		TipSHA: h.baseSHA, MeasuredFromImageID: image.ID, Class: domain.ImageUpkeepRefusalRepair,
		Findings: []domain.ImageUpkeepClauseFinding{{
			Clause: domain.RebuildRefusalProofFailed, Detail: "the image's proof failed at the tip",
		}},
		RecordedAt: h.now,
	}
}

// imageRepairCase shapes a maintenance-task harness: the refusal it is
// submitted for, and the project image the run is admitted with.
type imageRepairCase struct {
	// refusal adjusts the refusal recorded for the admitted image.
	refusal func(*domain.ImageUpkeepRefusalInput)
	// unserved builds the admitted image one commit behind the base with
	// another builder's preparation, so it cannot serve the base.
	unserved bool
}

// newImageRepairPublicationHarness is a maintenance task's implementation run
// whose candidate carries files over a base with real npm manifests. The
// engine can rebuild unless the returned builder is detached with
// p.rebuild = nil.
func newImageRepairPublicationHarness(
	t *testing.T, files map[string]string, c imageRepairCase,
) (*productionPublicationHarness, *recordingImageBuilder) {
	t.Helper()
	h := newPublicationHarnessWithBaseFiles(
		t, []byte(`{"commands":[["/usr/bin/true"]],"capture":"none"}`),
		map[string]string{"package.json": rebuildBasePackageJSON, "package-lock.json": rebuildBasePackageLock})
	if c.unserved {
		built := h.baseSHA
		// The candidate replaces README.md, so its tree carries no trace of
		// the advance.
		writeFile(t, h.baseDir, "README.md", "advanced\n")
		runGit(t, h.baseDir, "add", "-A")
		runGit(t, h.baseDir, "commit", "-q", "-m", "advance past the image")
		h.baseSHA = runGit(t, h.baseDir, "rev-parse", "HEAD")
		h.audit.AuditedCommitSHA = h.baseSHA
		h.projectImageInput = func(input *domain.ProjectImageInput) {
			input.CommitSHA = built
			input.Environment.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("ab", 32))
		}
	}
	h.repairRefusal = func(image domain.ProjectImage) domain.ImageUpkeepRefusalInput {
		input := repairRefusalOf(h, image)
		if c.refusal != nil {
			c.refusal(&input)
		}
		return input
	}
	p := newProductionPublicationHarnessWithMetadata(t, h, "", nil, nil, files, engine.ProductionPublication{}, nil, "")
	// A submitted task publishes only the candidate's public account.
	p.replay = withPublicAccount(t, p, p.replay, publicAccount)
	builder := &recordingImageBuilder{p: p}
	p.rebuiltImages = map[domain.Digest]bool{}
	p.rebuild = &engine.ProjectImageRebuild{
		Builder: builder,
		Inputs:  projectimage.BuildInputs{BaseBuildRef: "freeside-agent-claude:local"},
	}
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	return p, builder
}

// A maintenance task at its refusal's tip, in the image the refusal was
// measured from, is verified only in an image built at its head, whether or
// not the admitted image serves the base. A head that keeps the manifests
// the image baked is still rebuilt.
func TestImageRepairTaskVerifiesInAnImageBuiltAtItsHead(t *testing.T) {
	t.Parallel()
	for name, unserved := range map[string]bool{"image serves the base": false, "image cannot serve the base": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, builder := newImageRepairPublicationHarness(t, nil, imageRepairCase{unserved: unserved})
			p.publishReady(t)
			images := p.recordedImages(t)
			if len(builder.requests) != 1 || builder.requests[0].CommitSHA != p.replay.HeadSHA ||
				len(images) != 2 || images[1].CommitSHA != p.replay.HeadSHA {
				t.Fatalf("builds = %#v, recorded images = %#v, want one built at the head", builder.requests, images)
			}
			if p.room.runs != 1 || p.room.images[len(p.room.images)-1] != images[1].ID ||
				slices.Contains(p.room.images[1:], p.image.ID) {
				t.Fatalf("room runs = %d over images %v, want one run in %s", p.room.runs, p.room.images, images[1].ID)
			}
			if got := p.verificationCheckpointImage(t); got != images[1].ID {
				t.Fatalf("checkpoint names image %s, want the rebuilt %s", got, images[1].ID)
			}
		})
	}
}

// A maintenance task whose rebuild is refused or fails its proof is blocked
// as a failed verification, and no command runs in the admitted image.
func TestImageRepairTaskBlocksWhenItsRebuildIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		configure func(*productionPublicationHarness, *recordingImageBuilder)
		clause    projectimage.RebuildRefusal
		builds    int
	}{
		"no builder": {
			configure: func(p *productionPublicationHarness, _ *recordingImageBuilder) { p.rebuild = nil },
			clause:    projectimage.RebuildRefusalNotConfigured,
		},
		"proof failed": {
			configure: func(_ *productionPublicationHarness, builder *recordingImageBuilder) {
				builder.fail = fmt.Errorf("offline preflight exited 1: %w", projectimage.ErrProofFailed)
			},
			clause: projectimage.RebuildRefusalProofFailed, builds: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, builder := newImageRepairPublicationHarness(t, nil, imageRepairCase{unserved: true})
			tc.configure(p, builder)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			p.startAndRecordExport(t)
			result, err := p.reconcileLanes()
			if err != nil || result.BlockedItemsCreated != 1 || result.ReadyItemsCreated != 0 {
				t.Fatalf("refused repair = %#v, %v", result, err)
			}
			item, evidence := p.blockedEvidenceText(t, domain.ProductionBlockedItemID(p.runID))
			if item.Reason != verificationBlocked || !strings.Contains(evidence, string(tc.clause)) {
				t.Fatalf("blocked item %q with evidence %q, want clause %s", item.Reason, evidence, tc.clause)
			}
			if p.room.runs != 0 || len(builder.requests) != tc.builds {
				t.Fatalf("a refused repair ran %d commands and built %d times", p.room.runs, len(builder.requests))
			}
		})
	}
}

// The exception needs the refusal's own image and tip. A maintenance task
// admitted in another image, or verified at another base, takes the ordinary
// checks: a head that keeps the image's manifests is verified in the
// admitted image.
func TestImageRepairExceptionNeedsTheRefusalsImageAndTip(t *testing.T) {
	t.Parallel()
	for name, adjust := range map[string]func(*domain.ImageUpkeepRefusalInput){
		"another image": func(input *domain.ImageUpkeepRefusalInput) {
			input.MeasuredFromImageID = domain.Digest("sha256:" + strings.Repeat("cd", 32))
		},
		"another tip": func(input *domain.ImageUpkeepRefusalInput) { input.TipSHA = strings.Repeat("c", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, builder := newImageRepairPublicationHarness(t, nil, imageRepairCase{refusal: adjust})
			p.publishReady(t)
			if len(builder.requests) != 0 || !slices.Equal(p.room.images, []domain.Digest{p.image.ID}) {
				t.Fatalf("built %d times and used rooms %v, want only the admitted image",
					len(builder.requests), p.room.images)
			}
		})
	}
}

// A re-entered cycle still at the refusal's tip takes the repair gate at the
// commit it verifies, so a head that keeps the manifests is verified in the
// image already rebuilt for them, never the admitted one. One at an advanced
// base takes the ordinary checks.
func TestImageRepairReentryTakesTheRepairGateOnlyAtTheTip(t *testing.T) {
	t.Parallel()
	p, builder := newImageRepairPublicationHarness(t, nil, imageRepairCase{})
	ready := p.publishReady(t)
	head := p.pushToPullRequest(t, "FOLLOWUP.md", "follow-up\n")
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: ready.PRHeadSHA, Observed: head,
	}); !started {
		t.Fatal("a head change started no re-entry")
	}
	p.scriptCleanReview(2, p.baseSHA, head)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("re-entry result = %#v, %v", result, err)
	}
	images := p.recordedImages(t)
	if len(builder.requests) != 1 || len(images) != 2 ||
		p.room.runs != 2 || p.room.images[len(p.room.images)-1] != images[1].ID {
		t.Fatalf("re-entry built %d times, recorded %d images, ran %d commands in rooms %v, want the rebuilt %s",
			len(builder.requests), len(images), p.room.runs, p.room.images, images[1].ID)
	}

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
	if len(builder.requests) != 1 || p.room.runs != 3 || p.room.images[len(p.room.images)-1] != p.image.ID {
		t.Fatalf("base-advance re-entry built %d times, ran %d commands in rooms %v, want the admitted image",
			len(builder.requests), p.room.runs, p.room.images)
	}
}
