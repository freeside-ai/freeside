package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// tryGit is runGit for a call whose failure is an answer: it returns the exit
// status instead of failing the test. Identity and dates are fixed, so a
// commit it writes has the same SHA on every run.
func tryGit(dir string, args ...string) (string, int, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test running git on fixture arguments
	cmd.Env = append(gitenv.Scrubbed(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@test.invalid",
		"GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@test.invalid",
		"GIT_COMMITTER_DATE=1700000000 +0000",
	)
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return strings.TrimSpace(string(out)), exit.ExitCode(), nil
	}
	return strings.TrimSpace(string(out)), 0, err
}

// headRepo is the remote side of the pull request branches. PushHead only
// records a ref on the forge double, so a branch's commits live here, where
// FetchHead can fetch them the way the real transport fetches a branch.
func (tr *integrationTransport) headRepo() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if tr.headsDir == "" {
		tr.headsDir = tr.t.TempDir()
		runGit(tr.t, tr.headsDir, "init", "-q", "--bare")
		// GitHub serves a commit a pull request once held by SHA after a
		// force-push; a bare repository does so only when told to.
		runGit(tr.t, tr.headsDir, "config", "uploadpack.allowAnySHA1InWant", "true")
	}
	return tr.headsDir
}

// publishHead makes sha the tip of branch on the remote side, from a
// repository that holds the commit.
func (tr *integrationTransport) publishHead(dir, branch, sha string) {
	runGit(tr.t, dir, "push", "-q", "--force", tr.headRepo(), sha+":refs/heads/"+branch)
}

// pushBranchHead models someone pushing sha to the pull request's branch:
// the remote branch, the forge's ref, and the pull request's head all move.
func (tr *integrationTransport) pushBranchHead(dir, branch, sha string) {
	tr.publishHead(dir, branch, sha)
	tr.forge.mu.Lock()
	defer tr.forge.mu.Unlock()
	tr.forge.refs[branch] = sha
	for i := range tr.forge.prs {
		if tr.forge.prs[i].HeadRef == branch {
			tr.forge.prs[i].HeadSHA = sha
		}
	}
}

// landPullHead models a lander that rewrote the pull request: it
// force-pushes sha to the branch, and the forge's pull ref follows.
func (tr *integrationTransport) landPullHead(dir, branch string, number int, sha string) {
	tr.pushBranchHead(dir, branch, sha)
	runGit(tr.t, dir, "push", "-q", "--force", tr.headRepo(), sha+":refs/pull/"+strconv.Itoa(number)+"/head")
}

// deleteBranch models a lander deleting the branch after it merges. The
// pull ref outlives it.
func (tr *integrationTransport) deleteBranch(branch string) {
	runGit(tr.t, tr.headRepo(), "update-ref", "-d", "refs/heads/"+branch)
	tr.forge.mu.Lock()
	defer tr.forge.mu.Unlock()
	delete(tr.forge.refs, branch)
}

// scriptRebaseProof sets the verdict later ProveRebase calls return. The
// publish package proves the verdict itself against real rewrites; the
// cycle tests need only its outcome. Unscripted, ProveRebase fails as an
// unprovable transport failure would.
func (tr *integrationTransport) scriptRebaseProof(verdict publish.RebaseProof) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.rebaseVerdict = &verdict
}

func (tr *integrationTransport) proofCounts() (pullFetches, proofs int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.pullFetches, tr.proofs
}

// FetchPullHead fetches the pull ref and the reviewed head by SHA, as the
// real transport does, and requires headSHA on the pull ref.
func (tr *integrationTransport) FetchPullHead(
	ctx context.Context, checkout engine.PublicationCheckout, prNumber int, headSHA, reviewedHeadSHA string,
) error {
	sealed, ok := checkout.(integrationCheckout)
	if !ok || sealed.owner != tr {
		return engine.ErrForeignPublicationCheckout
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tr.mu.Lock()
	tr.pullFetches++
	tr.mu.Unlock()
	const ref = "refs/freeside/pull-head"
	if _, code, err := tryGit(sealed.dir, "fetch", "-q", "--no-tags", tr.headRepo(),
		"+refs/pull/"+strconv.Itoa(prNumber)+"/head:"+ref); err != nil || code != 0 {
		return fmt.Errorf("remote has no pull ref %d: %w", prNumber, errors.Join(err, publish.ErrRemoteMissingHead))
	}
	if _, code, err := tryGit(sealed.dir, "merge-base", "--is-ancestor", headSHA, ref); err != nil || code != 0 {
		return fmt.Errorf("head %s is not on pull ref %d: %w", headSHA, prNumber,
			errors.Join(err, publish.ErrRemoteMissingHead))
	}
	if _, code, err := tryGit(sealed.dir, "fetch", "-q", "--no-tags", tr.headRepo(),
		"+"+reviewedHeadSHA+":refs/freeside/reviewed-head"); err != nil || code != 0 {
		return fmt.Errorf("fetch reviewed head %s: %w", reviewedHeadSHA,
			errors.Join(err, publish.ErrReviewedHeadUnavailable))
	}
	return nil
}

// ProveRebase returns the scripted verdict once both heads are in the
// checkout.
func (tr *integrationTransport) ProveRebase(
	ctx context.Context, checkout engine.PublicationCheckout, reviewedBase, reviewedHead, newHead string,
) (publish.RebaseProof, error) {
	sealed, ok := checkout.(integrationCheckout)
	if !ok || sealed.owner != tr {
		return publish.RebaseProof{}, engine.ErrForeignPublicationCheckout
	}
	if err := ctx.Err(); err != nil {
		return publish.RebaseProof{}, err
	}
	for _, sha := range []string{reviewedBase, reviewedHead, newHead} {
		if _, code, err := tryGit(sealed.dir, "cat-file", "-e", sha+"^{commit}"); err != nil || code != 0 {
			return publish.RebaseProof{}, fmt.Errorf("commit %s is not in the checkout: %w",
				sha, errors.Join(err, publish.ErrGitTransport))
		}
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.proofs++
	if tr.rebaseVerdict == nil {
		return publish.RebaseProof{}, fmt.Errorf("no rebase verdict scripted: %w", publish.ErrGitTransport)
	}
	return *tr.rebaseVerdict, nil
}

var _ engine.RebaseProofTransport = (*integrationTransport)(nil)

// onHeadFetch runs hook before each later FetchHead, with the call's
// one-based count, so a test can move a branch between two fetches of one
// cycle.
func (tr *integrationTransport) onHeadFetch(hook func(call int)) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.headFetchHook, tr.headFetches = hook, 0
}

// rebuildMergesAs makes later prospective merges of the same pair a
// different commit, which a real transport's fixed message never does.
func (tr *integrationTransport) rebuildMergesAs(message string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.mergeMessage = message
}

func (tr *integrationTransport) mergeCount() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.merges
}

