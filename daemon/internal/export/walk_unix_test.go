//go:build unix

package export

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestWalkWorkspaceRealFilesystem drives the walker over a real on-disk
// workspace through os.DirFS, the exact fs.FS the helper binary uses:
// real symlinks (relative, absolute, dangling), a FIFO, a setuid file,
// and a submodule marked by a .git file. Complements the MapFS test,
// which fakes the modes a real unprivileged filesystem can't create.
func TestWalkWorkspaceRealFilesystem(t *testing.T) {
	dir := t.TempDir()
	mustMkdir(t, dir, ".git")
	mustWrite(t, dir, ".git/HEAD", "ref: refs/heads/main\n", 0o644)
	mustWrite(t, dir, "README.md", "# fixture\n", 0o644)
	mustWrite(t, dir, "bin/tool.sh", "#!/bin/sh\nexit 0\n", 0o755)
	mustWrite(t, dir, "nested/deep/file.txt", "content\n", 0o600)
	mustSymlink(t, dir, "docs-link", "README.md")
	mustSymlink(t, dir, "gone-link", "/nonexistent/target")
	mustWrite(t, dir, "vendor/dep/.git", "gitdir: ../../.git/modules/dep\n", 0o644)
	mustWrite(t, dir, "vendor/dep/code.go", "package dep\n", 0o644)

	fifo := filepath.Join(dir, "queue")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	mustWrite(t, dir, "suid-helper", "#!/bin/sh\n", 0o755)
	suid := filepath.Join(dir, "suid-helper")
	if err := os.Chmod(suid, 0o755|os.ModeSetuid); err != nil {
		t.Fatalf("chmod setuid: %v", err)
	}
	info, err := os.Lstat(suid)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSetuid == 0 {
		t.Skip("filesystem drops setuid; cannot exercise unusual_mode here")
	}

	got, err := walkWorkspace(os.DirFS(dir), 0)
	if err != nil {
		t.Fatalf("walkWorkspace: %v", err)
	}

	want := []Entry{
		{Path: ".git", Kind: EntryGitDir},
		{Path: "README.md", Kind: EntryRegular, Mode: ptrTo("0644"), Size: ptrTo(int64(10))},
		{Path: "bin/tool.sh", Kind: EntryRegular, Mode: ptrTo("0755"), Size: ptrTo(int64(17))},
		{Path: "docs-link", Kind: EntrySymlink, Target: ptrTo("README.md")},
		{Path: "gone-link", Kind: EntrySymlink, Target: ptrTo("/nonexistent/target")},
		// 0600 normalizes to 0644: only the owner-executable bit survives.
		{Path: "nested/deep/file.txt", Kind: EntryRegular, Mode: ptrTo("0644"), Size: ptrTo(int64(8))},
		{Path: "queue", Kind: EntrySpecial},
		{Path: "suid-helper", Kind: EntryUnusualMode, Mode: ptrTo("04755")},
		{Path: "vendor/dep", Kind: EntrySubmodule},
	}
	assertEntriesEqual(t, want, got)
}

// TestWalkWorkspaceDeviceNode records a real device node as special. mknod
// needs privileges almost everywhere, so the test skips on EPERM; FIFO and
// the MapFS fakes keep the special classification covered regardless.
func TestWalkWorkspaceDeviceNode(t *testing.T) {
	dir := t.TempDir()
	dev := filepath.Join(dir, "null-clone")
	if err := syscall.Mknod(dev, syscall.S_IFCHR|0o600, 0x0103); err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EINVAL) {
			t.Skipf("mknod unavailable without privileges: %v", err)
		}
		t.Fatalf("mknod: %v", err)
	}

	got, err := walkWorkspace(os.DirFS(dir), 0)
	if err != nil {
		t.Fatalf("walkWorkspace: %v", err)
	}
	want := []Entry{{Path: "null-clone", Kind: EntrySpecial}}
	assertEntriesEqual(t, want, got)
}

func mustMkdir(t *testing.T, dir, rel string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(rel)), 0o750); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, dir, rel, content string, mode fs.FileMode) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by umask; pin it explicitly so the
	// classification the test asserts is the mode actually on disk.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, dir, rel, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(dir, rel)); err != nil {
		t.Fatal(err)
	}
}
