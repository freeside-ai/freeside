package verify

import (
	"context"
	"encoding/hex"
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

// TestMaterializePreservesBytesUnderAttributes is the refute-pass
// regression: an in-tree .gitattributes declaring ident and eol
// conversion must not change a single materialized byte, or the recipe
// would verify content the bound head does not hold.
func TestMaterializePreservesBytesUnderAttributes(t *testing.T) {
	attrs := "id.txt ident\n*.txt text eol=crlf\n"
	idBlob := "$Id$\nhello\n"
	unixBlob := "a\nb\n"
	dir, base := initRepo(t, map[string]string{
		".gitattributes": attrs,
		"id.txt":         idBlob,
		"unix.txt":       unixBlob,
	})
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
	g := newTestRunner(t, dir)
	dest := filepath.Join(t.TempDir(), "workspace")
	if err := g.materialize(context.Background(), head, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	for path, want := range map[string]string{"id.txt": idBlob, "unix.txt": unixBlob} {
		got, err := os.ReadFile(filepath.Join(dest, path)) //nolint:gosec // G304: test-owned workspace
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want the committed bytes %q (attribute conversion leaked)", path, got, want)
		}
	}
}

// TestMaterializePreservesSymlinks pins that a base symlink survives
// materialization and the byte re-verification accepts it.
func TestMaterializePreservesSymlinks(t *testing.T) {
	dir, _ := initRepo(t, map[string]string{"real.txt": "content\n"})
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base+link")
	base := runGit(t, dir, "rev-parse", "HEAD")
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
	g := newTestRunner(t, dir)
	dest := filepath.Join(t.TempDir(), "workspace")
	if err := g.materialize(context.Background(), head, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dest, "link.txt"))
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	if target != "real.txt" {
		t.Errorf("symlink target = %q, want real.txt", target)
	}
}

// TestVerifyMaterializedFailsClosed corrupts the workspace after an
// honest materialization and proves the byte re-verification rejects
// both a changed file and a stray one.
func TestVerifyMaterializedFailsClosed(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"README.md": "base"})
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
	g := newTestRunner(t, dir)

	t.Run("converted file", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "ws")
		if err := g.materialize(context.Background(), head, dest); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dest, "main.go"), []byte("package main\r\n"), 0o644); err != nil { //nolint:gosec // G306: test-owned workspace
			t.Fatalf("corrupt: %v", err)
		}
		if err := g.verifyMaterialized(context.Background(), head, dest); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("err = %v, want ErrWorkspaceMismatch", err)
		}
	})
	t.Run("stray file", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "ws")
		if err := g.materialize(context.Background(), head, dest); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dest, "stray.txt"), []byte("x"), 0o644); err != nil { //nolint:gosec // G306: test-owned workspace
			t.Fatalf("plant stray: %v", err)
		}
		if err := g.verifyMaterialized(context.Background(), head, dest); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("err = %v, want ErrWorkspaceMismatch", err)
		}
	})
	t.Run("missing file", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "ws")
		if err := g.materialize(context.Background(), head, dest); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if err := os.Remove(filepath.Join(dest, "main.go")); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := g.verifyMaterialized(context.Background(), head, dest); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("err = %v, want ErrWorkspaceMismatch", err)
		}
	})
}

