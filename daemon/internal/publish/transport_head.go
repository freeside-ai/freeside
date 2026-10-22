package publish

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrRemoteMissingHead is returned when the managed repository's branch
// is gone, or does not contain the requested head commit. Like
// ErrRemoteMissingBase it is a definitive verdict a caller may act on,
// so it is decided from structured observations (ls-remote, the fetched
// ref, an ancestry exit status), never from a failure's prose.
var ErrRemoteMissingHead = errors.New("remote branch does not hold the requested head commit")

// ErrReviewedHeadUnavailable is FetchPullHead's class for a failed fetch
// of the reviewed head by SHA. It names which fetch failed and is not a
// verdict: a caller skips the rebase proof on it and records nothing.
var ErrReviewedHeadUnavailable = errors.New("reviewed head could not be fetched")

// ErrMergeConflict is the class a *MergeConflictError matches.
var ErrMergeConflict = errors.New("prospective merge has conflicts")

// transportHeadRef is where FetchHead lands a pull request's branch. It
// is private to the checkout like transportBaseRef and never pushed.
const transportHeadRef = "refs/freeside/head"

// transportPullHeadRef and transportReviewedHeadRef are where
// FetchPullHead lands the two heads a rebase proof compares, private to
// the checkout and never pushed like transportHeadRef.
const (
	transportPullHeadRef     = "refs/freeside/pull-head"
	transportReviewedHeadRef = "refs/freeside/reviewed-head"
)

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

// maxRangeDiffListingBytes bounds ProveRebase's range-diff listing. A
// proven listing is one short line per commit; one past the bound is
// refused rather than read in full.
const maxRangeDiffListingBytes = 1 << 20

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
	if err := ValidateBranchName(branch); err != nil {
		return HeadFetch{}, fmt.Errorf("head branch %q: %w", branch, err)
	}
	if !validCommitSHA(headSHA) {
		return HeadFetch{}, fmt.Errorf("head %q is not a full commit SHA", headSHA)
	}
	r, tok, url, cleanup, err := t.authedCheckoutRunner(ctx, co)
	if err != nil {
		return HeadFetch{}, err
	}
	defer cleanup()
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

// authedCheckoutRunner is the prelude of a fetch into a checkout: it
// re-gates the checkout, mints the token only after those local gates
// pass, and holds the minted binding to the repository the checkout was
// fetched from. The caller runs cleanup when it is done with the runner.
func (t *Transport) authedCheckoutRunner(
	ctx context.Context, co Checkout,
) (r *netRunner, tok InstallationToken, url string, cleanup func(), err error) {
	ref, err := parseTransportRepo(co.repo)
	if err != nil {
		return nil, InstallationToken{}, "", nil, err
	}
	scratch, err := os.MkdirTemp("", "freeside-transport-")
	if err != nil {
		return nil, InstallationToken{}, "", nil, fmt.Errorf("create transport scratch: %w", err)
	}
	cleanup = func() { _ = os.RemoveAll(scratch) } // best-effort scratch cleanup
	fail := func(err error) (*netRunner, InstallationToken, string, func(), error) {
		cleanup()
		return nil, InstallationToken{}, "", nil, err
	}
	r, err = newNetRunner(t.gitPath, scratch, t.scheme)
	if err != nil {
		return fail(err)
	}
	if err := t.regateCheckout(ctx, r, co); err != nil {
		return fail(err)
	}
	tok, err = t.tokens.Token(ctx, co.repo)
	if err != nil {
		return fail(err)
	}
	// Trust continuity, as in PushHead: a name transferred onto another
	// repository since the base was fetched must not have that
	// repository's refs fetched into this checkout.
	if tok.RepositoryID <= 0 {
		return fail(fmt.Errorf("trusted binding for %s carries no canonical repository id: %w", co.repo, ErrGitTransport))
	}
	if co.repositoryID != tok.RepositoryID {
		return fail(fmt.Errorf(
			"checkout is bound to repository id %d, trusted binding for %q is %d: %w",
			co.repositoryID, co.repo, tok.RepositoryID, ErrGitTransport,
		))
	}
	return r, tok, t.repoURL(ref), cleanup, nil
}

