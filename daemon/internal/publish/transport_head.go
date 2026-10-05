package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrRemoteMissingHead is returned when the managed repository's branch
// is gone, or does not contain the requested head commit. Like
// ErrRemoteMissingBase it is a definitive verdict a caller may act on,
// so it is decided from structured observations (ls-remote, the fetched
// ref, an ancestry exit status), never from a failure's prose.
var ErrRemoteMissingHead = errors.New("remote branch does not hold the requested head commit")

// ErrMergeConflict is the class a *MergeConflictError matches.
var ErrMergeConflict = errors.New("prospective merge has conflicts")

// transportHeadRef is where FetchHead lands a pull request's branch. It
// is private to the checkout like transportBaseRef and never pushed.
const transportHeadRef = "refs/freeside/head"

// checkoutAttributesPath is the checkout's repository-local attributes
// file, which merge-tree reads even without a working tree.
const checkoutAttributesPath = ".git/info/attributes"

// The prospective merge's identity is fixed so its SHA depends only on
// the two parents and the merged tree: the engine rebuilds the commit on
// every pass and compares it with the one it recorded, which a wall-clock
// timestamp or a configurable identity would defeat. The literals are
// per-lane copies of the importer's daemon identity by the same rule that
// keeps the two git runners apart.
const (
	prospectiveMergeAuthorName  = "freeside-daemon"
	prospectiveMergeAuthorEmail = "daemon@freeside.invalid"
	prospectiveMergeDate        = "@0 +0000"
	prospectiveMergeMessage     = "Prospective merge for readiness re-entry (never pushed)"
)

// maxMergeConflictListingBytes bounds how much of merge-tree's conflict
// listing is retained. The tree name leads the output and is far inside
// the bound, so truncation only ever shortens the list of paths.
const maxMergeConflictListingBytes = 1 << 20

// HeadFetch reports one FetchHead observation.
type HeadFetch struct {
	// TipSHA is the branch's tip as fetched. It equals the requested head
	// unless the branch moved past it.
	TipSHA string
	// DescendsFromBase reports whether the requested head descends from
	// the checkout's enforced base. A head built on an older base does
	// not, which is the ordinary state of a base-advance re-entry, so the
	// caller decides whether to require it.
	DescendsFromBase bool
}

// MergeConflictError reports that the head does not merge cleanly into
// the checkout's base. No commit was written. Paths are tree paths from
// the two candidates, so they are untrusted text: Error never renders
// them, and a caller that shows them quotes and bounds them.
type MergeConflictError struct {
	BaseSHA string
	HeadSHA string
	// Paths are the conflicted paths in merge-tree's order.
	Paths []string
	// Truncated reports that the listing exceeded the retained bound, so
	// Paths is a prefix.
	Truncated bool
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf(
		"merging head %s into base %s conflicts in %d paths: %s",
		e.HeadSHA, e.BaseSHA, len(e.Paths), ErrMergeConflict,
	)
}

// Is lets errors.Is(err, ErrMergeConflict) match the class.
func (e *MergeConflictError) Is(target error) bool { return target == ErrMergeConflict }

