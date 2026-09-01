package domain_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// surfaceItem is the base item the decision-surface tests mutate one field
// of: a finding_adjudication item, because it fills every presentation slot
// (evidence, a claim, and the adjudication binding) and offers several actions.
func surfaceItem(t *testing.T) domain.AttentionItem {
	t.Helper()
	in := validItemInput(domain.AttentionFindingAdjudication)
	in.PRHeadSHA = "cafebabe"
	in.EvidenceSnapshot = []domain.Artifact{evidenceArtifact(t, "art-evidence", "sha256:evidence")}
	in.AgentClaims = textClaims(domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown, Content: "the diff touches only docs",
	}, "")
	item, err := domain.NewAttentionItem(in, map[domain.Digest]bool{surfaceRecipe: true})
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	return item
}

const surfaceRecipe = domain.Digest("sha256:recipe")

func evidenceArtifact(t *testing.T, id domain.ArtifactID, digest domain.Digest) domain.Artifact {
	t.Helper()
	recipe := surfaceRecipe
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: id, Type: domain.ArtifactKindVerifyLog, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-1",
			HeadBinding:              domain.HeadBound,
			SourceHeadSHA:            "cafebabe",
			VerificationRecipeDigest: &recipe,
			SensitivityClass:         domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, map[domain.Digest]bool{recipe: true})
	if err != nil {
		t.Fatalf("NewArtifact: %v", err)
	}
	return artifact
}

func mustSurface(t *testing.T, item domain.AttentionItem) domain.DecisionSurface {
	t.Helper()
	surface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatalf("NewDecisionSurface: %v", err)
	}
	return surface
}

// unchanged asserts that item still carries current: the identity neither
// advances nor changes.
func unchanged(t *testing.T, current domain.DecisionSurface, item domain.AttentionItem, what string) {
	t.Helper()
	next, advanced, err := domain.NextDecisionSurface(current, item)
	if err != nil {
		t.Fatalf("%s: NextDecisionSurface: %v", what, err)
	}
	if advanced {
		t.Fatalf("%s advanced the decision surface to epoch %d", what, next.Epoch)
	}
	if next.Epoch != current.Epoch || next.Digest != current.Digest {
		t.Fatalf("%s returned epoch %d digest %s, want the stored epoch %d digest %s",
			what, next.Epoch, next.Digest, current.Epoch, current.Digest)
	}
}

// advanced asserts that item opens the next epoch with a fresh digest and
// returns the new record.
func advanced(t *testing.T, current domain.DecisionSurface, item domain.AttentionItem, what string) domain.DecisionSurface {
	t.Helper()
	next, moved, err := domain.NextDecisionSurface(current, item)
	if err != nil {
		t.Fatalf("%s: NextDecisionSurface: %v", what, err)
	}
	if !moved {
		t.Fatalf("%s did not advance the decision surface", what)
	}
	if next.Epoch != current.Epoch+1 {
		t.Fatalf("%s advanced to epoch %d, want %d", what, next.Epoch, current.Epoch+1)
	}
	if next.Digest == current.Digest {
		t.Fatalf("%s reused digest %s across epochs", what, next.Digest)
	}
	if err := domain.ValidateDecisionSurfaceTransition(current, next); err != nil {
		t.Fatalf("%s: transition rejected: %v", what, err)
	}
	return next
}

// TestDecisionSurfaceTelemetryStable is the telemetry-stable invariant and the
// rejected item_version coupling: delivery timing, a status transition, the
// decision stamp, an expiry, and a bare item_version advance leave the epoch
// and digest unchanged.
func TestDecisionSurfaceTelemetryStable(t *testing.T) {
	item := surfaceItem(t)
	current := mustSurface(t, item)
	if current.Epoch != 1 {
		t.Fatalf("creation epoch = %d, want 1", current.Epoch)
	}

	at := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	timed, err := item.WithTiming([]domain.AttentionDelivery{{
		ItemID: item.ID, DeviceID: "device-1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: at, Status: domain.DeliverySubmitted,
	}})
	if err != nil {
		t.Fatal(err)
	}
	unchanged(t, current, timed, "delivery timing")

	decided, err := item.WithDecidedAt(at)
	if err != nil {
		t.Fatal(err)
	}
	decided.Status = domain.StatusResolved
	decided.ItemVersion++
	unchanged(t, current, decided, "resolving decision")

	versioned := item
	versioned.ItemVersion += 5
	expires := at.Add(time.Hour)
	versioned.ExpiresWhen = &expires
	unchanged(t, current, versioned, "item_version and expiry")
}

