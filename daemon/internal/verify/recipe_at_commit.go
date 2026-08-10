package verify

import (
	"context"
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
	if !pathfold.ValidSHA1Hex(commitSHA) {
		return nil, fmt.Errorf("recipe commit %q: %w", commitSHA, ErrInvalidOptions)
	}
	scratch, err := os.MkdirTemp("", "freeside-recipe-read-*")
	if err != nil {
		return nil, fmt.Errorf("create recipe-read scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // Private scratch is best-effort after all handles close.
	runner, err := newGitRunner(ctx, gitPath, checkoutDir, scratch)
	if err != nil {
		return nil, fmt.Errorf("open recipe checkout: %w", err)
	}
	content, state, err := runner.blobAt(
		ctx, commitSHA, DefaultRecipePath, DefaultMaxRecipeBytes)
	if err != nil {
		return nil, fmt.Errorf("read recipe at %s: %w", commitSHA, err)
	}
	if state != blobPresent {
		return nil, fmt.Errorf(
			"recipe at %s is absent, non-regular, or over the size limit: %w",
			commitSHA, ErrRecipeUnreadable)
	}
	return content, nil
}
