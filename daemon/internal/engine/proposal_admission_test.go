package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func proposalAdmissionFixture(t *testing.T, commandID string) ProposalAdmission {
	t.Helper()
	policy := proposalAdmissionPolicy(t)
	return ProposalAdmission{
		ProjectID: "project-1", ProposalBatchID: "batch-1",
		AdmissionKey: domain.ProposalAdmissionKey{
			Source: domain.ProposalSourceClientCommand, SubmissionCommandID: commandID,
		},
		Kind: domain.EffectRunProposal,
		Parameters: domain.RunProposalParameters{
			SubjectHandle:     domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID)),
			Intent:            domain.RunProposalIntentImplement,
			ExpectedCostUnits: 10, Scope: domain.RunProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
		},
	}
}

func proposalAdmissionPolicy(t *testing.T) domain.ResolvedPolicy {
	t.Helper()
	policy, err := domain.NewResolvedPolicy("proposal-policy-run", []domain.PolicyKey{{
		Key: "rein", Value: "loose", Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset,
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}, {
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func seedProposalAdmissionPolicy(t *testing.T, ctx context.Context, st *store.Store, request ProposalAdmission) {
	t.Helper()
	policy := proposalAdmissionPolicy(t)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: request.ProjectID,
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, request.ProjectID, time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC))
		if err != nil {
			return err
		}
		return tx.RecordWorkUnitDeclaration(ctx, declaration)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProposalAdmissionRetryAndDeliberateRepeat(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	engine := &Engine{store: st}
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	request := proposalAdmissionFixture(t, "command-1")
	seedProposalAdmissionPolicy(t, ctx, st, request)
	first, err := engine.admitProposalAt(ctx, request, at)
	if err != nil {
		t.Fatal(err)
	}
	beforeRetry, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := engine.admitProposalAt(ctx, request, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	afterRetry, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Instance.ID != first.Instance.ID || retry.Item.ID != first.Item.ID {
		t.Fatalf("retry identities = %q/%q, want %q/%q", retry.Instance.ID, retry.Item.ID, first.Instance.ID, first.Item.ID)
	}
	if afterRetry.Revision != beforeRetry.Revision {
		t.Fatalf("retry revision = %d, want %d", afterRetry.Revision, beforeRetry.Revision)
	}

	repeatRequest := proposalAdmissionFixture(t, "command-2")
	repeat, err := engine.admitProposalAt(ctx, repeatRequest, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if repeat.Instance.ID == first.Instance.ID || repeat.Instance.Proposal.Digest != first.Instance.Proposal.Digest {
		t.Fatalf("deliberate repeat = id %q digest %q, first id %q digest %q",
			repeat.Instance.ID, repeat.Instance.Proposal.Digest, first.Instance.ID, first.Instance.Proposal.Digest)
	}
}

func TestProposalAdmissionCrashRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	request := proposalAdmissionFixture(t, "command-crash")
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	firstStore := storetest.Open(t, path, store.Options{})
	seedProposalAdmissionPolicy(t, ctx, firstStore, request)
	first, err := (&Engine{store: firstStore}).admitProposalAt(ctx, request, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}

	recoveredStore := storetest.Open(t, path, store.Options{})
	recovered, err := (&Engine{store: recoveredStore}).admitProposalAt(ctx, request, at.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Instance.ID != first.Instance.ID || recovered.Instance.CreatedAt != first.Instance.CreatedAt {
		t.Fatalf("recovered instance = %#v, want %#v", recovered.Instance, first.Instance)
	}
}

func TestProposalAdmissionGateUsesResolvedPolicyNotRein(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	request := proposalAdmissionFixture(t, "command-1")
	seedProposalAdmissionPolicy(t, ctx, st, request)
	result, err := (&Engine{store: st}).admitProposalAt(ctx, request, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("rein preset was treated as authority: %v", err)
	}
	changedPolicy := proposalAdmissionPolicy(t)
	changedPolicy.Digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	if err := domain.GateEffectProposal(result.Instance.Proposal, changedPolicy); !errors.Is(err, domain.ErrPolicyDigestMismatch) {
		t.Fatalf("forged resolved policy error = %v, want digest mismatch", err)
	}
}

func TestProposalAdmissionRejectsUnregisteredOrForeignWorkUnit(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	request := proposalAdmissionFixture(t, "command-unregistered")
	engine := &Engine{store: st}
	if _, err := engine.admitProposalAt(ctx, request, time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unregistered subject error = %v, want ErrNotFound", err)
	}
	seedProposalAdmissionPolicy(t, ctx, st, request)
	request.ProjectID = "project-other"
	if _, err := engine.admitProposalAt(ctx, request, time.Now().UTC()); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("foreign subject error = %v, want ErrParentKeyMismatch", err)
	}
}

func TestProposalAdmissionRejectsScopeOutsideDurableDeclaration(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	request := proposalAdmissionFixture(t, "command-scope-mismatch")
	seedProposalAdmissionPolicy(t, ctx, st, request)
	before, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	parameters := request.Parameters.(domain.RunProposalParameters)
	parameters.Scope.DeclaredPathCount++
	request.Parameters = parameters
	if _, err := (&Engine{store: st}).admitProposalAt(ctx, request, time.Now().UTC()); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
		t.Fatalf("scope mismatch error = %v, want ErrEffectProposalInconsistent", err)
	}
	after, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("scope mismatch revision = %d, want %d", after.Revision, before.Revision)
	}
}
