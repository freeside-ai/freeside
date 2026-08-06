package publish

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrGitTransport is the class sentinel for a failed git transport
// invocation; it is carried by *TransportGitError. Match the class
// with errors.Is and recover the invocation with errors.As.
var ErrGitTransport = errors.New("git transport invocation failed")

// TransportRefusal classifies why a transport invocation was refused,
// derived from fixed patterns in git's output. Only the matched enum
// member is ever retained: the raw stream may carry remote-controlled
// text and rides an authenticated channel, so it never enters errors
// (the forge's APIError discipline). The zero value "" is invalid by
// design.
type TransportRefusal string

const (
	// RefusalStaleLease is the create-only lease failing: the remote
	// ref exists (or appeared mid-push), so the push would move a ref
	// rather than create one.
	RefusalStaleLease TransportRefusal = "stale-lease"
	// RefusalNonFastForward is git's non-fast-forward rejection class.
	RefusalNonFastForward TransportRefusal = "non-fast-forward"
	// RefusalAuth is an authentication or authorization failure from
	// the remote HTTP endpoint.
	RefusalAuth TransportRefusal = "auth"
	// RefusalUnknown is any failure no fixed pattern matched.
	RefusalUnknown TransportRefusal = "unknown"
)

// AllTransportRefusals is the single registration point for the enum.
var AllTransportRefusals = []TransportRefusal{
	RefusalStaleLease,
	RefusalNonFastForward,
	RefusalAuth,
	RefusalUnknown,
}

func (r TransportRefusal) valid() bool {
	switch r {
	case RefusalStaleLease, RefusalNonFastForward, RefusalAuth, RefusalUnknown:
		return true
	default:
		return false
	}
}

// TransportGitError reports a failed transport invocation: the fixed
// daemon-authored argument vector, the exit code, and the classified
// refusal. Unlike the importer's GitError it never carries stderr:
// transport stderr is remote-influenced text on an authenticated
// channel, and the class enum is the only thing extracted from it.
type TransportGitError struct {
	Args     []string
	ExitCode int
	Refusal  TransportRefusal
	Err      error
}

func (e *TransportGitError) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.ExitCode, e.Refusal)
}

// Is lets errors.Is(err, ErrGitTransport) match the class.
func (e *TransportGitError) Is(target error) bool { return target == ErrGitTransport }

func (e *TransportGitError) Unwrap() error { return e.Err }

// classifyTransportFailure maps a failed invocation's output streams
// onto the refusal enum by fixed patterns. The streams are examined
// here and dropped; only the enum crosses out. The stale-lease class
// gates PushHead's converged-or-conflict re-observation, so it is
// matched only in a porcelain per-ref rejection line on stdout —
// git's own structured output — never in stderr, where a hostile
// remote's "remote:" lines land and could forge the class. For the
// same reason there is no missing-ref class here at all: whether the
// remote holds a ref is a question with an authoritative structured
// answer (ls-remote), so FetchBase asks it rather than reading a
// verdict out of prose. The classes that remain are diagnostic labels
// on an already-failed invocation, consumed by no decision, and match
// best-effort.
func classifyTransportFailure(stdout, stderr []byte) TransportRefusal {
	for _, line := range strings.Split(string(stdout), "\n") {
		if strings.HasPrefix(line, "!") && strings.Contains(line, "stale info") {
			return RefusalStaleLease
		}
	}
	combined := string(stdout) + "\n" + string(stderr)
	switch {
	case strings.Contains(combined, "non-fast-forward"):
		return RefusalNonFastForward
	case strings.Contains(combined, "could not read Username"),
		strings.Contains(combined, "Authentication failed"),
		strings.Contains(combined, "The requested URL returned error: 401"),
		strings.Contains(combined, "The requested URL returned error: 403"),
		strings.Contains(combined, "Permission") && strings.Contains(combined, "denied"):
		return RefusalAuth
	default:
		return RefusalUnknown
	}
}

