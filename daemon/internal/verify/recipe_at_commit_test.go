package verify

import (
	"context"
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