// TestDecisionSurfaceEligibilityIndependent is the identity half of the
// rejected own-artifact subtraction: a digest that reaches the item's binding
// set without a presentation slot (the shape #917's recommendation provenance
// takes) never enters the presented set and never advances the epoch.
func TestDecisionSurfaceEligibilityIndependent(t *testing.T) {
	item := surfaceItem(t)
	current := mustSurface(t, item)

	sourceOnly := item
	sourceOnly.ArtifactDigests = append(slices.Clone(item.ArtifactDigests), "sha256:recommendation-source")
	slices.Sort(sourceOnly.ArtifactDigests)
	if got := domain.PresentedArtifactDigests(sourceOnly); !slices.Equal(got, domain.PresentedArtifactDigests(item)) {
		t.Fatalf("presented set %v admitted a non-slot digest", got)
	}
	unchanged(t, current, sourceOnly, "a source-only digest joining the binding set")
	if fresh := mustSurface(t, sourceOnly); fresh.Digest != current.Digest {
		t.Fatalf("fresh identity %s differs from %s on a non-slot digest", fresh.Digest, current.Digest)
	}
}

// TestPresentedArtifactDigestsIsSlotUnion pins the structural presented-slot
// predicate: exactly the sorted, deduplicated union of the evidence, claim, and
// adjudication digests, independent of every other digest source on the item.
func TestPresentedArtifactDigestsIsSlotUnion(t *testing.T) {
	item := surfaceItem(t)
	want := []domain.Digest{
		item.EvidenceSnapshot[0].Digest, item.AgentClaims[0].Digest,
		item.FindingAdjudication.AdjudicationDigest,
	}
	slices.Sort(want)
	if got := domain.PresentedArtifactDigests(item); !slices.Equal(got, want) {
		t.Fatalf("PresentedArtifactDigests = %v, want %v", got, want)
	}

	bare := mustItem(t, validItemInput(domain.AttentionBlocked))
	if got := domain.PresentedArtifactDigests(bare); got == nil || len(got) != 0 {
		t.Fatalf("PresentedArtifactDigests(no slots) = %#v, want an empty non-nil set", got)
	}
}

// TestDecisionSurfaceDistinguishing is the surface-distinguishing invariant
// and the rejected surface collapse: every presentation slot, the head, and
// the offered action set advance the epoch; two items never share a digest;
// a reorder of requested_decision is not a change; and a field returning to a
// prior value is a third epoch, never a reuse.
func TestDecisionSurfaceDistinguishing(t *testing.T) {
	item := surfaceItem(t)
	current := mustSurface(t, item)

	replacedAdjudication := item
	replacedAdjudication.FindingAdjudication = clonePtr(item.FindingAdjudication)
	replacedAdjudication.FindingAdjudication.AdjudicationDigest = "sha256:adjudication-superseded"
	advanced(t, current, replacedAdjudication, "superseding the adjudication artifact")

	replacedEvidence := item
	replacedEvidence.EvidenceSnapshot = []domain.Artifact{evidenceArtifact(t, "art-evidence", "sha256:evidence-2")}
	advanced(t, current, replacedEvidence, "replacing an evidence artifact")

	replacedClaim := item
	replacedClaim.AgentClaims = textClaims(domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown, Content: "a different summary",
	}, "")
	advanced(t, current, replacedClaim, "replacing a claim")

	newHead := item
	newHead.PRHeadSHA = "deadbeef"
	advanced(t, current, newHead, "a new pr_head_sha")

	fewerActions := item
	fewerActions.RequestedDecision = item.RequestedDecision[:len(item.RequestedDecision)-1]
	advanced(t, current, fewerActions, "dropping an offered action")

	reordered := item
	reordered.RequestedDecision = slices.Clone(item.RequestedDecision)
	slices.Reverse(reordered.RequestedDecision)
	unchanged(t, current, reordered, "reordering requested_decision")

	twin := item
	twin.ID = "item-2"
	if twinSurface := mustSurface(t, twin); twinSurface.Digest == current.Digest {
		t.Fatalf("two items share decision surface digest %s", current.Digest)
	}

	// A→B→A on the head: three epochs and three distinct digests.
	second := advanced(t, current, newHead, "A→B")
	third := advanced(t, second, item, "B→A")
	if third.Epoch != 3 {
		t.Fatalf("B→A epoch = %d, want 3", third.Epoch)
	}
	digests := []domain.Digest{current.Digest, second.Digest, third.Digest}
	slices.Sort(digests)
	if len(slices.Compact(digests)) != 3 {
		t.Fatalf("A→B→A reused a digest: %v", digests)
	}
}

