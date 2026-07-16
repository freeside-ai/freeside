package importer

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

	"github.com/freeside-ai/freeside/daemon/internal/export"
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

// gitRunner runs git plumbing against the daemon-owned checkout under a
// hardened context: the git dir resolved once and pinned (discovery
// never walks the filesystem again), a scratch index and scratch HOME,
// no user or system config, commit-affecting checkout-local config
// pinned (hardenedConfig), no hooks, no fsmonitor, no protocol access,
// and a pinned daemon identity and date. Candidate bytes enter git only
// as blob content on stdin or from the audited blob store; they are
// never argument vector material.
type gitRunner struct {
	gitPath string
	dir     string // working directory for every invocation (the scratch)
	env     []string
}

// hardenedConfig is prepended to every invocation, and -c overrides
// outrank even repository-local config. protectHFS and protectNTFS back
// up the importer's own structural path gate with git's, on every
// platform rather than only where git defaults them on. The neutralized
// GIT_CONFIG_GLOBAL/SYSTEM env drops user and system config, but the
// daemon-owned checkout's own .git/config is still read; these keys pin
// the config that would otherwise make the produced commit object depend
// on checkout-local settings rather than only base+change+options —
// i18n.commitEncoding writes an encoding header, commit.gpgsign a
// signature, both changing the commit SHA. Pinning them keeps the
// daemon-authored commit a pure, reproducible function of its inputs.
var hardenedConfig = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "protocol.allow=never",
	"-c", "core.protectHFS=true",
	"-c", "core.protectNTFS=true",
	"-c", "i18n.commitEncoding=UTF-8",
	"-c", "commit.gpgsign=false",
}

// newGitRunner resolves and hardens one import's git context and fails
// closed unless the checkout uses the sha1 object format this package's
// content verification derives object names for.
func newGitRunner(ctx context.Context, opts Options, checkoutDir, scratch string) (*gitRunner, error) {
	home := filepath.Join(scratch, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("create scratch home: %w", err)
	}
	when := opts.CommitDate
	if when.IsZero() {
		when = time.Now()
	}
	date := strconv.FormatInt(when.Unix(), 10) + " +0000"
	g := &gitRunner{
		gitPath: opts.GitPath,
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
			// would let rev-parse (base enforcement) return the enforced
			// SHA while cat-file/ls-tree (derivation) read different,
			// substituted content, so the import would build against a
			// base object other than the one verifyBase checked.
			"GIT_NO_REPLACE_OBJECTS=1",
			"LC_ALL=C",
			"GIT_AUTHOR_NAME=" + opts.AuthorName,
			"GIT_AUTHOR_EMAIL=" + opts.AuthorEmail,
			"GIT_AUTHOR_DATE=" + date,
			"GIT_COMMITTER_NAME=" + opts.AuthorName,
			"GIT_COMMITTER_EMAIL=" + opts.AuthorEmail,
			"GIT_COMMITTER_DATE=" + date,
		},
	}
	out, err := g.run(ctx, nil, "-C", checkoutDir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return nil, err
	}
	g.env = append(g.env,
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

// runTo executes one plumbing command streaming stdout to w, so large
// base blobs never buffer in memory.
func (g *gitRunner) runTo(ctx context.Context, stdin io.Reader, w io.Writer, args ...string) error {
	argv := make([]string, 0, len(hardenedConfig)+len(args))
	argv = append(argv, hardenedConfig...)
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, g.gitPath, argv...) //nolint:gosec // G204: fixed plumbing argv from daemon options; candidate bytes travel via stdin and the audited blob store, never as arguments
	cmd.Dir = g.dir
	cmd.Env = g.env
	cmd.Stdin = stdin
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = w, &stderr
	if err := cmd.Run(); err != nil {
		return &GitError{Args: args, Stderr: stderr.String(), Err: err}
	}
	return nil
}

