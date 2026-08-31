package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"

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
	out, err := runner.Run(
		ctx, nil, "diff", "--numstat", "--no-renames", "-z", "--no-ext-diff", "--no-textconv",
		baseSHA, headSHA, "--",
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
