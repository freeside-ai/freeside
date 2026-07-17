package importer

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// TestOnlyGitRunnerImportsExec pins the process-boundary discipline: no
// import package file may execute a subprocess except the single
// hardened plumbing wrapper. A new os/exec import anywhere else is the
// escape this test exists to catch.
func TestOnlyGitRunnerImportsExec(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range sources {
		if strings.HasSuffix(name, "_test.go") {
			continue // test helpers legitimately run git to build fixtures
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			if imp.Path.Value == `"os/exec"` && filepath.Base(name) != "gitrunner.go" {
				t.Errorf("%s imports os/exec; only gitrunner.go may", name)
			}
		}
	}
}

// TestImportDoesNotInfluenceCheckoutGit is the criterion-5 guarantee:
// a hostile workspace .git (poisoned hooks, a rewritten config) can
// never influence the import. The exporter already records the
// workspace .git as one inert git_dir entry, so the bytes never reach
// the handoff; this drives the whole pipeline and then proves the
// daemon-owned checkout's own .git is byte-unchanged and no hook fired.
func TestImportDoesNotInfluenceCheckoutGit(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})

	// A sentinel-writing pre-commit hook in the checkout: if any git
	// step in the import ran a commit through the working tree (it must
	// not; construction is pure plumbing), this fires.
	sentinel := filepath.Join(t.TempDir(), "fired")
	hookDir := filepath.Join(checkout, ".git", "hooks")
	if err := os.MkdirAll(hookDir, 0o750); err != nil {
		t.Fatal(err)
	}
	hook := "#!/bin/sh\necho fired > " + sentinel + "\n"
	if err := os.WriteFile(filepath.Join(hookDir, "pre-commit"), []byte(hook), 0o700); err != nil { //nolint:gosec // G306: a hook must be executable; this is the attack we prove inert
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hookDir, "post-commit"), []byte(hook), 0o700); err != nil { //nolint:gosec // G306: same
		t.Fatal(err)
	}
	configBefore, err := os.ReadFile(filepath.Join(checkout, ".git", "config")) //nolint:gosec // G304: test-owned fixture checkout
	if err != nil {
		t.Fatal(err)
	}

	// The workspace carries its own hostile .git plus a hook, which the
	// exporter records as inert git_dir/blocked entries.
	ws := t.TempDir()
	for path, content := range map[string]string{
		"a.txt":                 "new\n",
		".git/hooks/pre-commit": "#!/bin/sh\necho pwned\n",
		".git/config":           "[core]\n\thooksPath = /tmp/evil\n",
	} {
		full := filepath.Join(ws, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handoff := exportWorkspace(t, ws)

	res, err := Import(t.Context(), handoff, checkout, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("clean change (only a.txt) should have imported")
	}
	if len(res.Changes) != 1 || res.Changes[0].Path != "a.txt" {
		t.Fatalf("changes = %+v, want only a.txt (workspace .git ignored)", res.Changes)
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("a checkout hook fired: construction is not pure plumbing")
	}
	configAfter, err := os.ReadFile(filepath.Join(checkout, ".git", "config")) //nolint:gosec // G304: test-owned fixture checkout
	if err != nil {
		t.Fatal(err)
	}
	if string(configBefore) != string(configAfter) {
		t.Error("the checkout's .git/config changed during import")
	}
}

// TestImportLeavesHandoffAndWorkingTreeUnchanged proves the import
// writes candidate bytes only into the git object database (and its
// private scratch), never the handoff or the checkout's working tree.
func TestImportLeavesHandoffAndWorkingTreeUnchanged(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("a.txt", "new\n", false),
		regularEntryFor("added.txt", "brand new\n", false),
	}, "new\n", "brand new\n")

	handoffBefore := snapshotTree(t, handoff)
	worktreeBefore := snapshotWorktree(t, checkout)

	res, err := Import(t.Context(), handoff, checkout, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("expected a commit")
	}
	if got := snapshotTree(t, handoff); got != handoffBefore {
		t.Error("handoff directory changed during import")
	}
	if got := snapshotWorktree(t, checkout); got != worktreeBefore {
		t.Error("checkout working tree changed during import (candidate bytes must land only in .git/objects)")
	}
	// The imported content is reachable as objects, proving it landed
	// where it should: in the object database, not the working tree.
	if got := rungit(t, checkout, "show", res.CommitSHA+":added.txt"); got != "brand new" {
		t.Errorf("added.txt in commit = %q, want it committed as an object", got)
	}
	if _, err := os.Stat(filepath.Join(checkout, "added.txt")); !os.IsNotExist(err) {
		t.Error("added.txt appeared in the working tree; import must not check out candidate content")
	}
}

// snapshotTree returns a stable digest of a directory's file paths and
// contents, for before/after equality.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		data, err := os.ReadFile(path) //nolint:gosec // G304: test-owned fixture tree
		if err != nil {
			return err
		}
		b.WriteString(rel + "\x00" + string(data) + "\x00")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// snapshotWorktree snapshots a checkout excluding its .git directory
// (object writes there are expected).
func snapshotWorktree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // G304: test-owned fixture tree
		if err != nil {
			return err
		}
		b.WriteString(rel + "\x00" + string(data) + "\x00")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
