package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestReviewedDiffScopeResolvesReviewedChange proves the engine's trusted
// reviewed-diff derivation feeds diffscope the real `git diff -U0` for the bound
// base/head pair, over a genuine two-commit checkout: an overlapping location on
// a modified or added path resolves, while an unchanged line, an untouched path,
// a concrete line on a candidate-deleted path, and a nil location all fail
// closed. The whole-file location on a candidate-deleted path (the #855
// representation) is accepted, since a deleted file has no new-side line.
//
// It also exercises the header-only changes the -U0 diff omits and the
// name-status companion pass recovers: a deleted *empty* file (the reachable
// #855 trap), a mode-only change, a binary-content change, and an added empty
// file all resolve for a whole-file finding, while a concrete line on them
// still fails closed.
func TestReviewedDiffScopeResolvesReviewedChange(t *testing.T) {
	repo := t.TempDir()
	runRemediationGit(t, repo, nil, "init", "-q", "-b", "main", "--object-format=sha1")
	writeReviewedFile(t, repo, "mod.txt", "keep-1\nold-2\nkeep-3\n")
	writeReviewedFile(t, repo, "steady.txt", "unchanged\n")
	writeReviewedFile(t, repo, "del.txt", "gone-1\ngone-2\n")
	writeReviewedFile(t, repo, "empty-del.txt", "")            // empty file, deleted in head
	writeReviewedFile(t, repo, "mode.sh", "#!/bin/sh\necho\n") // non-exec, chmod +x in head
	writeReviewedFile(t, repo, "bin.dat", "\x00\x01bin\x02")   // binary, changed in head
	runRemediationGit(t, repo, nil, "add", "-A")
	runRemediationGit(t, repo, nil, "commit", "-q", "-m", "base")
	baseSHA := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))

	writeReviewedFile(t, repo, "mod.txt", "keep-1\nNEW-2\nkeep-3\n")  // change line 2 only
	writeReviewedFile(t, repo, "add.txt", "added-1\nadded-2\n")       // new file
	if err := os.Remove(filepath.Join(repo, "del.txt")); err != nil { // delete file
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(repo, "empty-del.txt")); err != nil { // delete empty file
		t.Fatal(err)
	}
	// The exec bit forces git to record a mode-only change, which the -U0 diff
	// emits with no ---/+++ header; the name-status seed must still resolve it.
	if err := os.Chmod(filepath.Join(repo, "mode.sh"), 0o755); err != nil { //nolint:gosec // G302: the exec bit is the deliberate mode change under test
		t.Fatal(err)
	}
	writeReviewedFile(t, repo, "bin.dat", "\x00\x01BIN\x02\xff\xfe") // binary content change
	writeReviewedFile(t, repo, "empty-add.txt", "")                  // add empty file
	runRemediationGit(t, repo, nil, "add", "-A")
	runRemediationGit(t, repo, nil, "commit", "-q", "-m", "head")
	headSHA := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))
	// The reviewed-diff derivation must resolve both commits from an arbitrary
	// checkout state, so leave the working tree at head (as a retained review
	// workspace is), not reset to base.

	scope, err := reviewedDiffScope(t.Context(), t.TempDir(), repo, baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}

	loc := func(path string, start, end int) *domain.FindingLocation {
		return &domain.FindingLocation{Path: path, StartLine: start, EndLine: end}
	}
	cases := []struct {
		name string
		loc  *domain.FindingLocation
		want bool
	}{
		{"modified changed line overlaps", loc("mod.txt", 2, 2), true},
		{"modified unchanged line fails closed", loc("mod.txt", 1, 1), false},
		{"added file line overlaps", loc("add.txt", 1, 2), true},
		{"deleted file whole-file overlaps", loc("del.txt", 0, 0), true},
		{"deleted file concrete line fails closed", loc("del.txt", 1, 1), false},
		{"deleted empty file whole-file overlaps", loc("empty-del.txt", 0, 0), true},
		{"deleted empty file concrete line fails closed", loc("empty-del.txt", 1, 1), false},
		{"mode-only change whole-file overlaps", loc("mode.sh", 0, 0), true},
		{"binary change whole-file overlaps", loc("bin.dat", 0, 0), true},
		{"binary change concrete line fails closed", loc("bin.dat", 1, 1), false},
		{"added empty file whole-file overlaps", loc("empty-add.txt", 0, 0), true},
		{"untouched path fails closed", loc("steady.txt", 1, 1), false},
		{"whole-file on untouched path fails closed", loc("steady.txt", 0, 0), false},
		{"nil location fails closed", nil, false},
	}
	for _, tc := range cases {
		if got := scope.Overlaps(tc.loc); got != tc.want {
			t.Errorf("%s: Overlaps = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestReviewedDiffScopeFailsOnNonRepositoryCheckout proves a diff-derivation
// failure surfaces as an error (the engine-fault / retry path in the gate),
// never as a silently empty scope that would wave every finding through.
func TestReviewedDiffScopeFailsOnNonRepositoryCheckout(t *testing.T) {
	if _, err := reviewedDiffScope(
		t.Context(), t.TempDir(), t.TempDir(),
		strings.Repeat("a", 40), strings.Repeat("b", 40),
	); err == nil {
		t.Fatal("reviewedDiffScope over a non-repository checkout returned no error")
	}
}

// TestReviewedDiffScopeRendersRenameAsDeleteAndAdd proves the derivation
// disables git rename detection: a candidate that removes a file and adds a
// similar one keeps both endpoints resolvable, so the candidate-deleted file's
// whole-file (0,0) location (the §7/#855 representation) still overlaps, as does
// a line on the added file. With rename detection left on, git re-keys the
// change under the new path, the deleted path vanishes, and a valid deleted-file
// finding would be rejected in the wrong direction.
func TestReviewedDiffScopeRendersRenameAsDeleteAndAdd(t *testing.T) {
	repo := t.TempDir()
	runRemediationGit(t, repo, nil, "init", "-q", "-b", "main", "--object-format=sha1")
	// Shared content makes git's default rename heuristic pair the removed and
	// added files; --no-renames must defeat that pairing.
	writeReviewedFile(t, repo, "renamed-src.txt", "line-a\nline-b\nline-c\nline-d\nline-e\n")
	runRemediationGit(t, repo, nil, "add", "-A")
	runRemediationGit(t, repo, nil, "commit", "-q", "-m", "base")
	baseSHA := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))

	if err := os.Remove(filepath.Join(repo, "renamed-src.txt")); err != nil {
		t.Fatal(err)
	}
	writeReviewedFile(t, repo, "renamed-dst.txt", "line-a\nline-b\nline-c\nline-d\nZZZ\n")
	runRemediationGit(t, repo, nil, "add", "-A")
	runRemediationGit(t, repo, nil, "commit", "-q", "-m", "head")
	headSHA := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))

	scope, err := reviewedDiffScope(t.Context(), t.TempDir(), repo, baseSHA, headSHA)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Overlaps(&domain.FindingLocation{Path: "renamed-src.txt", StartLine: 0, EndLine: 0}) {
		t.Error("candidate-deleted file whole-file location did not overlap; rename detection was not disabled")
	}
	if !scope.Overlaps(&domain.FindingLocation{Path: "renamed-dst.txt", StartLine: 5, EndLine: 5}) {
		t.Error("added file changed line did not overlap")
	}
}

func writeReviewedFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
