package domain_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const approvedRecipe = domain.Digest("sha256:recipe-approved")

func approvedRecipes() map[domain.Digest]bool {
	return map[domain.Digest]bool{approvedRecipe: true}
}

func provenance(class domain.ProducerClass, recipe *domain.Digest) domain.Provenance {
	return domain.Provenance{
		ProducerClass:            class,
		ProducerInvocationID:     "inv-1",
		SourceHeadSHA:            "abc123",
		VerificationRecipeDigest: recipe,
		SensitivityClass:         domain.SensitivityNormal,
	}
}

// TestEligibleForEvidenceSnapshot is acceptance criterion 4: verifier/daemon
// artifacts under an approved recipe are admitted to evidence; an agent artifact
// is rejected (it belongs in a labeled claim); an unapproved recipe is rejected.
func TestEligibleForEvidenceSnapshot(t *testing.T) {
	tests := []struct {
		name    string
		class   domain.ProducerClass
		recipe  *domain.Digest
		wantErr error
	}{
		{"verifier approved", domain.ProducerVerifier, ptr(approvedRecipe), nil},
		{"daemon approved", domain.ProducerDaemon, ptr(approvedRecipe), nil},
		{"agent rejected", domain.ProducerAgent, nil, domain.ErrAgentArtifactInEvidence},
		{"verifier unapproved recipe", domain.ProducerVerifier, ptr(domain.Digest("sha256:not-approved")), domain.ErrUnapprovedRecipe},
		{"verifier no recipe", domain.ProducerVerifier, nil, domain.ErrUnapprovedRecipe},
		{"invalid producer", domain.ProducerClass("ghost"), ptr(approvedRecipe), domain.ErrInvalidProducerClass},
	}
	// The gate validates the artifact before the trust decision: a malformed
	// artifact under an approved recipe must not slip into evidence.
	malformed := domain.Artifact{ID: "", Type: "log", Digest: "sha256:x", Provenance: provenance(domain.ProducerVerifier, ptr(approvedRecipe))}
	if err := domain.EligibleForEvidenceSnapshot(malformed, approvedRecipes()); !errors.Is(err, domain.ErrEmptyID) {
		t.Fatalf("gate admitted a malformed artifact: %v", err)
	}
	// A stale publish_eligible must be rejected by the gate: an approved
	// verifier artifact that reads not-publishable contradicts trusted policy,
	// so the reconstruction path re-running the gate cannot admit it.
	stale := domain.Artifact{ID: "a1", Type: "log", Digest: "sha256:x", Provenance: provenance(domain.ProducerVerifier, ptr(approvedRecipe)), PublishEligible: false}
	if err := domain.EligibleForEvidenceSnapshot(stale, approvedRecipes()); !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("gate admitted a stale publish_eligible: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The eligibility bit must match policy for approved cases (nil
			// wantErr); the error cases are rejected before the bit is checked.
			a := domain.Artifact{ID: "a1", Type: "log", Digest: "sha256:x", Provenance: provenance(tt.class, tt.recipe), PublishEligible: tt.wantErr == nil}
			err := domain.EligibleForEvidenceSnapshot(a, approvedRecipes())
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestAgentArtifactRejectedFromItemEvidence checks the gate is wired into item
// construction: an agent artifact in the evidence snapshot fails NewAttentionItem.
func TestAgentArtifactRejectedFromItemEvidence(t *testing.T) {
	agentArt := domain.Artifact{ID: "a1", Type: "image", Digest: "sha256:x", Provenance: provenance(domain.ProducerAgent, nil)}
	in := validItemInput(domain.AttentionReadyForFinalReview)
	in.EvidenceSnapshot = []domain.Artifact{agentArt}
	_, err := domain.NewAttentionItem(in, approvedRecipes())
	if !errors.Is(err, domain.ErrAgentArtifactInEvidence) {
		t.Fatalf("error = %v, want ErrAgentArtifactInEvidence", err)
	}

	verifierArt := domain.Artifact{ID: "a2", Type: "log", Digest: "sha256:y", Provenance: provenance(domain.ProducerVerifier, ptr(approvedRecipe))}
	in.EvidenceSnapshot = []domain.Artifact{verifierArt}
	if _, err := domain.NewAttentionItem(in, approvedRecipes()); err != nil {
		t.Fatalf("verifier artifact under approved recipe rejected: %v", err)
	}
}

// TestNewArtifactDetachesRecipePointer checks that a constructed artifact does
// not alias the caller's recipe-digest pointer: mutating the caller's variable
// after construction leaves the validated artifact and its computed eligibility
// unchanged.
func TestNewArtifactDetachesRecipePointer(t *testing.T) {
	recipe := approvedRecipe
	a, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "a", Type: "log", Digest: "sha256:z",
		Provenance: provenance(domain.ProducerVerifier, &recipe),
	}, approvedRecipes())
	if err != nil {
		t.Fatal(err)
	}
	if !a.PublishEligible {
		t.Fatal("expected the verifier artifact under an approved recipe to be eligible")
	}
	recipe = "sha256:tampered" // reuse the caller-owned variable
	if got := a.Provenance.VerificationRecipeDigest; got == nil || *got != approvedRecipe {
		t.Errorf("artifact recipe digest changed via the caller's pointer: %v", got)
	}
}

