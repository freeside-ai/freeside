package publish

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// commitOn writes content to path on the fixture work tree's branch
// (created from the current HEAD when absent), pushes it, and returns
// the new commit.
func (r *localRemote) commitOn(t *testing.T, branch, path, content string) string {
	t.Helper()
	if gitOut(t, r.work, "branch", "--list", branch) == "" {
		gitOut(t, r.work, "checkout", "-b", branch)
	} else {
		gitOut(t, r.work, "checkout", branch)
	}
	if err := os.WriteFile(filepath.Join(r.work, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, r.work, "add", path)
	gitOut(t, r.work, "commit", "-m", branch+": "+path)
	gitOut(t, r.work, "push", r.bare, branch+":"+branch)
	return gitOut(t, r.work, "rev-parse", "HEAD")
}

func (r *localRemote) fetchBase(t *testing.T, baseSHA string) Checkout {
	t.Helper()
	co, err := r.transport.FetchBase(t.Context(), r.repo, "main", baseSHA, checkoutDir(t))
	if err != nil {
		t.Fatalf("FetchBase: %v", err)
	}
	return co
}

func TestFetchHeadProvesHeadOnBranch(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	head := remote.commitOn(t, "feat/pr", "pr.txt", "pr\n")
	co := remote.fetchBase(t, remote.baseSHA)

	got, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", head)
	if err != nil {
		t.Fatalf("FetchHead: %v", err)
	}
	if want := (HeadFetch{TipSHA: head, DescendsFromBase: true}); got != want {
		t.Errorf("FetchHead = %+v, want %+v", got, want)
	}
	if ref := gitOut(t, co.Dir(), "rev-parse", transportHeadRef); ref != head {
		t.Errorf("head ref = %s, want %s", ref, head)
	}
	// The checkout keeps the shape every later re-gate expects.
	if at := gitOut(t, co.Dir(), "rev-parse", "HEAD"); at != remote.baseSHA {
		t.Errorf("HEAD = %s, want the base %s", at, remote.baseSHA)
	}
}

// TestFetchHeadReportsTipPastTheHead pins the two answers the caller
// needs apart: the requested commit is on the branch, and the branch
// has moved past it. The caller evaluates the commit it named, never
// the tip.
func TestFetchHeadReportsTipPastTheHead(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	head := remote.commitOn(t, "feat/pr", "pr.txt", "pr\n")
	tip := remote.commitOn(t, "feat/pr", "pr.txt", "pr again\n")
	co := remote.fetchBase(t, remote.baseSHA)

	got, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", head)
	if err != nil {
		t.Fatalf("FetchHead: %v", err)
	}
	if want := (HeadFetch{TipSHA: tip, DescendsFromBase: true}); got != want {
		t.Errorf("FetchHead = %+v, want %+v", got, want)
	}
}

// TestFetchHeadReportsHeadBuiltOnAnOlderBase is the base-advance shape:
// the head is on its branch but does not descend from the checkout's
// newer base. That is reported, not refused; the caller decides.
func TestFetchHeadReportsHeadBuiltOnAnOlderBase(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	head := remote.commitOn(t, "feat/pr", "pr.txt", "pr\n")
	advanced := remote.commitOn(t, "main", "main.txt", "main\n")
	co := remote.fetchBase(t, advanced)

	got, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", head)
	if err != nil {
		t.Fatalf("FetchHead: %v", err)
	}
	if want := (HeadFetch{TipSHA: head, DescendsFromBase: false}); got != want {
		t.Errorf("FetchHead = %+v, want %+v", got, want)
	}
}

func TestFetchHeadRefusesHeadTheBranchDoesNotContain(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	remote.commitOn(t, "feat/pr", "pr.txt", "pr\n")
	gitOut(t, remote.work, "checkout", "main")
	side := remote.commitOn(t, "side", "side.txt", "side\n")
	advanced := remote.commitOn(t, "main", "main.txt", "main\n")
	co := remote.fetchBase(t, advanced)

	for name, head := range map[string]string{
		// Present in the checkout through the base branch's history, so
		// only the reachability proof can refuse it.
		"on the base branch only":   advanced,
		"on a branch never fetched": side,
		"absent from the remote":    strings.Repeat("deadbeef", 5),
	} {
		if _, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", head); !errors.Is(err, ErrRemoteMissingHead) {
			t.Errorf("%s: error = %v, want ErrRemoteMissingHead", name, err)
		}
	}
}

