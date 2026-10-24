package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
	"github.com/freeside-ai/freeside/daemon/internal/procbound"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// Git runs git in dir with ambient git state and global and system config
// neutralized and a fixed identity and date, so a commit it writes has the
// same SHA on every run. A nonzero exit is an answer, returned as code, not
// an error.
func Git(dir string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: the double runs git over its own repositories
	cmd.Env = append(gitenv.Scrubbed(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@test.invalid",
		"GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@test.invalid",
		"GIT_COMMITTER_DATE=1700000000 +0000",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := procbound.Run(cmd, procbound.DefaultWaitDelay)
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return strings.TrimSpace(out.String()), exit.ExitCode(), nil
	}
	return strings.TrimSpace(out.String()), 0, err
}

// mustGit is Git for a call whose failure is a broken double, not an answer.
func mustGit(dir string, args ...string) (string, error) {
	out, code, err := Git(dir, args...)
	if err != nil {
		return "", fmt.Errorf("git %v: %w", args, err)
	}
	if code != 0 {
		return "", fmt.Errorf("git %v exited %d: %s", args, code, out)
	}
	return out, nil
}

type checkout struct {
	dir, repo, baseRef, baseSHA string
	laterDir                    string
	dirCalls                    *int
	owner                       *Transport
}

func (c checkout) Dir() string {
	if c.dirCalls != nil {
		*c.dirCalls++
		if *c.dirCalls > 1 && c.laterDir != "" {
			return c.laterDir
		}
	}
	return c.dir
}
func (c checkout) Repo() string    { return c.repo }
func (c checkout) BaseRef() string { return c.baseRef }
func (c checkout) BaseSHA() string { return c.baseSHA }

// Transport is the publication transport over local git: FetchBase clones
// BaseDir, PushHead records the branch on Forge and pushes its commits to the
// bare repository at HeadsDir, which the re-entry fetches read back.
type Transport struct {
	// BaseDir is the repository FetchBase clones; BaseRef is the only branch
	// it serves, of Forge's repository.
	BaseDir string
	BaseRef string
	Forge   *Forge
	// HeadsDir is an empty directory that becomes the remote side of the
	// pull request branches on first use.
	HeadsDir string

	// Faults the tests set before the call they shape.
	ReturnDir              string
	LaterReturnDir         string
	SymlinkTarget          string
	ReplaceParent          string
	NoCheckout             bool
	MaterializeBaseErr     error
	SuccessorUpdateFailure error

	mu             sync.Mutex
	fetches        int
	fetchErr       error
	fetchEntered   chan struct{}
	fetchRelease   chan struct{}
	materializeErr error
	pushes         int
	lastMessage    string
	fail           bool
	conflict       bool
	// The re-entry fetch accounting.
	headsReady    bool
	headFetches   int
	headFetchHook func(call int)
	merges        int
	mergeMessage  string
	// The scripted rebase proof verdict and the proof accounting.
	rebaseVerdict *publish.RebaseProof
	pullFetches   int
	proofs        int
}

var _ engine.RebaseProofTransport = (*Transport)(nil)

func (tr *Transport) RetainWorktree(
	ctx context.Context, publication engine.PublicationCheckout, dest, headSHA string,
) error {
	sealed, ok := publication.(checkout)
	if !ok || sealed.owner != tr {
		return engine.ErrForeignPublicationCheckout
	}
	tr.mu.Lock()
	err := tr.materializeErr
	if headSHA == sealed.baseSHA {
		err = tr.MaterializeBaseErr
	}
	tr.mu.Unlock()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	got, err := mustGit(publication.Dir(), "rev-parse", "--verify", headSHA+"^{commit}")
	if err != nil {
		return err
	}
	if got != headSHA {
		return fmt.Errorf("materialization resolved %s to %s", headSHA, got)
	}
	if err := os.CopyFS(dest, os.DirFS(publication.Dir())); err != nil {
		return err
	}
	_, err = mustGit(dest, "reset", "-q", "--hard", headSHA)
	return err
}

func (tr *Transport) FailMaterialization(err error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.materializeErr = err
}

// BlockNextFetch parks the next FetchBase inside the transport, which is how a
// test holds the publication lane in a real external boundary. entered closes
// once the fetch has arrived; release lets it proceed, and canceling the
// fetch's context ends it too, the way a shutdown does.
func (tr *Transport) BlockNextFetch() (entered <-chan struct{}, release func()) {
	arrived := make(chan struct{})
	proceed := make(chan struct{})
	tr.mu.Lock()
	tr.fetchEntered, tr.fetchRelease = arrived, proceed
	tr.mu.Unlock()
	return arrived, sync.OnceFunc(func() { close(proceed) })
}

func (tr *Transport) FetchBase(
	ctx context.Context,
	repo, baseRef, baseSHA, dir string,
) (engine.PublicationCheckout, error) {
	tr.mu.Lock()
	tr.fetches++
	fetchErr := tr.fetchErr
	symlinkTarget := tr.SymlinkTarget
	replaceParent := tr.ReplaceParent
	// One-shot: taking the channels under the lock keeps a second fetch from
	// closing an already-closed entered channel.
	entered, release := tr.fetchEntered, tr.fetchRelease
	tr.fetchEntered, tr.fetchRelease = nil, nil
	tr.mu.Unlock()
	if release != nil {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if fetchErr != nil {
		return nil, fetchErr
	}
	if repo != tr.Forge.Repo() || baseRef != tr.BaseRef {
		return nil, errors.New("unexpected repository binding")
	}
	if replaceParent != "" {
		parent := filepath.Dir(dir)
		if err := os.Rename(parent, filepath.Join(replaceParent, "original")); err != nil {
			return nil, err
		}
		if err := os.Mkdir(parent, 0o700); err != nil {
			return nil, err
		}
	}
	if symlinkTarget != "" {
		if err := os.Symlink(symlinkTarget, dir); err != nil {
			return nil, err
		}
	} else {
		args := []string{"clone", "-q", "--no-hardlinks"}
		if tr.NoCheckout {
			args = append(args, "--no-checkout")
		}
		if _, err := mustGit(tr.BaseDir, append(args, ".", dir)...); err != nil {
			return nil, err
		}
		got, err := mustGit(dir, "rev-parse", "HEAD")
		if err != nil {
			return nil, err
		}
		if got != baseSHA {
			// The branch advanced past the exact base, as the real transport
			// allows: the base must still be on the fetched branch.
			if _, code, err := Git(dir, "merge-base", "--is-ancestor", baseSHA, "HEAD"); err != nil || code != 0 {
				return nil, fmt.Errorf("cloned base %s, want %s", got, baseSHA)
			}
			if tr.NoCheckout {
				_, err = mustGit(dir, "update-ref", "--no-deref", "HEAD", baseSHA)
			} else {
				_, err = mustGit(dir, "checkout", "-q", "--detach", baseSHA)
			}
			if err != nil {
				return nil, err
			}
		}
	}
	checkoutDir := dir
	if tr.ReturnDir != "" {
		checkoutDir = tr.ReturnDir
	}
	dirCalls := 0
	return checkout{
		dir: checkoutDir, repo: repo, baseRef: baseRef, baseSHA: baseSHA, owner: tr,
		laterDir: tr.LaterReturnDir, dirCalls: &dirCalls,
	}, nil
}

func (tr *Transport) FailFetch(err error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.fetchErr = err
}

func (tr *Transport) FetchCount() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.fetches
}

func (tr *Transport) PushHead(
	_ context.Context,
	publication engine.PublicationCheckout,
	gated publish.GatedHead,
) (publish.PushResult, error) {
	sealed, ok := publication.(checkout)
	if !ok || sealed.owner != tr {
		return publish.PushResult{}, engine.ErrForeignPublicationCheckout
	}
	// The real Git transport pushes from the checkout working tree, so it reads
	// checkout.Dir(); model that dependency here. A pre-publication review that
	// consumed (moved and deleted) the publication checkout would leave this
	// path missing, which the real transport would fail on but a double that
	// never touched Dir() would silently mask (issue #527 checkout-vs-workspace
	// seam).
	checkoutDir := publication.Dir()
	if checkoutDir != "" {
		if _, err := os.Stat(checkoutDir); err != nil {
			return publish.PushResult{}, fmt.Errorf(
				"publication checkout unavailable for push: %w", err)
		}
	}
	// Branch and head come off the gate capability, as they do in the real
	// transport: a double that took them from separately supplied input
	// could stand in for a push the Publisher never cleared.
	identity := gated.Identity()
	message, err := mustGit(sealed.dir, "show", "-s", "--format=%B", gated.SourceHeadSHA())
	if err != nil {
		return publish.PushResult{}, err
	}
	message = strings.TrimSpace(message)
	tr.mu.Lock()
	tr.pushes++
	tr.lastMessage = message
	fail := tr.fail
	tr.fail = false
	injectConflict := tr.conflict
	tr.conflict = false
	tr.mu.Unlock()
	if fail {
		return publish.PushResult{}, &publish.TransportGitError{
			Args: []string{"push"}, ExitCode: -1,
			Refusal: publish.RefusalUnknown, Err: context.DeadlineExceeded,
		}
	}
	if injectConflict {
		tr.Forge.SetRef(identity.BranchName(), strings.Repeat("9", 40))
		return publish.PushResult{}, fmt.Errorf(
			"injected publication ref conflict: %w", publish.ErrPublicationConflict,
		)
	}
	refConflict := tr.Forge.SetRef(identity.BranchName(), gated.SourceHeadSHA())
	if refConflict {
		return publish.PushResult{}, fmt.Errorf(
			"publication ref moved: %w", publish.ErrPublicationConflict,
		)
	}
	if err := tr.PublishHead(sealed.dir, identity.BranchName(), gated.SourceHeadSHA()); err != nil {
		return publish.PushResult{}, err
	}
	return publish.PushResult{Created: !refConflict}, nil
}

func (tr *Transport) PushCount() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.pushes
}