// TestValidateRejectsInconsistentPublishEligible is the deserialization
// backstop for criterion 5: an artifact reconstructed with publish_eligible
// inconsistent with its provenance is rejected by Validate without needing the
// approved-recipe set.
func TestValidateRejectsInconsistentPublishEligible(t *testing.T) {
	agent := domain.Artifact{ID: "a", Type: "img", Digest: "sha256:x", Provenance: provenance(domain.ProducerAgent, nil), PublishEligible: true}
	if err := agent.Validate(); !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("agent artifact marked publishable validated: %v", err)
	}
	noRecipe := domain.Artifact{ID: "b", Type: "log", Digest: "sha256:y", Provenance: provenance(domain.ProducerVerifier, nil), PublishEligible: true}
	if err := noRecipe.Validate(); !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("verifier artifact eligible without a recipe validated: %v", err)
	}
	// A present-but-empty recipe digest is a malformed content address, not "absent".
	emptyRecipe := domain.Provenance{
		ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "inv", SourceHeadSHA: "h",
		VerificationRecipeDigest: ptr(domain.Digest("")), SensitivityClass: domain.SensitivityNormal,
	}
	if err := emptyRecipe.Validate(); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("empty recipe digest behind a non-nil pointer validated: %v", err)
	}
	consistent := domain.Artifact{ID: "c", Type: "log", Digest: "sha256:z", Provenance: provenance(domain.ProducerVerifier, ptr(approvedRecipe)), PublishEligible: true}
	if err := consistent.Validate(); err != nil {
		t.Fatalf("consistent eligible artifact rejected: %v", err)
	}
}

// TestProvenanceRejectsAgentRecipe checks the producer/recipe contract: an
// agent artifact is never produced under a recipe, so a recipe digest on it is
// a machine-checkable falsehood.
func TestProvenanceRejectsAgentRecipe(t *testing.T) {
	recipe := approvedRecipe
	p := provenance(domain.ProducerAgent, &recipe)
	if err := p.Validate(); !errors.Is(err, domain.ErrProvenanceInconsistent) {
		t.Fatalf("agent provenance with a recipe digest validated: %v", err)
	}
	// Agent without a recipe is fine.
	if err := provenance(domain.ProducerAgent, nil).Validate(); err != nil {
		t.Fatalf("agent provenance without a recipe rejected: %v", err)
	}
}

// TestProvenanceRequiresSourceHead checks that an evidence-bearing artifact
// must carry the head it was produced against (plan §5.15 rule 2): an empty
// source_head_sha is rejected, so evidence can always be head-bound.
func TestProvenanceRequiresSourceHead(t *testing.T) {
	prov := provenance(domain.ProducerVerifier, ptr(approvedRecipe))
	prov.SourceHeadSHA = ""
	if err := prov.Validate(); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("Provenance.Validate error = %v, want ErrEmptyField", err)
	}
	_, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "a", Type: "log", Digest: "sha256:z", Provenance: prov,
	}, approvedRecipes())
	if !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("NewArtifact error = %v, want ErrEmptyField", err)
	}
}

// TestPublishEligibleNotAgentSettable is acceptance criterion 5: publish_eligible
// is not settable from any agent-supplied input path. ArtifactInput has no such
// field, and NewArtifact computes it: agent output is never eligible, and only a
// verifier/daemon artifact under an approved recipe becomes eligible.
func TestPublishEligibleNotAgentSettable(t *testing.T) {
	// Structural: the input struct exposes no publish-eligibility field.
	rt := reflect.TypeOf(domain.ArtifactInput{})
	for i := range rt.NumField() {
		if strings.Contains(strings.ToLower(rt.Field(i).Name), "publish") {
			t.Fatalf("ArtifactInput exposes %q; publish eligibility must not be caller-settable", rt.Field(i).Name)
		}
	}

	tests := []struct {
		name         string
		class        domain.ProducerClass
		recipe       *domain.Digest
		wantEligible bool
	}{
		{"agent never eligible", domain.ProducerAgent, nil, false},
		{"verifier approved eligible", domain.ProducerVerifier, ptr(approvedRecipe), true},
		{"daemon approved eligible", domain.ProducerDaemon, ptr(approvedRecipe), true},
		{"verifier unapproved not eligible", domain.ProducerVerifier, ptr(domain.Digest("sha256:nope")), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := domain.NewArtifact(domain.ArtifactInput{
				ID: "a", Type: "log", Digest: "sha256:z", Provenance: provenance(tt.class, tt.recipe),
			}, approvedRecipes())
			if err != nil {
				t.Fatal(err)
			}
			if a.PublishEligible != tt.wantEligible {
				t.Errorf("PublishEligible = %v, want %v", a.PublishEligible, tt.wantEligible)
			}
		})
	}
}