func TestFetchHeadRefusesMissingBranch(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	co := remote.fetchBase(t, remote.baseSHA)
	if _, err := remote.transport.FetchHead(t.Context(), co, "feat/absent", remote.baseSHA); !errors.Is(err, ErrRemoteMissingHead) {
		t.Errorf("error = %v, want ErrRemoteMissingHead", err)
	}
}

// TestFetchHeadFollowsRewrittenBranch covers a branch force-pushed
// between two calls on one checkout: the second fetch must land (the
// refspec is forced), and the first head, whose objects the checkout
// still holds, must then be refused because the branch no longer
// contains it.
func TestFetchHeadFollowsRewrittenBranch(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	first := remote.commitOn(t, "feat/pr", "pr.txt", "pr\n")
	co := remote.fetchBase(t, remote.baseSHA)
	if _, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", first); err != nil {
		t.Fatalf("first FetchHead: %v", err)
	}

	gitOut(t, remote.work, "checkout", "feat/pr")
	gitOut(t, remote.work, "reset", "--hard", remote.baseSHA)
	if err := os.WriteFile(filepath.Join(remote.work, "pr.txt"), []byte("rewritten\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitOut(t, remote.work, "add", "pr.txt")
	gitOut(t, remote.work, "commit", "-m", "rewritten")
	rewritten := gitOut(t, remote.work, "rev-parse", "HEAD")
	gitOut(t, remote.work, "push", "--force", remote.bare, "feat/pr:feat/pr")

	got, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", rewritten)
	if err != nil {
		t.Fatalf("FetchHead after rewrite: %v", err)
	}
	if got.TipSHA != rewritten {
		t.Errorf("tip = %s, want %s", got.TipSHA, rewritten)
	}
	if _, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", first); !errors.Is(err, ErrRemoteMissingHead) {
		t.Errorf("rewritten-away head: error = %v, want ErrRemoteMissingHead", err)
	}
}

// TestFetchHeadDoesNotInferMissingHeadFromFailure mirrors the base
// fetch's rule: a fetch that fails while the branch demonstrably exists
// stays a retryable transport failure, never the definitive verdict.
func TestFetchHeadDoesNotInferMissingHeadFromFailure(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	head := remote.commitOn(t, "feat/pr", "pr.txt", "pr\n")
	co := remote.fetchBase(t, remote.baseSHA)
	objects := filepath.Join(remote.bare, "objects")
	if err := os.Chmod(objects, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(objects, 0o700) }) //nolint:gosec // G302: restoring a test fixture's own directory mode

	_, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", head)
	if err == nil {
		t.Fatal("fetch from an unreadable remote succeeded")
	}
	if errors.Is(err, ErrRemoteMissingHead) {
		t.Errorf("a transport failure was reported as a definitive missing head: %v", err)
	}
}

// failingGit writes a git wrapper that exits with status (after printing
// stdout) when any argument matches the shell pattern, and runs the real
// git otherwise. It stands in for a git process that dies mid-call.
func failingGit(t *testing.T, pattern, stdout string, status int) string {
	t.Helper()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nfor a in \"$@\"; do case \"$a\" in " + pattern +
		") printf '" + stdout + "'; exit " + strconv.Itoa(status) + ";; esac; done\nexec '" + real + "' \"$@\"\n"
	path := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil { //nolint:gosec // G306: an executable test fixture
		t.Fatal(err)
	}
	return path
}

// TestFetchHeadDoesNotInferVerdictFromLocalFailure pins that the two
// local observations behind ErrRemoteMissingHead read an exit status,
// not a failure: a git process that dies while answering either one
// leaves a transport failure the drain retries, never a definitive
// verdict about a head the branch does hold.
func TestFetchHeadDoesNotInferVerdictFromLocalFailure(t *testing.T) {
	t.Parallel()
	for name, pattern := range map[string]string{
		"presence check": "*^{commit}",
		"ancestry check": "merge-base",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			remote := newLocalRemote(t)
			head := remote.commitOn(t, "feat/pr", "pr.txt", "pr\n")
			co := remote.fetchBase(t, remote.baseSHA)
			if name == "presence check" {
				pattern = head + pattern
			}
			remote.transport.gitPath = failingGit(t, pattern, "", 128)
			_, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", head)
			if err == nil {
				t.Fatal("FetchHead succeeded although git failed")
			}
			if errors.Is(err, ErrRemoteMissingHead) {
				t.Errorf("a failed git call was reported as a definitive missing head: %v", err)
			}
		})
	}
}

