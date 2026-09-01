package domain_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestCapabilityManifestIsVersionedAndContentAddressed(t *testing.T) {
	manifest, err := domain.NewCapabilityManifest("Provider web read", domain.EgressProviderWebRead)
	if err != nil {
		t.Fatal(err)
	}
	body, err := manifest.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := domain.DecodeCapabilityManifest(body)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != manifest {
		t.Fatalf("decoded = %#v, want %#v", decoded, manifest)
	}

	tampered := manifest
	tampered.EgressProfile = domain.EgressProviderOnly
	if err := tampered.Validate(); !errors.Is(err, domain.ErrCapabilityManifestDigestMismatch) {
		t.Fatalf("tampered manifest error = %v, want digest mismatch", err)
	}
}

func TestCapabilityManifestNamesUseTheCrossLanguageASCIIVocabulary(t *testing.T) {
	if _, err := domain.NewCapabilityManifest("Provider wéb read", domain.EgressProviderWebRead); !errors.Is(err, domain.ErrCapabilityManifestInvalid) {
		t.Fatalf("non-ASCII name error = %v, want ErrCapabilityManifestInvalid", err)
	}
}

func TestCapabilityManifestsFromPolicyRequiresCanonicalDistinctSet(t *testing.T) {
	web, err := domain.NewCapabilityManifest("Provider web read", domain.EgressProviderWebRead)
	if err != nil {
		t.Fatal(err)
	}
	clean, err := domain.NewCapabilityManifest("No network verification", domain.EgressCleanVerification)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal([]domain.CapabilityManifest{web, clean})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{{
		Key: domain.CapabilityManifestPolicyKey, Value: string(body),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: "sha256:manifest-policy",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := domain.CapabilityManifestsFromPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 2 || manifests[0].Name != clean.Name || manifests[1].Name != web.Name {
		t.Fatalf("manifests = %#v, want name-sorted set", manifests)
	}

	duplicateBody, err := json.Marshal([]domain.CapabilityManifest{web, web})
	if err != nil {
		t.Fatal(err)
	}
	duplicatePolicy, err := domain.NewResolvedPolicy("run-2", []domain.PolicyKey{{
		Key: domain.CapabilityManifestPolicyKey, Value: string(duplicateBody),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset, Digest: "sha256:manifest-policy",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.CapabilityManifestsFromPolicy(duplicatePolicy); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("duplicate manifests error = %v, want ErrDuplicate", err)
	}
}

func TestCapabilityManifestsFromPolicyDistinguishesAbsenceFromMalformedEmptyValues(t *testing.T) {
	provenance := domain.KeyProvenance{
		Source: domain.ProvenancePreset, Digest: "sha256:manifest-policy",
	}
	without, err := domain.NewResolvedPolicy("run-without-manifests", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/**", Provenance: provenance,
	}})
	if err != nil {
		t.Fatal(err)
	}
	manifests, err := domain.CapabilityManifestsFromPolicy(without)
	if err != nil || manifests != nil {
		t.Fatalf("absent manifests = %#v, %v, want nil", manifests, err)
	}

	for _, value := range []string{"", "null"} {
		policy, err := domain.NewResolvedPolicy(domain.RunID("run-malformed-"+value), []domain.PolicyKey{{
			Key: domain.CapabilityManifestPolicyKey, Value: value, Provenance: provenance,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := domain.CapabilityManifestsFromPolicy(policy); !errors.Is(err, domain.ErrCapabilityManifestInvalid) {
			t.Fatalf("value %q error = %v, want ErrCapabilityManifestInvalid", value, err)
		}
	}

	empty, err := domain.NewResolvedPolicy("run-empty-manifests", []domain.PolicyKey{{
		Key: domain.CapabilityManifestPolicyKey, Value: "[]", Provenance: provenance,
	}})
	if err != nil {
		t.Fatal(err)
	}
	manifests, err = domain.CapabilityManifestsFromPolicy(empty)
	if err != nil || len(manifests) != 0 || manifests == nil {
		t.Fatalf("empty manifest set = %#v, %v, want non-nil empty", manifests, err)
	}
}
