package agenttree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
)

// ErrCommit marks a tree source that is not a full commit object name in the
// checkout. A branch, tag, or abbreviation is refused: the daemon admits
// against one reviewed revision, and a name that can move is not one.
var ErrCommit = errors.New("agent tree commit is not an exact commit in the checkout")

var fullObjectName = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ReadCommit reads the tree's files at an exact commit of a local checkout,
// through git's object store and never the working tree, so an uncommitted
// or unreviewed edit cannot reach admission. scratch is a private directory
// for the runner's home and index.
func ReadCommit(ctx context.Context, checkoutDir, scratch, commit string) (Files, error) {
	if !fullObjectName.MatchString(commit) {
		return nil, fmt.Errorf("%q: %w", commit, ErrCommit)
	}
	runner, err := gitrun.New(gitrun.Options{Scratch: scratch})
	if err != nil {
		return nil, fmt.Errorf("agent tree git runner: %w", err)
	}
	if _, err := runner.PinCheckout(ctx, checkoutDir); err != nil {
		return nil, fmt.Errorf("agent tree checkout %s: %w", checkoutDir, err)
	}
	// Peeling must return the name itself: a tag object's name would peel to
	// another object, and the admission would record a name nobody reviewed.
	peeled, err := runner.Run(ctx, nil, "rev-parse", "--verify", "--end-of-options", commit+"^{commit}")
	if err != nil || strings.TrimSpace(string(peeled)) != commit {
		return nil, fmt.Errorf("%s: %w", commit, errors.Join(ErrCommit, err))
	}
	listing, err := runner.Run(ctx, nil, "ls-tree", "-r", "-z", "-l", "--full-tree", commit, "--",
		Root+"/agents", Root+"/fragments", Root+"/"+lineupPath, Root+"/"+lockPath)
	if err != nil {
		return nil, fmt.Errorf("list agent tree at %s: %w", commit, err)
	}
	files := Files{}
	for _, entry := range bytes.Split(bytes.TrimSuffix(listing, []byte{0}), []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		meta, path, ok := strings.Cut(string(entry), "\t")
		fields := strings.Fields(meta)
		rel, under := strings.CutPrefix(path, Root+"/")
		if !ok || !under || len(fields) != 4 {
			return nil, fmt.Errorf("agent tree listing entry %q: %w", entry, ErrMalformed)
		}
		// Only plain files: a symlink or submodule would make the tree's
		// content depend on something outside the reviewed commit.
		if fields[0] != "100644" || fields[1] != "blob" {
			return nil, fmt.Errorf("%s is a %s with mode %s, not a plain file: %w",
				path, fields[1], fields[0], ErrMalformed)
		}
		size, err := strconv.Atoi(fields[3])
		if err != nil || size > domain.MaxAgentFragmentBytes {
			return nil, fmt.Errorf("%s has size %s: %w", path, fields[3], ErrMalformed)
		}
		if len(files) == maxTreeFiles {
			return nil, fmt.Errorf("more than %d files: %w", maxTreeFiles, ErrMalformed)
		}
		body, err := runner.Run(ctx, nil, "cat-file", "blob", fields[2])
		if err != nil {
			return nil, fmt.Errorf("read %s at %s: %w", path, commit, err)
		}
		files[rel] = body
	}
	return files, nil
}
