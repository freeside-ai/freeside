package signet_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestSyncProjectsEvidenceAvailability is #922 acceptance for the read-time
// availability recompute (plan §5.15): the sync projection derives each
// evidence and claim reference's EvidenceMetadata.Availability from the blob
// store immediately before serialization, never trusting the persisted value.
// An item whose digests have no stored bytes syncs as bytes_absent and flips to
// available once the bytes land, with no re-put of the item.
func TestSyncProjectsEvidenceAvailability(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	recipe := domain.Digest("sha256:recipe-approved")
	approved := map[domain.Digest]bool{recipe: true}
	s, err := store.Open(ctx, t.TempDir()+"/signet.db", store.Options{ApprovedRecipes: approved})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	service := signet.NewService(s,
		signet.WithPairingKey(testPairingKey),
		signet.WithBlobStore(blobs),
		signet.WithClock(func() time.Time { return now }),
		signet.WithNtfy(signet.NtfyConfig{
			BaseURL: "https://ntfy.example", TopicKey: testTopicKey,
			ClickBaseURL: "https://daemon.example",
		}),
	)

	// A run-channel evidence artifact and a claim-channel image reference; the
	// persisted Availability is deliberately the opposite of the truth so the
	// test proves the projection overwrites it rather than echoing it.
	evidenceBytes := []byte(`{"outcome":"passed"}`)
	evidenceDigest := domain.Digest(contentaddr.Sum(evidenceBytes))
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "art-report", Type: domain.ArtifactKindVerificationReport, Digest: evidenceDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "inv-1",
			HeadBinding: domain.HeadIndependent, VerificationRecipeDigest: &recipe,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(evidenceBytes)),
			CreatedAt: now, Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
	}, approved)
	if err != nil {
		t.Fatalf("NewArtifact: %v", err)
	}
	claimBytes := []byte("\x89PNG\r\n\x1a\nfake-png")
	claimDigest := domain.Digest(contentaddr.Sum(claimBytes))
	claim := domain.AgentClaim{
		Label: "screenshot", Artifact: "art-shot", Digest: claimDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-2",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaImagePNG, SizeBytes: int64(len(claimBytes)),
			CreatedAt: now, Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
	expires := now.Add(24 * time.Hour)
	runID := domain.RunID("run-1")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "checks are green and the diff is ready",
		RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss},
		PRHeadSHA:         "cafebabe",
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 123},
		EvidenceSnapshot:  []domain.Artifact{artifact},
		AgentClaims:       []domain.AgentClaim{claim},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		ExpiresWhen:       &expires, Status: domain.StatusOpen,
	}, approved)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	if err := service.PutItem(ctx, item); err != nil {
		t.Fatalf("PutItem: %v", err)
	}

	read := func() (domain.EvidenceAvailability, domain.EvidenceAvailability) {
		got, err := service.GetAttentionItem(ctx, item.ID)
		if err != nil {
			t.Fatalf("GetAttentionItem: %v", err)
		}
		if len(got.Item.EvidenceSnapshot) != 1 || len(got.Item.AgentClaims) != 1 {
			t.Fatalf("unexpected item shape: %d evidence, %d claims",
				len(got.Item.EvidenceSnapshot), len(got.Item.AgentClaims))
		}
		return got.Item.EvidenceSnapshot[0].Metadata.Availability, got.Item.AgentClaims[0].Metadata.Availability
	}

	// No bytes stored yet: both references project bytes_absent, overriding the
	// persisted "available" value.
	if ev, cl := read(); ev != domain.EvidenceBytesAbsent || cl != domain.EvidenceBytesAbsent {
		t.Fatalf("before store: evidence=%q claim=%q, want both bytes_absent", ev, cl)
	}

	// Store the evidence bytes only: it flips to available while the claim,
	// still unstored, stays bytes_absent, proving the projection is per-digest.
	if _, err := blobs.Put(evidenceDigest, bytes.NewReader(evidenceBytes)); err != nil {
		t.Fatalf("put evidence bytes: %v", err)
	}
	if ev, cl := read(); ev != domain.EvidenceAvailable || cl != domain.EvidenceBytesAbsent {
		t.Fatalf("after evidence store: evidence=%q claim=%q, want available/bytes_absent", ev, cl)
	}

	// Store the claim bytes: both are now available, with no item re-put.
	if _, err := blobs.Put(claimDigest, bytes.NewReader(claimBytes)); err != nil {
		t.Fatalf("put claim bytes: %v", err)
	}
	if ev, cl := read(); ev != domain.EvidenceAvailable || cl != domain.EvidenceAvailable {
		t.Fatalf("after claim store: evidence=%q claim=%q, want both available", ev, cl)
	}
}
