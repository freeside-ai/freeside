package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
)

func deriveDiffStats(
	ctx context.Context, workDir, checkoutDir, baseSHA, headSHA string,
) (*domain.DiffStats, error) {
	scratch, err := os.MkdirTemp(workDir, ".diff-stats-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	runner, err := gitrun.New(gitrun.Options{Scratch: scratch})
	if err != nil {
		return nil, err
	}
	if _, err := runner.PinCheckout(ctx, checkoutDir); err != nil {
		return nil, fmt.Errorf("bind diff-stats checkout: %w", err)
	}
	return numstatDiffStats(ctx, runner, baseSHA, baseSHA, headSHA)
}

// deriveDiffStatsFromPatch counts the change from a commit the checkout no
// longer holds to headSHA. A review workspace holds only the current
// candidate, so the previous round's head is rebuilt as a tree: patch (the
// stored base-to-previous-head diff) is applied to patchBaseSHA in the scratch
// index. The patch is not trusted to name that commit. The rebuilt tree must
// equal wantTree, the tree the daemon recorded for absentSHA, or the count is
// refused. New objects go to a scratch object directory, never the checkout.
func deriveDiffStatsFromPatch(
	ctx context.Context, workDir, checkoutDir, patchBaseSHA string, patch []byte,
	wantTree, absentSHA, headSHA string,
) (*domain.DiffStats, error) {
	scratch, err := os.MkdirTemp(workDir, ".diff-stats-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	probe, err := gitrun.New(gitrun.Options{Scratch: scratch})
	if err != nil {
		return nil, err
	}
	checkoutObjects, err := probe.Run(
		ctx, nil, "-C", checkoutDir, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return nil, fmt.Errorf("resolve diff-stats object directory: %w", err)
	}
	objects := filepath.Join(scratch, "objects")
	if err := os.MkdirAll(objects, 0o700); err != nil {
		return nil, err
	}
	runner, err := gitrun.New(gitrun.Options{Scratch: scratch, EnvExtra: []string{
		"GIT_OBJECT_DIRECTORY=" + objects,
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + quoteAlternateObjectDirectory(
			strings.TrimSuffix(string(checkoutObjects), "\n")),
	}})
	if err != nil {
		return nil, err
	}
	if _, err := runner.PinCheckout(ctx, checkoutDir); err != nil {
		return nil, fmt.Errorf("bind diff-stats checkout: %w", err)
	}
	if _, err := runner.Run(ctx, nil, "read-tree", patchBaseSHA); err != nil {
		return nil, fmt.Errorf("read diff-stats patch base: %w", err)
	}
	// git apply rejects an empty patch, which is what an unchanged candidate
	// stores.
	if len(patch) > 0 {
		if _, err := runner.Run(
			ctx, bytes.NewReader(patch), "apply", "--cached", "--binary", "-",
		); err != nil {
			return nil, fmt.Errorf("apply diff-stats patch: %w", err)
		}
	}
	tree, err := runner.Run(ctx, nil, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("write diff-stats tree: %w", err)
	}
	if got := strings.TrimSpace(string(tree)); got != wantTree {
		return nil, fmt.Errorf("rebuilt tree %q for %q, want %q: %w",
			got, absentSHA, wantTree, domain.ErrParentKeyMismatch)
	}
	return numstatDiffStats(ctx, runner, wantTree, absentSHA, headSHA)
}

// quoteAlternateObjectDirectory renders one path as a single entry of
// GIT_ALTERNATE_OBJECT_DIRECTORIES. Git splits that list at ':' and reads an
// entry that starts with a double quote as C-style quoted, so a bare work
// directory holding either character would name other directories.
func quoteAlternateObjectDirectory(path string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(path) + `"`
}

// numstatDiffStats counts from..headSHA and labels the pair baseSHA..headSHA;
// from is baseSHA itself or the tree of a baseSHA the checkout lacks.
func numstatDiffStats(
	ctx context.Context, runner *gitrun.Runner, from, baseSHA, headSHA string,
) (*domain.DiffStats, error) {
	out, err := runner.Run(
		ctx, nil, "diff", "--numstat", "--no-renames", "-z", "--no-ext-diff", "--no-textconv",
		from, headSHA, "--",
	)
	if err != nil {
		return nil, fmt.Errorf("derive diff stats: %w", err)
	}
	stats := &domain.DiffStats{BaseSHA: baseSHA, HeadSHA: headSHA}
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		fields := bytes.SplitN(record, []byte{'\t'}, 3)
		if len(fields) != 3 || len(fields[2]) == 0 {
			return nil, fmt.Errorf("parse git diff --numstat record: %w", domain.ErrCardFactInconsistent)
		}
		additions, err := parseNumstatCount(fields[0])
		if err != nil {
			return nil, err
		}
		deletions, err := parseNumstatCount(fields[1])
		if err != nil {
			return nil, err
		}
		stats.FilesChanged++
		stats.Additions += additions
		stats.Deletions += deletions
	}
	if err := stats.Validate(); err != nil {
		return nil, err
	}
	return stats, nil
}

func parseNumstatCount(value []byte) (int, error) {
	if bytes.Equal(value, []byte("-")) {
		return 0, nil
	}
	parsed, err := strconv.Atoi(string(value))
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("parse git diff --numstat count %q: %w", value, domain.ErrCardFactInconsistent)
	}
	return parsed, nil
}
