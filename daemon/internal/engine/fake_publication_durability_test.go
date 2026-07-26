package engine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFakePublicationDurabilityHelpers(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "one", "two")
	if err := makeFakePublicationDirectory(nested, 0o700); err != nil {
		t.Fatalf("make durable directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "candidate.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncFakePublicationTree(filepath.Join(root, "one")); err != nil {
		t.Fatalf("sync tree: %v", err)
	}
	if err := syncFakePublicationDirectory(root); err != nil {
		t.Fatalf("sync directory: %v", err)
	}
}

func TestFakePublicationDurabilityHelpersRejectNonDirectoryAncestor(t *testing.T) {
	root := t.TempDir()
	notDirectory := filepath.Join(root, "file")
	if err := os.WriteFile(notDirectory, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := makeFakePublicationDirectory(filepath.Join(notDirectory, "child"), 0o700); err == nil {
		t.Fatal("created directory beneath a regular file")
	}
}
