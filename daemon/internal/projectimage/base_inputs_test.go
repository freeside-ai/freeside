package projectimage

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

// This digest is recorded in every project image and compared with the running
// binary's before an image is reused at another commit. It changes whenever
// the preparation helper, the Node launcher, the Containerfile template, or the
// Node pin does, and from then on every image built by an earlier binary is
// usable only at its build commit until the operator rebuilds it. Update the
// pin when that is the intended cost of the edit, and say so in the change.
func TestPreparationDigestPinsTheBuilderSources(t *testing.T) {
	const want = "sha256:5c2e06016b95b136214f516f45d231072307f876abadb4ec7e694bc402b99b66"
	got := PreparationDigest()
	if !contentaddr.Valid(string(got)) {
		t.Fatalf("PreparationDigest() = %q, want a content address", got)
	}
	if got != want {
		t.Fatalf("PreparationDigest() = %s, want %s: the builder's fixed sources changed", got, want)
	}
}

type baseInputsRepo struct {
	t   *testing.T
	dir string
}

func newBaseInputsRepo(t *testing.T) baseInputsRepo {
	t.Helper()
	repo := baseInputsRepo{t: t, dir: t.TempDir()}
	repo.git("init", "-q", "-b", "main", "--object-format=sha1")
	return repo
}

