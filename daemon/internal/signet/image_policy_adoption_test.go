package signet_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	adoptionRepositoryID int64            = 42
	adoptionProject      domain.ProjectID = "proj-adopt"
)

var (
	adoptionTip    = strings.Repeat("e", 40)
	adoptionRecipe = domain.Digest("sha256:" + strings.Repeat("d", 64))
)

// adoptionFixture seeds one image_policy_adoption card the way the upkeep
// job raises it: a project, an admitted image, a policy-class refusal
// measured from that image at the observed tip, and the card admitted under
// the refusal's key with its item opened in the same transaction.
type adoptionFixture struct {
	fixture
	service  *signet.Service
	image    domain.ProjectImage
	refusal  domain.ImageUpkeepRefusal
	instance domain.ProposalInstance
	item     domain.AttentionItem
	wakes    *int
}

func newAdoptionFixture(t *testing.T) adoptionFixture {
	t.Helper()
	base := newRunFixture(t)
	ctx := context.Background()
	now := *base.now
	project, err := domain.NewProject(adoptionProject, "owner/repo", adoptionRepositoryID)
	if err != nil {
		t.Fatal(err)
	}
	image, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "owner/repo", RepositoryID: adoptionRepositoryID, CommitSHA: strings.Repeat("a", 40),
		RecipeDigest:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		PreparationCommand: []string{"/prepare"},
		BaseImageRef:       domain.ImageRef("ghcr.io/owner/base@sha256:" + strings.Repeat("1", 64)),
		ImageRef:           domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("2", 64)),
	})
	if err != nil {
		t.Fatal(err)
	}
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: adoptionRepositoryID, Repository: "owner/repo",
		Harness: domain.HarnessClientClaudeCode, TipSHA: adoptionTip, MeasuredFromImageID: image.ID,
		Class: domain.ImageUpkeepRefusalPolicy,
		Findings: []domain.ImageUpkeepClauseFinding{
			{Clause: domain.RebuildRefusalRecipeChanged, Detail: "the recipe changed"},
		},
		RecordedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := base.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		if err := tx.RecordProjectImage(ctx, image); err != nil {
			return err
		}
		if err := tx.AdvanceAdmittedImagePointer(ctx, domain.AdmittedImagePointer{
			RepositoryID: adoptionRepositoryID, Repository: "owner/repo", Harness: domain.HarnessClientClaudeCode,
			ImageID: image.ID, ImageRef: image.ImageRef, AdvancedAt: now.Add(-2 * time.Hour),
		}); err != nil {
			return err
		}
		if _, err := tx.RecordImageUpkeepRefusal(ctx, refusal); err != nil {
			return err
		}
		return tx.RecordRepositoryTipObservation(ctx, domain.RepositoryTipObservation{
			RepositoryID: adoptionRepositoryID, Repository: "owner/repo", Branch: "main",
			TipSHA: adoptionTip, ObservedAt: now.Add(-time.Hour),
		})
	}); err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewImagePolicyAdoptionProposal(domain.ImagePolicyAdoptionParameters{
		ProjectID: adoptionProject, RefusalRecordID: refusal.ID,
		RecipeDigest: new(adoptionRecipe), RegistryAuthorities: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	var instance domain.ProposalInstance
	var item domain.AttentionItem
	if err := base.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpkeepRefusal, RefusalRecordID: refusal.ID},
			"batch-adoption", proposal, now)
		if err != nil {
			return err
		}
		item, err = signet.OpenImagePolicyAdoptionItem(ctx, tx, base.blobs, instance.ID, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wakes := new(int)
	service := signet.NewService(base.store,
		signet.WithClock(func() time.Time { return *base.now }),
		signet.WithImageUpkeepWake(func() { *wakes++ }))
	return adoptionFixture{
		fixture: base, service: service, image: image, refusal: refusal,
		instance: instance, item: item, wakes: wakes,
	}
}