// verifyBase enforces the base binding: the enforced SHA must resolve
// to a commit in the checkout and be exactly where HEAD points, so
// integration evidence belongs to one known base.
func (g *gitRunner) verifyBase(ctx context.Context, baseSHA string) error {
	out, err := g.run(ctx, nil, "rev-parse", "--verify", baseSHA+"^{commit}")
	if err != nil {
		return fmt.Errorf("base %s is not a commit in the checkout: %w: %w", baseSHA, ErrBaseMismatch, err)
	}
	if got := strings.TrimSpace(string(out)); got != baseSHA {
		return fmt.Errorf("base %s resolved to %s: %w", baseSHA, got, ErrBaseMismatch)
	}
	out, err = g.run(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head := strings.TrimSpace(string(out)); head != baseSHA {
		return fmt.Errorf("checkout HEAD %s is not the enforced base %s: %w", head, baseSHA, ErrBaseMismatch)
	}
	return nil
}

// treeEntry is one base-tree slot: its index mode and object name.
type treeEntry struct {
	mode string // "100644", "100755", "120000", or "160000"
	oid  string
}

// baseTree loads the enforced base's full recursive tree.
func (g *gitRunner) baseTree(ctx context.Context, baseSHA string) (map[string]treeEntry, error) {
	out, err := g.run(ctx, nil, "ls-tree", "-r", "-z", "--full-tree", baseSHA)
	if err != nil {
		return nil, err
	}
	tree := make(map[string]treeEntry)
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("unparseable ls-tree record %q: %w", rec, ErrGitPlumbing)
		}
		tree[string(path)] = treeEntry{mode: fields[0], oid: fields[2]}
	}
	return tree, nil
}

// blobContent returns a base blob's bytes (symlink targets and other
// deliberately small reads only).
func (g *gitRunner) blobContent(ctx context.Context, oid string) ([]byte, error) {
	return g.run(ctx, nil, "cat-file", "blob", oid)
}

// blobSize returns a base blob's size without reading its content
// (cat-file -s), so a caller can reject a size mismatch cheaply before
// deciding whether to stream and hash the blob.
func (g *gitRunner) blobSize(ctx context.Context, oid string) (int64, error) {
	out, err := g.run(ctx, nil, "cat-file", "-s", oid)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable cat-file -s output %q: %w", out, ErrGitPlumbing)
	}
	return n, nil
}

// blobDigest streams a base blob through sha256, returning the digest
// and size without buffering the content, so an omitted-blob entry can
// be compared against base content. Callers gate this on a cheap
// blobSize match first: the manifest chooses which base blob is hashed,
// so a hostile size claim must not force streaming an arbitrarily large
// base object.
func (g *gitRunner) blobDigest(ctx context.Context, oid string) (export.Digest, int64, error) {
	h := newBlobHasher()
	if err := g.runTo(ctx, nil, h, "cat-file", "blob", oid); err != nil {
		return "", 0, err
	}
	return h.digest(), h.size, nil
}

// baseMatchesDigest reports whether the base blob's content matches the
// given sha256 digest and size. The size is checked first (cheap): a
// mismatch cannot match the digest and avoids streaming; only on a size
// match does it stream the base blob through sha256. This gives the
// caller SHA-256 evidence of base content independent of git's SHA-1
// object identity.
func (g *gitRunner) baseMatchesDigest(ctx context.Context, oid string, digest export.Digest, size int64) (bool, error) {
	baseSize, err := g.blobSize(ctx, oid)
	if err != nil {
		return false, err
	}
	if baseSize != size {
		return false, nil
	}
	got, _, err := g.blobDigest(ctx, oid)
	if err != nil {
		return false, err
	}
	return got == digest, nil
}

