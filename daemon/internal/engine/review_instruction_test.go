package engine

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestDiscoverCodexReviewInstructionsUsesExactTreeAndOverrides(t *testing.T) {
	base := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(base, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("AGENTS.md", "trusted root\n")
	write("daemon/AGENTS.md", "shadowed daemon\n")
	write("daemon/AGENTS.override.md", "trusted daemon override\n")
	write("empty/AGENTS.override.md", "")
	write("empty/AGENTS.md", "trusted empty fallback\n")
	write(".git/AGENTS.md", "metadata poison\n")

	sources, err := discoverCodexReviewInstructions(base)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(sources))
	for i, source := range sources {
		paths[i] = source.Path
	}
	if !slices.Equal(paths, []string{
		"AGENTS.md", "daemon/AGENTS.override.md", "empty/AGENTS.override.md",
	}) {
		t.Fatalf("discovered sources = %v", paths)
	}
	if string(sources[1].Body) != "trusted daemon override\n" {
		t.Fatalf("override body = %q", sources[1].Body)
	}
	if len(sources[2].Body) != 0 {
		t.Fatalf("empty override body = %q", sources[2].Body)
	}
}

func TestDiscoverCodexReviewInstructionsRejectsSymlinkSource(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "rules.md")
	if err := os.WriteFile(target, []byte("outside selection\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := discoverCodexReviewInstructions(root); err == nil {
		t.Fatal("symlinked exact-base instruction source was accepted")
	}
}

func TestDiscoverCodexReviewInstructionsBoundsAggregateSources(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"AGENTS.md", "nested/AGENTS.md"} {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, bytes.Repeat([]byte("x"), int(domain.MaxVendorInstructionBytes/2+1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := discoverCodexReviewInstructions(root); err == nil {
		t.Fatal("aggregate exact-base instruction budget was not enforced")
	}
}
