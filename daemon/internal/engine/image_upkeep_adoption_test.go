package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const adoptionProject domain.ProjectID = "proj-upkeep"

const (
	adoptionOtherRecipe  = `{"commands":[["npm","run","check"]],"capture":"none"}`
	adoptionThirdRecipe  = `{"commands":[["npm","run","lint"]],"capture":"none"}`
	adoptionUndeclaredAt = "registry.example.com"
)

// upkeepUndeclaredLock adds a pinned package from a host outside the
// declared registry set.
var upkeepUndeclaredLock = upkeepLockfile(upkeepHeadJSON, upkeepLeft,
	`"node_modules/right":{"version":"2.0.0","resolved":"https://`+adoptionUndeclaredAt+
		`/right/-/right-2.0.0.tgz","integrity":"`+upkeepIntegrity+`"}`)

// adoptionHarness is an upkeep harness whose job raises adoption cards in a
// registered project, and a signet service an operator decides them through.
type adoptionHarness struct {
	*upkeepHarness
	service *signet.Service
	wakes   int
}

func newAdoptionHarness(t *testing.T, registrySet string) (*adoptionHarness, string) {
	t.Helper()
	ctx := context.Background()
	st := openUpkeepStore(t)
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	h := &adoptionHarness{upkeepHarness: newUpkeepHarness(t, st, repo, registrySet)}
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h.upkeep.Projects = []domain.ProjectID{"proj-unregistered", adoptionProject}
	h.upkeep.Blobs = blobs
	h.upkeep.AdoptionPending = h.upkeep.PendingAdoption
	project, err := domain.NewProject(adoptionProject, "owner/repo", 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RegisterProject(ctx, project); err != nil {
			return err
		}
		return tx.PutDevice(ctx, domain.Device{
			ID: "device-1", DisplayName: "phone", Status: domain.DeviceActive,
			PairedAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatal(err)
	}
	h.service = signet.NewService(st,
		signet.WithClock(func() time.Time { return time.Date(2026, 10, 27, 13, 0, 0, 0, time.UTC) }),
		signet.WithImageUpkeepWake(func() { h.wakes++ }))
	// The admitted image is current at its own build commit.
	h.seed(upkeepImage(t, built, func(input *domain.ProjectImageInput) {
		input.Environment.PreparationDigest = projectimage.PreparationDigest()
	}))
	return h, built
}

// advanceTip commits changes, observes the new tip, and runs the start pass.
func (h *adoptionHarness) advanceTip(changes map[string]string) string {
	h.t.Helper()
	tip := h.repo.commit(changes)
	h.observeTip(tip)
	if err := h.upkeep.RunAll(context.Background()); err != nil {
		h.t.Fatalf("RunAll at %s: %v", tip, err)
	}
	return tip
}

// cards returns every adoption card item, in item-ID order.
func (h *adoptionHarness) cards() []domain.AttentionItem {
	h.t.Helper()
	ctx := context.Background()
	var items []domain.AttentionItem
	if err := h.st.Read(ctx, func(tx *store.ReadTx) error {
		ids, err := tx.ProposalItemIDsForSubject(
			ctx, domain.OpaqueSubjectHandle(adoptionProject), domain.EffectImagePolicyAdoption)
		if err != nil {
			return err
		}
		for _, id := range ids {
			item, err := tx.GetAttentionItem(ctx, id)
			if err != nil {
				return err
			}
			items = append(items, item)
		}
		return nil
	}); err != nil {
		h.t.Fatal(err)
	}
	return items
}

// onlyCard returns the one adoption card and its parameters.
func (h *adoptionHarness) onlyCard() (domain.AttentionItem, domain.ImagePolicyAdoptionParameters) {
	h.t.Helper()
	cards := h.cards()
	if len(cards) != 1 {
		h.t.Fatalf("adoption cards = %d, want 1", len(cards))
	}
	var params domain.ImagePolicyAdoptionParameters
	if err := h.st.Read(context.Background(), func(tx *store.ReadTx) error {
		instance, _, err := tx.ProposalForItem(context.Background(), cards[0].ID)
		params = *instance.Proposal.AdoptionProposal
		return err
	}); err != nil {
		h.t.Fatal(err)
	}
	return cards[0], params
}

func (h *adoptionHarness) decide(item domain.AttentionItem, action domain.Action) {
	h.t.Helper()
	if _, err := h.service.Submit(context.Background(), signet.ClientCommand{
		CommandID: "decide-" + string(item.ID), DeviceID: "device-1", ExpectedEntityVersion: 1,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: action, ItemVersion: item.ItemVersion,
			ArtifactDigests: item.ArtifactDigests,
		},
	}); err != nil {
		h.t.Fatalf("%s %s: %v", action, item.ID, err)
	}
}

func (h *adoptionHarness) pending() *domain.ImageUpkeepHold {
	h.t.Helper()
	hold, err := h.upkeep.PendingResolution(context.Background())
	if err != nil {
		h.t.Fatal(err)
	}
	return hold
}

// A tip that changes only the recipe ends in one card naming the tip's
// digest. While it is open, held work waits on it; approving it records the
// adoption and wakes upkeep, whose next pass builds a new image at the tip
// under the adopted recipe, advances the pointer, and releases the hold.
// The publication lane's recipe set then holds the adopted digest for this
// repository only.
func TestImageUpkeepAdoptsAChangedRecipe(t *testing.T) {
	t.Parallel()
	h, _ := newAdoptionHarness(t, upkeepRegistrySet)
	tip := h.advanceTip(map[string]string{verify.DefaultRecipePath: adoptionOtherRecipe})
	adopted := verify.RecipeDigest([]byte(adoptionOtherRecipe))

	card, params := h.onlyCard()
	if card.Status != domain.StatusOpen || params.ProjectID != adoptionProject ||
		params.RecipeDigest == nil || *params.RecipeDigest != adopted || len(params.RegistryAuthorities) != 0 {
		t.Fatalf("card %s %#v, want an open card for recipe %s", card.Status, params, adopted)
	}
	if hold := h.pending(); hold == nil || hold.Pending != domain.ImageUpkeepPendingAdoptionProposal ||
		hold.ProposalItemID == nil || *hold.ProposalItemID != card.ID {
		t.Fatalf("hold before approve = %#v, want the adoption card", hold)
	}

	h.decide(card, domain.ActionApprove)
	if h.wakes != 1 {
		t.Fatalf("upkeep wakes = %d, want 1", h.wakes)
	}
	if hold := h.pending(); hold == nil || hold.Pending != domain.ImageUpkeepPendingAdoptionProposal {
		t.Fatalf("hold between approve and the pass = %#v, want the adoption card", hold)
	}
	if err := h.upkeep.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll after approve: %v", err)
	}
	pointed := h.pointer()
	if pointed.CommitSHA != tip || pointed.RecipeDigest != adopted {
		t.Fatalf("pointer = %s with recipe %s, want %s with %s", pointed.CommitSHA, pointed.RecipeDigest, tip, adopted)
	}
	if request := h.builder.requests[len(h.builder.requests)-1]; string(request.Recipe) != adoptionOtherRecipe ||
		!strings.HasPrefix(request.RefTag, "adopt-") {
		t.Fatalf("build request recipe %q tag %q, want the tip's recipe under an adopt- tag",
			request.Recipe, request.RefTag)
	}
	if hold := h.pending(); hold != nil {
		t.Fatalf("hold after the new image = %#v, want none", hold)
	}

	for _, test := range []struct {
		repositoryID int64
		want         bool
	}{{42, true}, {7, false}} {
		w := &productionPublicationWorkflow{
			store: h.st, repositoryID: test.repositoryID,
			operatorRecipes: map[domain.Digest]bool{verify.RecipeDigest([]byte(upkeepRecipe)): true},
		}
		recipes, err := w.recipes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if recipes[adopted] != test.want || !recipes[verify.RecipeDigest([]byte(upkeepRecipe))] {
			t.Fatalf("repository %d recipes = %v, want adopted %v beside the operator's", test.repositoryID, recipes, test.want)
		}
	}
}