// netRunner runs git transport commands (init, fetch, ls-remote, push,
// and the rev-parse checks around them) against a daemon-owned
// checkout under a hardened context: a fully replaced environment (no
// inherited variables, so no inherited GIT_* or credential state and
// no object-database alternates), no user or system config, no hooks,
// prompts disabled, and network protocol access closed to everything
// except the single allowed scheme. It is the transport counterpart of
// importer's gitRunner; the two stay per-lane copies by design.
//
// The installation token crosses into git through exactly one channel:
// a per-invocation GIT_CONFIG_{COUNT,KEY_0,VALUE_0} environment triple
// carrying http.extraHeader, appended to the constructed child
// environment and gone when the invocation exits. It is never argv
// material, never written to any config file, and the daemon's own
// environment is never mutated.
type netRunner struct {
	gitPath string
	dir     string // working directory for every invocation (the scratch)
	env     []string
	scheme  string
	pinned  bool // a repository is pinned, so its config is readable and gated
}

// transportConfig is the hardened -c prefix for every transport
// invocation. protocol.allow=never closes every transport, then the
// scheme-specific key re-opens exactly one (a more specific
// protocol.<scheme>.allow overrides the base key). credential.helper=
// clears the helper list so no system helper is consulted or handed
// the token to store. http.followRedirects=false keeps the credential
// header from ever riding a redirect (git re-sends configured extra
// headers to redirect targets, including cross-host ones); a renamed
// repository therefore fails loudly instead of being followed, which
// is the resolver's drift to fix, not the transport's.
// transfer.fsckObjects makes git verify objects received from the
// network before they enter the daemon-owned object database. The
// remaining keys mirror the importer runner's hardening.
func transportConfig(scheme string) []string {
	return []string{
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.fsmonitor=false",
		"-c", "protocol.allow=never",
		"-c", "protocol." + scheme + ".allow=always",
		"-c", "core.protectHFS=true",
		"-c", "core.protectNTFS=true",
		"-c", "credential.helper=",
		"-c", "http.followRedirects=false",
		"-c", "push.followTags=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "transfer.fsckObjects=true",
	}
}

// transportSchemeValid gates the runner to the two schemes the package
// ever opens: https in production, file for hermetic local tests.
func transportSchemeValid(scheme string) bool {
	return scheme == "https" || scheme == "file"
}

// newNetRunner builds the hardened base context. The checkout may not
// exist yet (FetchBase runs git init through this runner), so the git
// dir is pinned separately by pinRepo once it does.
func newNetRunner(gitPath, scratch, scheme string) (*netRunner, error) {
	if !transportSchemeValid(scheme) {
		return nil, fmt.Errorf("transport scheme %q is not allowed", scheme)
	}
	home := filepath.Join(scratch, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create scratch home: %w", err)
	}
	return &netRunner{
		gitPath: gitPath,
		dir:     scratch,
		scheme:  scheme,
		env: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
			"XDG_CONFIG_HOME=" + home,
			"GIT_CONFIG_GLOBAL=" + os.DevNull,
			"GIT_CONFIG_SYSTEM=" + os.DevNull,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"GIT_ASKPASS=" + os.DevNull,
			"GIT_OPTIONAL_LOCKS=0",
			"GIT_NO_REPLACE_OBJECTS=1",
			"LC_ALL=C",
		},
	}, nil
}

// pinRepo resolves the checkout's git dir once and pins it (discovery
// never walks the filesystem again), points the index into the
// scratch, and fails closed unless the checkout uses the sha1 object
// format the publish identities and ref checks assume. The resolved
// git dir must be the checkout's own .git: repository discovery walks
// upward, so a missing or emptied checkout under some ancestor
// repository would otherwise silently pin that repository — and an
// authenticated invocation honors the pinned repository's local
// config, which is exactly the state a transport must never inherit
// from anywhere it did not create.
func (r *netRunner) pinRepo(ctx context.Context, checkoutDir string) error {
	out, _, err := r.run(ctx, nil, "-C", checkoutDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	gitDir, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return fmt.Errorf("resolve git dir: %w", err)
	}
	wantDir, err := filepath.EvalSymlinks(checkoutDir)
	if err != nil {
		return fmt.Errorf("resolve checkout dir: %w", err)
	}
	if gitDir != filepath.Join(wantDir, ".git") {
		return fmt.Errorf("checkout %s resolved to foreign git dir %s: %w", checkoutDir, gitDir, ErrGitTransport)
	}
	r.env = append(r.env,
		"GIT_DIR="+gitDir,
		"GIT_INDEX_FILE="+filepath.Join(r.dir, "index"),
	)
	r.pinned = true
	format, _, err := r.run(ctx, nil, "rev-parse", "--show-object-format")
	if err != nil {
		return err
	}
	if f := strings.TrimSpace(string(format)); f != "sha1" {
		return fmt.Errorf("checkout object format %q: %w", f, ErrGitTransport)
	}
	return nil
}