// TestFetchHeadRejectedCallsMintNoToken pins the token-after-gates
// ordering for the head fetch: a call refused on its arguments or on
// the checkout's own state never reaches the token source.
func TestFetchHeadRejectedCallsMintNoToken(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	head := remote.commitOn(t, "feat/pr", "pr.txt", "pr\n")
	other := newLocalRemote(t)
	counter := &countingTokenSource{}

	fresh := func() Checkout {
		remote.transport.tokens = staticTokenSource{tok: stubToken()}
		co := remote.fetchBase(t, remote.baseSHA)
		remote.transport.tokens = counter
		return co
	}
	cases := map[string]func() (Checkout, string, string){
		"invalid branch name": func() (Checkout, string, string) { return fresh(), "not a branch", head },
		"invalid head":        func() (Checkout, string, string) { return fresh(), "feat/pr", "HEAD" },
		"foreign checkout": func() (Checkout, string, string) {
			return other.fetchBase(t, other.baseSHA), "feat/pr", head
		},
		"HEAD moved off the base": func() (Checkout, string, string) {
			co := fresh()
			gitOut(t, co.Dir(), "update-ref", "--no-deref", "HEAD", candidateHead(t, co))
			return co, "feat/pr", head
		},
		"non-daemon config key": func() (Checkout, string, string) {
			co := fresh()
			gitOut(t, co.Dir(), "config", "url.file:///elsewhere.insteadOf", "file://")
			return co, "feat/pr", head
		},
		"missing repository id stamp": func() (Checkout, string, string) {
			co := fresh()
			if err := os.Remove(filepath.Join(co.Dir(), ".git", "freeside-repository-id")); err != nil {
				t.Fatal(err)
			}
			return co, "feat/pr", head
		},
		"repository id stamp for another repository": func() (Checkout, string, string) {
			co := fresh()
			stamp := strconv.FormatInt(stubRepositoryID+1, 10) + "\n"
			if err := os.WriteFile(filepath.Join(co.Dir(), ".git", "freeside-repository-id"), []byte(stamp), 0o600); err != nil {
				t.Fatal(err)
			}
			return co, "feat/pr", head
		},
		"repo binding for another name": func() (Checkout, string, string) {
			co := fresh()
			gitOut(t, co.Dir(), "config", transportRepoKey, "owner/elsewhere")
			return co, "feat/pr", head
		},
	}
	for name, build := range cases {
		co, branch, sha := build()
		if _, err := remote.transport.FetchHead(t.Context(), co, branch, sha); err == nil {
			t.Errorf("%s: FetchHead accepted the call", name)
		}
	}
	if counter.mints != 0 {
		t.Errorf("rejected calls minted %d tokens, want 0", counter.mints)
	}
}

// TestFetchHeadRefusesRebindTrustedRepositoryID is the reused-name
// scenario on the fetch side: the name now resolves to another
// repository, whose branch must not be fetched into this checkout.
func TestFetchHeadRefusesRebindTrustedRepositoryID(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	head := remote.commitOn(t, "feat/pr", "pr.txt", "pr\n")
	co := remote.fetchBase(t, remote.baseSHA)
	rebound := stubToken()
	rebound.RepositoryID = stubRepositoryID + 1
	remote.transport.tokens = staticTokenSource{tok: rebound}
	if _, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", head); !errors.Is(err, ErrGitTransport) {
		t.Fatalf("FetchHead under a rebound trusted repository id: %v", err)
	}
	if _, err := os.Stat(filepath.Join(co.Dir(), ".git", filepath.FromSlash(transportHeadRef))); !os.IsNotExist(err) {
		t.Error("a refused fetch still landed the head ref")
	}
}

// baseAdvance builds the fixture a base-advance re-entry sees: a head
// built on the first base, and a base branch that has since moved.
func baseAdvance(t *testing.T, headContent, baseContent string) (remote *localRemote, head, advanced string) {
	t.Helper()
	remote = newLocalRemote(t)
	head = remote.commitOn(t, "feat/pr", "pr.txt", headContent)
	advanced = remote.commitOn(t, "main", "main.txt", baseContent)
	return remote, head, advanced
}

func mergeFixtureCheckout(t *testing.T, remote *localRemote, head, advanced string) Checkout {
	t.Helper()
	co := remote.fetchBase(t, advanced)
	if _, err := remote.transport.FetchHead(t.Context(), co, "feat/pr", head); err != nil {
		t.Fatalf("FetchHead: %v", err)
	}
	return co
}

