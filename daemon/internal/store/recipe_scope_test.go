package store_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestReadWithRecipeScopeRestrictsOnlyItsTransaction(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	policy, proposal, _ := proposalFixture(t)
	var compiled domain.Artifact
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, f.artifact); err != nil {
			return err
		}
		if err := putProposalPolicy(t, ctx, tx, policy); err != nil {
			return err
		}
		instance, _, err := tx.AllocateProposalInstance(ctx, domain.ProposalAdmissionKey{
			Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "command-recipe-scope",
		}, "batch-recipe-scope", proposal, evidenceMetaTime)
		if err != nil {
			return err
		}
		compiled, err = instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		return tx.PutArtifact(ctx, compiled)
	}); err != nil {
		t.Fatal(err)
	}
	readArtifact := func(tx *store.ReadTx) error {
		_, err := tx.GetArtifact(ctx, f.artifact.ID)
		return err
	}
	for _, recipes := range [][]domain.Digest{nil, {"sha256:other-recipe"}, {fixtureRecipe}} {
		err := st.ReadWithRecipeScope(ctx, recipes, readArtifact)
		if len(recipes) == 1 && recipes[0] == fixtureRecipe {
			if err != nil {
				t.Fatalf("approved recipe: %v", err)
			}
		} else if !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
			t.Fatalf("scope %v: %v", recipes, err)
		}
		if err := st.Read(ctx, readArtifact); err != nil {
			t.Fatalf("scope changed ordinary reads: %v", err)
		}
	}
	if err := st.ReadWithRecipeScope(ctx, nil, func(tx *store.ReadTx) error {
		_, err := tx.GetArtifact(ctx, compiled.ID)
		return err
	}); err != nil {
		t.Fatalf("compiled recipe lost approval: %v", err)
	}
}

func TestReadWithRecipeScopeCannotApproveUntrustedRecipe(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	path := tempDBPath(t)
	f := newFixtures(t)
	approving := openStoreAt(t, path, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	if err := approving.Write(ctx, func(tx *store.WriteTx) error { return tx.PutArtifact(ctx, f.artifact) }); err != nil {
		t.Fatal(err)
	}
	if err := approving.Close(); err != nil {
		t.Fatal(err)
	}
	closed := openStoreAt(t, path, store.Options{})
	err := closed.ReadWithRecipeScope(ctx, []domain.Digest{fixtureRecipe}, func(tx *store.ReadTx) error {
		_, err := tx.GetArtifact(ctx, f.artifact.ID)
		return err
	})
	if !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("caller widened store approvals: %v", err)
	}
}
