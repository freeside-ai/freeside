package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/contract"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// artifactStoreHarness binds the production artifactStore adapter to the shared
// Artifacts.RecordClaims contract, holding it to the same write-once semantics as
// the in-memory fake. The store adapter foreign-keys the claim record to an
// invocation row, so PrepareInvocation seeds one first.
type artifactStoreHarness struct {
	store   *store.Store
	adapter artifactStore
}

func newArtifactStoreHarness(t *testing.T) contract.ArtifactsHarness {
	t.Helper()
	return newArtifactStoreFixture(t)
}

func newArtifactStoreFixture(t *testing.T) *artifactStoreHarness {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(t.Context(), root+"/state.db", store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(root + "/blobs")
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	return &artifactStoreHarness{store: st, adapter: artifactStore{blobs: blobs, store: st}}
}

func (h *artifactStoreHarness) PrepareInvocation(t *testing.T, id domain.InvocationID) {
	t.Helper()
	err := h.store.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutAgentInvocation(t.Context(),
			domain.AgentInvocation{ID: id, InputIDs: []domain.ArtifactID{"art-input"}})
	})
	if err != nil {
		t.Fatalf("seed invocation %q: %v", id, err)
	}
}

func (h *artifactStoreHarness) RecordClaims(
	t *testing.T, id domain.InvocationID, claims []domain.AgentClaim,
) error {
	return h.adapter.RecordClaims(t.Context(), id, claims)
}

func (h *artifactStoreHarness) ReadClaims(
	t *testing.T, id domain.InvocationID,
) ([]domain.AgentClaim, bool, error) {
	t.Helper()
	return h.adapter.LookupClaims(t.Context(), id)
}

func (h *artifactStoreHarness) IsConflict(err error) bool {
	return errors.Is(err, store.ErrImmutableConflict)
}

func readAgentClaims(
	ctx context.Context, st *store.Store, id domain.InvocationID,
) ([]domain.AgentClaim, error) {
	var claims []domain.AgentClaim
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		claims, err = tx.GetAgentClaims(ctx, id)
		return err
	})
	return claims, err
}

func TestArtifactStoreRecordClaimsContract(t *testing.T) {
	t.Parallel()
	contract.RunArtifactsContract(t, contract.ArtifactsFactory{New: newArtifactStoreHarness})
}

// adapterClaimSet is the production-side fixture: an image claim and a text claim
// whose digest binds its inline content, so a lossy record (dropping the label or
// the text) fails the readback. Provenance names id as its producer.
func adapterClaimSet(id domain.InvocationID) []domain.AgentClaim {
	text := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown,
		Content:   "All checks green; the diff touches only docs.",
	}
	provenance := domain.Provenance{
		ProducerClass:        domain.ProducerAgent,
		ProducerInvocationID: id,
		HeadBinding:          domain.HeadBound,
		SourceHeadSHA:        "cafebabe",
		SensitivityClass:     domain.SensitivityNormal,
	}
	return []domain.AgentClaim{
		{
			Label: "screenshot", Artifact: "art-image", Digest: "sha256:img", Provenance: provenance,
			Metadata: testClaimEvidenceMetadata(domain.EvidenceMediaImagePNG),
		},
		{
			Label: "change summary", Artifact: "art-text", Digest: text.ComputeDigest(),
			Text: &text, Provenance: provenance,
			Metadata: testClaimTextEvidenceMetadata(text),
		},
	}
}

func sameClaimBodies(t *testing.T, want, got []domain.AgentClaim) {
	t.Helper()
	wantBody, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal expected claims: %v", err)
	}
	gotBody, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal read-back claims: %v", err)
	}
	if string(wantBody) != string(gotBody) {
		t.Fatalf("claim set mismatch:\nwant: %s\ngot:  %s", wantBody, gotBody)
	}
}

// TestRecordClaimsPersistsClaimRecordAndArtifactRows: a single RecordClaims lands
// the complete labeled claim record (label, provenance, and inline text intact)
// and every claim's artifact row in one transaction, so the record is the durable
// review surface, not the artifact rows alone.
func TestRecordClaimsPersistsClaimRecordAndArtifactRows(t *testing.T) {
	t.Parallel()
	h := newArtifactStoreFixture(t)
	id := domain.InvocationID("inv-adapter-complete")
	h.PrepareInvocation(t, id)
	claims := adapterClaimSet(id)

	if err := h.adapter.RecordClaims(t.Context(), id, claims); err != nil {
		t.Fatalf("RecordClaims: %v", err)
	}

	got, found, err := h.ReadClaims(t, id)
	if err != nil {
		t.Fatalf("read claim record: %v", err)
	}
	if !found {
		t.Fatal("claim record missing after RecordClaims")
	}
	sameClaimBodies(t, claims, got)

	for _, claim := range claims {
		var artifact domain.Artifact
		err := h.store.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			artifact, err = tx.GetArtifact(t.Context(), claim.Artifact)
			return err
		})
		if err != nil {
			t.Fatalf("artifact row %q missing: %v", claim.Artifact, err)
		}
		if artifact.Digest != claim.Digest {
			t.Fatalf("artifact %q digest = %q, want %q", claim.Artifact, artifact.Digest, claim.Digest)
		}
	}
}

