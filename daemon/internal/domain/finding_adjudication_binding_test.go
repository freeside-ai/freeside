package domain_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestFindingAdjudicationBindingRules(t *testing.T) {
	base := validItemInput(domain.AttentionFindingAdjudication)
	item, err := domain.NewAttentionItem(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(item.ArtifactDigests, base.FindingAdjudication.AdjudicationDigest) {
		t.Fatalf("artifact_digests %v does not bind adjudication digest", item.ArtifactDigests)
	}

	base.FindingAdjudication.Proposals[0].Rationale = "mutated"
	base.FindingAdjudication.Proposals[0].OfferedAlternatives[0].Consequence = "mutated"
	if item.FindingAdjudication.Proposals[0].Rationale == "mutated" ||
		item.FindingAdjudication.Proposals[0].OfferedAlternatives[0].Consequence == "mutated" {
		t.Fatal("constructed item aliases caller-owned finding adjudication binding")
	}

	t.Run("missing", func(t *testing.T) {
		in := validItemInput(domain.AttentionFindingAdjudication)
		in.FindingAdjudication = nil
		_, err := domain.NewAttentionItem(in, nil)
		if !errors.Is(err, domain.ErrFindingAdjudicationBindingMissing) {
			t.Fatalf("got %v, want ErrFindingAdjudicationBindingMissing", err)
		}
	})
	t.Run("outside type", func(t *testing.T) {
		in := validItemInput(domain.AttentionSpecApproval)
		in.FindingAdjudication = validItemInput(domain.AttentionFindingAdjudication).FindingAdjudication
		_, err := domain.NewAttentionItem(in, nil)
		if !errors.Is(err, domain.ErrFindingAdjudicationBindingOutsideItem) {
			t.Fatalf("got %v, want ErrFindingAdjudicationBindingOutsideItem", err)
		}
	})
	t.Run("subject mismatch", func(t *testing.T) {
		in := validItemInput(domain.AttentionFindingAdjudication)
		in.FindingAdjudication.RunID = "other-run"
		_, err := domain.NewAttentionItem(in, nil)
		if !errors.Is(err, domain.ErrFindingAdjudicationBindingMismatch) {
			t.Fatalf("got %v, want ErrFindingAdjudicationBindingMismatch", err)
		}
	})
	t.Run("choose action without alternative", func(t *testing.T) {
		in := validItemInput(domain.AttentionFindingAdjudication)
		for idx := range in.FindingAdjudication.Proposals {
			in.FindingAdjudication.Proposals[idx].OfferedAlternatives = nil
		}
		_, err := domain.NewAttentionItem(in, nil)
		if !errors.Is(err, domain.ErrFindingAdjudicationBindingMismatch) {
			t.Fatalf("got %v, want ErrFindingAdjudicationBindingMismatch", err)
		}
		in.RequestedDecision = slices.DeleteFunc(in.RequestedDecision, func(action domain.Action) bool {
			return action == domain.ActionChooseAlternativeRoute
		})
		if _, err := domain.NewAttentionItem(in, nil); err != nil {
			t.Fatalf("item without choose action: %v", err)
		}
	})
}

func TestFindingAdjudicationBindingValidationRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*domain.FindingAdjudicationBinding)
		want   error
	}{
		{"empty proposals", func(b *domain.FindingAdjudicationBinding) { b.Proposals = nil }, domain.ErrEmptyField},
		{"duplicate finding", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals = append(b.Proposals, b.Proposals[0])
		}, domain.ErrDuplicate},
		{"invalid axes", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].GoalRelationship = domain.GoalRequired
		}, domain.ErrAdjudicationAxisMismatch},
		{"invalid alternative", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].OfferedAlternatives[0].Route = "invalid"
		}, domain.ErrInvalidAdjudicationRoute},
		{"axis-incompatible alternative", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].OfferedAlternatives[0].Route = domain.RouteDefer
		}, domain.ErrAdjudicationAxisMismatch},
		{"recommended alternative", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].OfferedAlternatives[0].Route = b.Proposals[0].Route
		}, domain.ErrDuplicate},
		{"duplicate alternative", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].OfferedAlternatives = append(
				b.Proposals[0].OfferedAlternatives, b.Proposals[0].OfferedAlternatives[0])
		}, domain.ErrDuplicate},
		{"empty consequence", func(b *domain.FindingAdjudicationBinding) {
			b.Proposals[0].OfferedAlternatives[0].Consequence = "  "
		}, domain.ErrEmptyField},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			binding := *validItemInput(domain.AttentionFindingAdjudication).FindingAdjudication
			binding.Proposals = slices.Clone(binding.Proposals)
			binding.Proposals[0].OfferedAlternatives = slices.Clone(binding.Proposals[0].OfferedAlternatives)
			test.mutate(&binding)
			if err := binding.Validate(); !errors.Is(err, test.want) {
				t.Fatalf("Validate = %v, want %v", err, test.want)
			}
		})
	}
}
