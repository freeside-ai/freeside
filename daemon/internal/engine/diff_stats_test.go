package engine

import (
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
