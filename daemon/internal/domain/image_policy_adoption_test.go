package domain_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func validAdoptionParameters() domain.ImagePolicyAdoptionParameters {
	recipe := domain.Digest("sha256:" + strings.Repeat("c1", 32))
	return domain.ImagePolicyAdoptionParameters{
		RefusalRecordID:     domain.Digest("sha256:" + strings.Repeat("e1", 32)),
		RecipeDigest:        &recipe,
		RegistryAuthorities: []string{"npm.pkg.github.com", "registry.npmjs.org"},
	}
}

func TestImagePolicyAdoptionGoldenAndRoundTrip(t *testing.T) {
	proposal, err := domain.NewEffectProposal(
		domain.EffectImagePolicyAdoption, validAdoptionParameters(), proposalPolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	body, err := proposal.Encode()
	if err != nil {
		t.Fatal(err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, body, "", "  "); err != nil {
		t.Fatal(err)
	}
	pretty.WriteByte('\n')
	golden.Assert(t, "effect_proposal_image_policy_adoption", pretty.Bytes())
	decoded, err := domain.DecodeEffectProposal(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != proposal.Digest || decoded.AdoptionProposal == nil ||
		len(decoded.AdoptionProposal.RegistryAuthorities) != 2 {
		t.Fatalf("decoded = %#v", decoded)
	}
	if err := domain.GateEffectProposal(decoded, proposalPolicy(t)); err != nil {
		t.Fatalf("gate: %v", err)
	}

	// Another kind's parameters beside the adoption arm break the union.
	mixed := decoded
	mixed.FilingProposal = &domain.FollowUpFilingParameters{}
	if err := mixed.Validate(); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("mixed arms: Validate = %v, want ErrEffectProposalInconsistent", err)
	}
}

func TestImagePolicyAdoptionParametersValidate(t *testing.T) {
	t.Parallel()
	if err := validAdoptionParameters().Validate(); err != nil {
		t.Fatalf("valid: %v", err)
	}
	recipeOnly := validAdoptionParameters()
	recipeOnly.RegistryAuthorities = []string{}
	if err := recipeOnly.Validate(); err != nil {
		t.Fatalf("recipe only: %v", err)
	}
	authoritiesOnly := validAdoptionParameters()
	authoritiesOnly.RecipeDigest = nil
	if err := authoritiesOnly.Validate(); err != nil {
		t.Fatalf("authorities only: %v", err)
	}
	badRecipe := domain.Digest("sha256:short")
	tooMany := make([]string, domain.MaxImagePolicyAdoptionAuthorities+1)
	for i := range tooMany {
		tooMany[i] = "r" + strings.Repeat("a", i+1) + ".example.com"
	}
	for name, tc := range map[string]struct {
		mutate func(*domain.ImagePolicyAdoptionParameters)
		want   error
	}{
		"refusal id": {
			func(p *domain.ImagePolicyAdoptionParameters) { p.RefusalRecordID = "" },
			domain.ErrEffectProposalInconsistent,
		},
		"recipe digest": {
			func(p *domain.ImagePolicyAdoptionParameters) { p.RecipeDigest = &badRecipe },
			domain.ErrEffectProposalInconsistent,
		},
		"null authorities": {
			func(p *domain.ImagePolicyAdoptionParameters) { p.RegistryAuthorities = nil },
			domain.ErrEffectProposalInconsistent,
		},
		"proposes nothing": {func(p *domain.ImagePolicyAdoptionParameters) {
			p.RecipeDigest, p.RegistryAuthorities = nil, []string{}
		}, domain.ErrEffectProposalInconsistent},
		"unsorted": {func(p *domain.ImagePolicyAdoptionParameters) {
			p.RegistryAuthorities = []string{"registry.npmjs.org", "npm.pkg.github.com"}
		}, domain.ErrEffectProposalInconsistent},
		"duplicate": {func(p *domain.ImagePolicyAdoptionParameters) {
			p.RegistryAuthorities = []string{"registry.npmjs.org", "registry.npmjs.org"}
		}, domain.ErrEffectProposalInconsistent},
		"not a host": {func(p *domain.ImagePolicyAdoptionParameters) {
			p.RegistryAuthorities = []string{"https://registry.npmjs.org"}
		}, domain.ErrRegistrySetInvalid},
		"too many": {
			func(p *domain.ImagePolicyAdoptionParameters) { p.RegistryAuthorities = tooMany },
			domain.ErrProposalParameterTooLarge,
		},
	} {
		params := validAdoptionParameters()
		tc.mutate(&params)
		if err := params.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: Validate = %v, want %v", name, err, tc.want)
		}
	}
}