// TestMaterializeRejectsSymlinkDowngrade is the Codex-review
// regression: under core.symlinks=false, checkout-index writes a
// symlink entry's target text as a plain file, a type change a recipe
// can observe; materialization must fail closed rather than verify a
// workspace of a different shape than the bound head.
func TestMaterializeRejectsSymlinkDowngrade(t *testing.T) {
	dir, _ := initRepo(t, map[string]string{"real.txt": "content\n"})
	if err := os.Symlink("real.txt", filepath.Join(dir, "link.txt")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base+link")
	base := runGit(t, dir, "rev-parse", "HEAD")
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
	g := newTestRunner(t, dir)
	dest := filepath.Join(t.TempDir(), "ws")
	if err := g.materialize(context.Background(), head, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// verifyMaterialized is the backstop: if a symlink entry were ever
	// materialized as a plain file (a downgrade), the type check must
	// fail closed. Simulate that by replacing the link with a file.
	link := filepath.Join(dest, "link.txt")
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove link: %v", err)
	}
	if err := os.WriteFile(link, []byte("real.txt"), 0o600); err != nil {
		t.Fatalf("write plain file: %v", err)
	}
	if err := g.verifyMaterialized(context.Background(), head, dest); !errors.Is(err, ErrWorkspaceMismatch) {
		t.Fatalf("err = %v, want ErrWorkspaceMismatch for a symlink downgraded to a file", err)
	}
}

// TestMaterializePreservesExecutableBit closes the materialized-shape
// class over everything a git tree expresses: a 100755 entry
// materializes owner-executable and passes; flipping the bit in either
// direction after materialization is a workspace mismatch.
func TestMaterializePreservesExecutableBit(t *testing.T) {
	dir, _ := initRepo(t, map[string]string{"plain.txt": "data\n"})
	script := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil { //nolint:gosec // G306: fixture needs the exec bit; content is inert test data
		t.Fatalf("write script: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "base+script")
	base := runGit(t, dir, "rev-parse", "HEAD")
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
	g := newTestRunner(t, dir)

	dest := filepath.Join(t.TempDir(), "ws")
	if err := g.materialize(context.Background(), head, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "run.sh"))
	if err != nil {
		t.Fatalf("stat script: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Fatal("100755 entry materialized without the owner executable bit")
	}

	t.Run("exec bit stripped", func(t *testing.T) {
		if err := os.Chmod(filepath.Join(dest, "run.sh"), 0o644); err != nil { //nolint:gosec // G302: deliberately corrupting a test-owned workspace
			t.Fatalf("chmod: %v", err)
		}
		if err := g.verifyMaterialized(context.Background(), head, dest); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("err = %v, want ErrWorkspaceMismatch", err)
		}
	})
	t.Run("exec bit added", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "ws2")
		if err := g.materialize(context.Background(), head, dest); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if err := os.Chmod(filepath.Join(dest, "plain.txt"), 0o755); err != nil { //nolint:gosec // G302: deliberately corrupting a test-owned workspace
			t.Fatalf("chmod: %v", err)
		}
		if err := g.verifyMaterialized(context.Background(), head, dest); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("err = %v, want ErrWorkspaceMismatch", err)
		}
	})
}

// TestMaterializeGitlinkShape pins the gitlink half of the
// materialized-shape enumeration: a base submodule pointer materializes
// as an empty directory (clone parity) and passes; a populated
// directory or a file at the gitlink path fails closed.
func TestMaterializeGitlinkShape(t *testing.T) {
	dir, _ := initRepo(t, map[string]string{"a.txt": "hi\n"})
	runGit(t, dir, "update-index", "--add", "--cacheinfo",
		"160000,1111111111111111111111111111111111111111,sub")
	// The worktree needs the (empty) submodule directory, or the
	// candidate commit's `git add -A` stages the gitlink's deletion.
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil { //nolint:gosec // G301: test-owned fixture tree
		t.Fatalf("mkdir sub: %v", err)
	}
	runGit(t, dir, "commit", "-q", "-m", "base+gitlink")
	base := runGit(t, dir, "rev-parse", "HEAD")
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
	g := newTestRunner(t, dir)

	dest := filepath.Join(t.TempDir(), "ws")
	if err := g.materialize(context.Background(), head, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	info, err := os.Stat(filepath.Join(dest, "sub"))
	if err != nil || !info.IsDir() {
		t.Fatalf("gitlink did not materialize as a directory: info=%v err=%v", info, err)
	}

	t.Run("populated gitlink directory", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dest, "sub", "planted.txt"), []byte("x"), 0o600); err != nil {
			t.Fatalf("plant: %v", err)
		}
		if err := g.verifyMaterialized(context.Background(), head, dest); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("err = %v, want ErrWorkspaceMismatch", err)
		}
	})
	t.Run("file at gitlink path", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "ws2")
		if err := g.materialize(context.Background(), head, dest); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if err := os.RemoveAll(filepath.Join(dest, "sub")); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dest, "sub"), []byte("x"), 0o600); err != nil {
			t.Fatalf("replace: %v", err)
		}
		if err := g.verifyMaterialized(context.Background(), head, dest); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("err = %v, want ErrWorkspaceMismatch", err)
		}
	})
}

