package operations_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/operations"
)

func TestSetupCreatesOnePrivateIdempotentLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "freeside")
	first, err := operations.Setup(context.Background(), root)
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	second, err := operations.Setup(context.Background(), root)
	if err != nil {
		t.Fatalf("Setup replay: %v", err)
	}
	if first != second {
		t.Fatalf("replayed layout = %+v, want %+v", second, first)
	}
	for _, dir := range []string{
		first.ConfigDir, first.StateDir, first.CredentialsDir, first.FakeDriverDir,
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("%s mode = %04o, want 0700", dir, got)
		}
	}
	if first.FakeDriverDir != first.DBPath+".fake-stage-driver" {
		t.Fatalf("fake driver = %q, want beside db %q", first.FakeDriverDir, first.DBPath)
	}
	if first.AuthorityPath != filepath.Join(first.StateDir, "installation-authority.json") {
		t.Fatalf("authority path = %q", first.AuthorityPath)
	}
	if _, err := os.Stat(first.AuthorityPath); !os.IsNotExist(err) {
		t.Fatalf("layout-only setup unexpectedly created authority: %v", err)
	}
}

func TestSetupRejectsSymlinkAndBroadExistingDirectory(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.Setup(context.Background(), link); err == nil {
		t.Fatal("Setup accepted a symlinked config directory")
	}
	broad := filepath.Join(root, "broad")
	if err := os.Mkdir(broad, 0o755); err != nil { //nolint:gosec // G301: intentionally insecure fixture.
		t.Fatal(err)
	}
	if _, err := operations.Setup(context.Background(), broad); err == nil {
		t.Fatal("Setup accepted a group/world-readable config directory")
	}
	actualParent := filepath.Join(root, "actual-parent")
	if err := os.Mkdir(actualParent, 0o700); err != nil {
		t.Fatal(err)
	}
	parentLink := filepath.Join(root, "parent-link")
	if err := os.Symlink(actualParent, parentLink); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.Setup(
		context.Background(),
		filepath.Join(parentLink, "freeside"),
	); err == nil {
		t.Fatal("Setup followed a symbolic-link ancestor")
	}
}
