package engine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestDeriveDiffStatsCountsTextAndBinaryChanges(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	runRemediationGit(t, repo, nil, "init", "-q", "-b", "main", "--object-format=sha1")
	if err := os.WriteFile(filepath.Join(repo, "changed.txt"), []byte("one\ntwo\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "removed.txt"), []byte("gone\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runRemediationGit(t, repo, nil, "add", "-A")
	runRemediationGit(t, repo, nil, "commit", "-q", "-m", "base")
	baseSHA := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))

	if err := os.WriteFile(filepath.Join(repo, "changed.txt"), []byte("one\nthree\nfour\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "binary.bin"), []byte{0, 1, 2}, 0o600); err != nil {
		t.Fatal(err)
	}
	runRemediationGit(t, repo, nil, "add", "-A")
	runRemediationGit(t, repo, nil, "commit", "-q", "-m", "head")
	headSHA := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))

	got, err := deriveDiffStats(t.Context(), t.TempDir(), repo, baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	want := &domain.DiffStats{
		FilesChanged: 3, Additions: 2, Deletions: 2,
		BaseSHA: baseSHA, HeadSHA: headSHA,
	}
	if got == nil || *got != *want {
		t.Fatalf("diff stats = %#v, want %#v", got, want)
	}
}

// TestDeriveDiffStatsFromPatchRebuildsAnAbsentCommit pins the review-round
// case: the checkout holds the base and the new head, never the previous head,
// and the stored base-to-previous patch is enough to count from it.
func TestDeriveDiffStatsFromPatchRebuildsAnAbsentCommit(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	runRemediationGit(t, source, nil, "init", "-q", "-b", "main", "--object-format=sha1")
	write := func(repo, name string, body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(repo, message string) string {
		t.Helper()
		runRemediationGit(t, repo, nil, "add", "-A")
		runRemediationGit(t, repo, nil, "commit", "-q", "-m", message)
		return strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))
	}
	write(source, "kept.txt", []byte("one\ntwo\n"))
	write(source, "removed.txt", []byte("gone\n"))
	baseSHA := commit(source, "base")
	runRemediationGit(t, source, nil, "branch", "base")

	write(source, "kept.txt", []byte("one\nprevious\n"))
	write(source, "binary.bin", []byte{0, 1, 2})
	if err := os.Remove(filepath.Join(source, "removed.txt")); err != nil {
		t.Fatal(err)
	}
	previousSHA := commit(source, "previous")
	previousTree := strings.TrimSpace(string(
		runRemediationGit(t, source, nil, "rev-parse", previousSHA+"^{tree}")))
	patch, err := remediationCandidatePatch(t.Context(), t.TempDir(), source, baseSHA, previousSHA)
	if err != nil {
		t.Fatal(err)
	}

	checkout := t.TempDir()
	runRemediationGit(t, checkout, nil, "clone", "-q", "--no-local", "--single-branch", "--branch", "base", source, ".")
	write(checkout, "kept.txt", []byte("one\nprevious\nnext\nlast\n"))
	headSHA := commit(checkout, "head")
	objects := func() string {
		t.Helper()
		return string(runRemediationGit(t, checkout, nil, "count-objects", "-v"))
	}
	before := objects()

	if _, err := deriveDiffStats(t.Context(), t.TempDir(), checkout, previousSHA, headSHA); err == nil {
		t.Fatal("the checkout holds the previous head, so the test proves nothing")
	}
	got, err := deriveDiffStatsFromPatch(
		t.Context(), t.TempDir(), checkout, baseSHA, patch, previousTree, previousSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	// From the previous tree: kept.txt gains two lines, binary.bin goes away,
	// and removed.txt comes back.
	want := &domain.DiffStats{
		FilesChanged: 3, Additions: 3, BaseSHA: previousSHA, HeadSHA: headSHA,
	}
	if *got != *want {
		t.Fatalf("diff stats = %#v, want %#v", got, want)
	}
	if after := objects(); after != before {
		t.Fatalf("rebuild wrote to the checkout's object store:\n%s\nwas:\n%s", after, before)
	}

	// A work directory may hold the characters git gives meaning to in its
	// alternates list: the separator, the quote, and the quote's escape.
	awkward := filepath.Join(t.TempDir(), `with:colon "quote" back\slash`)
	if err := os.Mkdir(awkward, 0o700); err != nil {
		t.Fatal(err)
	}
	runRemediationGit(t, awkward, nil, "clone", "-q", "--no-local", checkout, ".")
	fromAwkward, err := deriveDiffStatsFromPatch(
		t.Context(), t.TempDir(), awkward, baseSHA, patch, previousTree, previousSHA, headSHA)
	if err != nil || *fromAwkward != *want {
		t.Fatalf("checkout under %q = %#v, %v, want %#v", awkward, fromAwkward, err, want)
	}

	baseTree := strings.TrimSpace(string(
		runRemediationGit(t, source, nil, "rev-parse", baseSHA+"^{tree}")))
	if _, err := deriveDiffStatsFromPatch(
		t.Context(), t.TempDir(), checkout, baseSHA, patch, baseTree, previousSHA, headSHA,
	); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("a patch that rebuilds another tree = %v, want parent key mismatch", err)
	}
	unchanged, err := deriveDiffStatsFromPatch(
		t.Context(), t.TempDir(), checkout, baseSHA, nil, baseTree, baseSHA, headSHA)
	if err != nil || unchanged.Additions != 3 || unchanged.Deletions != 1 || unchanged.FilesChanged != 1 {
		t.Fatalf("empty patch = %#v, %v", unchanged, err)
	}
}