func (tr *Transport) PushedCommitMessage() string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.lastMessage
}

func (tr *Transport) FailNextPush() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.fail = true
}

func (tr *Transport) ConflictNextPush() {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.conflict = true
}

// UpdateHead moves a pull request's branch the way a successor push does,
// accepting a repeat of the update it already applied.
func (tr *Transport) UpdateHead(_ context.Context, publication engine.PublicationCheckout, update publish.GatedUpdate) (publish.PushResult, error) {
	sealed, ok := publication.(checkout)
	if !ok || sealed.owner != tr || update.ExpectedOldHead() == "" || update.SourceHeadSHA() == "" {
		return publish.PushResult{}, engine.ErrForeignPublicationCheckout
	}
	tr.Forge.Mu.Lock()
	defer tr.Forge.Mu.Unlock()
	old := tr.Forge.Refs[update.Branch()]
	if old != update.ExpectedOldHead() && old != update.SourceHeadSHA() {
		return publish.PushResult{}, publish.ErrPublicationConflict
	}
	tr.Forge.Refs[update.Branch()] = update.SourceHeadSHA()
	for i := range tr.Forge.PRs {
		if tr.Forge.PRs[i].HeadRef == update.Branch() {
			tr.Forge.PRs[i].HeadSHA = update.SourceHeadSHA()
		}
	}
	if err := tr.PublishHead(sealed.dir, update.Branch(), update.SourceHeadSHA()); err != nil {
		return publish.PushResult{}, err
	}
	if tr.SuccessorUpdateFailure != nil {
		err := tr.SuccessorUpdateFailure
		tr.SuccessorUpdateFailure = nil
		return publish.PushResult{}, err
	}
	return publish.PushResult{}, nil
}