// A tip whose new dependency resolves from a host outside the declared set
// ends in one card naming the host. Approving it widens the registry set,
// so the next pass admits the delta and rebuilds the image as it would any
// within-policy change.
func TestImageUpkeepAdoptsARegistryHost(t *testing.T) {
	t.Parallel()
	h, _ := newAdoptionHarness(t, upkeepRegistrySet)
	tip := h.advanceTip(map[string]string{
		"package.json": upkeepHeadJSON, "package-lock.json": upkeepUndeclaredLock,
	})
	card, params := h.onlyCard()
	if params.RecipeDigest != nil || !slices.Equal(params.RegistryAuthorities, []string{adoptionUndeclaredAt}) {
		t.Fatalf("card parameters = %#v, want only %s", params, adoptionUndeclaredAt)
	}
	before := h.pointer()
	h.decide(card, domain.ActionApprove)
	if err := h.upkeep.RunAll(context.Background()); err != nil {
		t.Fatalf("RunAll after approve: %v", err)
	}
	pointed := h.pointer()
	if pointed.CommitSHA != tip || pointed.RecipeDigest != before.RecipeDigest {
		t.Fatalf("pointer = %s with recipe %s, want %s keeping %s",
			pointed.CommitSHA, pointed.RecipeDigest, tip, before.RecipeDigest)
	}
	if request := h.builder.requests[len(h.builder.requests)-1]; !strings.HasPrefix(request.RefTag, "rebuild-") {
		t.Fatalf("build request tag %q, want an ordinary rebuild", request.RefTag)
	}
	if hold := h.pending(); hold != nil {
		t.Fatalf("hold after the rebuild = %#v, want none", hold)
	}
}

