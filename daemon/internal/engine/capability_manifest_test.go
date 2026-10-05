package engine

import (
	"encoding/json"
	"path/filepath"
	"slices"
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

// A card offers only the capability choices the retry's admission would
// accept. A provider_registry manifest exposes what the policy declares, so
// without a valid declared set its retry could only hold at admission; it is
// not offered, even when the composition enforces the profile.
func TestOfferableManifestsOmitChoicesAdmissionWouldRefuse(t *testing.T) {
	registry, err := domain.NewCapabilityManifest("Package registries", domain.EgressProviderRegistry)
	if err != nil {
		t.Fatal(err)
	}
	webRead, err := domain.NewCapabilityManifest("Provider web read", domain.EgressProviderWebRead)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal([]domain.CapabilityManifest{registry, webRead})
	if err != nil {
		t.Fatal(err)
	}
	policy := func(t *testing.T, values map[string]string) domain.ResolvedPolicy {
		t.Helper()
		provenance := domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:capability-policy"}
		keys := []domain.PolicyKey{{
			Key: domain.CapabilityManifestPolicyKey, Value: string(body), Provenance: provenance,
		}}
		for key, value := range values {
			keys = append(keys, domain.PolicyKey{Key: key, Value: value, Provenance: provenance})
		}
		resolved, err := domain.NewResolvedPolicy("run-offer", keys)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	all := []domain.EgressProfile{
		domain.EgressProviderOnly, domain.EgressProviderRegistry, domain.EgressProviderWebRead,
	}
	declared := map[string]string{domain.RegistrySetPolicyKey: `["pypi.org"]`}
	for name, tc := range map[string]struct {
		enforceable []domain.EgressProfile
		policy      map[string]string
		admitted    domain.EgressProfile
		want        []domain.CapabilityManifestOffer
	}{
		"a declared set offers the registry manifest": {
			all, declared, domain.EgressProviderOnly,
			[]domain.CapabilityManifestOffer{registry.Offer(), webRead.Offer()},
		},
		"no declared set withholds the registry manifest": {
			all, nil, domain.EgressProviderOnly,
			[]domain.CapabilityManifestOffer{webRead.Offer()},
		},
		"a malformed set withholds the registry manifest": {
			all,
			map[string]string{domain.RegistrySetPolicyKey: `["PyPI.org"]`},
			domain.EgressProviderOnly,
			[]domain.CapabilityManifestOffer{webRead.Offer()},
		},
		"an unenforceable profile is withheld": {
			[]domain.EgressProfile{domain.EgressProviderOnly, domain.EgressProviderWebRead},
			declared, domain.EgressProviderOnly,
			[]domain.CapabilityManifestOffer{webRead.Offer()},
		},
		"the admitted profile is not offered again": {
			all, declared, domain.EgressProviderRegistry,
			[]domain.CapabilityManifestOffer{webRead.Offer()},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := offerableManifests(tc.enforceable, policy(t, tc.policy), tc.admitted)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("offers = %#v, want %#v", got, tc.want)
			}
		})
	}
}
