package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestProductionElaborationDeliveryValidatorRejectsPromptOverflow(t *testing.T) {
	st, blobs, run, promptDigest := deliveryValidatorFixture(t, true)
	priorBody := []byte(strings.Repeat("r", 32<<10))
	putDeliveryArtifact(t, st, blobs, "research-prompt-overflow", priorBody)
	materializer, err := productionMaterializer(blobs)
	if err != nil {
		t.Fatal(err)
	}
	err = productionElaborationDeliveryValidator(materializer)(t.Context(),
		deliveryStartSpec(t, run, promptDigest, []domain.Digest{
			domain.Digest(contentaddr.Sum(priorBody)),
		}))
	if !errors.Is(err, engine.ErrElaborationInputUndeliverable) ||
		!errors.Is(err, claude.ErrUnsupportedStart) {
		t.Fatalf("prompt overflow = %v, want durable undeliverable Claude input", err)
	}
}

func TestProductionElaborationDeliveryValidatorRejectsAggregateOverflow(t *testing.T) {
	st, blobs, run, promptDigest := deliveryValidatorFixture(t, true)
	body := bytes.Repeat([]byte("r"), int(exec.ProductionMaxInputBytes))
	prior := make([]domain.Digest, 9)
	for i := range prior {
		putDeliveryArtifact(t, st, blobs,
			domain.ArtifactID("research-aggregate-"+string(rune('a'+i))), body)
		prior[i] = domain.Digest(contentaddr.Sum(body))
	}
	materializer, err := productionMaterializer(blobs)
	if err != nil {
		t.Fatal(err)
	}
	err = productionElaborationDeliveryValidator(materializer)(t.Context(),
		deliveryStartSpec(t, run, promptDigest, prior))
	if !errors.Is(err, engine.ErrElaborationInputUndeliverable) ||
		!errors.Is(err, exec.ErrInputTooLarge) {
		t.Fatalf("aggregate overflow = %v, want durable undeliverable materialized input", err)
	}
}

func TestStoreAdmissionAuthorityRejectsMissingElaborationMarker(t *testing.T) {
	st, _, _, _ := deliveryValidatorFixture(t, true)
	authority := storeAdmissionAuthority{store: st}
	elaboration, err := authority.authenticateElaborationInvocation(
		t.Context(), "inv-elaborate-run-missing-2", domain.ExecutionAdmission{
			RunID: "run-missing", StageID: "elaborate-run-missing",
		},
	)
	if !elaboration || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing elaboration marker = elaboration %t, error %v; want fail-closed ErrNotFound",
			elaboration, err)
	}
}

func deliveryStartSpec(
	t *testing.T, run domain.Run, promptDigest domain.Digest, prior []domain.Digest,
) exec.StartSpec {
	t.Helper()
	inputDigest := domain.Digest(contentaddr.Sum([]byte("delivery binding")))
	snapshot, err := domain.NewStageInputSnapshot(domain.StageInputSnapshotInput{
		InputDigest: inputDigest, SpecificationDigest: run.SpecDigest,
		PromptPackageDigest: promptDigest, PolicyDigest: run.PolicyDigest,
		PriorArtifactDigests: prior, ImageInputDigests: []domain.Digest{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return exec.StartSpec{
		InputDigest: inputDigest, SpecDigest: run.SpecDigest,
		PolicyDigest: run.PolicyDigest, StageInputs: &snapshot,
	}
}

func deliveryValidatorFixture(
	t *testing.T, renderPriors bool,
) (*store.Store, *signet.BlobStore, domain.Run, domain.Digest) {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(t.Context(), root+"/state.db", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(root + "/blobs")
	if err != nil {
		t.Fatal(err)
	}
	put := func(body []byte) domain.Digest {
		t.Helper()
		digest := domain.Digest(contentaddr.Sum(body))
		if _, err := blobs.Put(digest, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		return digest
	}
	specDigest := put([]byte("specification"))
	policyDigest := put([]byte("policy"))
	prompt := []byte("prompt")
	if renderPriors {
		prompt = []byte("<!-- freeside:render-prior-artifacts=v1 -->\nprompt")
	}
	promptDigest := put(prompt)
	return st, blobs, domain.Run{
		ID: "elaboration-run", ProjectID: "project-1",
		SpecDigest: specDigest, PolicyDigest: policyDigest,
	}, promptDigest
}

func putDeliveryArtifact(
	t *testing.T,
	st *store.Store,
	blobs *signet.BlobStore,
	id domain.ArtifactID,
	body []byte,
) domain.ArtifactID {
	t.Helper()
	digest := domain.Digest(contentaddr.Sum(body))
	if _, err := blobs.Put(digest, bytes.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: id, Type: domain.ArtifactKindResearch, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: "inv-elaborate-test-1",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutArtifact(t.Context(), artifact)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}