// TestNextDecisionSurfaceIsDeterministic is the domain half of the non-cyclic
// invariant: the identity of a prospective epoch depends on nothing produced
// after it, so a producer computing it before finalizing its artifact and the
// store deriving it at admission agree.
func TestNextDecisionSurfaceIsDeterministic(t *testing.T) {
	item := surfaceItem(t)
	current := mustSurface(t, item)
	prospective := item
	prospective.FindingAdjudication = clonePtr(item.FindingAdjudication)
	prospective.FindingAdjudication.AdjudicationDigest = "sha256:adjudication-next"

	first := advanced(t, current, prospective, "producer pre-commit")
	second := advanced(t, current, prospective, "store admission")
	if first.Digest != second.Digest || first.Epoch != second.Epoch {
		t.Fatalf("pre-commit %d/%s and admission %d/%s disagree",
			first.Epoch, first.Digest, second.Epoch, second.Digest)
	}
	if err := domain.VerifyDecisionSurfaceCommitment(second, first.Digest); err != nil {
		t.Fatalf("pre-committed digest rejected at admission: %v", err)
	}

	foreign := item
	foreign.ID = "item-2"
	if _, _, err := domain.NextDecisionSurface(current, foreign); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("foreign item = %v, want ErrParentKeyMismatch", err)
	}
}

// TestVerifyDecisionSurfaceCommitment covers the per-record check #917
// applies: the current digest verifies, a prior epoch's digest and an empty
// commitment are refused.
func TestVerifyDecisionSurfaceCommitment(t *testing.T) {
	item := surfaceItem(t)
	current := mustSurface(t, item)
	if err := domain.VerifyDecisionSurfaceCommitment(current, current.Digest); err != nil {
		t.Fatalf("current digest: %v", err)
	}
	newHead := item
	newHead.PRHeadSHA = "deadbeef"
	next := advanced(t, current, newHead, "a new head")
	if err := domain.VerifyDecisionSurfaceCommitment(next, current.Digest); !errors.Is(err, domain.ErrDecisionSurfaceMismatch) {
		t.Fatalf("stale digest = %v, want ErrDecisionSurfaceMismatch", err)
	}
	if err := domain.VerifyDecisionSurfaceCommitment(next, ""); !errors.Is(err, domain.ErrEmptyID) {
		t.Fatalf("empty digest = %v, want ErrEmptyID", err)
	}
}