// ingestBlobs writes the immutable snapshots produced by content
// verification into the checkout's object database via one hash-object
// --stdin-paths invocation. Git never opens an untrusted pathname.
func (g *gitRunner) ingestBlobs(ctx context.Context, digests []export.Digest, expected map[export.Digest]blobInfo) (map[export.Digest]string, error) {
	if len(digests) == 0 {
		return map[export.Digest]string{}, nil
	}
	var in bytes.Buffer
	for _, d := range digests {
		info, ok := expected[d]
		if !ok {
			return nil, fmt.Errorf("blob %s has no verified metadata: %w", d, ErrTreeMismatch)
		}
		if info.verifiedPath == "" {
			return nil, fmt.Errorf("blob %s has no verified snapshot: %w", d, ErrTreeMismatch)
		}
		in.WriteString(info.verifiedPath)
		in.WriteByte('\n')
	}
	out, err := g.run(ctx, &in, "hash-object", "-w", "--no-filters", "--stdin-paths")
	if err != nil {
		return nil, err
	}
	oids := strings.Fields(string(out))
	if len(oids) != len(digests) {
		return nil, fmt.Errorf("ingested %d objects for %d blobs: %w", len(oids), len(digests), ErrGitPlumbing)
	}
	m := make(map[export.Digest]string, len(digests))
	for i, d := range digests {
		m[d] = oids[i]
	}
	return m, nil
}

// readTree seeds the scratch index with the enforced base's tree.
func (g *gitRunner) readTree(ctx context.Context, baseSHA string) error {
	_, err := g.run(ctx, nil, "read-tree", baseSHA)
	return err
}

// applyIndex applies one NUL-terminated --index-info record per line:
// "<mode> <oid>\t<path>" for adds and modifies, mode 0 with the null
// oid for deletions. The NUL channel is what keeps hostile path bytes
// out of argument or line-parsing positions.
func (g *gitRunner) applyIndex(ctx context.Context, records []string) error {
	var in bytes.Buffer
	for _, r := range records {
		in.WriteString(r)
		in.WriteByte(0)
	}
	_, err := g.run(ctx, &in, "update-index", "-z", "--index-info")
	return err
}

// writeTree writes the scratch index as a tree object.
func (g *gitRunner) writeTree(ctx context.Context) (string, error) {
	out, err := g.run(ctx, nil, "write-tree")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// diffRecord is one diff-tree record between base and the built tree.
type diffRecord struct {
	path    string
	status  string
	newMode string
	newOID  string
}

// diffTree compares the base commit against the built tree with renames
// disabled, for the exact-tree acceptance cross-check.
func (g *gitRunner) diffTree(ctx context.Context, baseSHA, treeSHA string) ([]diffRecord, error) {
	out, err := g.run(ctx, nil, "diff-tree", "-r", "-z", "--no-renames", baseSHA, treeSHA)
	if err != nil {
		return nil, err
	}
	tokens := bytes.Split(out, []byte{0})
	var recs []diffRecord
	for i := 0; i+1 < len(tokens); i += 2 {
		meta := string(tokens[i])
		if !strings.HasPrefix(meta, ":") {
			return nil, fmt.Errorf("unparseable diff-tree record %q: %w", meta, ErrGitPlumbing)
		}
		fields := strings.Fields(meta[1:])
		if len(fields) != 5 {
			return nil, fmt.Errorf("unparseable diff-tree record %q: %w", meta, ErrGitPlumbing)
		}
		recs = append(recs, diffRecord{
			path:    string(tokens[i+1]),
			status:  fields[4],
			newMode: fields[1],
			newOID:  fields[3],
		})
	}
	return recs, nil
}

// commitTree writes the daemon-authored commit object for the built
// tree onto the enforced base.
func (g *gitRunner) commitTree(ctx context.Context, treeSHA, parentSHA, message string) (string, error) {
	out, err := g.run(ctx, nil, "commit-tree", treeSHA, "-p", parentSHA, "-m", message)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// updateRef points the fully qualified ref at the produced commit,
// anchoring it against gc.
func (g *gitRunner) updateRef(ctx context.Context, ref, commitSHA string) error {
	_, err := g.run(ctx, nil, "update-ref", ref, commitSHA)
	return err
}
