package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func proposalFixture(t *testing.T) (domain.ResolvedPolicy, domain.EffectProposal, []domain.OpaqueSubjectHandle) {
	t.Helper()
	policy, err := domain.NewResolvedPolicy("proposal-policy-run", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	handles := []domain.OpaqueSubjectHandle{domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID))}
	proposal, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handles[0], Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 10, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	return policy, proposal, handles
}

func putProposalPolicy(t *testing.T, ctx context.Context, tx *store.WriteTx, policy domain.ResolvedPolicy) error {
	t.Helper()
	if err := tx.PutRun(ctx, domain.Run{
		ID: policy.RunID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
	}); err != nil {
		return err
	}
	if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
		return err
	}
	declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
		CompletionCriterion: domain.CompletionBoundPRMerged,
		DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
	}, policy.RunID, "project-1", time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC))
	if err != nil {
		return err
	}
	return tx.RecordWorkUnitDeclaration(ctx, declaration)
}

func TestAllocateProposalInstanceOccurrenceIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	policy, proposal, _ := proposalFixture(t)
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	firstKey := domain.ProposalAdmissionKey{
		Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "command-1",
	}
	secondKey := firstKey
	secondKey.SubmissionCommandID = "command-2"

	var first, retry, repeated domain.ProposalInstance
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := putProposalPolicy(t, ctx, tx, policy); err != nil {
			return err
		}
		var inserted bool
		var err error
		first, inserted, err = tx.AllocateProposalInstance(ctx, firstKey, "batch-1", proposal, at)
		if err != nil || !inserted {
			return errors.New("first allocation did not insert")
		}
		retry, inserted, err = tx.AllocateProposalInstance(ctx, firstKey, "batch-1", proposal, at)
		if err != nil || inserted {
			return errors.New("retry did not converge")
		}
		repeated, inserted, err = tx.AllocateProposalInstance(ctx, secondKey, "batch-1", proposal, at)
		if err != nil || !inserted {
			return errors.New("deliberate repeat did not insert")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != retry.ID {
		t.Fatalf("retry ids = %q/%q", first.ID, retry.ID)
	}
	if repeated.ID == first.ID {
		t.Fatalf("new command reused instance id %q", first.ID)
	}
	if repeated.Proposal.Digest != first.Proposal.Digest {
		t.Fatal("semantic collision fixture did not retain one digest")
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		batch, err := tx.ListProposalBatch(ctx, "batch-1")
		if err != nil {
			return err
		}
		if len(batch) != 2 {
			t.Fatalf("batch size = %d, want 2", len(batch))
		}
		got, err := tx.GetProposalInstance(ctx, first.ID)
		if err == nil && got.ID != first.ID {
			t.Fatalf("got id = %q", got.ID)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestAllocateProposalInstanceRefusesOccurrenceRewrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	policy, proposal, handles := proposalFixture(t)
	key := domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "event-1"}
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := putProposalPolicy(t, ctx, tx, policy); err != nil {
			return err
		}
		_, _, err := tx.AllocateProposalInstance(ctx, key, "batch-1", proposal, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	revised, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handles[0], Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 20, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *store.WriteTx) error {
		_, _, err := tx.AllocateProposalInstance(ctx, key, "batch-1", revised, at)
		return err
	})
	if !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("rewrite error = %v, want ErrImmutableConflict", err)
	}
}

func TestAllocateProposalInstanceUsesSubjectPolicyNotProposalClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	current, _, handles := proposalFixture(t)
	historical, err := domain.NewResolvedPolicy("historical-policy-run", current.Keys)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: handles[0], Intent: domain.RunProposalIntentImplement,
		ExpectedCostUnits: 10, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, historical)
	if err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *store.WriteTx) error {
		if err := putProposalPolicy(t, ctx, tx, current); err != nil {
			return err
		}
		if err := putProposalPolicy(t, ctx, tx, historical); err != nil {
			return err
		}
		_, _, err := tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "stale-policy"},
			"batch-1", proposal, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
		return err
	})
	if !errors.Is(err, domain.ErrProposalPolicyMismatch) {
		t.Fatalf("stale proposal policy error = %v, want ErrProposalPolicyMismatch", err)
	}
}

func TestProposalBuiltInRecipeBindsAttentionItem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	policy, proposal, _ := proposalFixture(t)
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	key := domain.ProposalAdmissionKey{Source: domain.ProposalSourceClientCommand, SubmissionCommandID: "command-1"}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := putProposalPolicy(t, ctx, tx, policy); err != nil {
			return err
		}
		instance, _, err := tx.AllocateProposalInstance(ctx, key, "batch-1", proposal, at)
		if err != nil {
			return err
		}
		artifact, err := instance.EvidenceArtifact()
		if err != nil {
			return err
		}
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: domain.ItemID(instance.ID), ProjectID: "project-1",
			Subject: domain.Subject{Type: domain.SubjectProposalBatch, ID: "batch-1"},
			Type:    domain.AttentionRunProposal, Priority: domain.PriorityNormal,
			Reason:            "start the accepted work",
			RequestedDecision: []domain.Action{domain.ActionStart, domain.ActionStartWithChanges, domain.ActionDecline, domain.ActionSnooze},
			EvidenceSnapshot:  []domain.Artifact{artifact}, ItemVersion: 1,
			InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
		}, map[domain.Digest]bool{domain.EffectProposalRecipeDigest: true})
		if err != nil {
			return err
		}
		if len(item.ArtifactDigests) != 1 || item.ArtifactDigests[0] != proposal.Digest {
			t.Fatalf("item binding = %v", item.ArtifactDigests)
		}
		return tx.PutAttentionItem(ctx, item)
	})
	if err != nil {
		t.Fatal(err)
	}
}
