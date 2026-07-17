package verify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// GitError carries a failed plumbing invocation: the argument vector
// and captured stderr. It wraps ErrGitPlumbing; match the class with
// errors.Is and recover the invocation with errors.As.
type GitError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *GitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

// Is lets errors.Is(err, ErrGitPlumbing) match the class.
func (e *GitError) Is(target error) bool { return target == ErrGitPlumbing }

func (e *GitError) Unwrap() error { return e.Err }

// gitRunner runs git plumbing against the daemon-owned checkout under
// the importer's hardening discipline (a deliberate package-local copy;
// the importer's runner is unexported and shared-package edits are out
// of this unit's scope): the git dir resolved once and pinned, a
// scratch index and scratch HOME, no user or system config, no hooks,
// no fsmonitor, no protocol access. Unlike the importer the verifier
// authors no commits, so the commit-reproducibility pins
// (i18n.commitEncoding, commit.gpgsign, author identity and date) are
// deliberately absent rather than copied dead.
type gitRunner struct {
	gitPath string
	dir     string // working directory for every invocation (the scratch)
	env     []string
}

// hardenedConfig is prepended to every invocation; -c overrides outrank
// even repository-local config. protectHFS and protectNTFS back the
// materialization path with git's own alias gate on every platform.
// autocrlf and eol are pinned off so no config-driven line-ending
// conversion can touch materialized bytes; attribute-driven conversion
// is neutralized by GIT_ATTR_SOURCE (see newGitRunner) and, decisively,
// by verifyMaterialized's byte comparison.
var hardenedConfig = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "protocol.allow=never",
	"-c", "core.protectHFS=true",
	"-c", "core.protectNTFS=true",
	"-c", "core.autocrlf=false",
	"-c", "core.eol=lf",
}

// emptyTreeSHA1 is git's well-known empty tree object under the sha1
// format this package requires.
const emptyTreeSHA1 = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// newGitRunner resolves and hardens one verification's git context. The
// sha1 object-format requirement matches the importer's: the head and
// base SHAs this package validates and compares are 40-hex sha1 names,
// so another format would silently break that binding.
func newGitRunner(ctx context.Context, gitPath, checkoutDir, scratch string) (*gitRunner, error) {
	home := filepath.Join(scratch, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create scratch home: %w", err)
	}
	if gitPath == "" {
		gitPath = "git"
	}
	g := &gitRunner{
		gitPath: gitPath,
		dir:     scratch,
		env: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + home,
			"GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_CONFIG_SYSTEM=" + os.DevNull,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_OPTIONAL_LOCKS=0",
			// Ignore any refs/replace/* substitutions: a replace object
			// would let rev-parse (head enforcement) return the enforced
			// SHA while cat-file and read-tree (recipe reads and
			// materialization) read different, substituted content, so
			// verification would bind evidence to a head other than the
			// content it actually exercised.
			"GIT_NO_REPLACE_OBJECTS=1",
			// Read gitattributes from the empty tree instead of the
			// candidate's: an in-tree .gitattributes (ident, text/eol,
			// filter) would otherwise rewrite bytes at checkout-index
			// time, so the recipe would run against content that is not
			// the verified head's. Older git ignores this variable; the
			// backstop either way is verifyMaterialized's byte
			// comparison, which fails closed on any conversion.
			"GIT_ATTR_SOURCE=" + emptyTreeSHA1,
			// Pathspecs this package passes (the recipe path) are
			// literal names, never globs.
			"GIT_LITERAL_PATHSPECS=1",
			"LC_ALL=C",
		},
	}
	out, err := g.run(ctx, nil, "-C", checkoutDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	g.env = append(
		g.env,
		"GIT_DIR="+strings.TrimSpace(string(out)),
		"GIT_INDEX_FILE="+filepath.Join(scratch, "index"),
	)
	format, err := g.run(ctx, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	if f := strings.TrimSpace(string(format)); f != "sha1" {
		return nil, fmt.Errorf("checkout object format %q: %w", f, ErrUnsupportedRepo)
	}
	return g, nil
}

// run executes one plumbing command and returns its stdout.
func (g *gitRunner) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	var stdout bytes.Buffer
	if err := g.runTo(ctx, stdin, &stdout, args...); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

// runTo executes one plumbing command streaming stdout to w.
func (g *gitRunner) runTo(ctx context.Context, stdin io.Reader, w io.Writer, args ...string) error {
	argv := make([]string, 0, len(hardenedConfig)+len(args))
	argv = append(argv, hardenedConfig...)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, g.gitPath, argv...) //nolint:gosec // G204: fixed plumbing argv from daemon options; candidate bytes never appear as arguments
	cmd.Dir = g.dir
	cmd.Env = g.env
	cmd.Stdin = stdin
	// Bound the pipe wait so an unexpected lingering descendant cannot
	// block plumbing past a context kill (same class as the room's
	// WaitDelay refute-pass finding).
	cmd.WaitDelay = 10 * time.Second
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := cmd.Run(); err != nil {
		return &GitError{Args: args, Stderr: stderr.String(), Err: err}
	}
	return nil
}

// blobState classifies what a commit's tree holds at a path.
type blobState int

const (
	// blobPresent: a regular blob within the size cap; content returned.
	blobPresent blobState = iota
	// blobAbsent: the path resolves to no object. A plumbing failure on
	// the type probe is classified here deliberately: for the trusted
	// base read absence fails closed (ErrRecipeUnreadable), and for the
	// candidate-head read it raises a divergence finding, so a masked
	// failure still lands on the safe side in both directions.
	blobAbsent
	// blobNotRegular: the path resolves to a tree or other non-blob.
	blobNotRegular
	// blobTooLarge: a blob beyond the read cap; content not read.
	blobTooLarge
)

// blobAt reads the blob at <commitSHA>:<path>, bounded by max bytes.
// commitSHA and path are daemon-supplied (validated options), never
// candidate bytes, so composing the spec as an argument is safe.
func (g *gitRunner) blobAt(ctx context.Context, commitSHA, path string, max int64) ([]byte, blobState, error) {
	spec := commitSHA + ":" + path
	out, err := g.run(ctx, nil, "cat-file", "-t", spec)
	if err != nil {
		return nil, blobAbsent, nil
	}
	if t := strings.TrimSpace(string(out)); t != "blob" {
		return nil, blobNotRegular, nil
	}
	out, err = g.run(ctx, nil, "cat-file", "-s", spec)
	if err != nil {
		return nil, blobAbsent, err
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return nil, blobAbsent, fmt.Errorf("unparseable cat-file -s output %q: %w", out, ErrGitPlumbing)
	}
	if size > max {
		return nil, blobTooLarge, nil
	}
	content, err := g.run(ctx, nil, "cat-file", "blob", spec)
	if err != nil {
		return nil, blobAbsent, err
	}
	return content, blobPresent, nil
}
