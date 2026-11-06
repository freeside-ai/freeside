package verify

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
)

// gitRunner runs git plumbing against the daemon-owned checkout under
// the shared hardening discipline: the git dir resolved once and pinned,
// a scratch index and scratch HOME, no user or system config, no hooks,
// no fsmonitor, no protocol access. Unlike the importer the verifier authors
// no commits, so the commit-reproducibility pins
// (i18n.commitEncoding, commit.gpgsign, author identity and date) are
// deliberately absent rather than copied dead.
//
// objects reads every blob through one long-lived batch process that runs
// under the context the runner was made with, so whoever makes a gitRunner
// closes it before that context ends.
type gitRunner struct {
	shared  *gitrun.Runner
	objects *gitrun.ObjectReader
}

// verifyConfig is appended to the shared baseline. protectHFS and protectNTFS
// in that baseline back the materialization path with git's own alias gate on
// every platform.
// autocrlf and eol are pinned off so no config-driven line-ending
// conversion can touch materialized bytes; attribute-driven conversion
// is neutralized by GIT_ATTR_SOURCE (see newGitRunner) and, decisively,
// by verifyMaterialized's byte comparison.
var verifyConfig = []string{
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
	shared, err := gitrun.New(gitrun.Options{
		GitPath:     gitPath,
		Scratch:     scratch,
		Class:       ErrGitPlumbing,
		ConfigExtra: verifyConfig,
		EnvExtra: []string{
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
		},
	})
	if err != nil {
		return nil, err
	}
	format, err := shared.PinCheckout(ctx, checkoutDir)
	if err != nil {
		return nil, err
	}
	if format != "sha1" {
		return nil, fmt.Errorf("checkout object format %q: %w", format, ErrUnsupportedRepo)
	}
	// Built only on a pinned sha1 runner; the reader starts no process
	// until its first read.
	return &gitRunner{shared: shared, objects: shared.Objects(ctx)}, nil
}

// close stops the object reader's process. The reader never fails Close.
func (g *gitRunner) close() {
	_ = g.objects.Close()
}

// run executes one plumbing command and returns its stdout.
func (g *gitRunner) run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	return g.shared.Run(ctx, stdin, args...)
}

// blobState classifies what a commit's tree holds at a path.
type blobState int

const (
	// blobPresent: a regular blob within the size cap; content returned.
	blobPresent blobState = iota
	// blobAbsent: the tree genuinely holds nothing at the path. This is
	// distinguished from a plumbing failure: ls-tree exits zero with
	// empty output for a missing path and non-zero on a real failure,
	// so a transient error can never masquerade as absence and silently
	// suppress a divergence flag (refute-pass finding).
	blobAbsent
	// blobNotRegular: the path resolves to a tree, a symlink, or another
	// non-regular entry; there are no regular content bytes to compare
	// or trust.
	blobNotRegular
	// blobTooLarge: a blob beyond the read cap; content not read.
	blobTooLarge
)

// blobAt reads the regular blob at path in commitSHA's tree, bounded by
// max bytes. commitSHA and path are daemon-supplied (validated
// options), never candidate bytes; GIT_LITERAL_PATHSPECS pins the path
// argument as a literal name. Plumbing failures propagate as errors.
//
// The listed size is checked against max before the read, and the read
// must find a blob of exactly that size. The batch read does not peel, so
// an entry naming a tag, whose listed size is the tag's own, fails with
// ErrMalformedTree instead of returning the tagged blob past the cap. The
// refused content is left unread, which usually ends the reader. Callers
// read past ErrCommitFileUnreadable, never ErrMalformedTree, so no caller
// reads again after a refusal.
func (g *gitRunner) blobAt(ctx context.Context, commitSHA, path string, max int64) ([]byte, blobState, error) {
	out, err := g.run(ctx, nil, "ls-tree", "-z", "-l", "--full-tree", commitSHA, "--", path)
	if err != nil {
		return nil, blobAbsent, err
	}
	rec, _, _ := strings.Cut(string(out), "\x00")
	if rec == "" {
		return nil, blobAbsent, nil
	}
	meta, _, ok := strings.Cut(rec, "\t")
	fields := strings.Fields(meta)
	if !ok || len(fields) != 4 {
		return nil, blobAbsent, fmt.Errorf("unparseable ls-tree record %q: %w", rec, ErrGitPlumbing)
	}
	mode, objectType, oid, sizeField := fields[0], fields[1], fields[2], fields[3]
	if objectType != "blob" || (mode != "100644" && mode != "100755") {
		return nil, blobNotRegular, nil
	}
	size, err := strconv.ParseInt(sizeField, 10, 64)
	if err != nil {
		return nil, blobAbsent, fmt.Errorf("unparseable ls-tree size %q: %w", sizeField, ErrGitPlumbing)
	}
	if size > max {
		return nil, blobTooLarge, nil
	}
	info, body, err := g.objects.Open(ctx, oid)
	if err != nil {
		return nil, blobAbsent, err
	}
	if info.Type != gitrun.ObjectBlob || info.Size != size {
		return nil, blobAbsent, fmt.Errorf("%s names a %s of %d bytes, listed as a blob of %d: %w",
			path, info.Type, info.Size, size, ErrMalformedTree)
	}
	content, err := io.ReadAll(body)
	if err != nil {
		return nil, blobAbsent, err
	}
	return content, blobPresent, nil
}
