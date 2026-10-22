package publish

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// rebaseFixture is what a lander leaves behind (issue #2129): a reviewed
// head of two commits built on the first base, a base that advanced to
// reviewedBase before the last review, and a pull request whose branch
// someone else rewrote and then deleted, so only refs/pull/450/head and
// the SHA name either head.
type rebaseFixture struct {
	remote       *localRemote
	reviewedBase string
	reviewedHead string
}

func newRebaseFixture(t *testing.T) *rebaseFixture {
	t.Helper()
	remote := newLocalRemote(t)
	remote.commitOn(t, "feat/pr", "pr1.txt", "one\n")
	reviewedHead := remote.commitOn(t, "feat/pr", "pr2.txt", "two\n")
	reviewedBase := remote.commitOn(t, "main", "main.txt", "main\n")
	// GitHub serves a commit a pull request once held by SHA; a bare
	// repository does so only when told to.
	gitOut(t, remote.bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	return &rebaseFixture{remote: remote, reviewedBase: reviewedBase, reviewedHead: reviewedHead}
}

// land rewrites the pull request the way a lander does: check out the
// reviewed head, let rewrite move it, publish the result as the pull
// request's head, and delete the branch. It returns the landed head.
func (f *rebaseFixture) land(t *testing.T, rewrite func(work string)) string {
	t.Helper()
	work := f.remote.work
	gitOut(t, work, "checkout", "-B", "landing", f.reviewedHead)
	rewrite(work)
	landed := gitOut(t, work, "rev-parse", "HEAD")
	gitOut(t, work, "push", "--force", f.remote.bare, landed+":refs/pull/450/head")
	gitOut(t, work, "push", f.remote.bare, ":feat/pr")
	return landed
}

func (f *rebaseFixture) prove(t *testing.T, landed string) RebaseProof {
	t.Helper()
	co := f.remote.fetchBase(t, f.reviewedBase)
	if err := f.remote.transport.FetchPullHead(t.Context(), co, 450, landed, f.reviewedHead); err != nil {
		t.Fatalf("FetchPullHead: %v", err)
	}
	proof, err := f.remote.transport.ProveRebase(t.Context(), co, f.reviewedBase, f.reviewedHead, landed)
	if err != nil {
		t.Fatalf("ProveRebase: %v", err)
	}
	if at := gitOut(t, co.Dir(), "rev-parse", "HEAD"); at != f.reviewedBase {
		t.Errorf("HEAD = %s, want the base %s", at, f.reviewedBase)
	}
	return proof
}

func writeAndCommit(t *testing.T, work, path, content, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(work, path), []byte(content), 0o600); err != nil { //nolint:gosec // test-only fixture path
		t.Fatal(err)
	}
	gitOut(t, work, "add", path)
	gitOut(t, work, "commit", "-m", message)
}

func TestProveRebaseProvesACleanRebase(t *testing.T) {
	t.Parallel()
	f := newRebaseFixture(t)
	landed := f.land(t, func(work string) { gitOut(t, work, "rebase", f.reviewedBase) })

	proof := f.prove(t, landed)
	if !proof.Proven || proof.Detail != "" {
		t.Fatalf("clean rebase = %+v, want proven", proof)
	}
	if want := gitOut(t, f.remote.work, "rev-parse", landed+"^{tree}"); proof.TreeSHA != want {
		t.Errorf("tree = %s, want the landed tree %s", proof.TreeSHA, want)
	}
}