// HeadRepo is the remote side of the pull request branches. PushHead only
// records a ref on the forge double, so a branch's commits live here, where
// FetchHead can fetch them the way the real transport fetches a branch.
func (tr *Transport) HeadRepo() (string, error) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	if !tr.headsReady {
		if _, err := mustGit(tr.HeadsDir, "init", "-q", "--bare"); err != nil {
			return "", err
		}
		// GitHub serves a commit a pull request once held by SHA after a
		// force-push; a bare repository does so only when told to.
		if _, err := mustGit(tr.HeadsDir, "config", "uploadpack.allowAnySHA1InWant", "true"); err != nil {
			return "", err
		}
		tr.headsReady = true
	}
	return tr.HeadsDir, nil
}

// PublishHead makes sha the tip of branch on the remote side, from a
// repository that holds the commit.
func (tr *Transport) PublishHead(dir, branch, sha string) error {
	heads, err := tr.HeadRepo()
	if err != nil {
		return err
	}
	_, err = mustGit(dir, "push", "-q", "--force", heads, sha+":refs/heads/"+branch)
	return err
}

// PushBranchHead models someone pushing sha to the pull request's branch:
// the remote branch, the forge's ref, and the pull request's head all move.
func (tr *Transport) PushBranchHead(dir, branch, sha string) error {
	if err := tr.PublishHead(dir, branch, sha); err != nil {
		return err
	}
	tr.Forge.Mu.Lock()
	defer tr.Forge.Mu.Unlock()
	tr.Forge.Refs[branch] = sha
	for i := range tr.Forge.PRs {
		if tr.Forge.PRs[i].HeadRef == branch {
			tr.Forge.PRs[i].HeadSHA = sha
		}
	}
	return nil
}

