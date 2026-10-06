package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadRecipeAtCommitUsesExactTree(t *testing.T) {
	dir, commit := initRepo(t, map[string]string{
		DefaultRecipePath: trustedRecipeBytes,
	})
	fields := strings.Fields(commit)
	commit = fields[len(fields)-1]
	content, err := ReadRecipeAtCommit(context.Background(), "git", dir, commit)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != trustedRecipeBytes {
		t.Fatalf("recipe = %q, want %q", content, trustedRecipeBytes)
	}
	if _, err := ReadRecipeAtCommit(
		context.Background(), "git", dir, "HEAD",
	); err == nil {
		t.Fatal("ReadRecipeAtCommit accepted a symbolic revision")
	}
}

func TestReadFileAtCommitSeparatesAbsentFromUnreadable(t *testing.T) {
	dir, base := initRepo(t, map[string]string{
		"package.json":   `{"name":"fixture"}`,
		"nested/file.js": "export {};\n",
	})
	if err := os.Symlink("package.json", filepath.Join(dir, "link.json")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "symlink")
	head := runGit(t, dir, "rev-parse", "HEAD")
	ctx := context.Background()

	content, present, err := ReadFileAtCommit(ctx, "git", dir, head, "package.json", 64)
	if err != nil || !present || string(content) != `{"name":"fixture"}` {
		t.Fatalf("regular blob = %q, %v, %v", content, present, err)
	}
	// Absence is a result, and it is read at the named commit: the symlink
	// exists at head but not at base.
	for _, tc := range []struct{ commit, path string }{
		{head, ".npmrc"},
		{base, "link.json"},
	} {
		content, present, err = ReadFileAtCommit(ctx, "git", dir, tc.commit, tc.path, 64)
		if err != nil || present || content != nil {
			t.Fatalf("%s at %s = %q, %v, %v; want absent", tc.path, tc.commit, content, present, err)
		}
	}
	// An entry that exists in a refused shape is never reported as absent.
	for name, tc := range map[string]struct {
		path string
		max  int64
	}{
		"symlink":  {"link.json", 64},
		"tree":     {"nested", 64},
		"over cap": {"package.json", 4},
	} {
		content, present, err = ReadFileAtCommit(ctx, "git", dir, head, tc.path, tc.max)
		if !errors.Is(err, ErrCommitFileUnreadable) || present || content != nil {
			t.Fatalf("%s = %q, %v, %v; want ErrCommitFileUnreadable", name, content, present, err)
		}
	}
	if _, _, err = ReadFileAtCommit(ctx, "git", dir, "HEAD", "package.json", 64); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("symbolic revision error = %v, want ErrInvalidOptions", err)
	}
}

// An exact-base fetch leaves the worktree empty on purpose; the reader works
// from the object store alone.
func TestReadFileAtCommitReadsWithoutAWorktree(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"package.json": `{"name":"fixture"}`})
	bare := filepath.Join(t.TempDir(), "bare.git")
	runGit(t, dir, "clone", "-q", "--bare", dir, bare)
	content, present, err := ReadFileAtCommit(
		context.Background(), "git", bare, base, "package.json", 64)
	if err != nil || !present || string(content) != `{"name":"fixture"}` {
		t.Fatalf("bare read = %q, %v, %v", content, present, err)
	}
}

func TestReadRecipeAtCommitRefusesAnAbsentRecipe(t *testing.T) {
	dir, base := initRepo(t, map[string]string{"package.json": `{"name":"fixture"}`})
	if _, err := ReadRecipeAtCommit(context.Background(), "git", dir, base); !errors.Is(err, ErrRecipeUnreadable) {
		t.Fatalf("absent recipe error = %v, want ErrRecipeUnreadable", err)
	}
}