// TestRecordClaimsSurvivesStoreReopen: the claim record is durable, not
// process-local. It reads back content-identical after the store is closed and
// reopened at the same path.
func TestRecordClaimsSurvivesStoreReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	path := root + "/state.db"
	id := domain.InvocationID("inv-adapter-reopen")
	claims := adapterClaimSet(id)

	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	blobs, err := signet.NewBlobStore(root + "/blobs")
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	adapter := artifactStore{blobs: blobs, store: st}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAgentInvocation(ctx,
			domain.AgentInvocation{ID: id, InputIDs: []domain.ArtifactID{"art-input"}})
	}); err != nil {
		t.Fatalf("seed invocation: %v", err)
	}
	if err := adapter.RecordClaims(ctx, id, claims); err != nil {
		t.Fatalf("RecordClaims: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	reopened, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	got, err := readAgentClaims(ctx, reopened, id)
	if err != nil {
		t.Fatalf("read claim record after reopen: %v", err)
	}
	sameClaimBodies(t, claims, got)
}

// TestRecordClaimsIsAtomicAndConverges: the claim record and its artifact rows
// share one transaction, so a failure leaves neither, and RecordClaims never
// reports success for a partial record. The evidence blobs the pipeline persists
// before the record transaction stay resolvable across that failure, so a re-run
// converges on a complete record.
func TestRecordClaimsIsAtomicAndConverges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, root+"/state.db", store.Options{})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(root + "/blobs")
	if err != nil {
		t.Fatalf("open blob store: %v", err)
	}
	adapter := artifactStore{blobs: blobs, store: st}
	id := domain.InvocationID("inv-adapter-atomic")
	claims := adapterClaimSet(id)

	// The evidence blob is content-addressed and persisted before the record
	// transaction, exactly as persistEvidence does. Its survival across the failed
	// record write below is what lets the re-run converge.
	evidence := []byte("evidence bytes the claim would name")
	evidenceDigest := domain.Digest(contentaddr.Sum(evidence))
	if err := adapter.PutBlob(ctx, evidenceDigest, evidence); err != nil {
		t.Fatalf("PutBlob: %v", err)
	}

	// Without the invocation row the claim-record write foreign-keys and rolls the
	// whole transaction back, so no artifact row leaks and no partial record is
	// reported successful.
	if err := adapter.RecordClaims(ctx, id, claims); err == nil {
		t.Fatal("RecordClaims without an invocation row succeeded, want a foreign-key failure")
	}
	for _, claim := range claims {
		err := st.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetArtifact(ctx, claim.Artifact)
			return err
		})
		if !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("artifact %q survived a rolled-back RecordClaims: %v", claim.Artifact, err)
		}
	}
	if _, err := readAgentClaims(ctx, st, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("claim record survived a rolled-back RecordClaims: %v", err)
	}
	blob, err := blobs.OpenContext(ctx, evidenceDigest)
	if err != nil {
		t.Fatalf("evidence blob lost across the failed record write: %v", err)
	}
	_ = blob.Close()

	// The re-run, once the invocation row exists, completes the record and its
	// artifact rows together.
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAgentInvocation(ctx,
			domain.AgentInvocation{ID: id, InputIDs: []domain.ArtifactID{"art-input"}})
	}); err != nil {
		t.Fatalf("seed invocation for re-run: %v", err)
	}
	if err := adapter.RecordClaims(ctx, id, claims); err != nil {
		t.Fatalf("RecordClaims re-run: %v", err)
	}
	got, err := readAgentClaims(ctx, st, id)
	if err != nil {
		t.Fatalf("read claim record after re-run: %v", err)
	}
	sameClaimBodies(t, claims, got)
	for _, claim := range claims {
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetArtifact(ctx, claim.Artifact)
			return err
		}); err != nil {
			t.Fatalf("artifact %q missing after re-run: %v", claim.Artifact, err)
		}
	}
}