// LandPullHead models a lander that rewrote the pull request: it
// force-pushes sha to the branch, and the forge's pull ref follows.
func (tr *Transport) LandPullHead(dir, branch string, number int, sha string) error {
	if err := tr.PushBranchHead(dir, branch, sha); err != nil {
		return err
	}
	heads, err := tr.HeadRepo()
	if err != nil {
		return err
	}
	_, err = mustGit(dir, "push", "-q", "--force", heads, sha+":refs/pull/"+strconv.Itoa(number)+"/head")
	return err
}

// DeleteBranch models a lander deleting the branch after it merges. The
// pull ref outlives it.
func (tr *Transport) DeleteBranch(branch string) error {
	heads, err := tr.HeadRepo()
	if err != nil {
		return err
	}
	if _, err := mustGit(heads, "update-ref", "-d", "refs/heads/"+branch); err != nil {
		return err
	}
	tr.Forge.Mu.Lock()
	defer tr.Forge.Mu.Unlock()
	delete(tr.Forge.Refs, branch)
	return nil
}

// ScriptRebaseProof sets the verdict later ProveRebase calls return. The
// publish package proves the verdict itself against real rewrites; the
// cycle tests need only its outcome. Unscripted, ProveRebase fails as an
// unprovable transport failure would.
func (tr *Transport) ScriptRebaseProof(verdict publish.RebaseProof) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.rebaseVerdict = &verdict
}

func (tr *Transport) ProofCounts() (pullFetches, proofs int) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.pullFetches, tr.proofs
}

