package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestRunningDaemonApprovesAdoptedRecipes pins the recipe check a command
// that borrows the running daemon's store passes: the operator's set, the
// effect-proposal recipe, and a recipe adopted for the daemon's repository
// pass; a recipe adopted for another repository, or adopted by no one, does
// not.
func TestRunningDaemonApprovesAdoptedRecipes(t *testing.T) {
	const repositoryID int64 = 42
	ctx := t.Context()
	at := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	operator := domain.Digest("sha256:" + strings.Repeat("a", 64))
	adopted := domain.Digest("sha256:" + strings.Repeat("d", 64))
	unknown := domain.Digest("sha256:" + strings.Repeat("f", 64))
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	project, err := domain.NewProject("proj-adopt", "owner/repo", repositoryID)
	if err != nil {
		t.Fatal(err)
	}
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "owner/repo", RepositoryID: repositoryID, CommitSHA: strings.Repeat("a", 40),
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		PreparationCommand: []string{"/prepare"},
		BaseImageRef:       domain.ImageRef("ghcr.io/owner/base@sha256:" + strings.Repeat("1", 64)),
		ImageRef:           domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("2", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: repositoryID, Repository: "owner/repo", Harness: domain.HarnessClientClaudeCode,
		TipSHA: strings.Repeat("e", 40), MeasuredFromImageID: image.ID, Class: domain.ImageUpkeepRefusalPolicy,
		Findings: []domain.ImageUpkeepClauseFinding{
			{Clause: domain.RebuildRefusalRecipeChanged, Detail: "the recipe changed"},
		},
		RecordedAt: at,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		if err := tx.RecordProjectImage(ctx, image); err != nil {
			return err
		}
		if err := tx.AdvanceAdmittedImagePointer(ctx, domain.AdmittedImagePointer{
			RepositoryID: repositoryID, Repository: "owner/repo", Harness: domain.HarnessClientClaudeCode,
			ImageID: image.ID, ImageRef: image.ImageRef, AdvancedAt: at,
		}); err != nil {
			return err
		}
		_, err := tx.RecordImageUpkeepRefusal(ctx, refusal)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewImagePolicyAdoptionProposal(domain.ImagePolicyAdoptionParameters{
		ProjectID: project.ID, RefusalRecordID: refusal.ID, RecipeDigest: &adopted, RegistryAuthorities: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var card domain.AttentionItem
	device := domain.DeviceID("device-adopt")
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		instance, _, err := tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpkeepRefusal, RefusalRecordID: refusal.ID},
			"batch-adopt", proposal, at)
		if err != nil {
			return err
		}
		if card, err = signet.OpenImagePolicyAdoptionItem(ctx, tx, blobs, instance.ID, at); err != nil {
			return err
		}
		return tx.PutDevice(ctx, domain.Device{ID: device, DisplayName: "Phone", Status: domain.DeviceActive, PairedAt: at})
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := signet.NewService(st).Submit(ctx, signet.ClientCommand{
		CommandID: "adopt", DeviceID: device, ExpectedEntityVersion: int64(card.ItemVersion),
		Payload: signet.DecisionPayload{
			ItemID: card.ID, Action: domain.ActionApprove, ItemVersion: card.ItemVersion,
			ArtifactDigests: card.ArtifactDigests,
		},
	}); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		repositoryID int64
		recipe       domain.Digest
		approved     bool
	}{
		{name: "operator", repositoryID: repositoryID, recipe: operator, approved: true},
		{name: "effect proposal", repositoryID: repositoryID, recipe: domain.EffectProposalRecipeDigest, approved: true},
		{name: "adopted here", repositoryID: repositoryID, recipe: adopted, approved: true},
		{name: "adopted elsewhere", repositoryID: 7, recipe: adopted},
		{name: "unknown", repositoryID: repositoryID, recipe: unknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			daemon := daemonStoreContext{
				store: st, approvedRecipes: map[domain.Digest]bool{operator: true}, repositoryID: tc.repositoryID,
			}
			err := daemon.requireApproved(ctx, map[domain.Digest]bool{tc.recipe: true})
			if tc.approved && err != nil {
				t.Fatalf("requireApproved = %v, want approved", err)
			}
			if !tc.approved && !errors.Is(err, domain.ErrUnapprovedRecipe) {
				t.Fatalf("requireApproved = %v, want %v", err, domain.ErrUnapprovedRecipe)
			}
		})
	}
}