// TestVerifyMaterializedRejectsStrayDir is the Codex-review regression:
// a pre-created or leftover directory the head tree does not hold is
// observable to a later command (test -d), so materialization must not
// accept it. materialize clears the destination first; verifyMaterialized
// rejects a stray directory as the backstop.
func TestVerifyMaterializedRejectsStrayDir(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"pkg/a.go": "package pkg\n"})
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
	g := newTestRunner(t, dir)

	t.Run("materialize clears a pre-created dest", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "ws")
		// An earlier step pre-creates the workspace with an extra dir.
		if err := os.MkdirAll(filepath.Join(dest, "extra"), 0o755); err != nil { //nolint:gosec // G301: test-owned workspace
			t.Fatalf("pre-create: %v", err)
		}
		if err := g.materialize(context.Background(), head, dest); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if _, err := os.Stat(filepath.Join(dest, "extra")); !errors.Is(err, os.ErrNotExist) {
			t.Error("pre-created stray directory survived materialization")
		}
	})
	t.Run("verifyMaterialized rejects a stray dir", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "ws")
		if err := g.materialize(context.Background(), head, dest); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(dest, "extra"), 0o755); err != nil { //nolint:gosec // G301: test-owned workspace
			t.Fatalf("plant dir: %v", err)
		}
		if err := g.verifyMaterialized(context.Background(), head, dest); !errors.Is(err, ErrWorkspaceMismatch) {
			t.Fatalf("err = %v, want ErrWorkspaceMismatch for a stray directory", err)
		}
	})
	t.Run("legitimate tree-prefix dirs pass", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "ws")
		if err := g.materialize(context.Background(), head, dest); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		// pkg/ is a legitimate ancestor of pkg/a.go and must not be a stray.
		if err := g.verifyMaterialized(context.Background(), head, dest); err != nil {
			t.Errorf("legitimate tree-prefix directory rejected: %v", err)
		}
	})
}

// TestMaterializeRunsNoFilters is the Codex-review (P1) regression:
// materialization must extract blob bytes directly and never run a
// git smudge/clean filter (which would execute host code outside the
// Room). A filter configured in the checkout that writes a sentinel
// must not fire, and the materialized bytes must be the committed blob.
func TestMaterializeRunsNoFilters(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"code.txt": "committed\n"})
	head := commitCandidate(t, dir, base, map[string]string{"main.go": "package main\n"})
	sentinel := filepath.Join(t.TempDir(), "filter-fired")
	// A smudge filter that would run host code and rewrite content.
	runGit(t, dir, "config", "filter.pwn.smudge", "sh -c 'echo fired > "+sentinel+"; echo PWNED'")
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("code.txt filter=pwn\n"), 0o644); err != nil { //nolint:gosec // G306: test-owned fixture .git
		t.Fatalf("write info/attributes: %v", err)
	}
	g := newTestRunner(t, dir)
	dest := filepath.Join(t.TempDir(), "ws")
	if err := g.materialize(context.Background(), head, dest); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := os.Stat(sentinel); !errors.Is(err, os.ErrNotExist) {
		t.Error("a smudge filter fired during materialization")
	}
	got, err := os.ReadFile(filepath.Join(dest, "code.txt")) //nolint:gosec // G304: test-owned workspace
	if err != nil {
		t.Fatalf("read materialized file: %v", err)
	}
	if string(got) != "committed\n" {
		t.Errorf("materialized content = %q, want the committed blob (filter altered it)", got)
	}
}

