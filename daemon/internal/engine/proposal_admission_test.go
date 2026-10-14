package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
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
		Kind: domain.EffectTaskProposal,
		Parameters: domain.TaskProposalParameters{
			SubjectHandle:     domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID)),
			Intent:            domain.TaskProposalIntentImplement,
			ExpectedCostUnits: 10, Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
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

// newProposalAdmissionEngine composes the artifact store production wiring
// supplies, since admission persists the proposal's evidence body.
func newProposalAdmissionEngine(t *testing.T, st *store.Store) *Engine {
	t.Helper()
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{store: st, artifacts: blobs}
}

func TestProposalAdmissionRetryAndDeliberateRepeat(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	engine := newProposalAdmissionEngine(t, st)
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	request := proposalAdmissionFixture(t, "command-1")
	seedProposalAdmissionPolicy(t, ctx, st, request)
	first, err := engine.admitProposalAt(ctx, request, at)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := engine.artifacts.(*signet.BlobStore).Verify(first.Instance.Proposal.Digest); err != nil || !ok {
		t.Fatalf("admitted proposal evidence body verifies = %v, %v; want true", ok, err)
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
	first, err := newProposalAdmissionEngine(t, firstStore).admitProposalAt(ctx, request, at)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstStore.Close(); err != nil {
		t.Fatal(err)
	}

	recoveredStore := storetest.Open(t, path, store.Options{})
	recovered, err := newProposalAdmissionEngine(t, recoveredStore).admitProposalAt(ctx, request, at.Add(time.Hour))
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
	result, err := newProposalAdmissionEngine(t, st).admitProposalAt(ctx, request, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
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
	engine := newProposalAdmissionEngine(t, st)
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
	parameters := request.Parameters.(domain.TaskProposalParameters)
	parameters.Scope.DeclaredPathCount++
	request.Parameters = parameters
	if _, err := newProposalAdmissionEngine(t, st).admitProposalAt(ctx, request, time.Now().UTC()); !errors.Is(err, domain.ErrEffectProposalInconsistent) {
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

// Admission writes the evidence carrier only after its body, so an engine with
// no artifact store refuses rather than committing a row whose bytes a backup
// can never verify; the unchanged revision shows the whole transaction rolled
// back.
func TestProposalAdmissionWithoutArtifactStoreFailsClosed(t *testing.T) {
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	request := proposalAdmissionFixture(t, "command-no-artifacts")
	seedProposalAdmissionPolicy(t, ctx, st, request)
	before, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&Engine{store: st}).admitProposalAt(ctx, request, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
	if !errors.Is(err, signet.ErrAttachmentsUnavailable) {
		t.Fatalf("admission without an artifact store error = %v, want ErrAttachmentsUnavailable", err)
	}
	after, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("refused admission revision = %d, want %d", after.Revision, before.Revision)
	}
}

// An admitted task proposal's evidence carrier verifies in a real local
// checkpoint's artifact closure, and stops verifying once its body is damaged.
func TestProposalAdmissionEvidenceBackupClosure(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "freeside.db")
	blobs, err := signet.NewBlobStore(path + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	files, err := store.NewDefaultLocalBackupFiles(path)
	if err != nil {
		t.Fatal(err)
	}
	source, err := files.NewCheckpointHealthSource(blobs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, path, store.Options{BackupHealthSource: source})
	request := proposalAdmissionFixture(t, "command-backup")
	seedProposalAdmissionPolicy(t, ctx, st, request)
	result, err := (&Engine{store: st, artifacts: blobs}).admitProposalAt(
		ctx, request, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	producer, err := files.NewProducer(st)
	if err != nil {
		t.Fatal(err)
	}
	closure := func() domain.BackupHealthStatus {
		t.Helper()
		if err := producer.Maintain(ctx); err != nil {
			t.Fatalf("Maintain: %v", err)
		}
		health, err := st.BackupHealth(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return health.ArtifactClosure
	}
	if got := closure(); got != domain.BackupHealthHealthy {
		t.Fatalf("artifact closure after admission = %q, want healthy", got)
	}
	body := filepath.Join(path+".blobs", "sha256-"+contentaddr.Hex(string(result.Instance.Proposal.Digest)))
	if err := os.Chmod(body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(body, 1); err != nil {
		t.Fatal(err)
	}
	if got := closure(); got == domain.BackupHealthHealthy {
		t.Fatal("artifact closure over a truncated proposal body is healthy")
	}
}
