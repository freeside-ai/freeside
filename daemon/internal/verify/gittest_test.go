package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runGit runs one git command against dir with a neutral environment,
// failing the test on error and returning trimmed stdout.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: test fixture running git over test-owned repos with test-chosen arguments
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_AUTHOR_DATE=1752600000 +0000", "GIT_COMMITTER_DATE=1752600000 +0000",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// initRepo creates a repository whose base commit holds files, and
// returns its path and base commit SHA.
func initRepo(t *testing.T, files map[string]string) (dir, baseSHA string) {
	t.Helper()
	dir = t.TempDir()
	runGit(t, dir, "init", "-q", "-b", "main", "--object-format=sha1")
	writeFiles(t, dir, files)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base")
	return dir, runGit(t, dir, "rev-parse", "HEAD")
}

// commitCandidate writes changes on top of the checkout's HEAD as a
// candidate commit (deleting paths mapped to the empty string), resets
// the checkout back to base so HEAD stays at the enforced base like an
// importer-produced checkout, and returns the candidate SHA.
func commitCandidate(t *testing.T, dir, baseSHA string, changes map[string]string) string {
	t.Helper()
	for path, content := range changes {
		if content == "" {
			runGit(t, dir, "rm", "-q", "--", path)
		}
	}
	writeFiles(t, dir, changes)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "candidate")
	head := runGit(t, dir, "rev-parse", "HEAD")
	// Anchor the candidate against gc, then move the checkout back to
	// the enforced base: the importer leaves HEAD at base and hands the
	// candidate over as a commit object.
	runGit(t, dir, "update-ref", "refs/freeside/candidate", head)
	runGit(t, dir, "reset", "-q", "--hard", baseSHA)
	return head
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for path, content := range files {
		if content == "" {
			continue
		}
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil { //nolint:gosec // G301: test-owned fixture tree
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil { //nolint:gosec // G306: test-owned fixture content
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

// newTestRunner builds a hardened runner over an existing repository.
func newTestRunner(t *testing.T, dir string) *gitRunner {
	t.Helper()
	g, err := newGitRunner(context.Background(), "", dir, t.TempDir())
	if err != nil {
		t.Fatalf("newGitRunner: %v", err)
	}
	return g
}
