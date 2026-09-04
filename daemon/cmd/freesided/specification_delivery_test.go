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
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestProductionSpecificationDeliveryValidatorRejectsPromptOverflow(t *testing.T) {
	st, blobs, run, promptDigest := deliveryValidatorFixture(t, true)
	priorBody := []byte(strings.Repeat("r", 32<<10))
	putDeliveryArtifact(t, st, blobs, "research-prompt-overflow", priorBody)
	materializer, err := productionMaterializer(blobs)
	if err != nil {
		t.Fatal(err)
	}
	err = productionSpecificationDeliveryValidator(materializer)(t.Context(),
		deliveryStartSpec(t, run, promptDigest, []domain.Digest{
			domain.Digest(contentaddr.Sum(priorBody)),
		}))
	if !errors.Is(err, engine.ErrSpecificationInputUndeliverable) ||
		!errors.Is(err, claude.ErrUnsupportedStart) {
		t.Fatalf("prompt overflow = %v, want durable undeliverable Claude input", err)
	}
}

func TestProductionSpecificationDeliveryValidatorRejectsAggregateOverflow(t *testing.T) {
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
	err = productionSpecificationDeliveryValidator(materializer)(t.Context(),
		deliveryStartSpec(t, run, promptDigest, prior))
	if !errors.Is(err, engine.ErrSpecificationInputUndeliverable) ||
		!errors.Is(err, exec.ErrInputTooLarge) {
		t.Fatalf("aggregate overflow = %v, want durable undeliverable materialized input", err)
	}
}

func TestProductionRemediationDeliveryValidatorRejectsPromptOverflow(t *testing.T) {
	st, blobs, run, promptDigest := deliveryValidatorFixture(t, true)
	priorBody := []byte(strings.Repeat("r", 32<<10))
	putDeliveryArtifact(t, st, blobs, "remediation-prompt-overflow", priorBody)
	materializer, err := productionMaterializer(blobs)
	if err != nil {
		t.Fatal(err)
	}
	err = productionImplementationDeliveryValidator(materializer)(t.Context(),
		deliveryStartSpec(t, run, promptDigest, []domain.Digest{
			domain.Digest(contentaddr.Sum(priorBody)),
		}))
	if !errors.Is(err, engine.ErrProductionInputUndeliverable) ||
		!errors.Is(err, claude.ErrUnsupportedStart) {
		t.Fatalf("prompt overflow = %v, want durable undeliverable Claude input", err)
	}
}

func TestProductionImplementationDeliveryValidatorRejectsInitialPromptOverflow(t *testing.T) {
	_, blobs, run, promptDigest := deliveryValidatorFixture(t, false)
	largeSpec := []byte(strings.Repeat("s", 32<<10))
	run.SpecDigest = domain.Digest(contentaddr.Sum(largeSpec))
	if _, err := blobs.Put(run.SpecDigest, bytes.NewReader(largeSpec)); err != nil {
		t.Fatal(err)
	}
	materializer, err := productionMaterializer(blobs)
	if err != nil {
		t.Fatal(err)
	}
	err = productionImplementationDeliveryValidator(materializer)(t.Context(),
		deliveryStartSpec(t, run, promptDigest, nil))
	if !errors.Is(err, engine.ErrProductionInputUndeliverable) ||
		!errors.Is(err, claude.ErrUnsupportedStart) {
		t.Fatalf("initial prompt overflow = %v, want durable undeliverable Claude input", err)
	}
}

func TestProductionRemediationDeliveryValidatorRejectsAggregateOverflow(t *testing.T) {
	st, blobs, run, promptDigest := deliveryValidatorFixture(t, true)
	body := bytes.Repeat([]byte("r"), int(exec.ProductionMaxInputBytes))
	prior := make([]domain.Digest, 9)
	for i := range prior {
		putDeliveryArtifact(t, st, blobs,
			domain.ArtifactID("remediation-aggregate-"+string(rune('a'+i))), body)
		prior[i] = domain.Digest(contentaddr.Sum(body))
	}
	materializer, err := productionMaterializer(blobs)
	if err != nil {
		t.Fatal(err)
	}
	err = productionImplementationDeliveryValidator(materializer)(t.Context(),
		deliveryStartSpec(t, run, promptDigest, prior))
	if !errors.Is(err, engine.ErrProductionInputUndeliverable) ||
		!errors.Is(err, exec.ErrInputTooLarge) {
		t.Fatalf("aggregate overflow = %v, want durable undeliverable materialized input", err)
	}
}

func TestStoreAdmissionAuthorityRejectsMissingSpecificationMarker(t *testing.T) {
	st, _, _, _ := deliveryValidatorFixture(t, true)
	authority := storeAdmissionAuthority{store: st}
	specification, err := authority.authenticateSpecificationInvocation(
		t.Context(), "inv-specify-run-missing-2", domain.ExecutionAdmission{
			RunID: "run-missing", StageID: "specify-run-missing",
		},
	)
	if !specification || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing specification marker = specification %t, error %v; want fail-closed ErrNotFound",
			specification, err)
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
	st := storetest.Open(t, root+"/state.db", store.Options{})
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
		ID: "specification-run", ProjectID: "project-1",
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
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: "inv-specify-test-1",
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: testRunEvidenceMetadata(int64(len(body))),
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