// TestProveRebaseRefusesRewrittenHeads covers the shapes the issue names
// beside the clean rebase. Each lands the same reviewed commits onto a
// base, so each could pass a check that looked only at descent.
func TestProveRebaseRefusesRewrittenHeads(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		rewrite func(t *testing.T, f *rebaseFixture, work string)
		detail  string
	}{
		{
			name: "content change",
			rewrite: func(t *testing.T, f *rebaseFixture, work string) {
				gitOut(t, work, "rebase", f.reviewedBase)
				if err := os.WriteFile(filepath.Join(work, "pr2.txt"), []byte("changed\n"), 0o600); err != nil { //nolint:gosec // test-only fixture path
					t.Fatal(err)
				}
				gitOut(t, work, "commit", "-a", "--amend", "--no-edit")
			},
			detail: "range-diff does not pair",
		},
		{
			name: "squash",
			rewrite: func(t *testing.T, f *rebaseFixture, work string) {
				gitOut(t, work, "rebase", f.reviewedBase)
				gitOut(t, work, "reset", "--soft", f.reviewedBase)
				gitOut(t, work, "commit", "-m", "squashed")
			},
			detail: "range-diff does not pair",
		},
		{
			name: "reworded message",
			rewrite: func(t *testing.T, f *rebaseFixture, work string) {
				gitOut(t, work, "rebase", f.reviewedBase)
				gitOut(t, work, "commit", "--amend", "-m", "feat/pr: pr2.txt, reflowed")
			},
			detail: "range-diff does not pair",
		},
		{
			name: "newer base",
			rewrite: func(t *testing.T, f *rebaseFixture, work string) {
				newer := f.remote.commitOn(t, "main", "newer.txt", "newer\n")
				gitOut(t, work, "checkout", "-B", "landing", f.reviewedHead)
				gitOut(t, work, "rebase", newer)
			},
			detail: "range-diff does not pair",
		},
		{
			name: "older base",
			rewrite: func(t *testing.T, f *rebaseFixture, work string) {
				// The reviewed commits re-recorded where they started: same
				// patches and tree history, but not on the reviewed base.
				gitOut(t, work, "commit", "--amend", "--no-edit", "--date=@1")
			},
			detail: "the head does not descend from the reviewed base",
		},
		{
			// range-diff walks without merges, so only the merge check
			// sees a no-op merge stacked on the rebased commits.
			name: "merge commit",
			rewrite: func(t *testing.T, f *rebaseFixture, work string) {
				gitOut(t, work, "rebase", f.reviewedBase)
				merge := gitOut(t, work, "commit-tree", "HEAD^{tree}", "-p", "HEAD", "-p", f.reviewedBase, "-m", "merge")
				gitOut(t, work, "reset", "-q", "--hard", merge)
			},
			detail: "the head adds a merge commit",
		},
		{
			name: "reordered commits",
			rewrite: func(t *testing.T, f *rebaseFixture, work string) {
				gitOut(t, work, "checkout", "-q", "-B", "landing", f.reviewedBase)
				gitOut(t, work, "cherry-pick", f.reviewedHead)
				gitOut(t, work, "cherry-pick", f.reviewedHead+"^")
			},
			detail: "range-diff pairs the reviewed commits out of order",
		},
		{
			name: "extra commit",
			rewrite: func(t *testing.T, f *rebaseFixture, work string) {
				gitOut(t, work, "rebase", f.reviewedBase)
				writeAndCommit(t, work, "extra.txt", "extra\n", "extra")
			},
			detail: "range-diff does not pair",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newRebaseFixture(t)
			landed := f.land(t, func(work string) { tc.rewrite(t, f, work) })
			proof := f.prove(t, landed)
			if proof.Proven {
				t.Fatalf("%s proved: %+v", tc.name, proof)
			}
			if !strings.HasPrefix(proof.Detail, tc.detail) {
				t.Errorf("detail = %q, want prefix %q", proof.Detail, tc.detail)
			}
			if proof.TreeSHA == "" {
				t.Error("a refusal after the merge was built lost the reviewed tree")
			}
		})
	}
}

// TestProveRebaseRefusesRewrittenBinaryHistory: range-diff prints "Binary
// files differ" for any binary change, so a rebase that rewrites a binary
// in one commit and restores it in the next pairs as "=" and ends at the
// reviewed tree. The per-commit blob check refuses it.
func TestProveRebaseRefusesRewrittenBinaryHistory(t *testing.T) {
	t.Parallel()
	remote := newLocalRemote(t)
	remote.commitOn(t, "feat/pr", "x.bin", "A\x00")
	reviewedHead := remote.commitOn(t, "feat/pr", "x.bin", "B\x00")
	reviewedBase := remote.commitOn(t, "main", "main.txt", "main\n")
	gitOut(t, remote.bare, "config", "uploadpack.allowAnySHA1InWant", "true")
	f := &rebaseFixture{remote: remote, reviewedBase: reviewedBase, reviewedHead: reviewedHead}

	clean := f.land(t, func(work string) { gitOut(t, work, "rebase", reviewedBase) })
	if proof := f.prove(t, clean); !proof.Proven {
		t.Fatalf("clean binary rebase = %+v, want proven", proof)
	}
	gitOut(t, remote.work, "push", remote.bare, reviewedHead+":refs/heads/feat/pr")
	forged := f.land(t, func(work string) {
		gitOut(t, work, "checkout", "-q", "-B", "landing", reviewedBase)
		gitOut(t, work, "cherry-pick", reviewedHead+"^")
		writeAndCommit(t, work, "x.bin", "Z\x00evil", "unused")
		gitOut(t, work, "reset", "-q", "--soft", "HEAD~2")
		gitOut(t, work, "commit", "-q", "-C", reviewedHead+"^")
		if err := os.WriteFile(filepath.Join(work, "x.bin"), []byte("B\x00"), 0o600); err != nil { //nolint:gosec // test-only fixture path
			t.Fatal(err)
		}
		gitOut(t, work, "commit", "-q", "-a", "-C", reviewedHead)
	})
	proof := f.prove(t, forged)
	if proof.Proven || !strings.Contains(proof.Detail, "to other content than reviewed commit") {
		t.Fatalf("forged binary history = %+v, want a blob refusal", proof)
	}
}

