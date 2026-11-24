package projectimage

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

type gitSource struct {
	gitPath string
	runner  commandRunner
	tokens  publish.TokenSource
	// credentialDir holds an authenticated clone's header file
	// (writeCredentialConfig).
	credentialDir string
}

// Fetch clones repository bare into destination. An authenticated clone
// hands git its token only in a header file that the clone's
// GIT_CONFIG_GLOBAL names and that Fetch removes when git returns: never
// argv, and never the environment, which any process on the account can
// read (plan §5.7, Cordoned Runs).
func (g gitSource) Fetch(
	ctx context.Context,
	repository string,
	repositoryID int64,
	commit string,
	destination string,
) (err error) {
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
		token, tokenErr := repositoryToken(ctx, g.tokens, repository, repositoryID)
		if tokenErr != nil {
			return tokenErr
		}
		config, configErr := writeCredentialConfig(g.credentialDir, token)
		if configErr != nil {
			return configErr
		}
		// The deferred removal joins into Fetch's named result, so nothing
		// in this block may shadow err.
		defer func() {
			// A file left behind still holds a live token, so failing to
			// remove it fails the clone.
			if removeErr := os.Remove(config); removeErr != nil {
				err = errors.Join(err, fmt.Errorf("remove clone credential config: %w", removeErr))
			}
		}()
		env = credentialEnv(env, config)
	}
	args := gitrun.TransportBaseline("https")
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

// writeCredentialConfig writes the installation token as an
// http.extraHeader line to a new config file in dir, mode 0600, and
// returns its path; the caller removes it once git returns. It is this
// lane's copy of the publish transport's: GitHub's git-over-HTTPS form
// for an installation token is basic auth with the x-access-token user.
func writeCredentialConfig(dir string, token publish.InstallationToken) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("authenticated clone has no credential directory: %w", ErrInvalidRequest)
	}
	f, err := os.CreateTemp(dir, "git-auth-*.config")
	if err != nil {
		return "", fmt.Errorf("create clone credential config: %w", err)
	}
	path := f.Name()
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token.Token.Reveal()))
	_, err = io.WriteString(f, "[http]\n\textraHeader = Authorization: Basic "+basic+"\n")
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", errors.Join(fmt.Errorf("write clone credential config: %w", err), os.Remove(path))
	}
	return path, nil
}

// credentialEnv is env with GIT_CONFIG_GLOBAL pointed at config, in a
// fresh slice; the -c hardening still outranks the file.
func credentialEnv(env []string, config string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_CONFIG_GLOBAL=") {
			e = "GIT_CONFIG_GLOBAL=" + config
		}
		out = append(out, e)
	}
	return out
}

// privateCredentialDir creates dir 0700 when it is missing and refuses it
// unless it is a directory, not a link, that only its owner can enter.
func privateCredentialDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("credential directory is required with tokens: %w", ErrInvalidRequest)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credential directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect credential directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("credential directory %s is not a private directory: %w", dir, ErrInvalidRequest)
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
