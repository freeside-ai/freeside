package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
)

func rebuildTestImage(t *testing.T, edit func(*domain.ProjectImageInput)) domain.ProjectImage {
	t.Helper()
	input := domain.ProjectImageInput{
		Repository: "owner/repo", RepositoryID: 42, CommitSHA: strings.Repeat("a", 40),
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("1", 64)),
		PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef:       domain.ImageRef("ghcr.io/owner/base@sha256:" + strings.Repeat("c", 64)),
		ImageRef:           domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("d", 64)),
		Environment: &domain.ProjectImageEnvironment{
			PackageJSONSHA256: strings.Repeat("2", 64), PackageLockSHA256: strings.Repeat("3", 64),
			PreparationDigest: projectimage.PreparationDigest(),
		},
	}
	if edit != nil {
		edit(&input)
	}
	image, err := domain.NewProjectImage(input)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// The record a rebuild binds a run to is decoded state. Whatever names it, an
// image verifies a candidate only when it keeps everything the admitted image
// binds and bakes the manifests the verified commit holds, with the running
// binary's preparation. That holds at the image's own build commit too, which
// is where every rebuilt image is asked.
func TestRebuiltImageServesOnlyWhatTheAdmittedImageBinds(t *testing.T) {
	t.Parallel()
	admitted := rebuildTestImage(t, nil)
	head := domain.ProjectImageBaseInputs{
		CommitSHA:         strings.Repeat("b", 40),
		PackageJSONSHA256: strings.Repeat("4", 64), PackageLockSHA256: strings.Repeat("5", 64),
	}
	for where, builtAt := range map[string]string{
		"built at the verified commit": head.CommitSHA,
		// The environment is what an image serves, not the commit.
		"built at another commit with the same manifests": strings.Repeat("e", 40),
	} {
		rebuilt := func(edit func(*domain.ProjectImageInput)) domain.ProjectImage {
			return rebuildTestImage(t, func(input *domain.ProjectImageInput) {
				input.CommitSHA = builtAt
				input.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("f", 64))
				input.Environment = &domain.ProjectImageEnvironment{
					PackageJSONSHA256: head.PackageJSONSHA256, PackageLockSHA256: head.PackageLockSHA256,
					PreparationDigest: projectimage.PreparationDigest(),
				}
				if edit != nil {
					edit(input)
				}
			})
		}
		refused := map[string]domain.ProjectImage{
			"the admitted image itself": admitted,
			"another repository": rebuilt(func(input *domain.ProjectImageInput) {
				input.Repository = "owner/other"
			}),
			"another repository id": rebuilt(func(input *domain.ProjectImageInput) { input.RepositoryID = 43 }),
			"another recipe": rebuilt(func(input *domain.ProjectImageInput) {
				input.RecipeDigest = domain.Digest("sha256:" + strings.Repeat("9", 64))
			}),
			"another base image": rebuilt(func(input *domain.ProjectImageInput) {
				input.BaseImageRef = domain.ImageRef("ghcr.io/owner/base@sha256:" + strings.Repeat("8", 64))
			}),
			// The room runs the preparation command first, as the workspace's
			// owner: only the fixed helper the builder bakes is ever run.
			"another preparation command": rebuilt(func(input *domain.ProjectImageInput) {
				input.PreparationCommand = []string{"/bin/sh", "-c", "true"}
			}),
			"other manifests": rebuilt(func(input *domain.ProjectImageInput) {
				input.Environment.PackageLockSHA256 = strings.Repeat("7", 64)
			}),
			// What a daemon upgrade leaves behind: an image an earlier binary
			// built with other toolchain and preparation sources.
			"another preparation helper": rebuilt(func(input *domain.ProjectImageInput) {
				input.Environment.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("6", 64))
			}),
			"no recorded environment": rebuilt(func(input *domain.ProjectImageInput) { input.Environment = nil }),
			"a tampered record": func() domain.ProjectImage {
				image := rebuilt(nil)
				image.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("0", 64))
				return image
			}(),
		}
		t.Run(where, func(t *testing.T) {
			t.Parallel()
			if err := rebuiltImageServes(rebuilt(nil), admitted, head, projectimage.PreparationDigest()); err != nil {
				t.Fatalf("an image baking the head's manifests was refused: %v", err)
			}
			for name, image := range refused {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					if err := rebuiltImageServes(image, admitted, head, projectimage.PreparationDigest()); err == nil {
						t.Fatal("the image was accepted")
					}
				})
			}
		})
	}
	unsupported := head
	unsupported.UnsupportedInputs = []string{".npmrc"}
	elsewhere := rebuildTestImage(t, func(input *domain.ProjectImageInput) {
		input.CommitSHA = strings.Repeat("e", 40)
		input.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("f", 64))
		input.Environment = &domain.ProjectImageEnvironment{
			PackageJSONSHA256: head.PackageJSONSHA256, PackageLockSHA256: head.PackageLockSHA256,
			PreparationDigest: projectimage.PreparationDigest(),
		}
	})
	if err := rebuiltImageServes(elsewhere, admitted, unsupported, projectimage.PreparationDigest()); !errors.Is(err, domain.ErrProjectImageIncompatible) {
		t.Fatalf("a head with installation configuration = %v, want incompatible", err)
	}
}