func (f adoptionFixture) submit(action domain.Action) error {
	_, err := f.service.Submit(context.Background(), signet.ClientCommand{
		CommandID: "adoption-" + string(action), DeviceID: f.device.ID, ExpectedEntityVersion: 1,
		Payload: signet.DecisionPayload{
			ItemID: f.item.ID, Action: action, ItemVersion: f.item.ItemVersion,
			ArtifactDigests: f.item.ArtifactDigests,
		},
	})
	return err
}

// adoptions reads the card's recorded adoption and the repository's
// effective recipe set.
func (f adoptionFixture) adoptions(t *testing.T) (bool, map[domain.Digest]bool) {
	t.Helper()
	ctx := context.Background()
	var (
		found   bool
		recipes map[domain.Digest]bool
	)
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if _, found, err = tx.ImagePolicyAdoptionForInstance(ctx, f.instance.ID); err != nil {
			return err
		}
		recipes, err = tx.EffectiveApprovedRecipes(ctx, adoptionRepositoryID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return found, recipes
}

// TestOpenImagePolicyAdoptionItemOpensTheCard pins the item the opener
// writes: a planned-gate effect_proposal on the card's batch with fixed
// daemon text and the three decisions a card takes, and a repeat call
// returns it rather than opening a second.
func TestOpenImagePolicyAdoptionItemOpensTheCard(t *testing.T) {
	f := newAdoptionFixture(t)
	item := f.item
	wantActions := []domain.Action{domain.ActionApprove, domain.ActionDecline, domain.ActionSnooze}
	if item.ID != signet.ImagePolicyAdoptionItemID(f.instance.ID) ||
		item.Type != domain.AttentionEffectProposal || item.Status != domain.StatusOpen ||
		item.InterruptionClass != domain.InterruptionPlannedGate ||
		item.Subject.Type != domain.SubjectProposalBatch ||
		item.Subject.ID != domain.SubjectID(f.instance.ProposalBatchID) ||
		item.ProjectID != adoptionProject ||
		!slices.Equal(item.RequestedDecision, wantActions) ||
		!slices.Equal(item.ArtifactDigests, []domain.Digest{f.instance.Proposal.Digest}) {
		t.Fatalf("adoption item = %#v", item)
	}
	ctx := context.Background()
	var again domain.AttentionItem
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		again, err = signet.OpenImagePolicyAdoptionItem(ctx, tx, f.blobs, f.instance.ID, (*f.now).Add(time.Hour))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if again.ID != item.ID || again.ItemVersion != item.ItemVersion || !again.CreatedAt.Equal(*item.CreatedAt) {
		t.Fatalf("reopened item = %#v, want the first item unchanged", again)
	}
}

// TestEffectProposalFactsServeTheAdoptionArm proves the facts read serves
// the card's exact digest and hosts on the adoption arm, with every other
// arm null.
func TestEffectProposalFactsServeTheAdoptionArm(t *testing.T) {
	f := newAdoptionFixture(t)
	facts, err := f.service.GetEffectProposalFacts(context.Background(), f.item.ID)
	if err != nil {
		t.Fatalf("GetEffectProposalFacts: %v", err)
	}
	adoption := facts.ImagePolicyAdoption
	if facts.EffectKind != domain.EffectImagePolicyAdoption || adoption == nil ||
		facts.SourceIssueClosure != nil || facts.FollowUpFiling != nil || facts.Supersedes != nil {
		t.Fatalf("facts = %#v, want only the adoption arm", facts)
	}
	if adoption.RefusalRecordID != f.refusal.ID || adoption.RecipeDigest == nil ||
		*adoption.RecipeDigest != adoptionRecipe || adoption.RegistryAuthorities == nil ||
		len(adoption.RegistryAuthorities) != 0 {
		t.Fatalf("adoption facts = %#v", adoption)
	}
}

// TestApprovingAnAdoptionCardRecordsTheAdoption proves approve records the
// adoption in the accepting transaction, widens the repository's effective
// recipe set to the card's digest, and wakes upkeep once after commit.
func TestApprovingAnAdoptionCardRecordsTheAdoption(t *testing.T) {
	f := newAdoptionFixture(t)
	if found, recipes := f.adoptions(t); found || recipes[adoptionRecipe] {
		t.Fatal("adoption recorded before the card was decided")
	}
	if err := f.submit(domain.ActionApprove); err != nil {
		t.Fatalf("approve: %v", err)
	}
	found, recipes := f.adoptions(t)
	if !found || !recipes[adoptionRecipe] {
		t.Fatalf("after approve: adoption found=%v, recipe adopted=%v", found, recipes[adoptionRecipe])
	}
	if *f.wakes != 1 {
		t.Fatalf("upkeep wakes = %d, want 1", *f.wakes)
	}
}

// TestDecliningAnAdoptionCardRecordsNothing proves decline ends the card
// with no adoption and no upkeep wake.
func TestDecliningAnAdoptionCardRecordsNothing(t *testing.T) {
	f := newAdoptionFixture(t)
	if err := f.submit(domain.ActionDecline); err != nil {
		t.Fatalf("decline: %v", err)
	}
	if found, recipes := f.adoptions(t); found || recipes[adoptionRecipe] {
		t.Fatal("decline recorded an adoption")
	}
	if *f.wakes != 0 {
		t.Fatalf("upkeep wakes = %d, want 0", *f.wakes)
	}
}

// TestApprovingAStaleAdoptionCardRecordsNothing proves approve refuses a
// card whose refusal no longer describes the repository, because the tip
// moved or the admitted image changed: the whole decision rolls back, so the
// card stays open, nothing is adopted, and upkeep is not woken.
func TestApprovingAStaleAdoptionCardRecordsNothing(t *testing.T) {
	for _, test := range []struct {
		name  string
		stale func(t *testing.T, f adoptionFixture, tx *store.InternalTx) error
	}{
		{
			name: "tip moved",
			stale: func(_ *testing.T, f adoptionFixture, tx *store.InternalTx) error {
				return tx.RecordRepositoryTipObservation(context.Background(), domain.RepositoryTipObservation{
					RepositoryID: adoptionRepositoryID, Repository: "owner/repo", Branch: "main",
					TipSHA: strings.Repeat("f", 40), ObservedAt: *f.now,
				})
			},
		},
		{
			name: "admitted image changed",
			stale: func(t *testing.T, f adoptionFixture, tx *store.InternalTx) error {
				input := domain.ProjectImageInput{
					Repository: "owner/repo", RepositoryID: adoptionRepositoryID, CommitSHA: adoptionTip,
					RecipeDigest:       f.image.RecipeDigest,
					PreparationCommand: f.image.PreparationCommand,
					BaseImageRef:       f.image.BaseImageRef,
					ImageRef:           domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("3", 64)),
				}
				next, err := domain.NewProjectImage(input)
				if err != nil {
					t.Fatal(err)
				}
				if err := tx.RecordProjectImage(context.Background(), next); err != nil {
					return err
				}
				return tx.AdvanceAdmittedImagePointer(context.Background(), domain.AdmittedImagePointer{
					RepositoryID: adoptionRepositoryID, Repository: "owner/repo", Harness: domain.HarnessClientClaudeCode,
					ImageID: next.ID, ImageRef: next.ImageRef, AdvancedAt: *f.now,
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAdoptionFixture(t)
			ctx := context.Background()
			if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
				return test.stale(t, f, tx)
			}); err != nil {
				t.Fatal(err)
			}
			if err := f.submit(domain.ActionApprove); !errors.Is(err, signet.ErrImagePolicyAdoptionStale) {
				t.Fatalf("approve stale card = %v, want ErrImagePolicyAdoptionStale", err)
			}
			if found, recipes := f.adoptions(t); found || recipes[adoptionRecipe] {
				t.Fatal("stale approve recorded an adoption")
			}
			var item domain.AttentionItem
			if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				item, err = tx.GetAttentionItem(ctx, f.item.ID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if item.Status != domain.StatusOpen || *f.wakes != 0 {
				t.Fatalf("after stale approve: status %s, wakes %d; want open, 0", item.Status, *f.wakes)
			}
		})
	}
}
