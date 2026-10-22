package gitenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fixtureGit runs git the way a fixture helper should: from Scrubbed, with
// its own pins appended.
func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture running git over test-owned repos with test-chosen arguments
	cmd.Env = append(Scrubbed(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestScrubbedDropsOnlyGitEntries(t *testing.T) {
	t.Setenv("GIT_DIR", "/nonexistent")
	t.Setenv("FREESIDE_GITENV_KEEP", "kept")

	env := Scrubbed()
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_") {
			t.Errorf("Scrubbed kept %q", entry)
		}
	}
	if !slices.Contains(env, "FREESIDE_GITENV_KEEP=kept") {
		t.Error("Scrubbed dropped a non-git entry")
	}
}

func TestScrubbedIgnoresAmbientGitState(t *testing.T) {
	// The hostile state names the decoy before the first command runs, so a
	// helper that loses its scrub writes there and never to a repository the
	// caller's own environment names.
	decoy := t.TempDir()
	decoyGit := filepath.Join(decoy, ".git")
	t.Setenv("GIT_DIR", decoyGit)
	t.Setenv("GIT_WORK_TREE", decoy)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(decoyGit, "index"))
	t.Setenv("GIT_OBJECT_DIRECTORY", filepath.Join(decoyGit, "objects"))
	t.Setenv("GIT_COMMON_DIR", decoyGit)
	fixtureGit(t, decoy, "init", "-q")
	fixture := t.TempDir()
	fixtureGit(t, fixture, "init", "-q")
	fixtureGit(t, fixture, "config", "freeside.fixture", "target")

	fixtureConfig, err := os.ReadFile(filepath.Join(fixture, ".git", "config")) //nolint:gosec // G304: fixed config path under a test-owned temp repository
	if err != nil {
		t.Errorf("fixture command did not create its target repository: %v", err)
	} else if !strings.Contains(string(fixtureConfig), "fixture = target") {
		t.Error("fixture command did not update its target repository")
	}
	decoyConfig, err := os.ReadFile(filepath.Join(decoyGit, "config")) //nolint:gosec // G304: fixed config path under a test-owned temp repository
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(decoyConfig), "fixture = target") {
		t.Error("fixture command inherited ambient git state and updated the decoy repository")
	}
}
