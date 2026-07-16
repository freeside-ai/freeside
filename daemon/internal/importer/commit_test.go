package importer

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// plannedFromHandoff builds the construction-facing change for one
// handoff blob, taking the expected oid from content verification.
func plannedFromHandoff(t *testing.T, blobs map[export.Digest]blobInfo, kind ChangeKind, path, content string) plannedChange {
	t.Helper()
	digest := sha256Digest(content)
	info, ok := blobs[digest]
	if !ok {
		t.Fatalf("fixture handoff lacks a verified blob for %q", path)
	}
	return plannedChange{
		path: path, kind: kind, mode: "100644",
		oid: info.gitOID, digest: digest, size: info.size, verifiedPath: info.verifiedPath,
	}
}

func testImportOptions(base string) Options {
	return Options{
		BaseSHA:    base,
		CommitDate: time.Unix(1700000100, 0).UTC(),
	}.withDefaults()
}

func TestBuildCommitClean(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{
		"a.txt":    "old\n",
		"del.txt":  "bye\n",
		"keep.txt": "same\n",
	})
	handoff := buildHandoff(t, []blobSpec{
		{path: "a.txt", content: "new\n"},
		{path: "dir/n.txt", content: "add\n"},
	})
	blobs, err := verifyBlobsForTest(t, handoff, loadFixtureManifest(t, handoff), Policy{}.withDefaults())
	if err != nil {
		t.Fatalf("verifyBlobs: %v", err)
	}
	changes := []plannedChange{
		plannedFromHandoff(t, blobs, ChangeModified, "a.txt", "new\n"),
		{path: "del.txt", kind: ChangeDeleted},
		plannedFromHandoff(t, blobs, ChangeAdded, "dir/n.txt", "add\n"),
	}
	opts := testImportOptions(base)
	opts.ImportRef = "refs/freeside/imports/test"

	g := newTestRunner(t, checkout, opts)
	tree, commit, err := buildCommit(t.Context(), g, opts, changes)
	if err != nil {
		t.Fatalf("buildCommit: %v", err)
	}
	if parent := rungit(t, checkout, "log", "-1", "--format=%P", commit); parent != base {
		t.Errorf("commit parent = %s, want the enforced base %s", parent, base)
	}
	if author := rungit(t, checkout, "log", "-1", "--format=%an <%ae>", commit); author != DefaultAuthorName+" <"+DefaultAuthorEmail+">" {
		t.Errorf("commit author = %q, want the daemon identity", author)
	}
	if got := rungit(t, checkout, "show", commit+":a.txt"); got != "new" {
		t.Errorf("a.txt content = %q, want new", got)
	}
	if got := rungit(t, checkout, "show", commit+":keep.txt"); got != "same" {
		t.Errorf("keep.txt content = %q, want same (untouched)", got)
	}
	names := rungit(t, checkout, "ls-tree", "-r", "--name-only", commit)
	if names != "a.txt\ndir/n.txt\nkeep.txt" {
		t.Errorf("tree paths = %q, want a.txt, dir/n.txt, keep.txt", names)
	}
	if got := rungit(t, checkout, "rev-parse", opts.ImportRef); got != commit {
		t.Errorf("import ref = %s, want %s", got, commit)
	}
	if treeOf := rungit(t, checkout, "log", "-1", "--format=%T", commit); treeOf != tree {
		t.Errorf("commit tree = %s, buildCommit returned %s", treeOf, tree)
	}

	// Determinism: a fresh runner and scratch with the same pinned date
	// must produce the identical commit object.
	g2 := newTestRunner(t, checkout, opts)
	_, commit2, err := buildCommit(t.Context(), g2, opts, changes)
	if err != nil {
		t.Fatalf("second buildCommit: %v", err)
	}
	if commit2 != commit {
		t.Errorf("re-import produced %s, first produced %s; want identical", commit2, commit)
	}
}

func TestBuildCommitEmptyChangeSet(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "same\n"})
	g := newTestRunner(t, checkout, testImportOptions(base))
	tree, commit, err := buildCommit(t.Context(), g, testImportOptions(base), nil)
	if err != nil {
		t.Fatalf("buildCommit: %v", err)
	}
	if baseTree := rungit(t, checkout, "log", "-1", "--format=%T", base); baseTree != tree {
		t.Errorf("empty change set tree = %s, want the base tree %s", tree, baseTree)
	}
	if parent := rungit(t, checkout, "log", "-1", "--format=%P", commit); parent != base {
		t.Errorf("commit parent = %s, want %s", parent, base)
	}
}

func TestBuildCommitRejectsLyingOID(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	handoff := buildHandoff(t, []blobSpec{
		{path: "a.txt", content: "new\n"},
		{path: "b.txt", content: "other\n"},
	})
	blobs, err := verifyBlobsForTest(t, handoff, loadFixtureManifest(t, handoff), Policy{}.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	lying := plannedFromHandoff(t, blobs, ChangeModified, "a.txt", "new\n")
	lying.oid = blobs[sha256Digest("other\n")].gitOID // claims the wrong object
	g := newTestRunner(t, checkout, testImportOptions(base))
	_, _, err = buildCommit(t.Context(), g, testImportOptions(base), []plannedChange{lying})
	if !errors.Is(err, ErrTreeMismatch) {
		t.Fatalf("buildCommit = %v, want %v", err, ErrTreeMismatch)
	}
}

func TestIngestBlobsUsesVerifiedSnapshotAfterHandoffReplacement(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{
			name: "same-size content swap",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.WriteFile(path, []byte("bad\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "oversized replacement",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				content := append([]byte("new\n"), make([]byte, 8<<20)...)
				if err := os.WriteFile(path, content, 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "fifo replacement",
			mutate: func(t *testing.T, path string) {
				t.Helper()
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := syscall.Mkfifo(path, 0o600); err != nil {
					t.Skipf("mkfifo unsupported: %v", err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
			handoff := buildHandoff(t, []blobSpec{{path: "a.txt", content: "new\n"}})
			blobs, err := verifyBlobsForTest(t, handoff, loadFixtureManifest(t, handoff), Policy{}.withDefaults())
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256Digest("new\n")
			blobPath := filepath.Join(handoff, "blobs", "sha256", string(digest)[len("sha256:"):])
			tc.mutate(t, blobPath)
			g := newTestRunner(t, checkout, testImportOptions(base))
			done := make(chan error, 1)
			go func() {
				_, err := g.ingestBlobs(t.Context(), []export.Digest{digest}, blobs)
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("ingestBlobs after handoff replacement: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("ingestBlobs touched a replaced handoff blob instead of the verified snapshot")
			}
		})
	}
}
func TestBuildCommitRejectsUnplannedElision(t *testing.T) {
	// A "modification" identical to base produces no diff-tree record;
	// the acceptance cross-check must refuse the mismatch rather than
	// silently blessing a tree that differs from the plan.
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "same\n"})
	handoff := buildHandoff(t, []blobSpec{{path: "a.txt", content: "same\n"}})
	blobs, err := verifyBlobsForTest(t, handoff, loadFixtureManifest(t, handoff), Policy{}.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	change := plannedFromHandoff(t, blobs, ChangeModified, "a.txt", "same\n")
	g := newTestRunner(t, checkout, testImportOptions(base))
	_, _, err = buildCommit(t.Context(), g, testImportOptions(base), []plannedChange{change})
	if !errors.Is(err, ErrTreeMismatch) {
		t.Fatalf("buildCommit = %v, want %v", err, ErrTreeMismatch)
	}
}
