package store

import (
	"maps"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestScopedCheckpointHealthRetainsLeaseAndCompiledRecipe(t *testing.T) {
	files, err := NewDefaultLocalBackupFiles(filepath.Join(t.TempDir(), "freeside.db"))
	if err != nil {
		t.Fatal(err)
	}
	recipe := domain.Digest("sha256:approved")
	approved := map[domain.Digest]bool{recipe: true}
	if _, err := files.NewCheckpointHealthSource(schemaCheckpointArtifacts{}, approved, nil); err != nil {
		t.Fatal(err)
	}
	want := map[domain.Digest]bool{recipe: true, domain.EffectProposalRecipeDigest: true}
	for _, compiledRequested := range []bool{false, true} {
		requested := map[domain.Digest]bool{recipe: true, "sha256:unapproved": true}
		if compiledRequested {
			requested[domain.EffectProposalRecipeDigest] = true
		}
		source, err := files.NewScopedCheckpointHealthSource(requested)
		if err != nil {
			t.Fatal(err)
		}
		scoped, ok := source.(*encryptedCheckpointHealthSource)
		if !ok || scoped.files != files {
			t.Fatal("scoped source does not share the producer's lease and live-gap state")
		}
		if !maps.Equal(scoped.approvedRecipes, want) {
			t.Fatalf("compiled requested=%v: scoped recipes = %v", compiledRequested, scoped.approvedRecipes)
		}
		clear(requested)
		if !scoped.approvedRecipes[recipe] || !maps.Equal(files.approvedRecipes, want) {
			t.Fatal("request or scoped policy mutated the daemon's approvals")
		}
	}
	if !maps.Equal(approved, map[domain.Digest]bool{recipe: true}) {
		t.Fatal("backup policy mutated the caller's approvals")
	}
}