// A rerun at the same tip converges on the same refusal and finds its card,
// so it raises no second one.
func TestImageUpkeepRaisesOneCardPerRefusal(t *testing.T) {
	t.Parallel()
	h, _ := newAdoptionHarness(t, upkeepRegistrySet)
	h.advanceTip(map[string]string{verify.DefaultRecipePath: adoptionOtherRecipe})
	for range 2 {
		if err := h.upkeep.RunAll(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if card, _ := h.onlyCard(); card.Status != domain.StatusOpen {
		t.Fatalf("card status after reruns = %s, want open", card.Status)
	}
}

// A recipe change beside an unpinned source is a repair: no adoption answers
// it, so no card is raised and nothing is pending on one.
func TestImageUpkeepRaisesNoCardForARepair(t *testing.T) {
	t.Parallel()
	h, _ := newAdoptionHarness(t, upkeepRegistrySet)
	h.advanceTip(map[string]string{
		verify.DefaultRecipePath: adoptionOtherRecipe,
		"package.json":           upkeepHeadJSON, "package-lock.json": upkeepUnpinnedLock,
	})
	if cards := h.cards(); len(cards) != 0 {
		t.Fatalf("adoption cards for a repair = %d, want 0", len(cards))
	}
	if refusals := h.refusals(); len(refusals) != 1 || refusals[0].Class == domain.ImageUpkeepRefusalPolicy {
		t.Fatalf("refusals = %#v, want one that is not policy", refusals)
	}
	if hold := h.pending(); hold != nil {
		t.Fatalf("hold for a repair = %#v, want none", hold)
	}
}

// Declining ends the card's pending resolution, so held work takes the
// blocked outcome, and the next pass builds nothing.
func TestImageUpkeepDeclinedCardLeavesWorkBlocked(t *testing.T) {
	t.Parallel()
	h, built := newAdoptionHarness(t, upkeepRegistrySet)
	h.advanceTip(map[string]string{verify.DefaultRecipePath: adoptionOtherRecipe})
	card, _ := h.onlyCard()
	h.decide(card, domain.ActionDecline)
	if h.wakes != 0 {
		t.Fatalf("upkeep wakes after decline = %d, want 0", h.wakes)
	}
	if hold := h.pending(); hold != nil {
		t.Fatalf("hold after decline = %#v, want none", hold)
	}
	if err := h.upkeep.RunAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pointed := h.pointer(); pointed.CommitSHA != built {
		t.Fatalf("pointer moved to %s after decline, want %s", pointed.CommitSHA, built)
	}
	if card, _ := h.onlyCard(); card.Status != domain.StatusDismissed {
		t.Fatalf("declined card status = %s, want dismissed", card.Status)
	}
}

// A newer refusal for the repository withdraws the older open card, so only
// the card for the current tip can be approved.
func TestImageUpkeepWithdrawsACardTheTipMovedPast(t *testing.T) {
	t.Parallel()
	h, _ := newAdoptionHarness(t, upkeepRegistrySet)
	h.advanceTip(map[string]string{verify.DefaultRecipePath: adoptionOtherRecipe})
	first, _ := h.onlyCard()
	h.advanceTip(map[string]string{verify.DefaultRecipePath: adoptionThirdRecipe})
	cards := h.cards()
	if len(cards) != 2 {
		t.Fatalf("adoption cards = %d, want 2", len(cards))
	}
	third := verify.RecipeDigest([]byte(adoptionThirdRecipe))
	for _, card := range cards {
		want := domain.StatusOpen
		if card.ID == first.ID {
			want = domain.StatusSuperseded
		}
		if card.Status != want {
			t.Fatalf("card %s status = %s, want %s", card.ID, card.Status, want)
		}
	}
	hold := h.pending()
	if hold == nil || hold.ProposalItemID == nil || *hold.ProposalItemID == first.ID {
		t.Fatalf("hold = %#v, want the newer card", hold)
	}
	var params domain.ImagePolicyAdoptionParameters
	if err := h.st.Read(context.Background(), func(tx *store.ReadTx) error {
		instance, _, err := tx.ProposalForItem(context.Background(), *hold.ProposalItemID)
		params = *instance.Proposal.AdoptionProposal
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if params.RecipeDigest == nil || *params.RecipeDigest != third {
		t.Fatalf("pending card recipe = %v, want %s", params.RecipeDigest, third)
	}
}

// An adopted build that fails does not leave the approved card standing in
// for the verdict. A host fault leaves the tip unsettled, so the schedule's
// backstop pass builds again instead of reusing the answered refusal; a
// proof failure records a repair, which the schedule then reuses.
func TestImageUpkeepRetriesAFailedAdoptedBuild(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		fail      error
		rebuilds  bool
		wantClass domain.ImageUpkeepRefusalClass
	}{
		{name: "host fault", fail: errors.New("the container runtime is gone"), rebuilds: true},
		{name: "proof failure", fail: projectimage.ErrProofFailed, wantClass: domain.ImageUpkeepRefusalRepair},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			h, built := newAdoptionHarness(t, upkeepRegistrySet)
			tip := h.advanceTip(map[string]string{verify.DefaultRecipePath: adoptionOtherRecipe})
			card, _ := h.onlyCard()
			h.decide(card, domain.ActionApprove)
			h.builder.fail = test.fail
			_ = h.upkeep.RunAll(ctx)
			h.builder.fail = nil
			attempts := len(h.builder.requests)

			if err := h.upkeep.RunScheduled(ctx); err != nil {
				t.Fatalf("RunScheduled: %v", err)
			}
			if test.rebuilds {
				if pointed := h.pointer(); pointed.CommitSHA != tip {
					t.Fatalf("pointer = %s after the backstop pass, want %s", pointed.CommitSHA, tip)
				}
				return
			}
			if len(h.builder.requests) != attempts || h.pointer().CommitSHA != built {
				t.Fatalf("the backstop pass rebuilt past a recorded repair")
			}
			hold := h.pending()
			if hold != nil && hold.Pending == domain.ImageUpkeepPendingAdoptionProposal {
				t.Fatalf("hold = %#v, want the repair's, not the approved card", hold)
			}
			refusals := h.refusals()
			if last := refusals[len(refusals)-1]; last.Class != test.wantClass {
				t.Fatalf("last refusal class = %s, want %s", last.Class, test.wantClass)
			}
		})
	}
}

// A lockfile host the registry set can't name, such as one with a port, is
// no card to raise: the pass records the policy refusal and raises nothing,
// and it does not fail.
func TestImageUpkeepRaisesNoCardForAnUnnamableHost(t *testing.T) {
	t.Parallel()
	h, _ := newAdoptionHarness(t, upkeepRegistrySet)
	lock := upkeepLockfile(upkeepHeadJSON, upkeepLeft,
		`"node_modules/right":{"version":"2.0.0","resolved":"https://`+adoptionUndeclaredAt+
			`:8443/right/-/right-2.0.0.tgz","integrity":"`+upkeepIntegrity+`"}`)
	h.advanceTip(map[string]string{"package.json": upkeepHeadJSON, "package-lock.json": lock})
	if cards := h.cards(); len(cards) != 0 {
		t.Fatalf("adoption cards = %d, want 0", len(cards))
	}
	if refusals := h.refusals(); len(refusals) != 1 {
		t.Fatalf("refusals = %#v, want one", refusals)
	}
}
