package atomicfile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Writer stages bytes in a temporary file for one later atomic commit.
type Writer struct {
	dir  string
	file *os.File
	name string
}

// Create opens a temporary file in dir. Abort removes it unless Commit has
// already renamed it into place.
func Create(dir, pattern string) (*Writer, error) {
	if dir == "" {
		return nil, errors.New("atomic file directory is empty")
	}
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return &Writer{dir: filepath.Clean(dir), file: file, name: file.Name()}, nil
}

// Write appends bytes to the staged file.
func (w *Writer) Write(p []byte) (int, error) {
	if w == nil || w.file == nil {
		return 0, fs.ErrClosed
	}
	return w.file.Write(p)
}

// Commit syncs and closes the staged file, renames it over target, and syncs
// the target's parent directory. Target must be in the directory passed to
// Create so the rename is one atomic filesystem operation.
func (w *Writer) Commit(target string) error {
	return w.commit(target, os.Rename, SyncDir)
}

// CommitWithSync is Commit with a caller-supplied directory barrier. It keeps
// failure injection at the real publication boundary for callers that already
// test retry after a visible-but-unsynced entry.
func (w *Writer) CommitWithSync(target string, syncDir func(string) error) error {
	if syncDir == nil {
		return errors.New("atomic file directory sync is nil")
	}
	return w.commit(target, os.Rename, syncDir)
}

func (w *Writer) commit(
	target string,
	rename func(string, string) error,
	syncDir func(string) error,
) error {
	if w == nil || w.file == nil {
		return fs.ErrClosed
	}
	if filepath.Clean(filepath.Dir(target)) != w.dir {
		return errors.New("atomic file target is outside the staging directory")
	}

	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	w.file = nil
	if err := errors.Join(syncErr, closeErr); err != nil {
		return err
	}
	if err := rename(w.name, target); err != nil {
		return err
	}
	w.name = ""
	return syncDir(w.dir)
}

// Abort closes and removes the staged file. It is idempotent and is a no-op
// after Commit has renamed the temporary path.
func (w *Writer) Abort() error {
	if w == nil {
		return nil
	}
	var closeErr error
	if w.file != nil {
		closeErr = w.file.Close()
		w.file = nil
	}
	var removeErr error
	if w.name != "" {
		removeErr = os.Remove(w.name)
	}
	if errors.Is(removeErr, fs.ErrNotExist) {
		removeErr = nil
	}
	return errors.Join(closeErr, removeErr)
}

// SyncDir persists directory-entry changes and reports both sync and close
// failures.
func SyncDir(dir string) error {
	file, err := os.Open(dir) //nolint:gosec // callers supply daemon-owned directories
	if err != nil {
		return err
	}
	return errors.Join(file.Sync(), file.Close())
}

// WriteFile durably replaces path with data and mode perm.
func WriteFile(path string, data []byte, perm fs.FileMode) error {
	return writeFile(path, data, perm, os.Rename)
}

// WriteFileNoReplace durably creates path without replacing an existing
// target. It returns an error matching fs.ErrExist when another target wins.
func WriteFileNoReplace(path string, data []byte, perm fs.FileMode) error {
	return writeFile(path, data, perm, RenameNoReplace)
}

func writeFile(
	path string,
	data []byte,
	perm fs.FileMode,
	rename func(string, string) error,
) (err error) {
	dir := filepath.Dir(path)
	w, err := Create(dir, "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		err = withCleanup(err, w.Abort())
	}()
	if err := w.file.Chmod(perm); err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	return w.commit(path, rename, SyncDir)
}

// withCleanup makes cleanup failure authoritative. In particular, callers
// must not classify an fs.ErrExist race as clean when its losing temporary
// file could not be removed.
func withCleanup(operationErr, cleanupErr error) error {
	if cleanupErr == nil {
		return operationErr
	}
	if operationErr == nil {
		return cleanupErr
	}
	operationMessage := operationErr.Error()
	return fmt.Errorf("atomic file operation failed (%s); cleanup: %w", operationMessage, cleanupErr)
}
