package verify

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// verifyHead enforces the head binding (§11 exit criterion: verification
// binds to the exact head): the requested candidate SHA must resolve to
// exactly itself as a commit in the daemon-owned checkout. A head the
// checkout does not hold, or one that resolves elsewhere, fails closed
// before any recipe command runs.
func (g *gitRunner) verifyHead(ctx context.Context, headSHA string) error {
	out, err := g.run(ctx, nil, "rev-parse", "--verify", headSHA+"^{commit}")
	if err != nil {
		return fmt.Errorf("head %s is not a commit in the checkout: %w: %w", headSHA, ErrHeadMismatch, err)
	}
	if got := strings.TrimSpace(string(out)); got != headSHA {
		return fmt.Errorf("head %s resolved to %s: %w", headSHA, got, ErrHeadMismatch)
	}
	return nil
}

// materialize writes the verified head's tree into dest as the fresh
// verification workspace (§5.6: fresh checkout, never the agent's
// workspace). It reads the head's tree into the scratch index, then
// cross-checks that write-tree reproduces exactly the head's tree
// object before a single file is written: what lands on disk is the
// head's content or nothing. The checkout's own index and worktree are
// never touched (the runner pins a scratch GIT_INDEX_FILE).
func (g *gitRunner) materialize(ctx context.Context, headSHA, dest string) error {
	if err := g.verifyHead(ctx, headSHA); err != nil {
		return err
	}
	if _, err := g.run(ctx, nil, "read-tree", headSHA); err != nil {
		return err
	}
	built, err := g.run(ctx, nil, "write-tree")
	if err != nil {
		return err
	}
	want, err := g.run(ctx, nil, "rev-parse", "--verify", headSHA+"^{tree}")
	if err != nil {
		return err
	}
	if b, w := strings.TrimSpace(string(built)), strings.TrimSpace(string(want)); b != w {
		return fmt.Errorf("scratch index tree %s is not head tree %s: %w", b, w, ErrHeadMismatch)
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return fmt.Errorf("create verification workspace: %w", err)
	}
	// --prefix requires the trailing separator; without it the last
	// component becomes a filename prefix instead of a directory.
	if _, err := g.run(ctx, nil, "checkout-index", "-a", "--prefix="+dest+string(os.PathSeparator)); err != nil {
		return err
	}
	return nil
}
