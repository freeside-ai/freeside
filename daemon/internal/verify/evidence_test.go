package verify

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// runVerifyFixture runs the shared fixture to completion and returns
// the result.
func runVerifyFixture(t *testing.T, approved map[domain.Digest]bool) Result {
	t.Helper()
	checkout, opts, _ := verifyFixture(
		t,
		map[string]string{"main.go": "package main\n"},
		[]importer.Change{{Path: "main.go", Kind: importer.ChangeAdded, Mode: "100644", Digest: "sha256:bb"}},
	)
	opts.ApprovedRecipes = approved
	res, err := Verify(context.Background(), checkout, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	return res
}

// TestEvidenceProvenanceAndStoreGates is acceptance 3: the emitted
// artifacts carry verifier provenance binding the exact head and recipe
// digest, pass the evidence-snapshot gate, and round-trip through the
// real store's §5.15 persistence and reconstruction gates.
func TestEvidenceProvenanceAndStoreGates(t *testing.T) {
	trusted := RecipeDigest([]byte(trustedRecipeBytes))
	approved := map[domain.Digest]bool{trusted: true}
	res := runVerifyFixture(t, approved)
	if len(res.Evidence) != 2 {
		t.Fatalf("evidence count = %d, want 2", len(res.Evidence))
	}
	for _, e := range res.Evidence {
		p := e.Artifact.Provenance
		if p.ProducerClass != domain.ProducerVerifier ||
			p.ProducerInvocationID != "inv-1" ||
			p.HeadBinding != domain.HeadBound ||
			p.SourceHeadSHA != res.HeadSHA ||
			p.VerificationRecipeDigest == nil || *p.VerificationRecipeDigest != trusted {
			t.Errorf("artifact %s provenance %+v does not bind verifier/inv-1/head/recipe", e.Artifact.ID, p)
		}
		if !e.Artifact.PublishEligible {
			t.Errorf("artifact %s under an approved recipe is not publish eligible", e.Artifact.ID)
		}
		if err := domain.EligibleForEvidenceSnapshot(e.Artifact, approved); err != nil {
			t.Errorf("artifact %s refused by the evidence gate: %v", e.Artifact.ID, err)
		}
	}

	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"), store.Options{ApprovedRecipes: approved})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	}()
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		for _, e := range res.Evidence {
			if err := tx.PutArtifact(ctx, e.Artifact); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("store write gate refused verifier evidence: %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		for _, e := range res.Evidence {
			got, err := tx.GetArtifact(ctx, e.Artifact.ID)
			if err != nil {
				return err
			}
			if got.Digest != e.Artifact.Digest || !got.PublishEligible {
				t.Errorf("reconstructed %s = %+v, want the persisted digest and eligibility", e.Artifact.ID, got)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("store read gate refused verifier evidence: %v", err)
	}
}

// TestEvidenceUnapprovedRecipeFailsClosed: under a policy that approves
// nothing the artifacts are structurally valid but publish-ineligible,
// and the evidence gate refuses them.
func TestEvidenceUnapprovedRecipeFailsClosed(t *testing.T) {
	res := runVerifyFixture(t, nil)
	for _, e := range res.Evidence {
		if e.Artifact.PublishEligible {
			t.Errorf("artifact %s publish eligible under a nil approved set", e.Artifact.ID)
		}
		if err := e.Artifact.Validate(); err != nil {
			t.Errorf("artifact %s invalid: %v", e.Artifact.ID, err)
		}
		if err := domain.EligibleForEvidenceSnapshot(e.Artifact, nil); !errors.Is(err, domain.ErrUnapprovedRecipe) {
			t.Errorf("evidence gate = %v, want ErrUnapprovedRecipe", err)
		}
	}
}

// TestAgentArtifactCannotEnterEvidence is acceptance 3's negative half
// at the domain seams the verifier relies on: agent provenance with a
// recipe digest is a machine-checkable falsehood, and a well-formed
// agent artifact is refused by the evidence-snapshot gate that
// NewAttentionItem and the store both run.
func TestAgentArtifactCannotEnterEvidence(t *testing.T) {
	trusted := RecipeDigest([]byte(trustedRecipeBytes))
	approved := map[domain.Digest]bool{trusted: true}
	forged := domain.Provenance{
		ProducerClass:            domain.ProducerAgent,
		ProducerInvocationID:     "inv-1",
		HeadBinding:              domain.HeadBound,
		SourceHeadSHA:            "0123456789abcdef0123456789abcdef01234567",
		VerificationRecipeDigest: &trusted,
		SensitivityClass:         domain.SensitivityNormal,
	}
	if err := forged.Validate(); !errors.Is(err, domain.ErrProvenanceInconsistent) {
		t.Fatalf("agent provenance with a recipe digest validated: %v", err)
	}

	agent, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "art-agent", Type: "image", Digest: "sha256:img",
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: "inv-1",
			HeadBinding:          domain.HeadBound,
			SourceHeadSHA:        "0123456789abcdef0123456789abcdef01234567",
			SensitivityClass:     domain.SensitivityNormal,
		},
	}, approved)
	if err != nil {
		t.Fatalf("NewArtifact: %v", err)
	}
	if err := domain.EligibleForEvidenceSnapshot(agent, approved); !errors.Is(err, domain.ErrAgentArtifactInEvidence) {
		t.Fatalf("evidence gate = %v, want ErrAgentArtifactInEvidence", err)
	}
}