// Every clause a person can be shown has its own sentence; none falls through
// to the fallback an invalid clause gets.
func TestRebuildRefusalMeaningCoversEveryClause(t *testing.T) {
	t.Parallel()
	fallback := rebuildRefusalMeaning("")
	seen := map[string]projectimage.RebuildRefusal{}
	for _, clause := range projectimage.AllRebuildRefusals {
		meaning := rebuildRefusalMeaning(clause)
		if meaning == fallback {
			t.Errorf("clause %s has no meaning of its own", clause)
		}
		if other, duplicate := seen[meaning]; duplicate {
			t.Errorf("clauses %s and %s share a meaning", clause, other)
		}
		seen[meaning] = clause
		account := rebuildRefusal{clause: clause, detail: "the detail"}.account()
		if !strings.HasPrefix(account, string(clause)+". "+meaning) || !strings.Contains(account, "the detail") {
			t.Errorf("account of %s = %q", clause, account)
		}
	}
}

// stallingImageBuilder is a build that ends only when its context does, as a
// proof running a candidate's hanging test would, or that fails at once.
type stallingImageBuilder struct {
	fail error
}

func (b stallingImageBuilder) Build(ctx context.Context, _ projectimage.Request) (domain.ProjectImage, error) {
	if b.fail != nil {
		return domain.ProjectImage{}, b.fail
	}
	<-ctx.Done()
	// What the real builder reports when a proof command is killed.
	return domain.ProjectImage{}, errors.Join(projectimage.ErrProofFailed, ctx.Err())
}

func TestProjectImageRebuildBoundsOneBuild(t *testing.T) {
	t.Parallel()
	t.Run("the bound ends a build that does not finish", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			rebuild := &ProjectImageRebuild{Builder: stallingImageBuilder{}}
			started := time.Now()
			_, timedOut, err := rebuild.build(t.Context(), projectimage.Request{})
			if !timedOut || err == nil || !strings.Contains(err.Error(), "1h0m0s") {
				t.Fatalf("timedOut = %t, err = %v; want the default bound's verdict", timedOut, err)
			}
			if waited := time.Since(started); waited != defaultProjectImageRebuildTimeout {
				t.Fatalf("build ended after %s, want %s", waited, defaultProjectImageRebuildTimeout)
			}
		})
	})
	t.Run("a configured bound replaces the default", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			rebuild := &ProjectImageRebuild{Builder: stallingImageBuilder{}, Timeout: time.Minute}
			started := time.Now()
			if _, timedOut, _ := rebuild.build(t.Context(), projectimage.Request{}); !timedOut ||
				time.Since(started) != time.Minute {
				t.Fatalf("timedOut = %t after %s, want the bound at one minute", timedOut, time.Since(started))
			}
		})
	})
	// A cancelled pass kills a running proof too. The proof failure the
	// builder then reports says nothing about the candidate.
	t.Run("a cancelled pass is not a verdict", func(t *testing.T) {
		t.Parallel()
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			time.AfterFunc(time.Second, cancel)
			rebuild := &ProjectImageRebuild{Builder: stallingImageBuilder{}}
			_, timedOut, err := rebuild.build(ctx, projectimage.Request{})
			if timedOut || !errors.Is(err, context.Canceled) || errors.Is(err, projectimage.ErrProofFailed) {
				t.Fatalf("timedOut = %t, err = %v; want the pass's cancellation alone", timedOut, err)
			}
		})
	})
	t.Run("a build's own failure passes through", func(t *testing.T) {
		t.Parallel()
		for _, fault := range []error{projectimage.ErrProofFailed, errors.New("registry unreachable")} {
			rebuild := &ProjectImageRebuild{Builder: stallingImageBuilder{fail: fault}}
			if _, timedOut, err := rebuild.build(t.Context(), projectimage.Request{}); timedOut || !errors.Is(err, fault) {
				t.Fatalf("timedOut = %t, err = %v; want %v", timedOut, err, fault)
			}
		}
	})
}

func TestRebuiltImageKeepsAdmittedNeedsNoHead(t *testing.T) {
	t.Parallel()
	admitted := rebuildTestImage(t, nil)
	rebuilt := rebuildTestImage(t, func(input *domain.ProjectImageInput) {
		input.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("e", 64))
	})
	if err := rebuiltImageKeepsAdmitted(rebuilt, admitted); err != nil {
		t.Fatalf("an image that keeps what the admitted one binds: %v", err)
	}
	other := rebuildTestImage(t, func(input *domain.ProjectImageInput) {
		input.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("e", 64))
		input.RecipeDigest = domain.Digest("sha256:" + strings.Repeat("9", 64))
	})
	for name, image := range map[string]domain.ProjectImage{"the admitted image": admitted, "another recipe": other} {
		if err := rebuiltImageKeepsAdmitted(image, admitted); !errors.Is(err, domain.ErrProjectImageIncompatible) {
			t.Fatalf("%s: err = %v, want ErrProjectImageIncompatible", name, err)
		}
	}
}
