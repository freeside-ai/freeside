package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	reviewWorkspaceStagingPrefix        = "staging-"
	reviewWorkspaceStagingWorkspace     = "workspace"
	reviewWorkspaceStagingRecordName    = "staging-owner.json"
	reviewWorkspaceStagingRecordVersion = 1
	reviewWorkspaceStagingRecordMax     = strictjson.Limit(4096)
)

// reviewWorkspaceStagingRecord authenticates an abandoned staging directory to
// the review invocation that created it. It lives in the staging directory root
// beside workspace/, outside candidate content, so candidate materialization
// cannot forge or overwrite it and renaming workspace/ never carries it into
// the retained workspace.
type reviewWorkspaceStagingRecord struct {
	Version      int                 `json:"version"`
	InvocationID domain.InvocationID `json:"invocation_id"`
	DirName      string              `json:"dir_name"`
}

// claimReviewWorkspaceStaging creates a fresh staging-* directory under parent,
// writes and syncs its invocation-bound ownership record beside workspace/, and
// syncs the staging directory so the record survives power loss before
// materialization starts. Any failure removes the directory this call created.
func claimReviewWorkspaceStaging(parent string, id domain.InvocationID) (staging string, err error) {
	staging, err = os.MkdirTemp(parent, reviewWorkspaceStagingPrefix)
	if err != nil {
		return "", fmt.Errorf("create review workspace staging: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			if rmErr := os.RemoveAll(staging); rmErr != nil {
				err = errors.Join(err, fmt.Errorf("cleanup failed review workspace staging: %w", rmErr))
			}
		}
	}()

	rec := reviewWorkspaceStagingRecord{
		Version:      reviewWorkspaceStagingRecordVersion,
		InvocationID: id,
		DirName:      filepath.Base(staging),
	}
	body, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("encode review workspace staging record: %w", err)
	}
	if err := writeReviewWorkspaceStagingRecord(staging, body); err != nil {
		return "", err
	}
	cleanup = false
	return staging, nil
}

