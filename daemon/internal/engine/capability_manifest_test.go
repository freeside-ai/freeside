package engine

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestCapabilityManifestForRunRegatesPolicyAndComposition(t *testing.T) {
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	manifest, err := domain.NewCapabilityManifest("Provider web read", domain.EgressProviderWebRead)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal([]domain.CapabilityManifest{manifest})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := domain.NewResolvedPolicy("run-capability", []domain.PolicyKey{{
		Key: domain.CapabilityManifestPolicyKey, Value: string(body),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: "sha256:capability-policy",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: policy.RunID, ProjectID: "project-capability",
		SpecDigest: "sha256:specification", PolicyDigest: policy.Digest,
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, policy)
	}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{store: st, admission: &admitter{environment: AdmissionEnvironment{
		EgressProfile: domain.EgressProviderOnly,
		EnforceableEgressProfiles: []domain.EgressProfile{
			domain.EgressProviderOnly, domain.EgressProviderWebRead,
		},
	}}}
	got, err := e.capabilityManifestForRun(ctx, run, manifest.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if got != manifest {
		t.Fatalf("manifest = %#v, want %#v", got, manifest)
	}
	e.admission.environment.EnforceableEgressProfiles = []domain.EgressProfile{domain.EgressProviderOnly}
	if _, err := e.capabilityManifestForRun(ctx, run, manifest.Digest); err == nil {
		t.Fatal("composition accepted an unenforceable manifest")
	}
}