// TestValidateDecisionSurfaceTransition covers the epoch rule at the writer
// boundary: stay or advance by exactly one, matching whether the surface
// changed, on a fixed item.
func TestValidateDecisionSurfaceTransition(t *testing.T) {
	item := surfaceItem(t)
	current := mustSurface(t, item)
	newHead := item
	newHead.PRHeadSHA = "deadbeef"
	next := advanced(t, current, newHead, "a new head")

	if err := domain.ValidateDecisionSurfaceTransition(current, current); err != nil {
		t.Fatalf("unchanged record: %v", err)
	}
	if err := domain.ValidateDecisionSurfaceTransition(next, current); !errors.Is(err, domain.ErrStaleTransition) {
		t.Fatalf("regressing epoch = %v, want ErrStaleTransition", err)
	}

	sameEpochChanged := next
	sameEpochChanged.Epoch = current.Epoch
	sameEpochChanged.Digest = mustDigest(t, sameEpochChanged)
	if err := domain.ValidateDecisionSurfaceTransition(current, sameEpochChanged); !errors.Is(err, domain.ErrStaleTransition) {
		t.Fatalf("changed surface under the same epoch = %v, want ErrStaleTransition", err)
	}

	// The presented set is outside the preimage, so a record differing only
	// there still validates; the transition must still see it as a change.
	presentedChanged := current
	presentedChanged.PresentedArtifactDigests = []domain.Digest{"sha256:other"}
	if err := domain.ValidateDecisionSurfaceTransition(current, presentedChanged); !errors.Is(err, domain.ErrStaleTransition) {
		t.Fatalf("changed presented set under the same epoch = %v, want ErrStaleTransition", err)
	}

	gratuitous := current
	gratuitous.Epoch++
	gratuitous.Digest = mustDigest(t, gratuitous)
	if err := domain.ValidateDecisionSurfaceTransition(current, gratuitous); !errors.Is(err, domain.ErrDecisionSurfaceEpoch) {
		t.Fatalf("advance with no change = %v, want ErrDecisionSurfaceEpoch", err)
	}

	skipped := next
	skipped.Epoch++
	skipped.Digest = mustDigest(t, skipped)
	if err := domain.ValidateDecisionSurfaceTransition(current, skipped); !errors.Is(err, domain.ErrDecisionSurfaceEpoch) {
		t.Fatalf("skipped epoch = %v, want ErrDecisionSurfaceEpoch", err)
	}

	retargeted := next
	retargeted.ItemID = "item-2"
	retargeted.Digest = mustDigest(t, retargeted)
	if err := domain.ValidateDecisionSurfaceTransition(current, retargeted); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("item change = %v, want ErrImmutableTransition", err)
	}

	tampered := next
	tampered.Digest = current.Digest
	if err := domain.ValidateDecisionSurfaceTransition(current, tampered); !errors.Is(err, domain.ErrDecisionSurfaceMismatch) {
		t.Fatalf("tampered digest = %v, want ErrDecisionSurfaceMismatch", err)
	}
}

// TestDecisionSurfaceValidate covers the shape refusals a decoded record
// meets before any consumer compares against it.
func TestDecisionSurfaceValidate(t *testing.T) {
	item := surfaceItem(t)
	valid := mustSurface(t, item)
	cases := []struct {
		name   string
		mutate func(*domain.DecisionSurface)
		want   error
	}{
		{"tampered digest", func(s *domain.DecisionSurface) { s.Digest = "sha256:forged" }, domain.ErrDecisionSurfaceMismatch},
		{"tampered epoch", func(s *domain.DecisionSurface) { s.Epoch++ }, domain.ErrDecisionSurfaceMismatch},
		{"tampered head", func(s *domain.DecisionSurface) { s.PRHeadSHA = "deadbeef" }, domain.ErrDecisionSurfaceMismatch},
		{"zero epoch", func(s *domain.DecisionSurface) { s.Epoch = 0 }, domain.ErrNonPositive},
		{"empty item", func(s *domain.DecisionSurface) { s.ItemID = "" }, domain.ErrEmptyID},
		{"unsorted actions", func(s *domain.DecisionSurface) {
			slices.Reverse(s.RequestedDecision)
		}, domain.ErrDecisionSurfaceNotCanonical},
		{"duplicate actions", func(s *domain.DecisionSurface) {
			s.RequestedDecision = append(s.RequestedDecision, s.RequestedDecision[0])
		}, domain.ErrDecisionSurfaceNotCanonical},
		{"invalid action", func(s *domain.DecisionSurface) {
			s.RequestedDecision = []domain.Action{"bogus"}
		}, domain.ErrInvalidAction},
		{"unsorted digests", func(s *domain.DecisionSurface) {
			slices.Reverse(s.PresentedArtifactDigests)
		}, domain.ErrDecisionSurfaceNotCanonical},
		{"empty digest", func(s *domain.DecisionSurface) {
			s.PresentedArtifactDigests = []domain.Digest{""}
		}, domain.ErrEmptyID},
		{"invalid subject", func(s *domain.DecisionSurface) { s.Subject.ID = "" }, domain.ErrEmptyID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := valid
			s.RequestedDecision = slices.Clone(valid.RequestedDecision)
			s.PresentedArtifactDigests = slices.Clone(valid.PresentedArtifactDigests)
			tc.mutate(&s)
			if err := s.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate = %v, want %v", err, tc.want)
			}
		})
	}
}

func mustDigest(t *testing.T, s domain.DecisionSurface) domain.Digest {
	t.Helper()
	digest, err := s.ComputeDigest()
	if err != nil {
		t.Fatalf("ComputeDigest: %v", err)
	}
	return digest
}

func clonePtr[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}
