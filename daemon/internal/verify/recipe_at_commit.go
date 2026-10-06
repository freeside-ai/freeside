package verify

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

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
	scratch, err := os.MkdirTemp("", "freeside-commit-read-*")
	if err != nil {
		return nil, false, fmt.Errorf("create commit-read scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // Private scratch is best-effort after all handles close.
	runner, err := newGitRunner(ctx, gitPath, checkoutDir, scratch)
	if err != nil {
		return nil, false, fmt.Errorf("open checkout: %w", err)
	}
	content, state, err := runner.blobAt(ctx, commitSHA, path, max)
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
