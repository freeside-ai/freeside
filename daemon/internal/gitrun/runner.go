package gitrun

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

// Baseline returns a fresh copy of the hardened config prefix shared by every
// daemon-owned git invocation. The -c overrides outrank repository-local
// config as well as the user and system config disabled by Runner's base
// environment.
func Baseline() []string {
	return []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never",
		"-c", "core.protectHFS=true",
		"-c", "core.protectNTFS=true",
	}
}

// TransportBaseline returns the shared baseline plus the config that opens
// exactly scheme while keeping credentials, redirects, recursive fetches, and
// received objects under the network-transport hardening policy. Scheme
// validation belongs to the caller.
func TransportBaseline(scheme string) []string {
	config := Baseline()
	return append(config,
		"-c", "protocol."+scheme+".allow=always",
		"-c", "credential.helper=",
		"-c", "http.followRedirects=false",
		"-c", "push.followTags=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "transfer.fsckObjects=true",
	)
}

// GitError carries a failed plumbing invocation and its captured stderr. Class
// is the caller-owned sentinel matched by errors.Is; Err remains available
// through errors.Unwrap.
type GitError struct {
	Args   []string
	Stderr string
	Class  error
	Err    error
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

// Is lets each caller retain its package-local plumbing error class.
func (e *GitError) Is(target error) bool { return target == e.Class }

func (e *GitError) Unwrap() error { return e.Err }

// Options configures a Runner. ConfigExtra and EnvExtra carry the caller's
// lane-specific pins; every invocation still receives Baseline and the
// replacement base environment.
type Options struct {
	GitPath     string
	Scratch     string
	ConfigExtra []string
	EnvExtra    []string
	Class       error
}

// Runner executes git plumbing under the shared hardened context. Call
// PinCheckout before commands that must bind to a daemon-owned repository.
type Runner struct {
	gitPath string
	dir     string
	config  []string
	env     []string
	class   error
}

// New creates the runner's scratch home and replacement base environment.
func New(opts Options) (*Runner, error) {
	home := filepath.Join(opts.Scratch, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create scratch home: %w", err)
	}
	gitPath := opts.GitPath
	if gitPath == "" {
		gitPath = "git"
	}
	return &Runner{
		gitPath: gitPath,
		dir:     opts.Scratch,
		config:  append([]string(nil), opts.ConfigExtra...),
		env: append([]string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + home,
			"GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_CONFIG_SYSTEM=" + os.DevNull,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_OPTIONAL_LOCKS=0",
			// A refs/replace substitution could let an identity check resolve
			// the enforced object name while later plumbing reads substituted
			// content. Every consumer binds work to the unsubstituted objects.
			"GIT_NO_REPLACE_OBJECTS=1",
			"LC_ALL=C",
		}, opts.EnvExtra...),
		class: opts.Class,
	}, nil
}

// PinCheckout resolves the checkout's git directory once, pins it and a
// scratch index into the runner environment, and returns the checkout's object
// format for the caller to validate under its own sentinel.
func (r *Runner) PinCheckout(ctx context.Context, checkoutDir string) (string, error) {
	out, err := r.Run(ctx, nil, "-C", checkoutDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", err
	}
	r.env = append(r.env,
		"GIT_DIR="+strings.TrimSpace(string(out)),
		"GIT_INDEX_FILE="+filepath.Join(r.dir, "index"),
	)
	format, err := r.Run(ctx, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(format)), nil
}

// Run executes one plumbing command and returns its stdout.
func (r *Runner) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := r.RunTo(ctx, stdin, &stdout, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// RunTo executes one plumbing command while streaming stdout to w.
func (r *Runner) RunTo(ctx context.Context, stdin io.Reader, w io.Writer, args ...string) error {
	baseline := Baseline()
	argv := make([]string, 0, len(baseline)+len(r.config)+len(args))
	argv = append(argv, baseline...)
	argv = append(argv, r.config...)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, r.gitPath, argv...) //nolint:gosec // G204: callers supply fixed daemon plumbing argv; candidate bytes travel through stdin or audited stores
	cmd.Dir = r.dir
	cmd.Env = r.env
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := procbound.Run(cmd, procbound.DefaultWaitDelay); err != nil {
		return &GitError{Args: args, Stderr: stderr.String(), Class: r.class, Err: err}
	}
	return nil
}
