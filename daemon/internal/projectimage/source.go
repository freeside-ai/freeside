package projectimage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

type gitSource struct {
	gitPath string
	runner  commandRunner
	tokens  publish.TokenSource
}

func (g gitSource) Fetch(
	ctx context.Context,
	repository string,
	repositoryID int64,
	commit string,
	destination string,
) error {
	url := "https://github.com/" + repository + ".git"
	home := filepath.Join(filepath.Dir(destination), "git-home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create clone config home: %w", err)
	}
	env := []string{
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
	}
	authenticated := g.tokens != nil
	if authenticated {
		token, err := repositoryToken(ctx, g.tokens, repository, repositoryID)
		if err != nil {
			return err
		}
		basic := base64.StdEncoding.EncodeToString(
			[]byte("x-access-token:" + token.Token.Reveal()),
		)
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic "+basic,
		)
	}
	args := []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "protocol.allow=never",
		"-c", "protocol.https.allow=always",
	}
	if authenticated {
		args = append(args, "-c", "http.followRedirects=false")
	}
	args = append(args, "clone", "--quiet", "--bare", url, destination)
	output, err := g.runner.Run(ctx, commandSpec{
		Path: g.gitPath,
		Args: args,
		Env:  env,
	})
	if err != nil {
		if authenticated {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return fmt.Errorf("clone repository: %w", ctxErr)
			}
			return errors.New("clone repository: authenticated git invocation failed")
		}
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
