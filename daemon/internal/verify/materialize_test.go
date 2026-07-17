package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMaterializeWritesHeadTree(t *testing.T) {
	dir, base := initRepo(t, map[string]string{
		testRecipePath: trustedRecipeBytes,
		"README.md":    "base readme",
		"pkg/lib.go":   "package pkg\n",
	})
	head := commitCandidate(t, dir, base, map[string]string{
		"README.md":  "candidate readme",
		"pkg/new.go": "package pkg // new\n",
		"pkg/lib.go": "",
	})
	g := newTestRunner(t, dir)
	dest := filepath.Join(t.TempDir(), "workspace")
	if err := g.materialize(context.Background(), head, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	for path, want := range map[string]string{
		"README.md":    "candidate readme",
		"pkg/new.go":   "package pkg // new\n",
		testRecipePath: trustedRecipeBytes,
	} {
		got, err := os.ReadFile(filepath.Join(dest, filepath.FromSlash(path))) //nolint:gosec // G304: test-owned workspace
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dest, "pkg", "lib.go")); !errors.Is(err, os.ErrNotExist) {
		t.Error("deleted pkg/lib.go materialized anyway")
	}
	if _, err := os.Stat(filepath.Join(dest, ".git")); !errors.Is(err, os.ErrNotExist) {
		t.Error("workspace carries a .git directory")
	}
}

// TestMaterializeMismatchedHeadFailsClosed is acceptance 2's fail-closed
// half: a head the checkout does not hold is a typed error and no
// workspace is materialized.
func TestMaterializeMismatchedHeadFailsClosed(t *testing.T) {
	dir, _ := initRepo(t, map[string]string{"README.md": "base"})
	g := newTestRunner(t, dir)
	dest := filepath.Join(t.TempDir(), "workspace")
	err := g.materialize(context.Background(), "0123456789abcdef0123456789abcdef01234567", dest)
	if !errors.Is(err, ErrHeadMismatch) {
		t.Fatalf("err = %v, want ErrHeadMismatch", err)
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("workspace was created despite the head mismatch")
	}
}

// TestMaterializeLeavesCheckoutUntouched pins that materialization uses
// only the scratch index: the daemon-owned checkout's worktree and HEAD
// stay exactly at the enforced base.
func TestMaterializeLeavesCheckoutUntouched(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"README.md": "base readme"})
	head := commitCandidate(t, dir, base, map[string]string{"README.md": "candidate readme"})
	g := newTestRunner(t, dir)
	if err := g.materialize(context.Background(), head, filepath.Join(t.TempDir(), "ws")); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got := runGit(t, dir, "rev-parse", "HEAD"); got != base {
		t.Errorf("checkout HEAD moved to %s, want base %s", got, base)
	}
	got, err := os.ReadFile(filepath.Join(dir, "README.md")) //nolint:gosec // G304: test-owned fixture checkout
	if err != nil {
		t.Fatalf("read checkout README: %v", err)
	}
	if string(got) != "base readme" {
		t.Errorf("checkout worktree README = %q, want the base content", got)
	}
	if status := runGit(t, dir, "status", "--porcelain"); status != "" {
		t.Errorf("checkout status not clean after materialize:\n%s", status)
	}
}