// FetchPullHead brings into the checkout the two heads a rebase proof
// compares (issue #2129): headSHA, a head someone else pushed to pull
// request prNumber, and reviewedHeadSHA, the head Freeside reviewed. A
// lander that rebases force-pushes the pull request's branch and may
// delete it after the merge, so neither head is fetched by branch.
// headSHA comes from the pull request's own ref, which outlives the
// branch, and must be reachable from it, so a commit that lives only on
// the base branch is refused with ErrRemoteMissingHead exactly as in
// FetchHead. reviewedHeadSHA is no longer on any ref the remote
// advertises, so it is fetched by SHA, which GitHub serves for a commit a
// pull request once held.
//
// A failed fetch of the reviewed head returns ErrReviewedHeadUnavailable.
// Git reports a refused want and a dropped connection with one exit
// status, so the class says which fetch failed, never that the remote
// lacks the commit: a caller may skip the proof on it but must not record
// a verdict.
func (t *Transport) FetchPullHead(ctx context.Context, co Checkout, prNumber int, headSHA, reviewedHeadSHA string) error {
	if prNumber <= 0 {
		return fmt.Errorf("pull request number %d is not positive", prNumber)
	}
	for _, sha := range []string{headSHA, reviewedHeadSHA} {
		if !validCommitSHA(sha) {
			return fmt.Errorf("head %q is not a full commit SHA", sha)
		}
	}
	r, tok, url, cleanup, err := t.authedCheckoutRunner(ctx, co)
	if err != nil {
		return err
	}
	defer cleanup()
	pullRef := "refs/pull/" + strconv.Itoa(prNumber) + "/head"
	if _, _, err := r.runAuthed(ctx, tok, fetchPullHeadArgs(url, prNumber)...); err != nil {
		tip, lsErr := lsRemoteHead(ctx, r, tok, url, pullRef)
		if lsErr == nil && tip == "" {
			return fmt.Errorf("remote %s has no %s: %w: %w", co.repo, pullRef, ErrRemoteMissingHead, err)
		}
		return err
	}
	out, _, err := r.run(ctx, nil, "rev-parse", "--verify", transportPullHeadRef+"^{commit}")
	if err != nil {
		return err
	}
	tip := strings.TrimSpace(string(out))
	if !validCommitSHA(tip) {
		return fmt.Errorf("fetched %s resolved to an invalid object name: %w", pullRef, ErrGitTransport)
	}
	present, err := commitPresent(ctx, r, headSHA)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("head %s is not on remote %s: %w", headSHA, pullRef, ErrRemoteMissingHead)
	}
	onPull, err := isAncestor(ctx, r, headSHA, tip)
	if err != nil {
		return err
	}
	if !onPull {
		return fmt.Errorf("head %s is not reachable from remote %s: %w", headSHA, pullRef, ErrRemoteMissingHead)
	}
	if _, _, err := r.runAuthed(ctx, tok, fetchReviewedHeadArgs(url, reviewedHeadSHA)...); err != nil {
		return fmt.Errorf("fetch reviewed head %s: %w: %w", reviewedHeadSHA, ErrReviewedHeadUnavailable, err)
	}
	present, err = commitPresent(ctx, r, reviewedHeadSHA)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("reviewed head %s is not a commit after its fetch: %w", reviewedHeadSHA, ErrReviewedHeadUnavailable)
	}
	return nil
}

// RebaseProof is ProveRebase's verdict on one head.
type RebaseProof struct {
	Proven bool
	// TreeSHA is the tree of the reviewed pair's prospective merge, empty
	// when the proof failed before it was computed.
	TreeSHA string
	// Detail is the first reason the proof failed, empty when Proven. It
	// may quote one range-diff line, which carries a commit subject from
	// the pull request's branch, so the line is Go-quoted into ASCII and
	// the whole detail is cut to domain.WorkUnitRebaseProofDetailLimit.
	Detail string
}