func (tr *integrationTransport) FetchHead(
	ctx context.Context, checkout engine.PublicationCheckout, branch, headSHA string,
) (publish.HeadFetch, error) {
	sealed, ok := checkout.(integrationCheckout)
	if !ok || sealed.owner != tr {
		return publish.HeadFetch{}, engine.ErrForeignPublicationCheckout
	}
	if err := ctx.Err(); err != nil {
		return publish.HeadFetch{}, err
	}
	tr.mu.Lock()
	tr.headFetches++
	call, hook := tr.headFetches, tr.headFetchHook
	tr.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	const ref = "refs/freeside/head"
	if _, code, err := tryGit(sealed.dir, "fetch", "-q", "--no-tags", tr.headRepo(),
		"+refs/heads/"+branch+":"+ref); err != nil || code != 0 {
		return publish.HeadFetch{}, fmt.Errorf(
			"remote has no branch %s: %w", branch, errors.Join(err, publish.ErrRemoteMissingHead))
	}
	tip := runGit(tr.t, sealed.dir, "rev-parse", "--verify", ref+"^{commit}")
	if _, code, err := tryGit(sealed.dir, "cat-file", "-e", headSHA+"^{commit}"); err != nil || code != 0 {
		return publish.HeadFetch{}, fmt.Errorf(
			"head %s is not on remote branch %s: %w", headSHA, branch,
			errors.Join(err, publish.ErrRemoteMissingHead))
	}
	if _, code, err := tryGit(sealed.dir, "merge-base", "--is-ancestor", headSHA, tip); err != nil || code != 0 {
		return publish.HeadFetch{}, fmt.Errorf(
			"head %s is not reachable from remote branch %s: %w", headSHA, branch,
			errors.Join(err, publish.ErrRemoteMissingHead))
	}
	_, code, err := tryGit(sealed.dir, "merge-base", "--is-ancestor", sealed.baseSHA, headSHA)
	if err != nil {
		return publish.HeadFetch{}, err
	}
	return publish.HeadFetch{TipSHA: tip, DescendsFromBase: code == 0}, nil
}

func (tr *integrationTransport) ProspectiveMerge(
	ctx context.Context, checkout engine.PublicationCheckout, headSHA string,
) (string, error) {
	sealed, ok := checkout.(integrationCheckout)
	if !ok || sealed.owner != tr {
		return "", engine.ErrForeignPublicationCheckout
	}
	return tr.prospectiveMerge(ctx, sealed, sealed.baseSHA, headSHA)
}

// ProspectiveMergeOnto merges into a base the checkout's branch advanced to,
// as the real transport does for a re-entry before publication.
func (tr *integrationTransport) ProspectiveMergeOnto(
	ctx context.Context, checkout engine.PublicationCheckout, baseSHA, headSHA string,
) (string, error) {
	sealed, ok := checkout.(integrationCheckout)
	if !ok || sealed.owner != tr {
		return "", engine.ErrForeignPublicationCheckout
	}
	if _, code, err := tryGit(sealed.dir, "merge-base", "--is-ancestor", baseSHA, "refs/remotes/origin/main"); err != nil || code != 0 {
		return "", fmt.Errorf("base %s is not on the fetched branch: %w",
			baseSHA, errors.Join(err, publish.ErrRemoteMissingBase))
	}
	return tr.prospectiveMerge(ctx, sealed, baseSHA, headSHA)
}

func (tr *integrationTransport) prospectiveMerge(
	ctx context.Context, sealed integrationCheckout, baseSHA, headSHA string,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if headSHA == baseSHA {
		return "", errors.New("head is the base commit")
	}
	tr.mu.Lock()
	tr.merges++
	message := tr.mergeMessage
	tr.mu.Unlock()
	if message == "" {
		message = "prospective merge"
	}
	out, code, err := tryGit(sealed.dir, "merge-tree", "--write-tree", "--name-only",
		"--no-messages", baseSHA, headSHA)
	if err != nil {
		return "", err
	}
	lines := strings.Split(out, "\n")
	switch code {
	case 0:
	case 1:
		return "", &publish.MergeConflictError{
			BaseSHA: baseSHA, HeadSHA: headSHA, Paths: lines[1:],
		}
	default:
		return "", fmt.Errorf("merge-tree exited %d", code)
	}
	merge, code, err := tryGit(sealed.dir, "commit-tree", lines[0],
		"-p", baseSHA, "-p", headSHA, "-m", message)
	if err != nil || code != 0 {
		return "", fmt.Errorf("commit-tree exited %d: %w", code, err)
	}
	return merge, nil
}