func writeReviewWorkspaceStagingRecord(staging string, body []byte) error {
	recordPath := filepath.Join(staging, reviewWorkspaceStagingRecordName)
	f, err := os.OpenFile(recordPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: fresh MkdirTemp staging directory
	if err != nil {
		return fmt.Errorf("create review workspace staging record: %w", err)
	}
	_, writeErr := f.Write(body)
	var syncErr error
	if writeErr == nil {
		syncErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write review workspace staging record: %w", err)
	}

	dir, err := os.Open(staging) //nolint:gosec // G304: fresh MkdirTemp staging directory
	if err != nil {
		return fmt.Errorf("open review workspace staging for sync: %w", err)
	}
	if err := errors.Join(dir.Sync(), dir.Close()); err != nil {
		return fmt.Errorf("sync review workspace staging: %w", err)
	}
	return nil
}

// reclaimReviewWorkspaceStaging deletes abandoned staging-* directories under
// parent that authenticate to id, and leaves every other entry untouched.
//
// Before deleting any entry this Tier A gate re-checks:
//   - the staging-* entry is a real directory (not a symlink) and matches the
//     child os.Root opened for it;
//   - its ownership record is a bounded regular file matching the opened file,
//     decodes strictly with the expected version, and names id and the entry;
//   - after clearing children through the child root (record last so a partial
//     failure stays authenticatable), the entry still matches before removal.
//
// Unauthenticated entries are skipped; deletion I/O errors are returned.
func reclaimReviewWorkspaceStaging(parent string, id domain.InvocationID) error {
	parentRoot, err := os.OpenRoot(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open review workspaces directory: %w", err)
	}
	defer func() { _ = parentRoot.Close() }()

	entries, err := fs.ReadDir(parentRoot.FS(), ".")
	if err != nil {
		return fmt.Errorf("list review workspaces directory: %w", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, reviewWorkspaceStagingPrefix) {
			continue
		}
		stagingRoot, entryInfo, ok := authenticateReviewWorkspaceStaging(parentRoot, name, id)
		if !ok {
			continue
		}
		if err := deleteAuthenticatedReviewWorkspaceStaging(parentRoot, stagingRoot, name, entryInfo); err != nil {
			return err
		}
	}
	return nil
}

func authenticateReviewWorkspaceStaging(
	parentRoot *os.Root, name string, id domain.InvocationID,
) (*os.Root, os.FileInfo, bool) {
	entryInfo, err := parentRoot.Lstat(name)
	if err != nil || entryInfo.Mode()&os.ModeSymlink != 0 || !entryInfo.IsDir() {
		return nil, nil, false
	}
	stagingRoot, err := parentRoot.OpenRoot(name)
	if err != nil {
		return nil, nil, false
	}
	keepOpen := false
	defer func() {
		if !keepOpen {
			_ = stagingRoot.Close()
		}
	}()

	openedInfo, err := stagingRoot.Stat(".")
	if err != nil || !os.SameFile(openedInfo, entryInfo) {
		return nil, nil, false
	}
	recLstat, err := stagingRoot.Lstat(reviewWorkspaceStagingRecordName)
	if err != nil || !recLstat.Mode().IsRegular() || recLstat.Size() > int64(reviewWorkspaceStagingRecordMax) {
		return nil, nil, false
	}
	// OpenFile on os.Root can still follow an in-root symlink if the record was
	// swapped after Lstat; O_NONBLOCK prevents blocking on a swapped FIFO, and
	// the post-open f.Stat + SameFile check rejects any swapped inode.
	f, err := stagingRoot.OpenFile(reviewWorkspaceStagingRecordName, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, false
	}
	defer func() { _ = f.Close() }()

	recFstat, err := f.Stat()
	if err != nil || !recFstat.Mode().IsRegular() ||
		!os.SameFile(recFstat, recLstat) ||
		recFstat.Size() > int64(reviewWorkspaceStagingRecordMax) {
		return nil, nil, false
	}
	var rec reviewWorkspaceStagingRecord
	if err := strictjson.DecodeReader(f, &rec, strictjson.RejectInvalidUTF8, reviewWorkspaceStagingRecordMax); err != nil {
		return nil, nil, false
	}
	if rec.Version != reviewWorkspaceStagingRecordVersion || rec.InvocationID != id || rec.DirName != name {
		return nil, nil, false
	}
	keepOpen = true
	return stagingRoot, entryInfo, true
}

func deleteAuthenticatedReviewWorkspaceStaging(
	parentRoot, stagingRoot *os.Root, name string, entryInfo os.FileInfo,
) error {
	closed := false
	defer func() {
		if !closed {
			_ = stagingRoot.Close()
		}
	}()

	children, err := fs.ReadDir(stagingRoot.FS(), ".")
	if err != nil {
		return fmt.Errorf("list review workspace staging %q: %w", name, err)
	}
	for _, child := range children {
		childName := child.Name()
		if childName == reviewWorkspaceStagingRecordName {
			continue
		}
		if err := stagingRoot.RemoveAll(childName); err != nil {
			return fmt.Errorf("remove review workspace staging %q child %q: %w", name, childName, err)
		}
	}
	if err := stagingRoot.Remove(reviewWorkspaceStagingRecordName); err != nil {
		return fmt.Errorf("remove review workspace staging %q record: %w", name, err)
	}
	closed = true
	if err := stagingRoot.Close(); err != nil {
		return fmt.Errorf("close review workspace staging %q: %w", name, err)
	}

	currentInfo, err := parentRoot.Lstat(name)
	if err != nil {
		return fmt.Errorf("verify review workspace staging %q before remove: %w", name, err)
	}
	if !os.SameFile(currentInfo, entryInfo) {
		return fmt.Errorf("review workspace staging %q changed during reclaim: %w",
			name, domain.ErrPathBoundaryMismatch)
	}
	if err := parentRoot.Remove(name); err != nil {
		return fmt.Errorf("remove review workspace staging %q: %w", name, err)
	}
	return nil
}
