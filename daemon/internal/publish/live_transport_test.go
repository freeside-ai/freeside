package publish_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// liveGit runs plain git for live-test fixture work (resolving the
// public base tip, authoring the throwaway candidate commit); the
// hardened transport under test never builds fixtures for itself.
func liveGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{
		"-c", "user.name=freeside-live-test",
		"-c", "user.email=live-test@freeside.invalid",
		"-c", "commit.gpgsign=false",
	}
	cmd := exec.Command("git", append(base, args...)...) //nolint:gosec // G204: test-authored fixture arguments
	cmd.Env = scrubbedLiveGitEnv()
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// scrubbedLiveGitEnv drops every GIT_* entry from the process
// environment, the external-test-package copy of the internal tests'
// scrubbedGitEnv: fixture git must never inherit ambient git state
// (a rebase --exec exports GIT_DIR, retargeting fixture commands at
// this repository itself).
func scrubbedLiveGitEnv() []string {
	var env []string
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			env = append(env, e)
		}
	}
	return env
}

// TestLiveTransportFetchPush is issue #277 acceptance 8 against the
// real GitHub: materialize a checkout of the live repo at its current
// base tip, author an empty daemon commit on it, push it to a per-run
// nonce branch, prove the re-push converges with no new remote
// effect, and clean the ref up. The base tip is resolved with an
// unauthenticated ls-remote, so the live repo must be public (the
// designated live fixture, freeasinbird/gh-imgup, is).
func TestLiveTransportFetchPush(t *testing.T) {
	m, repo, _ := newLiveMinter(t)
	baseRef := os.Getenv("FREESIDE_PUBLISH_LIVE_BASE")
	if baseRef == "" {
		baseRef = "main"
	}
	ctx := context.Background()
	ts := publish.NewCachedTokenSource(m, time.Now)
	tr, err := publish.NewTransport(ts, publish.TransportOptions{})
	if err != nil {
		t.Fatal(err)
	}

	remoteHead := liveGit(t, "", "ls-remote", "https://github.com/"+repo+".git", "refs/heads/"+baseRef)
	baseSHA, _, ok := strings.Cut(remoteHead, "\t")
	if !ok || len(baseSHA) != 40 {
		t.Fatalf("could not resolve live base tip from %q", remoteHead)
	}

	co, err := tr.FetchBase(ctx, repo, baseRef, baseSHA, filepath.Join(t.TempDir(), "checkout"))
	if err != nil {
		t.Fatalf("live FetchBase: %v", err)
	}
	// The canonical-identity stamp against the real trusted binding: the
	// checkout carries the live repository's numeric ID in the exact
	// canonical form the re-gates (and ward's seeding gate) parse back.
	if co.RepositoryID() <= 0 {
		t.Fatalf("live checkout repository id = %d", co.RepositoryID())
	}
	stamp, err := os.ReadFile(filepath.Join(co.Dir(), ".git", "freeside-repository-id")) //nolint:gosec // G703: checkout under this test's TempDir
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("%d\n", co.RepositoryID()); string(stamp) != want {
		t.Errorf("live repository id stamp = %q, want %q", stamp, want)
	}

	// An explicit per-run nonce in the commit message makes the head
	// SHA (and therefore the derived branch) unique even when two runs
	// author from the same base within one second, so concurrent runs'
	// deferred cleanups can never race on a shared branch.
	nonce := fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	tree := liveGit(t, co.Dir(), "rev-parse", co.BaseSHA()+"^{tree}")
	head := liveGit(t, co.Dir(), "commit-tree", tree, "-p", co.BaseSHA(), "-m", "freeside live transport check "+nonce+" (no content change)")
	in := publish.IdentityInput{
		Repo:            repo,
		BaseRef:         baseRef,
		SourceHeadSHA:   head,
		ArtifactDigests: []domain.Digest{domain.Digest("sha256:" + strings.Repeat("ab", 32))},
	}
	id, err := publish.DeriveIdentity(in)
	if err != nil {
		t.Fatal(err)
	}
	branch := id.BranchName()
	// This test drives the transport directly rather than through a
	// Publisher, so it stands in for the one production mint site.
	gated := publish.GateHeadForTest(t, tr, in)

	client := &http.Client{Timeout: 30 * time.Second}

	res, err := tr.PushHead(ctx, co, gated)
	if err != nil {
		t.Fatalf("live PushHead: %v", err)
	}
	if !res.Created {
		// Terminating, not t.Error: Created=false means the branch was
		// already there, so this run does not own it and must not fall
		// through to arm the deletion below.
		t.Fatal("first live push reported Created=false; this run did not create the branch")
	}
	// Cleanup is armed only once this run has actually created the
	// branch, and deletes only a ref still at the head this run
	// pushed: a live test must never remove a ref it did not create.
	defer cleanupLiveBranch(t, client, "https://api.github.com", ts, repo, branch, head)
	again, err := tr.PushHead(ctx, co, gated)
	if err != nil {
		t.Fatalf("live re-push: %v", err)
	}
	if again.Created {
		t.Error("live re-push of the identical head reported Created=true")
	}
	confirm := liveGit(t, "", "ls-remote", "https://github.com/"+repo+".git", "refs/heads/"+branch)
	if got, _, _ := strings.Cut(confirm, "\t"); got != head {
		t.Errorf("live branch = %q, want %s", got, head)
	}
}

// cleanupLiveBranch deletes the nonce branch the live transport test
// created, and only while it still points at wantHead. Deleting by
// name alone would remove a foreign branch that happened to occupy
// the derived name; the read-then-delete narrows that to the window
// between the two calls, which no other actor has reason to enter for
// a per-run nonce ref.
func cleanupLiveBranch(t *testing.T, client *http.Client, baseURL string, ts publish.TokenSource, repo, branch, wantHead string) {
	t.Helper()
	tok, err := ts.Token(context.Background(), repo)
	if err != nil {
		t.Logf("cleanup: token: %v", err)
		return
	}
	auth := "Bearer " + tok.Token.Reveal()
	body, err := doLiveCleanupRequest(context.Background(), client, http.MethodGet,
		fmt.Sprintf("%s/repos/%s/git/ref/heads/%s", baseURL, repo, branch),
		nil, auth, http.StatusOK, http.StatusNotFound)
	if err != nil {
		t.Logf("cleanup: read branch %s: %v", branch, err)
		return
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		t.Logf("cleanup: decode branch %s: %v", branch, err)
		return
	}
	if ref.Object.SHA != wantHead {
		t.Logf("cleanup: branch %s is at %s, not this run's head; leaving it alone", branch, ref.Object.SHA)
		return
	}
	if _, err := doLiveCleanupRequest(context.Background(), client, http.MethodDelete,
		fmt.Sprintf("%s/repos/%s/git/refs/heads/%s", baseURL, repo, branch),
		nil, auth, http.StatusNoContent, http.StatusNotFound); err != nil {
		t.Logf("cleanup: delete branch %s: %v", branch, err)
	}
}
