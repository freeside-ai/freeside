package verify

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

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

func TestCommitReaderPinsOnceAndKeepsReadChecks(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"file": "base", "nested/file": "nested"})
	if err := os.Symlink("file", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	head := commitCandidate(t, dir, base, map[string]string{"file": "head"})
	git, check := countedCommitGit(t)
	reader, err := OpenCommitReader(t.Context(), git, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, tc := range []struct {
		commit, path, want string
		max                int64
		present            bool
		err                error
	}{
		{base, "file", "base", 64, true, nil},
		{head, "file", "head", 64, true, nil},
		{head, "absent", "", 64, false, nil},
		{head, "link", "", 64, false, ErrCommitFileUnreadable},
		{head, "nested", "", 64, false, ErrCommitFileUnreadable},
		{head, "file", "", 1, false, ErrCommitFileUnreadable},
		{"HEAD", "file", "", 64, false, ErrInvalidOptions},
		{head, "", "", 64, false, ErrInvalidOptions},
		{head, "file", "", 0, false, ErrInvalidOptions},
		{strings.Repeat("0", 40), "absent", "", 64, false, ErrGitPlumbing},
	} {
		content, present, err := reader.ReadFile(t.Context(), tc.commit, tc.path, tc.max)
		if !errors.Is(err, tc.err) || present != tc.present || string(content) != tc.want || (!present && content != nil) {
			t.Fatalf("read %s at %s = %q, %t, %v; want %q, %t, %v", tc.path, tc.commit, content, present, err, tc.want, tc.present, tc.err)
		}
	}
	check(7, 2)
}

func TestCommitReaderSurvivesHeadAndWorktreeMovement(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"file": "base"})
	reader, err := OpenCommitReader(t.Context(), "git", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	assertCommitRead(t, reader, base, "base")
	head := commitCandidate(t, dir, base, map[string]string{"file": "head"})
	runGit(t, dir, "reset", "-q", "--hard", head)
	writeFiles(t, dir, map[string]string{"file": "dirty worktree"})
	assertCommitRead(t, reader, base, "base")
	assertCommitRead(t, reader, head, "head")
}

func TestCommitReaderKeepsTheResolvedCheckout(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"file": "base"})
	other, otherHead := initRepo(t, map[string]string{"file": "other"})
	link := filepath.Join(t.TempDir(), "checkout")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenCommitReader(t.Context(), "git", link)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	assertCommitRead(t, reader, base, "base")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	assertCommitRead(t, reader, base, "base")
	assertCommitReadFault(t, reader, otherHead)
}

func TestCommitReaderFailsClosedWhenItsGitDirectoryChanges(t *testing.T) {
	for _, replacement := range []string{"sha1", "sha256", "removed"} {
		t.Run(replacement, func(t *testing.T) {
			dir, base := initRepo(t, map[string]string{"file": "base"})
			reader, err := OpenCommitReader(t.Context(), "git", dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			assertCommitRead(t, reader, base, "base")
			gitDir := filepath.Join(dir, ".git")
			if err := os.Rename(gitDir, filepath.Join(t.TempDir(), "original.git")); err != nil {
				t.Fatal(err)
			}
			if replacement != "removed" {
				other := t.TempDir()
				runGit(t, other, "init", "-q", "--object-format="+replacement)
				runGit(t, other, "commit", "-q", "--allow-empty", "-m", "other")
				if err := os.Rename(filepath.Join(other, ".git"), gitDir); err != nil {
					t.Fatal(err)
				}
			}
			assertCommitReadFault(t, reader, base)
		})
	}
}

func assertCommitRead(t *testing.T, reader *CommitReader, commit, want string) {
	t.Helper()
	content, present, err := reader.ReadFile(t.Context(), commit, "file", 64)
	if err != nil || !present || string(content) != want {
		t.Fatalf("read at %s = %q, %t, %v; want %q", commit, content, present, err, want)
	}
}

func assertCommitReadFault(t *testing.T, reader *CommitReader, commit string) {
	t.Helper()
	for _, path := range []string{"file", "absent"} {
		content, present, err := reader.ReadFile(t.Context(), commit, path, 64)
		if !errors.Is(err, ErrGitPlumbing) || present || content != nil {
			t.Fatalf("read %s at %s = %q, %t, %v; want plumbing fault", path, commit, content, present, err)
		}
	}
}

func TestCommitReaderRemovesScratchOnSuccessAndFailure(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"file": "base"})
	unsupported := t.TempDir()
	runGit(t, unsupported, "init", "-q", "--object-format=sha256")
	scratchRoot := t.TempDir()
	t.Setenv("TMPDIR", scratchRoot)
	for _, tc := range []struct {
		checkout, commit string
		want             error
	}{
		{dir, base, nil},
		{dir, strings.Repeat("0", 40), ErrGitPlumbing},
		{filepath.Join(dir, "missing"), base, ErrGitPlumbing},
		{unsupported, base, ErrUnsupportedRepo},
		{filepath.Join(dir, "missing"), "HEAD", ErrInvalidOptions},
	} {
		_, _, err := ReadFileAtCommit(t.Context(), "git", tc.checkout, tc.commit, "file", 64)
		if !errors.Is(err, tc.want) {
			t.Fatalf("single read error = %v, want %v", err, tc.want)
		}
		entries, err := os.ReadDir(scratchRoot)
		if err != nil || len(entries) != 0 {
			t.Fatalf("scratch after single read = %v, %v", entries, err)
		}
	}
	reader, err := OpenCommitReader(t.Context(), "git", dir)
	if err != nil {
		t.Fatal(err)
	}
	assertCommitRead(t, reader, base, "base")
	entries, err := os.ReadDir(scratchRoot)
	if err != nil || len(entries) != 1 {
		t.Fatalf("open group scratch = %v, %v; want one directory", entries, err)
	}
	reader.Close()
	assertCommitReadFault(t, reader, base)
	entries, err = os.ReadDir(scratchRoot)
	if err != nil || len(entries) != 0 {
		t.Fatalf("closed group scratch = %v, %v", entries, err)
	}
}
