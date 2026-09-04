package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// agentClaimFixtures builds an invocation and a two-claim set (an image claim
// and a text claim, the latter with its digest bound to its content) for the
// agent-claims store tests. The claims are content-valid, so a rewrite variant
// that mutates one still passes AgentClaim.Validate and reaches putImmutable's
// byte-identity check rather than failing validation first.
func agentClaimFixtures() (domain.AgentInvocation, []domain.AgentClaim, domain.ClaimText) {
	invocation := domain.AgentInvocation{ID: "inv-1", InputIDs: []domain.ArtifactID{"art-1"}}
	claimText := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown,
		Content:   "All checks green; the diff touches only docs.",
	}
	agentProvenance := domain.Provenance{
		ProducerClass:        domain.ProducerAgent,
		ProducerInvocationID: invocation.ID,
		HeadBinding:          domain.HeadBound,
		SourceHeadSHA:        "cafebabe",
		SensitivityClass:     domain.SensitivityNormal,
	}
	claims := []domain.AgentClaim{
		{
			Label: "screenshot", Artifact: "art-2", Digest: "sha256:img",
			Provenance: agentProvenance,
			Metadata:   claimMeta(domain.EvidenceMediaImagePNG),
		},
		{
			Label: "change summary", Artifact: "art-3", Digest: claimText.ComputeDigest(),
			Text: &claimText, Provenance: agentProvenance,
			Metadata: claimTextMeta(claimText),
		},
	}
	return invocation, claims, claimText
}

// TestAgentClaimsRoundTripAcrossReopen: PutAgentClaims then GetAgentClaims
// returns the content-identical claim set bound to the correct invocation, and
// the binding survives a store reopen (durable state is all the reconstruction
// may rely on). Two invocations with distinct claim sets prove the record keys
// by invocation, not a global bag.
func TestAgentClaimsRoundTripAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := tempDBPath(t)

	invA, claimsA, _ := agentClaimFixtures()
	invB := domain.AgentInvocation{ID: "inv-2", InputIDs: []domain.ArtifactID{"art-9"}}
	claimsB := []domain.AgentClaim{{
		Label: "log", Artifact: "art-9", Digest: "sha256:otherimg",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: invB.ID,
			HeadBinding: domain.HeadBound, SourceHeadSHA: "deadbeef",
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: claimMeta(domain.EvidenceMediaImagePNG),
	}}

	s := storetest.Open(t, path, store.Options{})
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		// The invocation rows come first: PutAgentClaims foreign-keys to them.
		if err := tx.PutAgentInvocation(ctx, invA); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invB); err != nil {
			return err
		}
		if err := tx.PutAgentClaims(ctx, invA.ID, claimsA); err != nil {
			return err
		}
		return tx.PutAgentClaims(ctx, invB.ID, claimsB)
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openStoreAt(t, path, store.Options{})
	cases := []struct {
		name string
		id   domain.InvocationID
		want []domain.AgentClaim
	}{
		{"inv-1", invA.ID, claimsA},
		{"inv-2", invB.ID, claimsB},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []domain.AgentClaim
			if err := reopened.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				got, err = tx.GetAgentClaims(ctx, tc.id)
				return err
			}); err != nil {
				t.Fatalf("GetAgentClaims: %v", err)
			}
			if string(marshalIndent(t, got)) != string(marshalIndent(t, tc.want)) {
				t.Fatalf("round-trip mismatch:\ngot:  %s\nwant: %s",
					marshalIndent(t, got), marshalIndent(t, tc.want))
			}
		})
	}
}

// TestAgentClaimsIdempotentAndConflict: a byte-identical replay converges on the
// existing row, while any differing set is refused as an ErrImmutableConflict.
// The canonical encoding is order-sensitive by identity (#381), so each axis a
// claim set can differ on (label, digest, membership, text content, order) is a
// distinct conflict, never a silent overwrite.
func TestAgentClaimsIdempotentAndConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	invocation, claims, _ := agentClaimFixtures()

	seed := func(tx *store.WriteTx) error {
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		return tx.PutAgentClaims(ctx, invocation.ID, claims)
	}

	// A text claim that displays different content under a matching digest, for
	// the text-content axis: the digest is recomputed so the claim stays valid
	// and the difference is a genuine body difference, not a validation failure.
	rewrittenText := domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: "Different summary."}

	cases := []struct {
		name    string
		rewrite []domain.AgentClaim
	}{
		{"label", func() []domain.AgentClaim {
			c := append([]domain.AgentClaim(nil), claims...)
			c[0].Label = "renamed"
			return c
		}()},
		{"digest", func() []domain.AgentClaim {
			c := append([]domain.AgentClaim(nil), claims...)
			c[0].Digest = "sha256:other"
			return c
		}()},
		{"membership", []domain.AgentClaim{claims[0]}},
		{"text content", func() []domain.AgentClaim {
			c := append([]domain.AgentClaim(nil), claims...)
			c[1].Text = &rewrittenText
			c[1].Digest = rewrittenText.ComputeDigest()
			c[1].Metadata.SizeBytes = int64(len(rewrittenText.Content))
			return c
		}()},
		{"order", []domain.AgentClaim{claims[1], claims[0]}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t, store.Options{})
			if err := s.Write(ctx, seed); err != nil {
				t.Fatalf("seed: %v", err)
			}
			// Byte-identical replay converges without error.
			if err := s.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAgentClaims(ctx, invocation.ID, claims)
			}); err != nil {
				t.Fatalf("identical replay errored, want idempotent success: %v", err)
			}
			err := s.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutAgentClaims(ctx, invocation.ID, tc.rewrite)
			})
			if !errors.Is(err, store.ErrImmutableConflict) {
				t.Fatalf("rewrite error = %v, want ErrImmutableConflict", err)
			}
		})
	}
}

// TestAgentClaimsForeignKeyRequiresInvocation: the record foreign-keys to the
// invocation, so writing claims for an invocation that was never persisted is
// refused rather than orphaned.
func TestAgentClaimsForeignKeyRequiresInvocation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, claims, _ := agentClaimFixtures()

	s := openStore(t, store.Options{})
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAgentClaims(ctx, "inv-absent", claims)
	})
	if err == nil {
		t.Fatal("PutAgentClaims for an absent invocation succeeded, want a foreign-key error")
	}
}

// TestAgentClaimsNotFound: a Get for an invocation with no claims row surfaces
// the store's not-found sentinel, not an empty success.
func TestAgentClaimsNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetAgentClaims(ctx, "inv-missing")
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetAgentClaims error = %v, want ErrNotFound", err)
	}
}
