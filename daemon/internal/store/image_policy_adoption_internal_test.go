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
	proposal, err := domain.NewImagePolicyAdoptionProposal(domain.ImagePolicyAdoptionParameters{
		ProjectID:           "project-alpha",
		RefusalRecordID:     domain.Digest("sha256:" + strings.Repeat("e1", 32)),
		RegistryAuthorities: []string{"registry.npmjs.org"},
	})
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
