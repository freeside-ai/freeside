package repotemplate

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestFilesKeyIsDeterministicAndPreservesBytes(t *testing.T) {
	first := map[string]string{}
	first["a"], first["b"] = "one", "two"
	second := map[string]string{}
	second["b"], second["a"] = "two", "one"
	if FilesKey(first) != FilesKey(second) {
		t.Fatal("map insertion order changed the key")
	}
	for _, pair := range [][2]map[string]string{
		{{"file": "\xff"}, {"file": "\xfe"}},
		{{"\xff": "same"}, {"\xfe": "same"}},
		{{"file": ""}, {}},
	} {
		if FilesKey(pair[0]) == FilesKey(pair[1]) {
			t.Fatalf("distinct inputs share a key: %q, %q", pair[0], pair[1])
		}
	}
}

func TestCacheBuildsOncePerKey(t *testing.T) {
	var cache Cache[string]
	var builds atomic.Int32
	build := func() (string, string) {
		builds.Add(1)
		dir := t.TempDir()
		write(t, filepath.Join(dir, "file"), "original", 0o640)
		return dir, "metadata"
	}
	first, meta := cache.Copy(t, "first", build)
	second, _ := cache.Copy(t, "first", build)
	if first == second || meta != "metadata" || builds.Load() != 1 {
		t.Fatalf("copies %s, %s; metadata %q; builds %d", first, second, meta, builds.Load())
	}
	var callers sync.WaitGroup
	dirs := make(chan string, 16)
	for range 16 {
		callers.Go(func() {
			dir, got := cache.Copy(t, "parallel", build)
			if got != "metadata" {
				t.Errorf("metadata = %q", got)
			}
			dirs <- dir
		})
	}
	callers.Wait()
	close(dirs)
	seen := map[string]bool{first: true, second: true}
	for dir := range dirs {
		if seen[dir] {
			t.Errorf("shared copy %s", dir)
		}
		seen[dir] = true
		data, err := os.ReadFile(filepath.Join(dir, "file")) //nolint:gosec // G304: cache output in t.TempDir.
		if err != nil || string(data) != "original" {
			t.Errorf("copy contents = %q, %v", data, err)
		}
	}
	if builds.Load() != 2 {
		t.Fatalf("builds = %d, want one for each key", builds.Load())
	}
}