// TestMaterializeRejectsMalformedTree is the Codex-review (P1)
// regression: a malformed tree with both a symlink `a` and a blob
// `a/b` (which a well-formed tree cannot hold) must fail closed before
// any write, so materialize cannot follow the symlink prefix and write
// `a/b` outside the workspace to an arbitrary host path.
func TestMaterializeRejectsMalformedTree(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"seed": "x\n"})

	// Blob whose bytes are a symlink target escaping the workspace.
	escape := filepath.Join(t.TempDir(), "escape-target")
	linkBlob := runGitStdin(t, dir, []byte(escape), "hash-object", "-w", "--stdin")
	fileBlob := runGitStdin(t, dir, []byte("pwned\n"), "hash-object", "-w", "--stdin")

	// Subtree holding `b`.
	subtree := runGitStdin(t, dir, treeRecord("100644", "b", fileBlob), "hash-object", "-w", "-t", "tree", "--literally", "--stdin")
	// Malformed top tree: `a` as a symlink and `a` as a subtree.
	raw := append(treeRecord("120000", "a", linkBlob), treeRecord("040000", "a", subtree)...)
	topTree := runGitStdin(t, dir, raw, "hash-object", "-w", "-t", "tree", "--literally", "--stdin")
	head := runGit(t, dir, "commit-tree", topTree, "-p", base, "-m", "malformed")

	g := newTestRunner(t, dir)
	dest := filepath.Join(t.TempDir(), "ws")
	err := g.materialize(context.Background(), head, dest)
	if !errors.Is(err, ErrMalformedTree) {
		t.Fatalf("err = %v, want ErrMalformedTree", err)
	}
	if _, statErr := os.Stat(filepath.Join(escape, "b")); !errors.Is(statErr, os.ErrNotExist) {
		t.Error("materialize wrote through the symlink prefix, escaping the workspace")
	}
}

// TestMaterializeRejectsMalformedTreePaths is the #140 hardening
// enumeration: git write-tree never emits a path with a traversal,
// absolute, or empty component, nor a duplicate path, but a tree crafted
// with `hash-object -t tree --literally` can. Each would let
// filepath.Join(dest, path) escape the workspace or desync the
// symlink-entrypoint guard (reads the first record) from materialization
// (writes the last), so listTree must reject every one as
// ErrMalformedTree before any write.
func TestMaterializeRejectsMalformedTreePaths(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"seed": "x\n"})
	blob := runGitStdin(t, dir, []byte("data\n"), "hash-object", "-w", "--stdin")

	cases := []struct {
		name string
		raw  []byte
	}{
		{"dotdot component", treeRecord("100644", "..", blob)},
		{"dot component", treeRecord("100644", ".", blob)},
		{"absolute path", treeRecord("100644", "/abs", blob)},
		{"empty middle component", treeRecord("100644", "a//b", blob)},
		{"trailing slash", treeRecord("100644", "a/", blob)},
		{"nested dotdot", treeRecord("100644", "x/../y", blob)},
		{"duplicate path", append(treeRecord("100644", "dup", blob), treeRecord("100644", "dup", blob)...)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree := runGitStdin(t, dir, tc.raw, "hash-object", "-w", "-t", "tree", "--literally", "--stdin")
			head := runGit(t, dir, "commit-tree", tree, "-p", base, "-m", tc.name)
			g := newTestRunner(t, dir)
			dest := filepath.Join(t.TempDir(), "ws")
			if err := g.materialize(context.Background(), head, dest); !errors.Is(err, ErrMalformedTree) {
				t.Fatalf("materialize = %v, want ErrMalformedTree", err)
			}
		})
	}
}

// treeRecord builds one raw git tree entry: "<mode> <name>\0" followed
// by the 20 raw bytes of the hex object id.
func treeRecord(mode, name, hexOID string) []byte {
	raw, err := hex.DecodeString(hexOID)
	if err != nil || len(raw) != 20 {
		panic("bad oid: " + hexOID)
	}
	return append([]byte(mode+" "+name+"\x00"), raw...)
}