// TestFetchPullHeadReportsAnUnavailableReviewedHead is the remote that no
// longer serves the reviewed head. The class names the failed fetch; it
// is not a verdict, so nothing here is a refusal.
func TestFetchPullHeadReportsAnUnavailableReviewedHead(t *testing.T) {
	t.Parallel()
	f := newRebaseFixture(t)
	landed := f.land(t, func(work string) { gitOut(t, work, "rebase", f.reviewedBase) })
	// A commit the remote never held stands in for one it collected.
	gitOut(t, f.remote.work, "checkout", "-B", "local-only", f.reviewedBase)
	writeAndCommit(t, f.remote.work, "local.txt", "local\n", "never pushed")
	unpushed := gitOut(t, f.remote.work, "rev-parse", "HEAD")

	co := f.remote.fetchBase(t, f.reviewedBase)
	err := f.remote.transport.FetchPullHead(t.Context(), co, 450, landed, unpushed)
	if !errors.Is(err, ErrReviewedHeadUnavailable) {
		t.Fatalf("FetchPullHead = %v, want %v", err, ErrReviewedHeadUnavailable)
	}
}

func TestFetchPullHeadRefusesHeadsThePullRequestNeverHeld(t *testing.T) {
	t.Parallel()
	f := newRebaseFixture(t)
	landed := f.land(t, func(work string) { gitOut(t, work, "rebase", f.reviewedBase) })
	newer := f.remote.commitOn(t, "main", "newer.txt", "newer\n")
	co := f.remote.fetchBase(t, newer)

	// The base branch's tip is in the checkout but was never the pull
	// request's head.
	err := f.remote.transport.FetchPullHead(t.Context(), co, 450, newer, f.reviewedHead)
	if !errors.Is(err, ErrRemoteMissingHead) {
		t.Errorf("base tip as the pull head = %v, want %v", err, ErrRemoteMissingHead)
	}
	err = f.remote.transport.FetchPullHead(t.Context(), co, 451, landed, f.reviewedHead)
	if !errors.Is(err, ErrRemoteMissingHead) {
		t.Errorf("missing pull ref = %v, want %v", err, ErrRemoteMissingHead)
	}
	if err := f.remote.transport.FetchPullHead(t.Context(), co, 0, landed, f.reviewedHead); err == nil {
		t.Error("pull request 0 accepted")
	}
}

func TestProveRebaseRequiresTheHeadInTheCheckout(t *testing.T) {
	t.Parallel()
	f := newRebaseFixture(t)
	landed := f.land(t, func(work string) { gitOut(t, work, "rebase", f.reviewedBase) })
	co := f.remote.fetchBase(t, f.reviewedBase)
	// Without FetchPullHead neither head is in the checkout: no verdict.
	_, err := f.remote.transport.ProveRebase(t.Context(), co, f.reviewedBase, f.reviewedHead, landed)
	if err == nil {
		t.Fatal("ProveRebase without the heads reached a verdict")
	}
	if _, err := f.remote.transport.ProveRebase(
		t.Context(), co, f.reviewedBase, f.reviewedHead, f.reviewedHead,
	); err == nil {
		t.Error("proving the reviewed head against itself reached a verdict")
	}
}

func TestRangeDiffMismatch(t *testing.T) {
	t.Parallel()
	identical := "1:  1111111 = 1:  2222222 feat: one\n2:  3333333 = 2:  4444444 feat: two\n"
	if got := rangeDiffMismatch([]byte(identical)); got != "" {
		t.Errorf("identical listing = %q, want none", got)
	}
	if got := rangeDiffMismatch(nil); got != "range-diff found no commits to pair" {
		t.Errorf("empty listing = %q", got)
	}
	// The subject is branch-controlled text: it is quoted into ASCII, so a
	// terminal escape in it never reaches a reader raw.
	hostile := "1:  1111111 = 1:  2222222 one\n2:  3333333 ! 2:  4444444 \x1b[2Jtwo\n    @@ diff\n"
	got := rangeDiffMismatch([]byte(hostile))
	if want := `range-diff does not pair the reviewed commits as identical: "2:  3333333 ! 2:  4444444 \x1b[2Jtwo"`; got != want {
		t.Errorf("hostile listing = %q, want %q", got, want)
	}
	for _, line := range []string{
		"-:  ------- > 1:  2222222 extra", "1:  1111111 < -:  ------- dropped", "1:  1111111 = 2:  2222222 moved",
	} {
		if got := rangeDiffMismatch([]byte(line + "\n")); got == "" {
			t.Errorf("%q passed", line)
		}
	}
	long := truncateDetail(strings.Repeat("x", domain.WorkUnitRebaseProofDetailLimit+10))
	if len(long) != domain.WorkUnitRebaseProofDetailLimit {
		t.Errorf("truncated detail is %d bytes, want %d", len(long), domain.WorkUnitRebaseProofDetailLimit)
	}
}