func TestCopyPreservesTreeAndIsolation(t *testing.T) {
	source := t.TempDir()
	if err := os.MkdirAll(filepath.Join(source, "nested", "empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(source, "nested", "file"), "bytes\x00\n", 0o654)
	write(t, filepath.Join(source, "script"), "#!/bin/sh\ntrue\n", 0o751)
	if err := os.Symlink("nested/file", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", filepath.Join(source, "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(source, "nested"), 0o550); err != nil { //nolint:gosec // G302: deliberately read-only test directory, checked for copy fidelity.
		t.Fatal(err)
	}
	cleanupCopy := func(dir string) {
		t.Cleanup(func() {
			// TempDir cleanup needs write access to the deliberately read-only parent.
			if err := os.Chmod(filepath.Join(dir, "nested"), 0o700); err != nil { //nolint:gosec // G302: owner-only write access for test directory cleanup.
				t.Error(err)
			}
		})
	}
	var cache Cache[int]
	first, meta := cache.Copy(t, "tree", func() (string, int) { return source, 42 })
	cleanupCopy(first)
	assertTree(t, source, first)
	if meta != 42 {
		t.Fatalf("metadata = %d, want 42", meta)
	}
	if err := os.Chmod(filepath.Join(first, "nested"), 0o700); err != nil { //nolint:gosec // G302: owner-only write access to mutate a test directory.
		t.Fatal(err)
	}
	write(t, filepath.Join(first, "nested", "file"), "changed", 0o600)
	if err := os.Remove(filepath.Join(first, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("script", filepath.Join(first, "link")); err != nil {
		t.Fatal(err)
	}
	second, _ := cache.Copy(t, "tree", func() (string, int) {
		t.Fatal("cached build ran again")
		return "", 0
	})
	cleanupCopy(second)
	assertTree(t, source, second)
	// The snapshot also survives removal of the builder's original directory.
	if err := os.Chmod(filepath.Join(source, "nested"), 0o700); err != nil { //nolint:gosec // G302: owner-only write access to remove the test's source directory.
		t.Fatal(err)
	}
	if err := os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	third, _ := cache.Copy(t, "tree", func() (string, int) {
		t.Fatal("snapshot depended on the original directory")
		return "", 0
	})
	cleanupCopy(third)
	assertTree(t, second, third)
}

func TestFailedBuildIsNotCached(t *testing.T) {
	var cache Cache[int]
	builds := 0
	message := fatalAttempt(t, func(tb testing.TB) {
		cache.Copy(tb, "retry", func() (string, int) {
			builds++
			tb.Fatal("failed build")
			return "", 0
		})
	})
	if message != "failed build" {
		t.Fatalf("failure = %q", message)
	}
	_, meta := cache.Copy(t, "retry", func() (string, int) {
		builds++
		return t.TempDir(), 7
	})
	if builds != 2 || meta != 7 {
		t.Fatalf("builds = %d, metadata = %d", builds, meta)
	}
}

func TestUnsupportedFileTypeFailsAndCanRetry(t *testing.T) {
	var cache Cache[string]
	source := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(source, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	message := fatalAttempt(t, func(tb testing.TB) {
		cache.Copy(tb, "retry", func() (string, string) { return source, "invalid" })
	})
	if !strings.Contains(message, "unsupported file type at pipe") {
		t.Fatalf("failure = %q", message)
	}
	_, meta := cache.Copy(t, "retry", func() (string, string) { return t.TempDir(), "valid" })
	if meta != "valid" {
		t.Fatalf("metadata = %q", meta)
	}
}

func TestCopyIsAStandaloneGitRepository(t *testing.T) {
	source := t.TempDir()
	git(t, source, "init", "-q", "-b", "main", "--object-format=sha1")
	git(t, source, "config", "maintenance.auto", "false")
	write(t, filepath.Join(source, "file"), "base\n", 0o644)
	git(t, source, "add", "-A")
	git(t, source, "commit", "-q", "-m", "base")
	head := git(t, source, "rev-parse", "HEAD")
	var cache Cache[string]
	dir, meta := cache.Copy(t, "git", func() (string, string) { return source, head })
	if got := git(t, dir, "rev-parse", "HEAD"); got != head || meta != head {
		t.Fatalf("HEAD = %q, metadata = %q, want %q", got, meta, head)
	}
	if got := git(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("copy is dirty: %s", got)
	}
	git(t, dir, "fsck")
	write(t, filepath.Join(dir, "file"), "candidate\n", 0o644)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "candidate")
	second, _ := cache.Copy(t, "git", func() (string, string) {
		t.Fatal("git fixture rebuilt")
		return "", ""
	})
	if got := git(t, second, "rev-parse", "HEAD"); got != head {
		t.Fatalf("mutation leaked HEAD %q, want %q", got, head)
	}
	if got := git(t, second, "status", "--porcelain"); got != "" {
		t.Fatalf("later copy is dirty: %s", got)
	}
	git(t, second, "fsck")
}

func assertTree(t *testing.T, want, got string) {
	t.Helper()
	err := filepath.WalkDir(want, func(path string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(want, path)
		if err != nil {
			return err
		}
		copyPath := filepath.Join(got, rel)
		a, err := item.Info()
		if err != nil {
			return err
		}
		b, err := os.Lstat(copyPath)
		if err != nil {
			return err
		}
		if a.Mode() != b.Mode() {
			t.Errorf("%s mode = %s, want %s", rel, b.Mode(), a.Mode())
		}
		switch {
		case a.IsDir():
			x, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			y, err := os.ReadDir(copyPath)
			if err != nil {
				return err
			}
			if len(x) != len(y) {
				t.Errorf("%s entries = %d, want %d", rel, len(y), len(x))
			}
		case a.Mode()&fs.ModeSymlink != 0:
			x, err := os.Readlink(path)
			if err != nil {
				return err
			}
			y, err := os.Readlink(copyPath)
			if err != nil {
				return err
			}
			if x != y {
				t.Errorf("%s target = %q, want %q", rel, y, x)
			}
		case a.Mode().IsRegular():
			x, err := os.ReadFile(path) //nolint:gosec // G304: test-owned fixture tree.
			if err != nil {
				return err
			}
			y, err := os.ReadFile(copyPath) //nolint:gosec // G304: test-owned copy.
			if err != nil {
				return err
			}
			if !bytes.Equal(x, y) {
				t.Errorf("%s contents = %q, want %q", rel, y, x)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

// Exercise the real Goexit behavior without marking the containing test failed.
type fatalTB struct {
	testing.TB
	message chan<- string
}

func (t fatalTB) Fatal(args ...any) {
	t.message <- fmt.Sprint(args...)
	runtime.Goexit()
}

func (t fatalTB) Fatalf(format string, args ...any) {
	t.message <- fmt.Sprintf(format, args...)
	runtime.Goexit()
}

func fatalAttempt(t *testing.T, run func(testing.TB)) string {
	t.Helper()
	message := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		run(fatalTB{TB: t, message: message})
	}()
	<-done
	select {
	case got := <-message:
		return got
	default:
		t.Fatal("expected a fatal failure")
		return ""
	}
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // G204: fixed git commands on test-owned repositories.
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(),
		"GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.invalid",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.invalid",
		"GIT_AUTHOR_DATE=1752600000 +0000", "GIT_COMMITTER_DATE=1752600000 +0000",
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
