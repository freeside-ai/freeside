package commitplancheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test helper runs git with test-controlled arguments
	cmd.Env = append(gitenv.Scrubbed(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func initTestRepo(t *testing.T, files map[string]string, setImportBase bool) (repoDir, baseSHA string) {
	t.Helper()
	repoDir = t.TempDir()
	runGit(t, repoDir, "init", "--object-format=sha1", "-b", "main")
	runGit(t, repoDir, "config", "maintenance.auto", "false")
	runGit(t, repoDir, "config", "gc.auto", "0")
	for rel, content := range files {
		full := filepath.Join(repoDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "--allow-empty", "-m", "base")
	baseSHA = runGit(t, repoDir, "rev-parse", "HEAD")
	if setImportBase {
		runGit(t, repoDir, "update-ref", DefaultBaseRef, baseSHA)
	}
	return repoDir, baseSHA
}

func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			snap[rel] = "dir"
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path) //nolint:gosec // G304: walking the test's own temp directory
		if err != nil {
			return err
		}
		sum := sha256.Sum256(body)
		snap[rel] = info.Mode().String() + ":" + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snap
}

func TestValidateUsesImportBaseAfterWriterCommits(t *testing.T) {
	t.Parallel()
	repo, baseSHA := initTestRepo(t, map[string]string{"sub/a.txt": "old\n"}, true)
	// Writer modifies a file, commits locally (moving HEAD past import-base),
	// and writes a valid commit plan.
	if err := os.WriteFile(filepath.Join(repo, "sub", "a.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "sub/a.txt")
	runGit(t, repo, "commit", "-m", "local work")
	if head := runGit(t, repo, "rev-parse", "HEAD"); head == baseSHA {
		t.Fatal("HEAD did not advance past base")
	}
	plan := `{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"Update a","remainder":true}]}`
	if err := os.WriteFile(filepath.Join(repo, export.CommitPlanFilename), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	// Run from a subdirectory to prove toplevel discovery works.
	var stdout, stderr bytes.Buffer
	code := runInDir(t.Context(), filepath.Join(repo, "sub"), []string{"validate"}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var rep Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v\nraw=%q", err, stdout.String())
	}
	if rep.Version != ReportVersion || !rep.OK || rep.BaseSHA != baseSHA || len(rep.Problems) != 0 {
		t.Fatalf("report = %+v, want ok with base %s", rep, baseSHA)
	}
}

func TestValidateBaseFlagAndRejectedPlan(t *testing.T) {
	t.Parallel()
	// No refs/freeside/import-base set: default fails with exit 2, -base works.
	repo, baseSHA := initTestRepo(t, map[string]string{"a.txt": "old\n"}, false)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := `{"version":"freeside.commit-plan/v1","groups":[{"name":"bad","message":"Fixes #9","remainder":true}]}`
	if err := os.WriteFile(filepath.Join(repo, export.CommitPlanFilename), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := runInDir(t.Context(), repo, []string{"validate"}, &stdout, &stderr); code != 2 || stdout.Len() != 0 || !strings.Contains(stderr.String(), DefaultBaseRef) {
		t.Fatalf("missing default ref: code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	code := runInDir(t.Context(), repo, []string{"validate", "-base", "HEAD"}, &stdout, &stderr)
	if code != 1 || stderr.Len() != 0 {
		t.Fatalf("-base HEAD rejected plan: code=%d stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var rep Report
	if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
		t.Fatalf("decode report: %v\nraw=%q", err, stdout.String())
	}
	if rep.OK || rep.BaseSHA != baseSHA || len(rep.Problems) != 1 ||
		rep.Problems[0].Code != importer.PlanProblemMessageCloseDirective || rep.Problems[0].Group != 1 {
		t.Fatalf("report = %+v", rep)
	}
}

func TestValidateUsageAndNonRepoExit2(t *testing.T) {
	t.Parallel()
	notRepo := t.TempDir()
	for _, tc := range []struct {
		name string
		dir  string
		args []string
	}{
		{"no args", notRepo, nil},
		{"unknown verb", notRepo, []string{"check"}},
		{"unknown flag", notRepo, []string{"validate", "-bogus"}},
		{"extra positional", notRepo, []string{"validate", "extra"}},
		{"outside git repo", notRepo, []string{"validate"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := runInDir(t.Context(), tc.dir, tc.args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("code = %d, want 2 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("exit 2 wrote to stdout: %q", stdout.String())
			}
			lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
			if len(lines) != 1 || lines[0] == "" {
				t.Errorf("exit 2 stderr must be one line, got %q", stderr.String())
			}
		})
	}
}

// TestValidateLeavesWorkspaceUntouchedAndRemovesTempDir verifies that validate
// modifies nothing in the worktree or .git (no files, objects, or refs) and
// deletes every temporary directory it created.
func TestValidateLeavesWorkspaceUntouchedAndRemovesTempDir(t *testing.T) {
	// Serial because t.Setenv pins TMPDIR so both commitplancheck and importer
	// temp directories land under tempRoot where we assert complete removal.
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	repo, _ := initTestRepo(t, map[string]string{"a.txt": "old\n", "del.txt": "bye\n"}, true)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "del.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "added.txt"), []byte("added\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := `{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"Apply changes","remainder":true}]}`
	if err := os.WriteFile(filepath.Join(repo, export.CommitPlanFilename), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	before := snapshotTree(t, repo)

	var stdout, stderr bytes.Buffer
	code := runInDir(t.Context(), repo, []string{"validate"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("validate = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	after := snapshotTree(t, repo)
	if len(before) != len(after) {
		t.Fatalf("workspace entry count changed: before=%d after=%d", len(before), len(after))
	}
	for path, want := range before {
		if got, ok := after[path]; !ok || got != want {
			t.Errorf("workspace path %q changed: before=%q after=%q (present=%v)", path, want, got, ok)
		}
	}

	leftovers, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		names := make([]string, 0, len(leftovers))
		for _, e := range leftovers {
			names = append(names, e.Name())
		}
		t.Fatalf("validate left temporary directories behind: %v", names)
	}
}

type cancelOnOpenFS struct {
	fs.ReadLinkFS
	target string
	cancel context.CancelFunc
}

func (c cancelOnOpenFS) Open(name string) (fs.File, error) {
	if name == c.target {
		c.cancel()
	}
	return c.ReadLinkFS.Open(name)
}

// TestValidateCanceledContextCleansUpTempDir verifies that context
// cancellation mid-export aborts the check with exit 2, empty stdout, one
// stderr line, and removes the temporary directory holding the partial export.
func TestValidateCanceledContextCleansUpTempDir(t *testing.T) {
	// Serial because t.Setenv pins TMPDIR so we can assert complete removal.
	tempRoot := t.TempDir()
	t.Setenv("TMPDIR", tempRoot)

	repo, _ := initTestRepo(t, map[string]string{"a.txt": "old\n"}, true)
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := `{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"Update a","remainder":true}]}`
	if err := os.WriteFile(filepath.Join(repo, export.CommitPlanFilename), []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var stdout, stderr bytes.Buffer
	code := runInDirWithExportFS(ctx, repo, []string{"validate"}, &stdout, &stderr, func(base fs.FS) fs.FS {
		rl, ok := base.(fs.ReadLinkFS)
		if !ok {
			t.Fatalf("base FS %T does not implement fs.ReadLinkFS", base)
		}
		return cancelOnOpenFS{ReadLinkFS: rl, target: "a.txt", cancel: cancel}
	})
	if code != 2 {
		t.Fatalf("code = %d, want 2 (stdout=%q stderr=%q)", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("canceled run wrote to stdout: %q", stdout.String())
	}
	lines := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], context.Canceled.Error()) {
		t.Errorf("stderr = %q, want one line mentioning %q", stderr.String(), context.Canceled.Error())
	}

	leftovers, err := os.ReadDir(tempRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(leftovers) != 0 {
		names := make([]string, 0, len(leftovers))
		for _, e := range leftovers {
			names = append(names, e.Name())
		}
		t.Fatalf("canceled validate left temporary directories behind: %v", names)
	}
}