func (r baseInputsRepo) git(args ...string) string {
	r.t.Helper()
	cmd := exec.CommandContext(r.t.Context(), "git", args...) //nolint:gosec // G204: fixed test git invocation
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.test")
	output, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// commit applies changes (an empty value removes the path) and returns the
// new commit.
func (r baseInputsRepo) commit(changes map[string]string) string {
	r.t.Helper()
	for path, content := range changes {
		full := filepath.Join(r.dir, filepath.FromSlash(path))
		if content == "" {
			if err := os.RemoveAll(full); err != nil {
				r.t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			r.t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			r.t.Fatal(err)
		}
	}
	r.git("add", "-A")
	r.git("commit", "-q", "--allow-empty", "-m", "change")
	return r.git("rev-parse", "HEAD")
}

func TestObserveBaseInputsReadsEachInputAtTheExactCommit(t *testing.T) {
	const (
		packageJSON = `{"scripts":{}}`
		packageLock = `{"lockfileVersion":3}`
	)
	repo := newBaseInputsRepo(t)
	build := repo.commit(map[string]string{
		"package.json": packageJSON, "package-lock.json": packageLock,
		verify.DefaultRecipePath: testRecipe, "src/index.js": "export {};\n",
	})
	// The image a build at this commit would record.
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "freeasinbird/gh-imgup", RepositoryID: 1278475858,
		CommitSHA: build, RecipeDigest: verify.RecipeDigest([]byte(testRecipe)),
		PreparationCommand: []string{PreparationPath},
		BaseImageRef:       validRequest().BaseImageRef,
		ImageRef:           domain.ImageRef("127.0.0.1:5100/project@" + testImageDigest),
		Environment: &domain.ProjectImageEnvironment{
			PackageJSONSHA256: manifestSHA256([]byte(packageJSON)),
			PackageLockSHA256: manifestSHA256([]byte(packageLock)),
			PreparationDigest: PreparationDigest(),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	observe := func(t *testing.T, commit string) domain.ProjectImageBaseInputs {
		t.Helper()
		inputs, err := ObserveBaseInputs(t.Context(), "git", repo.dir, commit)
		if err != nil {
			t.Fatalf("ObserveBaseInputs: %v", err)
		}
		if inputs.CommitSHA != commit {
			t.Fatalf("observed commit = %s, want %s", inputs.CommitSHA, commit)
		}
		return inputs
	}

	sourceOnly := repo.commit(map[string]string{
		"src/index.js": "export const changed = true;\n", "README.md": "docs\n",
	})
	if err := image.AdmissibleAt(observe(t, sourceOnly), PreparationDigest()); err != nil {
		t.Fatalf("source-only commit refused: %v", err)
	}

	// Each case is one commit on top of the source-only one, then reverted,
	// so every refusal is caused by exactly the named input.
	refusals := []struct {
		name    string
		changes map[string]string
		revert  map[string]string
		names   string
	}{
		{
			"changed package.json",
			map[string]string{"package.json": `{"scripts":{"build":"tsc"}}`},
			map[string]string{"package.json": packageJSON},
			"package.json",
		},
		{
			"changed package-lock.json",
			map[string]string{"package-lock.json": `{"lockfileVersion":3,"packages":{}}`},
			map[string]string{"package-lock.json": packageLock},
			"package-lock.json",
		},
		{
			"removed package-lock.json",
			map[string]string{"package-lock.json": ""},
			map[string]string{"package-lock.json": packageLock},
			"package-lock.json",
		},
		{
			"added .npmrc",
			map[string]string{".npmrc": "registry=https://registry.example.test/\n"},
			map[string]string{".npmrc": ""},
			".npmrc",
		},
		{
			".npmrc as a directory",
			map[string]string{".npmrc/config": "registry=https://registry.example.test/\n"},
			map[string]string{".npmrc": ""},
			".npmrc",
		},
		{
			"added npm-shrinkwrap.json",
			map[string]string{"npm-shrinkwrap.json": packageLock},
			map[string]string{"npm-shrinkwrap.json": ""},
			"npm-shrinkwrap.json",
		},
		{
			"changed recipe",
			map[string]string{verify.DefaultRecipePath: `{"commands":[["npm","test"]],"capture":"none"}`},
			map[string]string{verify.DefaultRecipePath: testRecipe},
			"recipe",
		},
	}
	for _, tc := range refusals {
		t.Run(tc.name, func(t *testing.T) {
			drifted := repo.commit(tc.changes)
			err := image.AdmissibleAt(observe(t, drifted), PreparationDigest())
			if !errors.Is(err, domain.ErrProjectImageIncompatible) ||
				!strings.Contains(err.Error(), tc.names) {
				t.Fatalf("AdmissibleAt at the drifted commit = %v, want a refusal naming %q", err, tc.names)
			}
			// The observation is of the named commit, not of the checkout's
			// HEAD or worktree: the earlier commit still passes from here.
			if err := image.AdmissibleAt(observe(t, sourceOnly), PreparationDigest()); err != nil {
				t.Fatalf("source-only commit refused after the checkout moved: %v", err)
			}
			restored := repo.commit(tc.revert)
			if err := image.AdmissibleAt(observe(t, restored), PreparationDigest()); err != nil {
				t.Fatalf("restored commit refused: %v", err)
			}
		})
	}

	// A base whose tree declares no recipe contradicts nothing the image
	// baked: this is every commit of a repository onboarded with a recipe
	// supplied outside its tree.
	undeclared := observe(t, repo.commit(map[string]string{verify.DefaultRecipePath: ""}))
	if undeclared.RecipeDigest != "" {
		t.Fatalf("recipe digest of a base without a recipe = %q, want empty", undeclared.RecipeDigest)
	}
	if err := image.AdmissibleAt(undeclared, PreparationDigest()); err != nil {
		t.Fatalf("base without an in-tree recipe refused: %v", err)
	}
	repo.commit(map[string]string{verify.DefaultRecipePath: testRecipe})

	// Both unsupported inputs are reported, in the builder's order.
	both := observe(t, repo.commit(map[string]string{
		".npmrc": "audit=false\n", "npm-shrinkwrap.json": packageLock,
	}))
	if !slices.Equal(both.UnsupportedInputs, []string{"npm-shrinkwrap.json", ".npmrc"}) {
		t.Fatalf("unsupported inputs = %v", both.UnsupportedInputs)
	}
}

// A manifest the base holds as a symlink has no bytes to compare, and the
// builder would refuse to build from that commit. The observation fails
// instead of reporting an empty hash as if the file were merely absent.
func TestObserveBaseInputsRefusesANonRegularManifest(t *testing.T) {
	repo := newBaseInputsRepo(t)
	repo.commit(map[string]string{"real.json": `{"scripts":{}}`, "package-lock.json": `{"lockfileVersion":3}`})
	if err := os.Symlink("real.json", filepath.Join(repo.dir, "package.json")); err != nil {
		t.Fatal(err)
	}
	commit := repo.commit(nil)
	if _, err := ObserveBaseInputs(t.Context(), "git", repo.dir, commit); !errors.Is(err, verify.ErrCommitFileUnreadable) {
		t.Fatalf("ObserveBaseInputs = %v, want ErrCommitFileUnreadable", err)
	}
	if _, err := ObserveBaseInputs(t.Context(), "git", repo.dir, "HEAD"); !errors.Is(err, verify.ErrInvalidOptions) {
		t.Fatalf("symbolic revision = %v, want ErrInvalidOptions", err)
	}
}

func countedCommitGit(t *testing.T) (string, func(int, int)) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "calls")
	script := filepath.Join(dir, "git")
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	body := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %s\nexec %s \"$@\"\n", quote(log), quote(realGit))
	// A concurrent fork must not inherit the executable's write descriptor.
	syscall.ForkLock.RLock()
	err = os.WriteFile(script, []byte(body), 0o700) //nolint:gosec // G306: test-owned executable
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	return script, func(trees, blobs int) {
		t.Helper()
		data, err := os.ReadFile(log) //nolint:gosec // G304: test-owned invocation log in t.TempDir.
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
			args := strings.Fields(line)
			command := ""
			for i, arg := range args {
				if arg == "ls-tree" || arg == "cat-file" {
					command = arg
					break
				}
				if arg == "rev-parse" && i+1 < len(args) {
					command = args[i+1]
					break
				}
			}
			counts[command]++
		}
		want := map[string]int{"--absolute-git-dir": 1, "--show-object-format": 1, "ls-tree": trees, "cat-file": blobs}
		if len(counts) != len(want) {
			t.Fatalf("git commands = %v, want only %v", counts, want)
		}
		for command, n := range want {
			if counts[command] != n {
				t.Fatalf("git commands = %v, want %v", counts, want)
			}
		}
	}
}

func TestObserveBaseInputsPinsOnceAndClosesItsReader(t *testing.T) {
	repo := newBaseInputsRepo(t)
	base := repo.commit(map[string]string{
		"package.json": gateBasePackageJSON, "package-lock.json": gateBasePackageLock,
		verify.DefaultRecipePath: testRecipe,
	})
	git, check := countedCommitGit(t)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	if _, err := ObserveBaseInputs(t.Context(), git, repo.dir, base); err != nil {
		t.Fatal(err)
	}
	check(5, 3)
	// A later call opens its own reader, including one whose read fails.
	if _, err := ObserveBaseInputs(t.Context(), "git", repo.dir, strings.Repeat("0", 40)); !errors.Is(err, verify.ErrGitPlumbing) {
		t.Fatalf("missing commit = %v, want plumbing fault", err)
	}
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("scratch after calls = %v, %v", entries, err)
	}
}