// transportRepoKey is the config key FetchBase stamps and every
// authenticated invocation re-reads to bind a checkout to its managed
// repository. It lives here with the config allowlist that admits it.
const transportRepoKey = "freeside.transport.repo"

// pristineConfigKeys is every configuration key the checkout's own
// config may hold: what `git init` writes, plus the transport's repo
// marker. The `-c` hardening outranks local config for the keys it
// pins, but a checkout config is still read, and families it does not
// pin can redirect an authenticated invocation — `url.*.insteadOf`
// rewrites the very URL the credential header rides to, and
// `include.path` can pull in anything. So the rule is an allowlist of
// exact keys, not a denylist of known-bad ones: anything the daemon
// did not put there fails the invocation closed.
var pristineConfigKeys = map[string]bool{
	"core.bare":                    true,
	"core.filemode":                true,
	"core.ignorecase":              true,
	"core.logallrefupdates":        true,
	"core.precomposeunicode":       true,
	"core.repositoryformatversion": true,
	"core.symlinks":                true,
	"core.worktree":                true,
	"extensions.objectformat":      true,
	transportRepoKey:               true,
}

// assertPristineConfig fails closed unless the pinned repository's
// configuration holds only daemon-authored keys, so no authenticated
// invocation ever runs against a checkout whose config could redirect
// it. Keys are compared lowercased, the form git reports and matches
// them in.
func (r *netRunner) assertPristineConfig(ctx context.Context) error {
	out, _, err := r.run(ctx, nil, "config", "--local", "--list", "--name-only", "-z")
	if err != nil {
		return err
	}
	for _, key := range strings.Split(string(out), "\x00") {
		if key == "" {
			continue
		}
		if !pristineConfigKeys[strings.ToLower(key)] {
			return fmt.Errorf("checkout config carries non-daemon key %q: %w", key, ErrGitTransport)
		}
	}
	return nil
}

// run executes one unauthenticated transport-context command and
// returns stdout and stderr. Callers never fold the streams into
// errors; a failed invocation surfaces as *TransportGitError carrying
// only the classified refusal.
func (r *netRunner) run(ctx context.Context, tok *InstallationToken, args ...string) (stdout, stderr []byte, err error) {
	argv := make([]string, 0, len(transportConfig(r.scheme))+len(args))
	argv = append(argv, transportConfig(r.scheme)...)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, r.gitPath, argv...) //nolint:gosec // G204: fixed transport argv from daemon-validated refs and SHAs; the token travels via the child environment, never as an argument
	cmd.Dir = r.dir
	env := r.env
	if tok != nil {
		// The one credential crossing: a fresh slice so the runner's
		// own env never retains the header between invocations.
		env = append(append([]string(nil), r.env...), tokenEnv(*tok)...)
	}
	cmd.Env = env
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	if runErr := cmd.Run(); runErr != nil {
		return nil, nil, &TransportGitError{
			Args:     args,
			ExitCode: cmd.ProcessState.ExitCode(),
			Refusal:  classifyTransportFailure(outBuf.Bytes(), errBuf.Bytes()),
			Err:      runErr,
		}
	}
	return outBuf.Bytes(), errBuf.Bytes(), nil
}

