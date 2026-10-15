package integration_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
)

const (
	realRunImageBuildSHA = "e781a0180000000000000000000000000000e781"
	realRunImageBaseSHA  = "7ddba3440000000000000000000000000000d7dd"
)

// realRunImageFixture is an admitted record built at realRunImageBuildSHA by
// this binary's preparation sources, with environment evidence unless
// legacy is set.
func realRunImageFixture(t *testing.T, legacy bool) domain.ProjectImage {
	t.Helper()
	input := domain.ProjectImageInput{
		Repository: "freeasinbird/gh-imgup", RepositoryID: 1278475858,
		CommitSHA:          realRunImageBuildSHA,
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("ef", 32)),
		PreparationCommand: []string{projectimage.PreparationPath},
		BaseImageRef:       domain.ImageRef("ghcr.io/freeside-ai/agent-claude@sha256:" + strings.Repeat("ab", 32)),
		ImageRef:           domain.ImageRef("127.0.0.1:5100/freeside-project-freeasinbird-gh-imgup@sha256:" + strings.Repeat("cd", 32)),
	}
	if !legacy {
		input.Environment = &domain.ProjectImageEnvironment{
			PackageJSONSHA256: strings.Repeat("12", 32),
			PackageLockSHA256: strings.Repeat("34", 32),
			PreparationDigest: projectimage.PreparationDigest(),
		}
	}
	image, err := domain.NewProjectImage(input)
	if err != nil {
		t.Fatalf("NewProjectImage: %v", err)
	}
	return image
}

// realRunImageEnv is the run authority the fixture was onboarded under, at a
// base two commits past the build.
func realRunImageEnv(image domain.ProjectImage) realRunEnv {
	return realRunEnv{
		agentImage: image.ImageRef, repo: image.Repository, repositoryID: image.RepositoryID,
		baseSHA: realRunImageBaseSHA, approvedRecipe: image.RecipeDigest,
	}
}

// unchangedBaseInputs are the base's inputs when the source-only commits
// since the build left every environment input as the image baked it.
func unchangedBaseInputs() domain.ProjectImageBaseInputs {
	return domain.ProjectImageBaseInputs{
		CommitSHA:         realRunImageBaseSHA,
		PackageJSONSHA256: strings.Repeat("12", 32),
		PackageLockSHA256: strings.Repeat("34", 32),
	}
}

func observeOnce(inputs domain.ProjectImageBaseInputs, err error) (func() (domain.ProjectImageBaseInputs, error), *int) {
	calls := 0
	return func() (domain.ProjectImageBaseInputs, error) {
		calls++
		return inputs, err
	}, &calls
}

func TestRealRunProjectImageAdmitsReuseAcrossSourceOnlyCommits(t *testing.T) {
	t.Parallel()
	image := realRunImageFixture(t, false)
	observe, calls := observeOnce(unchangedBaseInputs(), nil)
	if err := checkRealRunProjectImage(image, realRunImageEnv(image), observe, projectimage.PreparationDigest()); err != nil {
		t.Fatalf("image built before an unchanged-environment base was refused: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("observed the base %d times, want once", *calls)
	}
}

func TestRealRunProjectImageAdmitsBuildAtBaseWithoutCheckout(t *testing.T) {
	t.Parallel()
	for _, legacy := range []bool{false, true} {
		image := realRunImageFixture(t, legacy)
		env := realRunImageEnv(image)
		env.baseSHA = image.CommitSHA
		observe := func() (domain.ProjectImageBaseInputs, error) {
			t.Fatal("an image built at the base must be admitted from the record alone")
			return domain.ProjectImageBaseInputs{}, nil
		}
		if err := checkRealRunProjectImage(image, env, observe, projectimage.PreparationDigest()); err != nil {
			t.Fatalf("legacy=%t: image built at the base was refused: %v", legacy, err)
		}
	}
}

func TestRealRunProjectImageRefusesWhatTheDaemonRefuses(t *testing.T) {
	t.Parallel()
	changedManifest := unchangedBaseInputs()
	changedManifest.PackageJSONSHA256 = strings.Repeat("99", 32)
	unsupported := unchangedBaseInputs()
	unsupported.UnsupportedInputs = []string{".npmrc"}
	otherRecipe := unchangedBaseInputs()
	otherRecipe.RecipeDigest = domain.Digest("sha256:" + strings.Repeat("aa", 32))
	cases := []struct {
		name        string
		legacy      bool
		inputs      domain.ProjectImageBaseInputs
		preparation domain.Digest
		reason      string
	}{
		{"changed manifest", false, changedManifest, projectimage.PreparationDigest(), "package.json"},
		{"unsupported npm input", false, unsupported, projectimage.PreparationDigest(), ".npmrc"},
		{"different in-tree recipe", false, otherRecipe, projectimage.PreparationDigest(), "verification recipe"},
		{
			"different preparation sources", false, unchangedBaseInputs(),
			domain.Digest("sha256:" + strings.Repeat("77", 32)), "preparation sources",
		},
		{"record without environment evidence", true, unchangedBaseInputs(), projectimage.PreparationDigest(), "rebuild"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			image := realRunImageFixture(t, tc.legacy)
			observe, _ := observeOnce(tc.inputs, nil)
			err := checkRealRunProjectImage(image, realRunImageEnv(image), observe, tc.preparation)
			if !errors.Is(err, domain.ErrProjectImageIncompatible) {
				t.Fatalf("err = %v, want ErrProjectImageIncompatible", err)
			}
			if !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("err = %v, want the reason naming %q", err, tc.reason)
			}
		})
	}
}