// TestVerifyNeverEmitsWorkspaceFiles pins the capture-none evidence
// channel: the verifier emits only its own authored account, so a
// candidate-planted "evidence" file never reaches Result.Evidence.
func TestVerifyNeverEmitsWorkspaceFiles(t *testing.T) {
	planted := "PLANTED-EVIDENCE-BYTES"
	checkout, opts, _ := verifyFixture(
		t,
		map[string]string{"evidence_snapshot.png": planted, "verification_report": planted},
		nil,
	)
	res, err := Verify(context.Background(), checkout, opts)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(res.Evidence) != 2 {
		t.Fatalf("evidence count = %d, want exactly the report and transcript", len(res.Evidence))
	}
	for _, e := range res.Evidence {
		if e.Artifact.Type != ArtifactTypeVerificationReport && e.Artifact.Type != ArtifactTypeCommandTranscript {
			t.Errorf("unexpected evidence type %s", e.Artifact.Type)
		}
		if bytes.Contains(e.Content, []byte(planted)) {
			t.Errorf("evidence %s carries planted workspace bytes", e.Artifact.ID)
		}
	}
}

// TestEvidenceIDsDistinctAcrossInvocations is the Codex-review (P1)
// regression: two runs can emit byte-identical evidence content while
// their provenance differs, and the store persists immutably by
// artifact ID, so the ID must carry the invocation. Both runs' evidence
// must persist side by side, and a same-invocation replay must stay
// idempotent.
func TestEvidenceIDsDistinctAcrossInvocations(t *testing.T) {
	trusted := RecipeDigest([]byte(trustedRecipeBytes))
	approved := map[domain.Digest]bool{trusted: true}
	checkout, opts, _ := verifyFixture(t, map[string]string{"main.go": "package main\n"}, nil)
	opts.ApprovedRecipes = approved

	first, err := Verify(context.Background(), checkout, opts)
	if err != nil {
		t.Fatalf("Verify (inv-1): %v", err)
	}
	opts.InvocationID = "inv-2"
	second, err := Verify(context.Background(), checkout, opts)
	if err != nil {
		t.Fatalf("Verify (inv-2): %v", err)
	}
	// The transcript bytes are identical across the two runs (same
	// recipe, same canned room output); only the invocation differs.
	if !bytes.Equal(first.Evidence[1].Content, second.Evidence[1].Content) {
		t.Fatal("fixture drifted: transcripts differ, so the collision under test cannot occur")
	}
	if first.Evidence[1].Artifact.ID == second.Evidence[1].Artifact.ID {
		t.Fatal("identical transcript bytes across invocations share an artifact ID")
	}

	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "store.db"), store.Options{ApprovedRecipes: approved})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	}()
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		for _, res := range []Result{first, second} {
			for _, e := range res.Evidence {
				if err := tx.PutArtifact(ctx, e.Artifact); err != nil {
					return err
				}
			}
		}
		// Same-invocation replay: identical ID and body must stay
		// storable (idempotent), never a conflict.
		return tx.PutArtifact(ctx, first.Evidence[1].Artifact)
	}); err != nil {
		t.Fatalf("store refused evidence from distinct invocations: %v", err)
	}
}
