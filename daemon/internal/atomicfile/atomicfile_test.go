package atomicfile

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func TestRenameNoReplaceRefusesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(source, []byte("candidate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("winner"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := RenameNoReplace(source, target)
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("RenameNoReplace error = %v, want fs.ErrExist", err)
	}
	assertFileContent(t, source, "candidate")
	assertFileContent(t, target, "winner")
}

func TestCreateRejectsEmptyDirectory(t *testing.T) {
	if _, err := Create("", ".state-*"); err == nil {
		t.Fatal("Create accepted an empty directory")
	}
}

func TestWriteFileReplacesWholeFileWithRequestedMode(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state.json")
	if err := os.WriteFile(target, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(target, []byte("replacement"), 0o640); err != nil {
		t.Fatal(err)
	}

	assertFileContent(t, target, "replacement")
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %04o, want 0640", got)
	}
	assertDirectoryEntries(t, dir, "state.json")
}

func TestWriteFileReadersSeeOnlyWholeVersions(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state")
	oldBody := bytes.Repeat([]byte("old-version\n"), 4096)
	newBody := bytes.Repeat([]byte("new-version\n"), 4096)
	if err := WriteFile(target, oldBody, 0o600); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	readErr := make(chan error, 1)
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			body, err := os.ReadFile(target) //nolint:gosec // test-owned temporary path
			if err != nil {
				select {
				case readErr <- err:
				default:
				}
				return
			}
			if !bytes.Equal(body, oldBody) && !bytes.Equal(body, newBody) {
				select {
				case readErr <- errors.New("reader observed a partial file"):
				default:
				}
				return
			}
		}
	}()
	for i := range 12 {
		body := oldBody
		if i%2 == 0 {
			body = newBody
		}
		if err := WriteFile(target, body, 0o600); err != nil {
			close(stop)
			readers.Wait()
			t.Fatal(err)
		}
	}
	close(stop)
	readers.Wait()
	select {
	case err := <-readErr:
		t.Fatal(err)
	default:
	}
}

func TestWriteFileFailureLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "occupied")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(target, []byte("candidate"), 0o600); err == nil {
		t.Fatal("WriteFile unexpectedly replaced a directory")
	}
	assertDirectoryEntries(t, dir, "occupied")
}

func TestWriterCommitAndAbort(t *testing.T) {
	dir := t.TempDir()
	w, err := Create(dir, ".blob-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("blob")); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "digest")
	if err := w.Commit(target); err != nil {
		t.Fatal(err)
	}
	if w.name != "" {
		t.Fatalf("committed writer retained temporary path %q", w.name)
	}
	if err := w.Abort(); err != nil {
		t.Fatalf("Abort after Commit: %v", err)
	}
	assertFileContent(t, target, "blob")

	aborted, err := Create(dir, ".blob-*")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aborted.Write([]byte("discard")); err != nil {
		t.Fatal(err)
	}
	if err := aborted.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := aborted.Abort(); err != nil {
		t.Fatalf("second Abort: %v", err)
	}
	assertDirectoryEntries(t, dir, "digest")
}

func TestWriterCommitWithSyncUsesInjectedBarrier(t *testing.T) {
	dir := t.TempDir()
	w, err := Create(dir, ".blob-*")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Abort() }()
	if _, err := w.Write([]byte("blob")); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected directory sync failure")
	var synced string
	target := filepath.Join(dir, "digest")
	err = w.CommitWithSync(target, func(path string) error {
		synced = path
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("CommitWithSync error = %v, want injected failure", err)
	}
	if synced != dir {
		t.Fatalf("synced directory = %q, want %q", synced, dir)
	}
	assertFileContent(t, target, "blob")
}

func TestWriteFileNoReplaceCleansLosingTemporary(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "key")
	if err := os.WriteFile(target, []byte("winner"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileNoReplace(target, []byte("loser"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("WriteFileNoReplace error = %v, want fs.ErrExist", err)
	}
	assertFileContent(t, target, "winner")
	assertDirectoryEntries(t, dir, "key")
}

func TestCleanupFailureSupersedesExpectedOperationError(t *testing.T) {
	cleanupErr := errors.New("cleanup failed")
	err := withCleanup(fs.ErrExist, cleanupErr)
	if errors.Is(err, fs.ErrExist) {
		t.Fatalf("cleanup error %v retained fs.ErrExist classification", err)
	}
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("cleanup error = %v, want cleanup failure", err)
	}
}

func assertFileContent(t *testing.T, path, want string) {
	t.Helper()
	body, err := os.ReadFile(path) //nolint:gosec // test-owned temporary path
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want {
		t.Fatalf("%s content = %q, want %q", path, body, want)
	}
}

func assertDirectoryEntries(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(entries))
	for i, entry := range entries {
		got[i] = entry.Name()
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s entries = %v, want %v", dir, got, want)
	}
}
