package verify

import (
	"context"
	"fmt"
	"os"
)

// MaterializeExact writes one commit's tree as exact Git object bytes without
// checkout filters or repository metadata. It is the shared source boundary
// for verification workspaces and project-image construction.
func MaterializeExact(
	ctx context.Context,
	gitPath, repositoryDir, commitSHA, destination string,
) error {
	scratch, err := os.MkdirTemp("", "freeside-materialize-")
	if err != nil {
		return fmt.Errorf("create materialization scratch: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	g, err := newGitRunner(ctx, gitPath, repositoryDir, scratch)
	if err != nil {
		return err
	}
	return g.materialize(ctx, commitSHA, destination)
}
