package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// forgedEligibleArtifact is a verifier artifact carrying publish_eligible=true
// under a recipe the store's policy does not approve, built as an exported
// struct literal so it never passed NewArtifact. It is the value #31's attacker
// persists to smuggle unapproved evidence past the boundary.
func forgedEligibleArtifact() domain.Artifact {
	return domain.Artifact{
		ID: "art-forged", Type: "verify_log", Digest: "sha256:forged",
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-1",
			SourceHeadSHA:            "cafebabe",
			VerificationRecipeDigest: ptrDigest("sha256:unapproved-recipe"),
			SensitivityClass:         domain.SensitivityNormal,
		},
		PublishEligible: true,
	}
}

func ptrDigest(d domain.Digest) *domain.Digest { return &d }

// TestPutArtifactRejectsForgedPublishEligible is issue #31 acceptance 1/3 for a
// standalone artifact row: a caller bypassing NewArtifact cannot persist a
// publish_eligible artifact under an unapproved recipe. The gate is the store's,
// so it fires even under a store that approves other recipes.
func TestPutArtifactRejectsForgedPublishEligible(t *testing.T) {
	ctx := context.Background()
	// A store that approves the fixture recipe but NOT the forged one.
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutArtifact(ctx, forgedEligibleArtifact())
	})
	if !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("PutArtifact error = %v, want ErrPublishEligibleInconsistent", err)
	}

	// Even the fixture's own eligible artifact fails closed against a store that
	// approves nothing: the store, not the caller, owns the approval decision.
	f := newFixtures(t)
	closed := openStore(t, store.Options{})
	err = closed.Write(ctx, func(tx *store.WriteTx) error { return tx.PutArtifact(ctx, f.artifact) })
	if !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("PutArtifact under empty policy error = %v, want ErrPublishEligibleInconsistent", err)
	}
}

// TestPutArtifactAllowsLegalNonEvidence checks the gate does not over-block: a
// legal non-evidence artifact (agent output, publish_eligible=false) persists
// even under a store that approves nothing.
func TestPutArtifactAllowsLegalNonEvidence(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})
	agentArt := domain.Artifact{
		ID: "art-agent", Type: "image", Digest: "sha256:img",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-1",
			SourceHeadSHA: "cafebabe", SensitivityClass: domain.SensitivityNormal,
		},
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutArtifact(ctx, agentArt) }); err != nil {
		t.Fatalf("legal non-evidence artifact rejected: %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetArtifact(ctx, "art-agent")
		return err
	}); err != nil {
		t.Fatalf("GetArtifact of legal non-evidence artifact: %v", err)
	}
}

// TestGetArtifactRejectsUnapprovedRecipe is issue #31 acceptance 1 for
// reconstruction: an eligible artifact persisted under one policy fails closed
// when read back under a policy that no longer approves its recipe (a forged
// row, or one written by an older binary, cannot leak as valid evidence).
func TestGetArtifactRejectsUnapprovedRecipe(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)
	f := newFixtures(t)

	approving := openStoreAt(t, path, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	if err := approving.Write(ctx, func(tx *store.WriteTx) error { return tx.PutArtifact(ctx, f.artifact) }); err != nil {
		t.Fatalf("seed eligible artifact: %v", err)
	}
	if err := approving.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the same database with a policy that approves nothing.
	closed := openStoreAt(t, path, store.Options{})
	err := closed.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetArtifact(ctx, f.artifact.ID)
		return err
	})
	if !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("GetArtifact under empty policy error = %v, want ErrPublishEligibleInconsistent", err)
	}
}

// TestPutAttentionItemRejectsUnapprovedEvidence is issue #31 acceptance 1/3/4
// for embedded evidence: an item whose evidence snapshot rides an artifact under
// an unapproved recipe fails closed at the persistence boundary. The gate runs
// before the insert, so it fires without the item's foreign keys being seeded.
func TestPutAttentionItemRejectsUnapprovedEvidence(t *testing.T) {
	ctx := context.Background()
	f := newFixtures(t) // f.item carries evidence under fixtureRecipe
	s := openStore(t, store.Options{})
	err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, f.item) })
	if !errors.Is(err, domain.ErrUnapprovedRecipe) {
		t.Fatalf("PutAttentionItem under empty policy error = %v, want ErrUnapprovedRecipe", err)
	}
}

// TestGetAttentionItemRejectsUnapprovedEvidence is issue #31 acceptance 1/4 for
// reconstruction of embedded evidence: an item persisted under an approving
// policy fails closed when read back under one that no longer approves its
// evidence recipe.
func TestGetAttentionItemRejectsUnapprovedEvidence(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)
	f := newFixtures(t)

	approving := openStoreAt(t, path, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	err := approving.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, f.item)
	})
	if err != nil {
		t.Fatalf("seed item with evidence: %v", err)
	}
	if err := approving.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	closed := openStoreAt(t, path, store.Options{})
	err = closed.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, f.item.ID)
		return err
	})
	if !errors.Is(err, domain.ErrUnapprovedRecipe) {
		t.Fatalf("GetAttentionItem under empty policy error = %v, want ErrUnapprovedRecipe", err)
	}
}
