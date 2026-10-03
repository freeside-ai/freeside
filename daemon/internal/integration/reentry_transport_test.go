package integration_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// tryGit is runGit for a call whose failure is an answer: it returns the exit
// status instead of failing the test. Identity and dates are fixed, so a
// commit it writes has the same SHA on every run.
func tryGit(dir string, args ...string) (string, int, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test running git on fixture arguments
	cmd.Env = append(scrubbedGitEnv(),
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
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if headSHA == sealed.baseSHA {
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
		"--no-messages", sealed.baseSHA, headSHA)
	if err != nil {
		return "", err
	}
	lines := strings.Split(out, "\n")
	switch code {
	case 0:
	case 1:
		return "", &publish.MergeConflictError{
			BaseSHA: sealed.baseSHA, HeadSHA: headSHA, Paths: lines[1:],
		}
	default:
		return "", fmt.Errorf("merge-tree exited %d", code)
	}
	merge, code, err := tryGit(sealed.dir, "commit-tree", lines[0],
		"-p", sealed.baseSHA, "-p", headSHA, "-m", message)
	if err != nil || code != 0 {
		return "", fmt.Errorf("commit-tree exited %d: %w", code, err)
	}
	return merge, nil
}