// ProveRebase decides whether newHead is reviewedHead rebased onto
// reviewedBase, with nothing added, dropped, squashed, or reworded:
// newHead descends from reviewedBase; git range-diff pairs every commit
// of the reviewed range, in order, with one of the rebased range as
// identical ("="), so a reflowed message or a resolved conflict is a
// refusal by design; the rebased range adds no merge commit; each
// rebased commit changes the same paths as its reviewed pair, to the same
// contents and modes wherever both start from the same contents, which
// covers the binary changes range-diff does not compare; and newHead's
// tree is the tree of the prospective merge the review covered. The
// checks run in that order after the merge is built, and the first that
// fails names the refusal. Like ProspectiveMerge the call is
// local, mints no token, and leaves HEAD at the base; all three commits
// must already be in the checkout (FetchPullHead brings in the two heads).
//
// A refusal is a verdict, returned with a nil error. An error means no
// verdict was reached; the one exception is a reviewed pair that no
// longer merges cleanly, which refuses with no tree.
func (t *Transport) ProveRebase(
	ctx context.Context, co Checkout, reviewedBase, reviewedHead, newHead string,
) (RebaseProof, error) {
	if !validCommitSHA(newHead) {
		return RebaseProof{}, fmt.Errorf("head %q is not a full commit SHA", newHead)
	}
	if newHead == reviewedHead {
		return RebaseProof{}, fmt.Errorf("head %s is the reviewed head; there is nothing to prove", newHead)
	}
	merge, err := t.ProspectiveMergeOnto(ctx, co, reviewedBase, reviewedHead)
	var conflict *MergeConflictError
	if errors.As(err, &conflict) {
		return RebaseProof{Detail: "the reviewed head no longer merges cleanly into the reviewed base"}, nil
	}
	if err != nil {
		return RebaseProof{}, err
	}
	scratch, err := os.MkdirTemp("", "freeside-transport-")
	if err != nil {
		return RebaseProof{}, fmt.Errorf("create transport scratch: %w", err)
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // best-effort scratch cleanup
	r, err := newNetRunner(t.gitPath, scratch, t.scheme)
	if err != nil {
		return RebaseProof{}, err
	}
	if err := t.regateCheckout(ctx, r, co); err != nil {
		return RebaseProof{}, err
	}
	tree, err := revParseTree(ctx, r, merge)
	if err != nil {
		return RebaseProof{}, err
	}
	refuse := func(detail string) (RebaseProof, error) {
		return RebaseProof{TreeSHA: tree, Detail: truncateDetail(detail)}, nil
	}
	present, err := commitPresent(ctx, r, newHead)
	if err != nil {
		return RebaseProof{}, err
	}
	if !present {
		return RebaseProof{}, fmt.Errorf("head %s is not a commit in the checkout: %w", newHead, ErrGitTransport)
	}
	descends, err := isAncestor(ctx, r, reviewedBase, newHead)
	if err != nil {
		return RebaseProof{}, err
	}
	if !descends {
		return refuse("the head does not descend from the reviewed base")
	}
	out, _, err := r.run(ctx, nil, "merge-base", reviewedBase, reviewedHead)
	if err != nil {
		return RebaseProof{}, err
	}
	forkPoint := strings.TrimSpace(string(out))
	if !validCommitSHA(forkPoint) {
		return RebaseProof{}, fmt.Errorf("merge-base returned an invalid commit name: %w", ErrGitTransport)
	}
	listing := &truncatingBuffer{remaining: maxRangeDiffListingBytes}
	if err := r.runTo(ctx, listing, rangeDiffArgs(forkPoint, reviewedHead, reviewedBase, newHead)...); err != nil {
		return RebaseProof{}, err
	}
	if listing.truncated {
		return refuse("the range-diff listing exceeded its bound")
	}
	if detail := rangeDiffMismatch(listing.buffer.Bytes()); detail != "" {
		return refuse(detail)
	}
	// range-diff walks without merges and prints "Binary files differ" for
	// any binary change, so an all-"=" listing still admits a merge commit
	// over the reviewed commits and a rebased commit whose binary bytes
	// differ from the reviewed one. Both rewrite reviewed history.
	merges, _, err := r.run(ctx, nil, rebasedMergesArgs(reviewedBase, newHead)...)
	if err != nil {
		return RebaseProof{}, err
	}
	if len(bytes.TrimSpace(merges)) != 0 {
		return refuse("the head adds a merge commit over the reviewed base")
	}
	detail, err := rebasedBlobMismatch(ctx, r, forkPoint, reviewedHead, reviewedBase, newHead)
	if err != nil {
		return RebaseProof{}, err
	}
	if detail != "" {
		return refuse(detail)
	}
	headTree, err := revParseTree(ctx, r, newHead)
	if err != nil {
		return RebaseProof{}, err
	}
	if headTree != tree {
		return refuse("the head's tree " + headTree + " is not the reviewed merge tree " + tree)
	}
	return RebaseProof{Proven: true, TreeSHA: tree}, nil
}

// rangeDiffMismatch returns why a range-diff listing does not pair every
// commit as identical, or "" when it does. Each pairing line reads
// "<n>:  <sha> <status> <m>:  <sha> <subject>"; any status but "=" fails,
// and so does a listing with no pairing at all. Only "!" pairings carry
// indented diff lines, and the first one already fails.
func rangeDiffMismatch(listing []byte) string {
	paired := 0
	for _, line := range strings.Split(string(listing), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[2] != "=" {
			return "range-diff does not pair the reviewed commits as identical: " + strconv.QuoteToASCII(line)
		}
		// Each reviewed commit must pair with the rebased commit at its
		// own position: an "=" pairing across positions is a reorder.
		if fields[0] != fields[3] {
			return "range-diff pairs the reviewed commits out of order: " + strconv.QuoteToASCII(line)
		}
		paired++
	}
	if paired == 0 {
		return "range-diff found no commits to pair"
	}
	return ""
}

// rawChange is one path's entry in a commit's raw tree diff.
type rawChange struct {
	oldBlob, newBlob, newMode string
}

// rebasedBlobMismatch returns why the rebased commits do not reproduce the
// reviewed commits byte for byte, or "" when they do. It pairs the commits
// by position, which the range-diff listing already required. For each
// pair, both commits must change the same paths, and a path that starts
// from the same blob must end at the same blob and mode. A text change
// passes whenever range-diff said "=", since the same hunks on the same
// content give the same result; the check exists for binary changes, which
// range-diff does not compare. A path whose starting blob differs (the base
// changed it) is left to range-diff and the final tree comparison.
func rebasedBlobMismatch(
	ctx context.Context, r *netRunner, forkPoint, reviewedHead, reviewedBase, newHead string,
) (string, error) {
	reviewed, err := revListCommits(ctx, r, "--no-merges", forkPoint+".."+reviewedHead)
	if err != nil {
		return "", err
	}
	rebased, err := revListCommits(ctx, r, reviewedBase+".."+newHead)
	if err != nil {
		return "", err
	}
	if len(reviewed) != len(rebased) {
		return fmt.Sprintf("the head carries %d commits over the reviewed base, not the %d reviewed",
			len(rebased), len(reviewed)), nil
	}
	for i := range reviewed {
		was, truncated, err := rawTreeChanges(ctx, r, reviewed[i])
		if err != nil {
			return "", err
		}
		now, truncatedNow, err := rawTreeChanges(ctx, r, rebased[i])
		if err != nil {
			return "", err
		}
		if truncated || truncatedNow {
			return "a commit's change listing exceeded its bound", nil
		}
		if len(was) != len(now) {
			return fmt.Sprintf("rebased commit %s changes %d paths, reviewed commit %s %d",
				rebased[i], len(now), reviewed[i], len(was)), nil
		}
		for path, before := range was {
			after, ok := now[path]
			if !ok {
				return fmt.Sprintf("rebased commit %s does not change %s", rebased[i], strconv.QuoteToASCII(path)), nil
			}
			if before.oldBlob == after.oldBlob && (before.newBlob != after.newBlob || before.newMode != after.newMode) {
				return fmt.Sprintf("rebased commit %s changes %s to other content than reviewed commit %s",
					rebased[i], strconv.QuoteToASCII(path), reviewed[i]), nil
			}
		}
	}
	return "", nil
}

// revListCommits lists a range's commits oldest first.
func revListCommits(ctx context.Context, r *netRunner, args ...string) ([]string, error) {
	out, _, err := r.run(ctx, nil, append([]string{"rev-list", "--reverse"}, args...)...)
	if err != nil {
		return nil, err
	}
	commits := strings.Fields(string(out))
	for _, commit := range commits {
		if !validCommitSHA(commit) {
			return nil, fmt.Errorf("rev-list returned an invalid commit name: %w", ErrGitTransport)
		}
	}
	return commits, nil
}

// rawTreeChanges reads a non-merge commit's raw tree diff against its
// parent, keyed by path. The listing is bounded like the range-diff one;
// truncated reports that it was cut.
func rawTreeChanges(ctx context.Context, r *netRunner, commit string) (map[string]rawChange, bool, error) {
	listing := &truncatingBuffer{remaining: maxRangeDiffListingBytes}
	if err := r.runTo(ctx, listing, rawTreeDiffArgs(commit)...); err != nil {
		return nil, false, err
	}
	if listing.truncated {
		return nil, true, nil
	}
	// With -z each entry is ":<old mode> <new mode> <old blob> <new blob>
	// <status>" and then the path, each NUL-terminated.
	fields := strings.Split(strings.TrimSuffix(listing.buffer.String(), "\x00"), "\x00")
	if len(fields) == 1 && fields[0] == "" {
		return map[string]rawChange{}, false, nil
	}
	if len(fields)%2 != 0 {
		return nil, false, fmt.Errorf("diff-tree of %s returned an unpaired entry: %w", commit, ErrGitTransport)
	}
	changes := make(map[string]rawChange, len(fields)/2)
	for i := 0; i < len(fields); i += 2 {
		meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(meta) != 5 || !strings.HasPrefix(fields[i], ":") {
			return nil, false, fmt.Errorf("diff-tree of %s returned a malformed entry: %w", commit, ErrGitTransport)
		}
		changes[fields[i+1]] = rawChange{oldBlob: meta[2], newBlob: meta[3], newMode: meta[1]}
	}
	return changes, false, nil
}

// truncateDetail cuts a refusal detail to the stored bound. Every detail
// is ASCII (a quoted range-diff line is QuoteToASCII output), so a byte
// cut stays valid UTF-8.
func truncateDetail(detail string) string {
	if len(detail) > domain.WorkUnitRebaseProofDetailLimit {
		return detail[:domain.WorkUnitRebaseProofDetailLimit]
	}
	return detail
}

// revParseTree returns the tree a commit names.
func revParseTree(ctx context.Context, r *netRunner, commit string) (string, error) {
	out, _, err := r.run(ctx, nil, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSpace(string(out))
	if !validCommitSHA(tree) {
		return "", fmt.Errorf("rev-parse returned an invalid tree name: %w", ErrGitTransport)
	}
	return tree, nil
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
	return t.prospectiveMerge(ctx, co, co.baseSHA, headSHA)
}

// ProspectiveMergeOnto is ProspectiveMerge into baseSHA instead of the
// checkout's own base. It serves a run whose target base advanced before its
// first publication (issue #2058): the checkout stays bound to the base the
// head was imported over, which PushHead requires, while the merge names the
// newer base. baseSHA must already be in the checkout and reachable from the
// base branch FetchBase fetched, so the merge is built only against a commit
// the remote's branch held; one that is not fails with ErrRemoteMissingBase.
// The commit is the one ProspectiveMerge builds in a checkout of baseSHA.
func (t *Transport) ProspectiveMergeOnto(ctx context.Context, co Checkout, baseSHA, headSHA string) (string, error) {
	if !validCommitSHA(baseSHA) {
		return "", fmt.Errorf("base %q is not a full commit SHA", baseSHA)
	}
	return t.prospectiveMerge(ctx, co, baseSHA, headSHA)
}

func (t *Transport) prospectiveMerge(ctx context.Context, co Checkout, baseSHA, headSHA string) (string, error) {
	if !validCommitSHA(headSHA) {
		return "", fmt.Errorf("head %q is not a full commit SHA", headSHA)
	}
	// commit-tree drops a duplicate parent, so this would be a one-parent
	// commit, not a merge.
	if headSHA == baseSHA {
		return "", fmt.Errorf("head %s is the merge base; there is nothing to merge", headSHA)
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
	if baseSHA != co.baseSHA {
		present, err := commitPresent(ctx, r, baseSHA)
		if err != nil {
			return "", err
		}
		if !present {
			return "", fmt.Errorf("base %s is not a commit in the checkout: %w", baseSHA, ErrRemoteMissingBase)
		}
		// An ancestry verdict is the only structured answer here; a failure
		// with any other exit status stays a plain transport failure.
		if _, _, err := r.run(ctx, nil, "merge-base", "--is-ancestor", baseSHA, transportBaseRef); err != nil {
			var gitErr *TransportGitError
			if errors.As(err, &gitErr) && gitErr.ExitCode == 1 {
				return "", fmt.Errorf("base %s is not reachable from the fetched base branch: %w: %w",
					baseSHA, ErrRemoteMissingBase, err)
			}
			return "", err
		}
	}
	listing := &truncatingBuffer{remaining: maxMergeConflictListingBytes}
	mergeErr := r.runTo(ctx, listing, mergeTreeArgs(baseSHA, headSHA)...)
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
			BaseSHA: baseSHA, HeadSHA: headSHA, Paths: paths, Truncated: listing.truncated,
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
		"commit-tree", tree, "-p", baseSHA, "-p", headSHA, "-m", prospectiveMergeMessage,
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

// fetchPullHeadArgs is the fixed pull-request-head fetch argument
// vector: the forge's pull ref, no tags, forced onto the private ref.
func fetchPullHeadArgs(url string, prNumber int) []string {
	return []string{
		"fetch", "--no-tags", url,
		"+refs/pull/" + strconv.Itoa(prNumber) + "/head:" + transportPullHeadRef,
	}
}

// fetchReviewedHeadArgs is the fixed by-SHA fetch argument vector for
// the reviewed head, which no advertised ref names any more.
func fetchReviewedHeadArgs(url, reviewedHeadSHA string) []string {
	return []string{"fetch", "--no-tags", url, "+" + reviewedHeadSHA + ":" + transportReviewedHeadRef}
}

// rebasedMergesArgs lists at most one merge commit the head adds over the
// reviewed base.
func rebasedMergesArgs(reviewedBase, newHead string) []string {
	return []string{"rev-list", "--merges", "--max-count=1", reviewedBase + ".." + newHead}
}

// rawTreeDiffArgs is one commit's raw diff against its parent: full blob
// names, NUL-separated paths, and no rename detection, so each path is one
// entry.
func rawTreeDiffArgs(commit string) []string {
	return []string{"diff-tree", "-r", "-z", "--raw", "--no-abbrev", "--no-renames", "--no-commit-id", commit}
}

// rangeDiffArgs is the fixed range-diff argument vector: the reviewed
// range from its fork point, the rebased range from the reviewed base,
// uncolored and without notes so the listing is plain pairing lines.
func rangeDiffArgs(forkPoint, reviewedHead, reviewedBase, newHead string) []string {
	return []string{
		"range-diff", "--no-color", "--no-notes",
		forkPoint + ".." + reviewedHead, reviewedBase + ".." + newHead,
	}
}

// mergeTreeArgs is the fixed merge argument vector: write the merged
// tree, list conflicted paths NUL-separated after the tree name, and
// emit no free-text messages. Pure so the golden test pins it.
func mergeTreeArgs(baseSHA, headSHA string) []string {
	return []string{"merge-tree", "--write-tree", "-z", "--name-only", "--no-messages", baseSHA, headSHA}
}