func TestProspectiveMergeBuildsDeterministicMerge(t *testing.T) {
	t.Parallel()
	remote, head, advanced := baseAdvance(t, "pr\n", "main\n")
	co := mergeFixtureCheckout(t, remote, head, advanced)
	refsBefore := gitOut(t, co.Dir(), "for-each-ref")

	merge, err := remote.transport.ProspectiveMerge(t.Context(), co, head)
	if err != nil {
		t.Fatalf("ProspectiveMerge: %v", err)
	}
	if got := gitOut(t, co.Dir(), "rev-list", "--parents", "-n", "1", merge); got != merge+" "+advanced+" "+head {
		t.Errorf("merge parents = %q, want base %s then head %s", got, advanced, head)
	}
	// The merged tree carries both sides' changes.
	if got := gitOut(t, co.Dir(), "ls-tree", "-r", "--name-only", merge); got != "a.txt\nmain.txt\npr.txt" {
		t.Errorf("merged tree = %q", got)
	}
	wantIdentity := "freeside-daemon <daemon@freeside.invalid> 0 +0000"
	if got := gitOut(t, co.Dir(), "show", "-s", "--format=%an <%ae> %ad|%cn <%ce> %cd|%B", "--date=raw", merge); got !=
		wantIdentity+"|"+wantIdentity+"|"+prospectiveMergeMessage {
		t.Errorf("merge identity and message = %q", got)
	}
	if refsAfter := gitOut(t, co.Dir(), "for-each-ref"); refsAfter != refsBefore {
		t.Errorf("merge moved refs:\nbefore %s\nafter  %s", refsBefore, refsAfter)
	}
	if at := gitOut(t, co.Dir(), "rev-parse", "HEAD"); at != advanced {
		t.Errorf("HEAD = %s, want the base %s", at, advanced)
	}

	// The SHA depends only on the two parents and the merge result: a
	// rebuild in the same checkout and one in a fresh checkout agree.
	again, err := remote.transport.ProspectiveMerge(t.Context(), co, head)
	if err != nil || again != merge {
		t.Errorf("rebuild = %s, %v; want %s", again, err, merge)
	}
	elsewhere, err := remote.transport.ProspectiveMerge(t.Context(), mergeFixtureCheckout(t, remote, head, advanced), head)
	if err != nil || elsewhere != merge {
		t.Errorf("rebuild in a fresh checkout = %s, %v; want %s", elsewhere, err, merge)
	}
}

func TestProspectiveMergeReportsConflictWithoutCommit(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	head := remote.commitOn(t, "feat/pr", "a.txt", "pr\n")
	advanced := remote.commitOn(t, "main", "a.txt", "main\n")
	co := mergeFixtureCheckout(t, remote, head, advanced)
	commits := func() string {
		var out []string
		for _, line := range strings.Split(gitOut(t, co.Dir(), "cat-file", "--batch-all-objects", "--batch-check"), "\n") {
			if strings.Contains(line, " commit ") {
				out = append(out, line)
			}
		}
		return strings.Join(out, "\n")
	}
	before := commits()

	merge, err := remote.transport.ProspectiveMerge(t.Context(), co, head)
	var conflict *MergeConflictError
	if !errors.As(err, &conflict) || !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("ProspectiveMerge = %q, %v; want a merge conflict", merge, err)
	}
	want := MergeConflictError{BaseSHA: advanced, HeadSHA: head, Paths: []string{"a.txt"}}
	if !reflect.DeepEqual(*conflict, want) {
		t.Errorf("conflict = %+v, want %+v", *conflict, want)
	}
	if merge != "" {
		t.Errorf("a conflicted merge returned commit %s", merge)
	}
	if after := commits(); after != before {
		t.Errorf("a conflicted merge wrote a commit:\nbefore %s\nafter  %s", before, after)
	}
	// Tree paths are candidate-controlled text and never ride the error.
	if strings.Contains(err.Error(), "a.txt") {
		t.Errorf("conflict error renders a path: %v", err)
	}
}

