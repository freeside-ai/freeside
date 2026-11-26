package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestProjectRecipeSetAfterOpenReadsEvidence pins that the store's recipe
// set follows the stored projects' current recipes per transaction (#3209):
// evidence under a recipe no project approved when the store opened is
// refused, reads once a project's settings approve it with no reopen, and a
// recipe-scoped read still admits it only when the scope names it.
func TestProjectRecipeSetAfterOpenReadsEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	fx := newAdoptionFixture(t)
	artifact := adoptedEvidence(t)
	put := func() error {
		return fx.s.Write(ctx, func(tx *WriteTx) error { return tx.PutArtifact(ctx, artifact) })
	}
	if err := put(); !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("PutArtifact before any project approves the recipe = %v, want ErrPublishEligibleInconsistent", err)
	}

	value, err := json.Marshal(adoptedRecipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.s.WriteInternal(ctx, func(tx *InternalTx) error {
		_, err := tx.AppendProjectSettingsRevision(ctx, domain.ProjectSettingsInput{
			ProjectID: otherProjectID, Section: domain.ProjectSectionApprovedRecipe,
			Value: value, SetBy: "cli:operator",
		}, adoptionAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := put(); err != nil {
		t.Fatalf("PutArtifact after a project approves the recipe: %v", err)
	}
	read := func(tx *ReadTx) error {
		_, err := tx.GetArtifact(ctx, artifact.ID)
		return err
	}
	if err := fx.s.Read(ctx, read); err != nil {
		t.Fatalf("GetArtifact after a project approves the recipe: %v", err)
	}
	// The store's set is a union for readability, not a grant: an item of
	// another project carrying the evidence reads too.
	item := evidenceItem(t, adoptingProjectID, artifact)
	if err := fx.s.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatalf("item of another project: %v", err)
	}

	if err := fx.s.ReadWithRecipeScope(ctx, nil, read); !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("empty scope = %v, want ErrPublishEligibleInconsistent", err)
	}
	if err := fx.s.ReadWithRecipeScope(ctx, []domain.Digest{adoptedRecipe}, read); err != nil {
		t.Fatalf("scope naming the project's recipe: %v", err)
	}
}
