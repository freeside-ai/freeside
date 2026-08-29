package domain

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// decisionSurfacePreimageDigest is the fixed digest of the preimage golden. A
// change here means the identity contract moved: every persisted
// attention_decision_surfaces row and every committed source record would
// disagree with a recomputation, so it needs its own contract unit.
const decisionSurfacePreimageDigest = Digest("sha256:d25bea35a25697eb46c2f68a9d0c92e3eb4876d4d95aeaf32ef99a43da30466e")

// TestDecisionSurfacePreimageGolden pins the digest preimage: its field order,
// that requested_decision is the canonical set, and that no artifact digest
// appears in it (the non-cyclic invariant, plan §4), for an item that presents
// artifacts in every slot.
func TestDecisionSurfacePreimageGolden(t *testing.T) {
	runID := RunID("run-1")
	item := AttentionItem{
		ID:                "item-1",
		Subject:           Subject{Type: SubjectRun, ID: "run-1", RunID: &runID},
		RequestedDecision: []Action{ActionStop, ActionDiscuss, ActionApprove, ActionDiscuss},
		PRHeadSHA:         "cafebabe",
		EvidenceSnapshot:  []Artifact{{Digest: "sha256:evidence"}},
		AgentClaims:       []AgentClaim{{Digest: "sha256:claim"}},
		FindingAdjudication: &FindingAdjudicationBinding{
			AdjudicationDigest: "sha256:adjudication",
		},
	}
	surface, err := NewDecisionSurface(item)
	if err != nil {
		t.Fatal(err)
	}
	preimage, err := surface.preimage()
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "decision_surface_preimage", preimage)
	for _, digest := range surface.PresentedArtifactDigests {
		if strings.Contains(string(preimage), string(digest)) {
			t.Fatalf("preimage %s contains presented digest %s", preimage, digest)
		}
	}
	if got := Digest(contentaddr.Sum(preimage)); got != decisionSurfacePreimageDigest || surface.Digest != got {
		t.Fatalf("digest = %s (record %s), want %s", got, surface.Digest, decisionSurfacePreimageDigest)
	}
}