// runTo is run's streaming-stdout counterpart for raw object extraction and
// bounded plumbing listings. It deliberately classifies failures from stderr
// only: stdout can be candidate blob content and must never enter error
// classification or retention.
func (r *netRunner) runTo(ctx context.Context, stdout io.Writer, args ...string) error {
	argv := make([]string, 0, len(transportConfig(r.scheme))+len(args))
	argv = append(argv, transportConfig(r.scheme)...)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, r.gitPath, argv...) //nolint:gosec // G204: fixed transport argv from daemon-validated SHAs
	cmd.Dir = r.dir
	cmd.Env = r.env
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return &TransportGitError{
			Args: args, ExitCode: -1, Refusal: RefusalUnknown, Err: err,
		}
	}
	if err := cmd.Start(); err != nil {
		return &TransportGitError{
			Args: args, ExitCode: -1, Refusal: RefusalUnknown, Err: err,
		}
	}
	if _, copyErr := io.Copy(stdout, stdoutPipe); copyErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return &TransportGitError{
			Args:     args,
			ExitCode: exitCode,
			Refusal:  classifyTransportFailure(nil, errBuf.Bytes()),
			Err:      copyErr,
		}
	}
	if runErr := cmd.Wait(); runErr != nil {
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return &TransportGitError{
			Args:     args,
			ExitCode: exitCode,
			Refusal:  classifyTransportFailure(nil, errBuf.Bytes()),
			Err:      runErr,
		}
	}
	return nil
}

// interact runs one plumbing process while the caller exchanges a bounded
// request/response protocol over its stdin and stdout.
func (r *netRunner) interact(
	ctx context.Context,
	interaction func(io.Writer, io.Reader) error,
	args ...string,
) error {
	argv := make([]string, 0, len(transportConfig(r.scheme))+len(args))
	argv = append(argv, transportConfig(r.scheme)...)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, r.gitPath, argv...) //nolint:gosec // G204: fixed plumbing argv; validated object IDs travel over stdin
	cmd.Dir = r.dir
	cmd.Env = r.env
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return &TransportGitError{Args: args, ExitCode: -1, Refusal: RefusalUnknown, Err: err}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return &TransportGitError{Args: args, ExitCode: -1, Refusal: RefusalUnknown, Err: err}
	}
	if err := cmd.Start(); err != nil {
		return &TransportGitError{Args: args, ExitCode: -1, Refusal: RefusalUnknown, Err: err}
	}
	interactionErr := interaction(stdin, stdout)
	closeErr := stdin.Close()
	if interactionErr != nil || closeErr != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		cause := interactionErr
		if cause == nil {
			cause = closeErr
		}
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return &TransportGitError{
			Args: args, ExitCode: exitCode,
			Refusal: classifyTransportFailure(nil, errBuf.Bytes()), Err: cause,
		}
	}
	if runErr := cmd.Wait(); runErr != nil {
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		return &TransportGitError{
			Args: args, ExitCode: exitCode,
			Refusal: classifyTransportFailure(nil, errBuf.Bytes()), Err: runErr,
		}
	}
	return nil
}

// runAuthed executes one authenticated transport command with the
// installation token injected for this invocation only.
//
// The config allowlist is re-asserted here, immediately before the
// credentialed process starts, rather than once at a call site: a
// check that far from its use leaves the whole token mint (a network
// round trip) inside the window during which a redirecting key could
// appear. Binding it to the invocation makes the window as small as
// two consecutive process spawns. It cannot be closed entirely —
// git offers no way to run one command with the repository's own
// config ignored — but the residual requires an adversary with
// concurrent write access to a 0700 daemon-private directory, i.e.
// one already running as the daemon user, who can read the App
// private key and mint tokens outright.
func (r *netRunner) runAuthed(ctx context.Context, tok InstallationToken, args ...string) (stdout, stderr []byte, err error) {
	if r.pinned {
		if err := r.assertPristineConfig(ctx); err != nil {
			return nil, nil, err
		}
	}
	return r.run(ctx, &tok, args...)
}

// tokenEnv renders the installation token as the per-invocation
// environment config triple. GitHub's documented git-over-HTTPS form
// for installation tokens is basic auth with the x-access-token user;
// Reveal sits here so the crossing is visible at exactly one site.
func tokenEnv(tok InstallationToken) []string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok.Token.Reveal()))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}