// truncatingBuffer keeps the first bytes written to it and drops the
// rest without failing the write. merge-tree's exit status carries the
// verdict, so an oversized conflict listing must not kill the process
// and turn a conflict into an unclassified failure.
type truncatingBuffer struct {
	// Named, not embedded: bytes.Buffer's promoted ReadFrom would let
	// io.Copy bypass Write and the bound with it.
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func (b *truncatingBuffer) Write(p []byte) (int, error) {
	keep := min(len(p), b.remaining)
	b.buffer.Write(p[:keep])
	b.remaining -= keep
	if keep < len(p) {
		b.truncated = true
	}
	return len(p), nil
}

// FetchHead fetches the managed repository's branch into the checkout
// and proves that headSHA is a commit that branch contains. It is the
// one place the transport brings in a commit Freeside did not produce:
// the pull request's branch is writable by anyone with push access, so
// nothing about the fetched tip is trusted beyond "the remote's branch
// pointed here". The caller names the exact commit it will evaluate and
// this call only answers whether the branch holds it; the caller never
// evaluates the tip in its place.
//
// The checkout is re-gated exactly as PushHead re-gates it, and the
// token is minted only after those local gates pass. The fetch is
// forced onto a private ref because a branch may be rewritten between
// two calls on one checkout. Calls on one checkout share that ref and
// must not overlap; the production lane gives every pass its own
// checkout.
func (t *Transport) FetchHead(ctx context.Context, co Checkout, branch, headSHA string) (HeadFetch, error) {
	ref, err := parseTransportRepo(co.repo)
	if err != nil {
		return HeadFetch{}, err
	}
	if err := ValidateBranchName(branch); err != nil {
		return HeadFetch{}, fmt.Errorf("head branch %q: %w", branch, err)
	}
	if !validCommitSHA(headSHA) {
		return HeadFetch{}, fmt.Errorf("head %q is not a full commit SHA", headSHA)
	}
	scratch, err := os.MkdirTemp("", "freeside-transport-")
	if err != nil {
		return HeadFetch{}, fmt.Errorf("create transport scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort scratch cleanup
	r, err := newNetRunner(t.gitPath, scratch, t.scheme)
	if err != nil {
		return HeadFetch{}, err
	}
	if err := t.regateCheckout(ctx, r, co); err != nil {
		return HeadFetch{}, err
	}
	tok, err := t.tokens.Token(ctx, co.repo)
	if err != nil {
		return HeadFetch{}, err
	}
	// Trust continuity, as in PushHead: a name transferred onto another
	// repository since the base was fetched must not have that
	// repository's branch fetched into this checkout.
	if tok.RepositoryID <= 0 {
		return HeadFetch{}, fmt.Errorf("trusted binding for %s carries no canonical repository id: %w", co.repo, ErrGitTransport)
	}
	if co.repositoryID != tok.RepositoryID {
		return HeadFetch{}, fmt.Errorf(
			"checkout is bound to repository id %d, trusted binding for %q is %d: %w",
			co.repositoryID, co.repo, tok.RepositoryID, ErrGitTransport,
		)
	}
	url := t.repoURL(ref)
	if _, _, err := r.runAuthed(ctx, tok, fetchHeadArgs(url, branch)...); err != nil {
		// One bounded structured observation decides whether the branch is
		// gone; any other failure stays a transport failure the drain may
		// retry (FetchBase's rule for ErrRemoteMissingBase).
		tip, lsErr := lsRemoteHead(ctx, r, tok, url, "refs/heads/"+branch)
		if lsErr == nil && tip == "" {
			return HeadFetch{}, fmt.Errorf("remote %s has no branch %s: %w: %w", co.repo, branch, ErrRemoteMissingHead, err)
		}
		return HeadFetch{}, err
	}
	out, _, err := r.run(ctx, nil, "rev-parse", "--verify", transportHeadRef+"^{commit}")
	if err != nil {
		return HeadFetch{}, err
	}
	tip := strings.TrimSpace(string(out))
	if !validCommitSHA(tip) {
		return HeadFetch{}, fmt.Errorf("fetched branch %s resolved to an invalid object name: %w", branch, ErrGitTransport)
	}
	// The exact-head binding: the requested commit must be present and
	// reachable from the fetched branch. The checkout also holds the base
	// branch's history, so presence alone proves nothing; a commit that
	// lives only on the base branch is refused here.
	present, err := commitPresent(ctx, r, headSHA)
	if err != nil {
		return HeadFetch{}, err
	}
	if !present {
		return HeadFetch{}, fmt.Errorf("head %s is not on remote branch %s: %w", headSHA, branch, ErrRemoteMissingHead)
	}
	// Against the tip this call observed, not the ref, so the verdict and
	// the reported tip describe one fetch.
	onBranch, err := isAncestor(ctx, r, headSHA, tip)
	if err != nil {
		return HeadFetch{}, err
	}
	if !onBranch {
		return HeadFetch{}, fmt.Errorf("head %s is not reachable from remote branch %s: %w", headSHA, branch, ErrRemoteMissingHead)
	}
	descends, err := isAncestor(ctx, r, co.baseSHA, headSHA)
	if err != nil {
		return HeadFetch{}, err
	}
	return HeadFetch{TipSHA: tip, DescendsFromBase: descends}, nil
}

// ProspectiveMerge builds the merge of headSHA into the checkout's base
// as a commit in the checkout's object database and returns its SHA. The
// commit has exactly two parents, the base then the head, and a fixed
// author, committer, timestamp, and message, so rebuilding it from the
// same two commits yields the same SHA. Nothing is pushed and no ref is
// moved: the call is local, mints no token, and leaves HEAD at the base.
//
// A conflict returns *MergeConflictError and writes no commit. The merge
// reads no attributes and no merge drivers, so the result is git's
// default three-way merge of the trees: merge-tree ignores attributes
// committed in either tree, assertPristineConfig refuses a config that
// could name a driver or an attributes file, and the checkout's own
// info/attributes, the one remaining source, is refused here.
func (t *Transport) ProspectiveMerge(ctx context.Context, co Checkout, headSHA string) (string, error) {
	if !validCommitSHA(headSHA) {
		return "", fmt.Errorf("head %q is not a full commit SHA", headSHA)
	}
	// commit-tree drops a duplicate parent, so this would be a one-parent
	// commit, not a merge.
	if headSHA == co.baseSHA {
		return "", fmt.Errorf("head %s is the checkout's base; there is nothing to merge", headSHA)
	}
	scratch, err := os.MkdirTemp("", "freeside-transport-")
	if err != nil {
		return "", fmt.Errorf("create transport scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort scratch cleanup
	r, err := newNetRunner(t.gitPath, scratch, t.scheme)
	if err != nil {
		return "", err
	}
	if err := t.regateCheckout(ctx, r, co); err != nil {
		return "", err
	}
	switch _, err := os.Lstat(filepath.Join(co.dir, filepath.FromSlash(checkoutAttributesPath))); {
	case err == nil:
		return "", fmt.Errorf("checkout %s carries repository-local merge attributes: %w", co.dir, ErrGitTransport)
	case !errors.Is(err, os.ErrNotExist):
		return "", fmt.Errorf("checkout %s merge attributes could not be ruled out: %w", co.dir, ErrGitTransport)
	}
	present, err := commitPresent(ctx, r, headSHA)
	if err != nil {
		return "", err
	}
	if !present {
		return "", fmt.Errorf("head %s is not a commit in the checkout: %w", headSHA, ErrGitTransport)
	}
	listing := &truncatingBuffer{remaining: maxMergeConflictListingBytes}
	mergeErr := r.runTo(ctx, listing, mergeTreeArgs(co.baseSHA, headSHA)...)
	tree, paths := parseMergeTreeListing(listing.buffer.Bytes(), listing.truncated)
	if mergeErr != nil {
		// merge-tree exits 1 for a conflict and anything else for a merge
		// it could not complete. Only the first is a verdict; it must also
		// have named the tree it wrote, which no other exit-1 path does.
		var gitErr *TransportGitError
		if !errors.As(mergeErr, &gitErr) || gitErr.ExitCode != 1 || !validCommitSHA(tree) {
			return "", mergeErr
		}
		return "", &MergeConflictError{
			BaseSHA: co.baseSHA, HeadSHA: headSHA, Paths: paths, Truncated: listing.truncated,
		}
	}
	if !validCommitSHA(tree) {
		return "", fmt.Errorf("merge-tree returned an invalid tree name: %w", ErrGitTransport)
	}
	r.env = append(r.env,
		"GIT_AUTHOR_NAME="+prospectiveMergeAuthorName,
		"GIT_AUTHOR_EMAIL="+prospectiveMergeAuthorEmail,
		"GIT_AUTHOR_DATE="+prospectiveMergeDate,
		"GIT_COMMITTER_NAME="+prospectiveMergeAuthorName,
		"GIT_COMMITTER_EMAIL="+prospectiveMergeAuthorEmail,
		"GIT_COMMITTER_DATE="+prospectiveMergeDate,
	)
	out, _, err := r.run(ctx, nil,
		"commit-tree", tree, "-p", co.baseSHA, "-p", headSHA, "-m", prospectiveMergeMessage,
	)
	if err != nil {
		return "", err
	}
	merge := strings.TrimSpace(string(out))
	if !validCommitSHA(merge) {
		return "", fmt.Errorf("commit-tree returned an invalid commit name: %w", ErrGitTransport)
	}
	return merge, nil
}

// parseMergeTreeListing splits `merge-tree --write-tree -z --name-only
// --no-messages` output into the tree name and the conflicted paths
// that follow it, each NUL-terminated. A truncated listing may end
// inside a path, so its last element is dropped.
func parseMergeTreeListing(listing []byte, truncated bool) (tree string, paths []string) {
	fields := bytes.Split(listing, []byte{0})
	tree = string(fields[0])
	rest := fields[1:]
	if truncated && len(rest) > 0 {
		rest = rest[:len(rest)-1]
	}
	for _, path := range rest {
		if len(path) > 0 {
			paths = append(paths, string(path))
		}
	}
	return tree, paths
}

// regateCheckout pins the runner to a checkout this transport handed out
// earlier and re-proves what the capability claims about it: this
// instance materialized it, it is still bound to the same repository
// name and canonical ID, HEAD is still the enforced base, and its config
// holds only daemon-authored keys. These are the local re-gates PushHead
// applies before its token mint; the checkout directory is written by
// later pipeline stages, so every operation that returns to it re-proves
// them instead of trusting the capability's fields alone.
func (t *Transport) regateCheckout(ctx context.Context, r *netRunner, co Checkout) error {
	if co.owner != t {
		return errors.New("checkout was not materialized by this transport instance; run FetchBase on it")
	}
	if !validCommitSHA(co.baseSHA) {
		return fmt.Errorf("checkout base %q is not a full commit SHA", co.baseSHA)
	}
	if err := r.pinRepo(ctx, co.dir); err != nil {
		return err
	}
	bound, _, err := r.run(ctx, nil, "config", "--get", transportRepoKey)
	if err != nil {
		return fmt.Errorf("checkout %s carries no transport repo binding: %w", co.dir, err)
	}
	if got := strings.TrimSpace(string(bound)); got != co.repo {
		return fmt.Errorf("checkout is bound to repository %q, capability names %q: %w", got, co.repo, ErrGitTransport)
	}
	stampedID, err := readRepoIDBinding(co.dir)
	if err != nil {
		return err
	}
	if stampedID != co.repositoryID {
		return fmt.Errorf(
			"checkout is stamped with repository id %d, transport fetched it as %d: %w",
			stampedID, co.repositoryID, ErrGitTransport,
		)
	}
	head, _, err := r.run(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if got := strings.TrimSpace(string(head)); got != co.baseSHA {
		return fmt.Errorf("checkout HEAD %s is not the enforced base %s: %w", got, co.baseSHA, ErrGitTransport)
	}
	return r.assertPristineConfig(ctx)
}

// commitPresent answers whether the full SHA names a commit in the
// checkout, from `rev-parse --verify --quiet`'s exit status: 0 is yes, 1
// is absent or not a commit, and anything else (a killed process, a
// cancelled context) is a failed invocation, never a verdict.
func commitPresent(ctx context.Context, r *netRunner, sha string) (bool, error) {
	out, _, err := r.run(ctx, nil, "rev-parse", "--verify", "--quiet", sha+"^{commit}")
	if err == nil {
		// A full SHA resolves to itself or not at all.
		return strings.TrimSpace(string(out)) == sha, nil
	}
	var gitErr *TransportGitError
	if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// isAncestor answers `merge-base --is-ancestor` from its exit status:
// 0 is yes, 1 is no, and anything else is a failed invocation, never a
// verdict.
func isAncestor(ctx context.Context, r *netRunner, ancestor, descendant string) (bool, error) {
	_, _, err := r.run(ctx, nil, "merge-base", "--is-ancestor", ancestor, descendant)
	if err == nil {
		return true, nil
	}
	var gitErr *TransportGitError
	if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
		return false, nil
	}
	return false, err
}

// fetchHeadArgs is the fixed head-fetch argument vector: single branch,
// no tags, forced onto the private head ref. Pure so the golden test
// pins it byte-for-byte.
func fetchHeadArgs(url, branch string) []string {
	return []string{"fetch", "--no-tags", url, "+refs/heads/" + branch + ":" + transportHeadRef}
}

// mergeTreeArgs is the fixed merge argument vector: write the merged
// tree, list conflicted paths NUL-separated after the tree name, and
// emit no free-text messages. Pure so the golden test pins it.
func mergeTreeArgs(baseSHA, headSHA string) []string {
	return []string{"merge-tree", "--write-tree", "-z", "--name-only", "--no-messages", baseSHA, headSHA}
}