// FetchPullHead fetches the pull ref and the reviewed head by SHA, as the
// real transport does, and requires headSHA on the pull ref.
func (tr *Transport) FetchPullHead(
	ctx context.Context, publication engine.PublicationCheckout, prNumber int, headSHA, reviewedHeadSHA string,
) error {
	sealed, ok := publication.(checkout)
	if !ok || sealed.owner != tr {
		return engine.ErrForeignPublicationCheckout
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	tr.mu.Lock()
	tr.pullFetches++
	tr.mu.Unlock()
	heads, err := tr.HeadRepo()
	if err != nil {
		return err
	}
	const ref = "refs/freeside/pull-head"
	if _, code, err := Git(sealed.dir, "fetch", "-q", "--no-tags", heads,
		"+refs/pull/"+strconv.Itoa(prNumber)+"/head:"+ref); err != nil || code != 0 {
		return fmt.Errorf("remote has no pull ref %d: %w", prNumber, errors.Join(err, publish.ErrRemoteMissingHead))
	}
	if _, code, err := Git(sealed.dir, "merge-base", "--is-ancestor", headSHA, ref); err != nil || code != 0 {
		return fmt.Errorf("head %s is not on pull ref %d: %w", headSHA, prNumber,
			errors.Join(err, publish.ErrRemoteMissingHead))
	}
	if _, code, err := Git(sealed.dir, "fetch", "-q", "--no-tags", heads,
		"+"+reviewedHeadSHA+":refs/freeside/reviewed-head"); err != nil || code != 0 {
		return fmt.Errorf("fetch reviewed head %s: %w", reviewedHeadSHA,
			errors.Join(err, publish.ErrReviewedHeadUnavailable))
	}
	return nil
}

// ProveRebase returns the scripted verdict once both heads are in the
// checkout.
func (tr *Transport) ProveRebase(
	ctx context.Context, publication engine.PublicationCheckout, reviewedBase, reviewedHead, newHead string,
) (publish.RebaseProof, error) {
	sealed, ok := publication.(checkout)
	if !ok || sealed.owner != tr {
		return publish.RebaseProof{}, engine.ErrForeignPublicationCheckout
	}
	if err := ctx.Err(); err != nil {
		return publish.RebaseProof{}, err
	}
	for _, sha := range []string{reviewedBase, reviewedHead, newHead} {
		if _, code, err := Git(sealed.dir, "cat-file", "-e", sha+"^{commit}"); err != nil || code != 0 {
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

// OnHeadFetch runs hook before each later FetchHead, with the call's
// one-based count, so a test can move a branch between two fetches of one
// cycle.
func (tr *Transport) OnHeadFetch(hook func(call int)) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.headFetchHook, tr.headFetches = hook, 0
}

// RebuildMergesAs makes later prospective merges of the same pair a
// different commit, which a real transport's fixed message never does.
func (tr *Transport) RebuildMergesAs(message string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.mergeMessage = message
}

func (tr *Transport) MergeCount() int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return tr.merges
}

func (tr *Transport) FetchHead(
	ctx context.Context, publication engine.PublicationCheckout, branch, headSHA string,
) (publish.HeadFetch, error) {
	sealed, ok := publication.(checkout)
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
	heads, err := tr.HeadRepo()
	if err != nil {
		return publish.HeadFetch{}, err
	}
	const ref = "refs/freeside/head"
	if _, code, err := Git(sealed.dir, "fetch", "-q", "--no-tags", heads,
		"+refs/heads/"+branch+":"+ref); err != nil || code != 0 {
		return publish.HeadFetch{}, fmt.Errorf(
			"remote has no branch %s: %w", branch, errors.Join(err, publish.ErrRemoteMissingHead))
	}
	tip, err := mustGit(sealed.dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return publish.HeadFetch{}, err
	}
	if _, code, err := Git(sealed.dir, "cat-file", "-e", headSHA+"^{commit}"); err != nil || code != 0 {
		return publish.HeadFetch{}, fmt.Errorf(
			"head %s is not on remote branch %s: %w", headSHA, branch,
			errors.Join(err, publish.ErrRemoteMissingHead))
	}
	if _, code, err := Git(sealed.dir, "merge-base", "--is-ancestor", headSHA, tip); err != nil || code != 0 {
		return publish.HeadFetch{}, fmt.Errorf(
			"head %s is not reachable from remote branch %s: %w", headSHA, branch,
			errors.Join(err, publish.ErrRemoteMissingHead))
	}
	_, code, err := Git(sealed.dir, "merge-base", "--is-ancestor", sealed.baseSHA, headSHA)
	if err != nil {
		return publish.HeadFetch{}, err
	}
	return publish.HeadFetch{TipSHA: tip, DescendsFromBase: code == 0}, nil
}

func (tr *Transport) ProspectiveMerge(
	ctx context.Context, publication engine.PublicationCheckout, headSHA string,
) (string, error) {
	sealed, ok := publication.(checkout)
	if !ok || sealed.owner != tr {
		return "", engine.ErrForeignPublicationCheckout
	}
	return tr.prospectiveMerge(ctx, sealed, sealed.baseSHA, headSHA)
}

// ProspectiveMergeOnto merges into a base the checkout's branch advanced to,
// as the real transport does for a re-entry before publication.
func (tr *Transport) ProspectiveMergeOnto(
	ctx context.Context, publication engine.PublicationCheckout, baseSHA, headSHA string,
) (string, error) {
	sealed, ok := publication.(checkout)
	if !ok || sealed.owner != tr {
		return "", engine.ErrForeignPublicationCheckout
	}
	if _, code, err := Git(sealed.dir, "merge-base", "--is-ancestor", baseSHA, "refs/remotes/origin/"+tr.BaseRef); err != nil || code != 0 {
		return "", fmt.Errorf("base %s is not on the fetched branch: %w",
			baseSHA, errors.Join(err, publish.ErrRemoteMissingBase))
	}
	return tr.prospectiveMerge(ctx, sealed, baseSHA, headSHA)
}

func (tr *Transport) prospectiveMerge(
	ctx context.Context, sealed checkout, baseSHA, headSHA string,
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
	out, code, err := Git(sealed.dir, "merge-tree", "--write-tree", "--name-only",
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
	merge, code, err := Git(sealed.dir, "commit-tree", lines[0],
		"-p", baseSHA, "-p", headSHA, "-m", message)
	if err != nil || code != 0 {
		return "", fmt.Errorf("commit-tree exited %d: %w", code, err)
	}
	return merge, nil
}
