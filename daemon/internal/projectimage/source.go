package projectimage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

type gitSource struct {
	gitPath string
	runner  commandRunner
}

func (g gitSource) Fetch(ctx context.Context, repository, commit, destination string) error {
	url := "https://github.com/" + repository + ".git"
	home := filepath.Join(filepath.Dir(destination), "git-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create clone config home: %w", err)
	}
	output, err := g.runner.Run(ctx, commandSpec{
		Path: g.gitPath,
		Args: []string{
			"-c", "core.hooksPath=/dev/null",
			"-c", "protocol.allow=never",
			"-c", "protocol.https.allow=always",
			"clone", "--quiet", "--bare", url, destination,
		},
		Env: []string{
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + home,
			"PATH=/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin:/opt/homebrew/bin",
			"GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_CONFIG_SYSTEM=" + os.DevNull,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_OPTIONAL_LOCKS=0",
			"GIT_NO_REPLACE_OBJECTS=1",
			"LC_ALL=C",
		},
	})
	if err != nil {
		return runError("clone repository", output, err)
	}
	return nil
}

func (g gitSource) Copy(ctx context.Context, sourceDir, commit, destination string) error {
	absoluteSource, err := filepath.Abs(sourceDir)
	if err != nil {
		return fmt.Errorf("resolve local source: %w", err)
	}
	if err := verify.MaterializeExact(ctx, g.gitPath, absoluteSource, commit, destination); err != nil {
		return fmt.Errorf("materialize exact commit: %w", err)
	}
	return nil
}
