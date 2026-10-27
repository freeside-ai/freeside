package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Nothing raises an adoption card until #2335, which decides how a stored
// card binds its subject. Until then the store resolves no subject for one,
// so it cannot be admitted, gated, or served.
func TestImagePolicyAdoptionResolvesNoSubject(t *testing.T) {
	t.Parallel()
	policy, err := domain.NewResolvedPolicy("run-policy", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewEffectProposal(domain.EffectImagePolicyAdoption, domain.ImagePolicyAdoptionParameters{
		RefusalRecordID:     domain.Digest("sha256:" + strings.Repeat("e1", 32)),
		RegistryAuthorities: []string{"registry.npmjs.org"},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ProposalSubjectHandle(proposal); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("ProposalSubjectHandle = %v, want ErrEffectProposalInconsistent", err)
	}
	if _, err := revisionActionForKind(domain.EffectImagePolicyAdoption); !errors.Is(err, domain.ErrTransitionCommandMismatch) {
		t.Fatalf("revisionActionForKind = %v, want ErrTransitionCommandMismatch", err)
	}
}
