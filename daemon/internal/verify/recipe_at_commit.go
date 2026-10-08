package verify

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

// CommitReader reads exact commits through one hardened checkout pin. Open and
// close it within one caller call on one checkout; never store it or use it
// concurrently. Each read validates its commit and reads only tree/blob data,
// independent of HEAD, the worktree, and the scratch index.
type CommitReader struct {
	runner  *gitRunner
	scratch string
}

// OpenCommitReader resolves the checkout's git directory and requires SHA-1
// once for the group. The caller owns proving the checkout's repository identity.
func OpenCommitReader(ctx context.Context, gitPath, checkoutDir string) (*CommitReader, error) {
	scratch, err := os.MkdirTemp("", "freeside-commit-read-*")
	if err != nil {
		return nil, fmt.Errorf("create commit-read scratch: %w", err)
	}
	runner, err := newGitRunner(ctx, gitPath, checkoutDir, scratch)
	if err != nil {
		_ = os.RemoveAll(scratch) // Private scratch cleanup is best effort, as for Close.
		return nil, fmt.Errorf("open checkout: %w", err)
	}
	return &CommitReader{runner: runner, scratch: scratch}, nil
}

// Close removes the group's private scratch directory, best effort. Reads after
// Close fail because git's working directory no longer exists.
func (r *CommitReader) Close() {
	_ = os.RemoveAll(r.scratch) // Private scratch is best-effort after all handles close.
}

// ReadRecipeAtCommit reads the default recipe as a regular, size-bounded blob
// from one exact SHA-1 commit through the verifier's hardened git plumbing.
func ReadRecipeAtCommit(
	ctx context.Context,
	gitPath string,
	checkoutDir string,
	commitSHA string,
) ([]byte, error) {
	content, present, err := ReadFileAtCommit(
		ctx, gitPath, checkoutDir, commitSHA, DefaultRecipePath, DefaultMaxRecipeBytes)
	if errors.Is(err, ErrCommitFileUnreadable) || (err == nil && !present) {
		return nil, fmt.Errorf(
			"recipe at %s is absent, non-regular, or over the size limit: %w",
			commitSHA, ErrRecipeUnreadable)
	}
	if err != nil {
		return nil, err
	}
	return content, nil
}

// ReadFileAtCommit reads one path as a regular, size-bounded blob from one
// exact SHA-1 commit through the verifier's hardened git plumbing. It reads
// the commit's tree, never a worktree, so it works on a checkout whose
// worktree is empty.
//
// present is false only when the tree holds nothing at path. An entry that
// exists but is not a regular blob within max (a tree, a symlink, a
// submodule, an oversized blob) fails with ErrCommitFileUnreadable instead,
// so a caller testing for absence can never read "exists in a shape this
// reader refuses" as "absent".
func ReadFileAtCommit(
	ctx context.Context,
	gitPath string,
	checkoutDir string,
	commitSHA string,
	path string,
	max int64,
) (content []byte, present bool, err error) {
	if !pathfold.ValidSHA1Hex(commitSHA) {
		return nil, false, fmt.Errorf("commit %q: %w", commitSHA, ErrInvalidOptions)
	}
	if path == "" || max <= 0 {
		return nil, false, fmt.Errorf("read %q within %d bytes: %w", path, max, ErrInvalidOptions)
	}
	reader, err := OpenCommitReader(ctx, gitPath, checkoutDir)
	if err != nil {
		return nil, false, err
	}
	defer reader.Close()
	return reader.ReadFile(ctx, commitSHA, path, max)
}

// ReadFile has ReadFileAtCommit's content, presence, and error contract, using
// the repository resolved at open even if the checkout path is later repointed.
// After open it runs only ls-tree and cat-file, validating each commit name.
func (r *CommitReader) ReadFile(ctx context.Context, commitSHA, path string, max int64) ([]byte, bool, error) {
	if !pathfold.ValidSHA1Hex(commitSHA) {
		return nil, false, fmt.Errorf("commit %q: %w", commitSHA, ErrInvalidOptions)
	}
	if path == "" || max <= 0 {
		return nil, false, fmt.Errorf("read %q within %d bytes: %w", path, max, ErrInvalidOptions)
	}
	content, state, err := r.runner.blobAt(ctx, commitSHA, path, max)
	if err != nil {
		return nil, false, fmt.Errorf("read %s at %s: %w", path, commitSHA, err)
	}
	switch state {
	case blobPresent:
		return content, true, nil
	case blobAbsent:
		return nil, false, nil
	case blobNotRegular, blobTooLarge:
	}
	return nil, false, fmt.Errorf(
		"%s at %s is non-regular or over the %d-byte limit: %w",
		path, commitSHA, max, ErrCommitFileUnreadable)
}