func TestProspectiveMergeRefusals(t *testing.T) {
	t.Parallel()
	remote, head, advanced := baseAdvance(t, "pr\n", "main\n")
	counter := &countingTokenSource{}
	other := newLocalRemote(t)

	co := remote.fetchBase(t, advanced)
	remote.transport.tokens = counter
	// The head was never fetched into this checkout.
	if _, err := remote.transport.ProspectiveMerge(t.Context(), co, head); err == nil || errors.Is(err, ErrMergeConflict) {
		t.Errorf("merge of an absent head: %v", err)
	}
	if _, err := remote.transport.ProspectiveMerge(t.Context(), co, "HEAD"); err == nil {
		t.Error("merge accepted a non-SHA head")
	}
	if _, err := remote.transport.ProspectiveMerge(t.Context(), other.fetchBase(t, other.baseSHA), other.baseSHA); err == nil {
		t.Error("merge accepted a checkout another transport materialized")
	}
	// A head equal to the base would come back as a one-parent commit.
	if merge, err := remote.transport.ProspectiveMerge(t.Context(), co, advanced); err == nil {
		t.Errorf("merge of the base into itself returned %s", merge)
	}
	gitOut(t, co.Dir(), "config", "merge.evil.driver", "true")
	if _, err := remote.transport.ProspectiveMerge(t.Context(), co, head); !errors.Is(err, ErrGitTransport) {
		t.Errorf("merge in a checkout with a configured merge driver: %v", err)
	}
	// Local only: a merge, built or refused, mints no token.
	if counter.mints != 0 {
		t.Errorf("ProspectiveMerge minted %d tokens, want 0", counter.mints)
	}
}

// TestProspectiveMergeRefusesLocalAttributes pins the one attribute
// source merge-tree reads without a working tree. A union driver named
// there would turn a real conflict into a clean merge, so the checkout
// is refused before any merge runs.
func TestProspectiveMergeRefusesLocalAttributes(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	head := remote.commitOn(t, "feat/pr", "a.txt", "pr\n")
	advanced := remote.commitOn(t, "main", "a.txt", "main\n")
	co := mergeFixtureCheckout(t, remote, head, advanced)
	attributes := filepath.Join(co.Dir(), ".git", "info", "attributes")
	if err := os.MkdirAll(filepath.Dir(attributes), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(attributes, []byte("a.txt merge=union\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	merge, err := remote.transport.ProspectiveMerge(t.Context(), co, head)
	if !errors.Is(err, ErrGitTransport) || errors.Is(err, ErrMergeConflict) {
		t.Errorf("merge under local attributes = %q, %v; want a transport refusal", merge, err)
	}
}

// TestProspectiveMergeReadsConflictFromExitStatusOne pins that only
// merge-tree's exit status 1 is a conflict verdict: another failing
// status that still printed a tree name stays a transport failure.
func TestProspectiveMergeReadsConflictFromExitStatusOne(t *testing.T) {
	t.Parallel()
	remote, head, advanced := baseAdvance(t, "pr\n", "main\n")
	co := mergeFixtureCheckout(t, remote, head, advanced)
	remote.transport.gitPath = failingGit(t, "merge-tree", strings.Repeat("0", 40)+`\0a.txt\0`, 2)
	merge, err := remote.transport.ProspectiveMerge(t.Context(), co, head)
	if err == nil || errors.Is(err, ErrMergeConflict) {
		t.Errorf("merge-tree exit 2 = %q, %v; want a failure that is not a conflict", merge, err)
	}
}

func TestParseMergeTreeListing(t *testing.T) {
	t.Parallel()
	tree := strings.Repeat("ab", 20)
	for name, tc := range map[string]struct {
		listing   string
		truncated bool
		wantPaths []string
	}{
		"clean":                {listing: tree + "\x00"},
		"conflicts":            {listing: tree + "\x00a.txt\x00dir/b c.txt\x00", wantPaths: []string{"a.txt", "dir/b c.txt"}},
		"cut inside a path":    {listing: tree + "\x00a.txt\x00dir/b", truncated: true, wantPaths: []string{"a.txt"}},
		"cut after a path":     {listing: tree + "\x00a.txt\x00", truncated: true, wantPaths: []string{"a.txt"}},
		"cut inside the first": {listing: tree + "\x00a.t", truncated: true},
	} {
		gotTree, gotPaths := parseMergeTreeListing([]byte(tc.listing), tc.truncated)
		if gotTree != tree || !reflect.DeepEqual(gotPaths, tc.wantPaths) {
			t.Errorf("%s: parse = %q, %q; want %q, %q", name, gotTree, gotPaths, tree, tc.wantPaths)
		}
	}
}

func TestTruncatingBufferKeepsAPrefixWithoutFailing(t *testing.T) {
	t.Parallel()
	b := &truncatingBuffer{remaining: 5}
	for _, chunk := range []string{"abc", "defg", "hi"} {
		if n, err := b.Write([]byte(chunk)); n != len(chunk) || err != nil {
			t.Fatalf("Write(%q) = %d, %v", chunk, n, err)
		}
	}
	if got := b.buffer.String(); got != "abcde" || !b.truncated {
		t.Errorf("buffer = %q, truncated = %v", got, b.truncated)
	}
}