func TestRealRunProjectImageRefusesAuthorityMismatchBeforeObserving(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*realRunEnv){
		"repository":    func(env *realRunEnv) { env.repo = "freeasinbird/other" },
		"repository id": func(env *realRunEnv) { env.repositoryID++ },
		"recipe":        func(env *realRunEnv) { env.approvedRecipe = domain.Digest("sha256:" + strings.Repeat("aa", 32)) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			image := realRunImageFixture(t, false)
			env := realRunImageEnv(image)
			mutate(&env)
			observe, calls := observeOnce(unchangedBaseInputs(), nil)
			err := checkRealRunProjectImage(image, env, observe, projectimage.PreparationDigest())
			if err == nil || !strings.Contains(err.Error(), "disagrees with the real-run authority") {
				t.Fatalf("err = %v, want the real-run authority mismatch", err)
			}
			if *calls != 0 {
				t.Fatalf("observed the base %d times on an authority mismatch, want none", *calls)
			}
		})
	}
}

// The daemon refuses a record whose decoded preparation command is not the
// fixed approved one whatever commit it was built at, so the verifier must
// refuse it before the build-at-base shortcut and without observing.
func TestRealRunProjectImageRefusesUnapprovedPreparationCommand(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{"built at another commit": false, "built at the base": true}
	for name, atBase := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			image := realRunImageFixture(t, false)
			image.PreparationCommand = []string{"/bin/sh", "-c", "true"}
			env := realRunImageEnv(image)
			if atBase {
				image.CommitSHA = env.baseSHA
			}
			observe, calls := observeOnce(unchangedBaseInputs(), nil)
			err := checkRealRunProjectImage(image, env, observe, projectimage.PreparationDigest())
			if err == nil || !strings.Contains(err.Error(), "is not the approved") {
				t.Fatalf("err = %v, want the unapproved preparation command refused", err)
			}
			if *calls != 0 {
				t.Fatalf("observed the base %d times for an unapproved command, want none", *calls)
			}
		})
	}
}

func TestRealRunProjectImageReportsObservationFailures(t *testing.T) {
	t.Parallel()
	image := realRunImageFixture(t, false)
	readErr := errors.New(realRunRepositoryCheckoutEnv + " is unset")
	observe, _ := observeOnce(domain.ProjectImageBaseInputs{}, readErr)
	err := checkRealRunProjectImage(image, realRunImageEnv(image), observe, projectimage.PreparationDigest())
	if !errors.Is(err, readErr) || errors.Is(err, domain.ErrProjectImageIncompatible) {
		t.Fatalf("err = %v, want the read error and not an incompatibility", err)
	}

	wrongCommit := unchangedBaseInputs()
	wrongCommit.CommitSHA = image.CommitSHA
	observe, _ = observeOnce(wrongCommit, nil)
	err = checkRealRunProjectImage(image, realRunImageEnv(image), observe, projectimage.PreparationDigest())
	if err == nil || !strings.Contains(err.Error(), "not the run base") {
		t.Fatalf("err = %v, want refusal of inputs observed at another commit", err)
	}
}

// No t.Parallel: the observer reads the process environment.
func TestRealRunBaseObserverNamesMissingCheckout(t *testing.T) {
	t.Setenv(realRunRepositoryCheckoutEnv, "")
	image := realRunImageFixture(t, false)
	err := checkRealRunProjectImage(image, realRunImageEnv(image),
		realRunBaseObserver(context.Background(), realRunImageEnv(image)), projectimage.PreparationDigest())
	if err == nil || !strings.Contains(err.Error(), realRunRepositoryCheckoutEnv) ||
		errors.Is(err, domain.ErrProjectImageIncompatible) {
		t.Fatalf("err = %v, want a read error naming %s", err, realRunRepositoryCheckoutEnv)
	}
}

// No t.Parallel: the observer reads the process environment.
func TestRealRunBaseObserverReadsTheCheckoutAtTheBase(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // G204: fixed test git invocation
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	commit := func(path, content string) string {
		t.Helper()
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", "change")
		return git("rev-parse", "HEAD")
	}
	git("init", "-q", "-b", "main", "--object-format=sha1")
	commit("package-lock.json", `{"lockfileVersion":3}`)
	build := commit("package.json", `{"scripts":{}}`)
	sourceOnly := commit("src/index.js", "export {};\n")
	changedManifest := commit("package.json", `{"scripts":{"test":"true"}}`)

	// The record a build at the first commit makes, from the same read.
	built, err := projectimage.ObserveBaseInputs(t.Context(), "git", dir, build)
	if err != nil {
		t.Fatal(err)
	}
	fixture := realRunImageFixture(t, false)
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: fixture.Repository, RepositoryID: fixture.RepositoryID,
		CommitSHA: build, RecipeDigest: fixture.RecipeDigest,
		PreparationCommand: fixture.PreparationCommand,
		BaseImageRef:       fixture.BaseImageRef, ImageRef: fixture.ImageRef,
		Environment: &domain.ProjectImageEnvironment{
			PackageJSONSHA256: built.PackageJSONSHA256, PackageLockSHA256: built.PackageLockSHA256,
			PreparationDigest: projectimage.PreparationDigest(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv(realRunRepositoryCheckoutEnv, dir)
	check := func(base string) error {
		env := realRunImageEnv(image)
		env.baseSHA = base
		return checkRealRunProjectImage(image, env, realRunBaseObserver(t.Context(), env), projectimage.PreparationDigest())
	}
	if err := check(sourceOnly); err != nil {
		t.Fatalf("source-only base was refused: %v", err)
	}
	if err := check(changedManifest); !errors.Is(err, domain.ErrProjectImageIncompatible) ||
		!strings.Contains(err.Error(), "package.json") {
		t.Fatalf("changed-manifest base = %v, want an incompatibility naming package.json", err)
	}
}
