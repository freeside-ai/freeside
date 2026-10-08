package engine_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func submittedEgressKeys(values map[string]string) []domain.PolicyKey {
	keys := submissionInitiator("daemon/**").PolicyKeys
	for key, value := range values {
		keys = append(keys, domain.PolicyKey{Key: key, Value: value, Provenance: keys[0].Provenance})
	}
	return keys
}

func TestSubmittedEgressPolicy(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, values map[string]string, wantErr error, wantKey string) {
		t.Helper()
		policy, err := domain.NewResolvedPolicy("run-egress", submittedEgressKeys(values))
		if err != nil {
			t.Fatal(err)
		}
		before := policy
		before.Keys = slices.Clone(policy.Keys)
		err = engine.SubmittedEgressPolicy(policy)
		if !errors.Is(err, wantErr) || (wantErr != nil && !strings.Contains(err.Error(), wantKey)) {
			t.Fatalf("validation = %v, want %v naming %s", err, wantErr, wantKey)
		}
		if !reflect.DeepEqual(policy, before) {
			t.Fatal("validation changed policy keys or digest")
		}
	}
	for name, values := range map[string]map[string]string{
		"defaults":              {},
		"provider only":         {domain.EgressProfilePolicyKey: "provider_only"},
		"set alone":             {domain.RegistrySetPolicyKey: `["pypi.org"]`},
		"provider only set":     {domain.EgressProfilePolicyKey: "provider_only", domain.RegistrySetPolicyKey: `["pypi.org"]`},
		"provider registry set": {domain.EgressProfilePolicyKey: "provider_registry", domain.RegistrySetPolicyKey: `["pypi.org"]`},
	} {
		t.Run(name, func(t *testing.T) { check(t, values, nil, "") })
	}
	for _, profile := range []string{"", "provider_unknown", " provider_only", "provider_only ", "provider_web_read", "clean_verification"} {
		t.Run("profile/"+profile, func(t *testing.T) {
			check(t, map[string]string{domain.EgressProfilePolicyKey: profile}, domain.ErrInvalidEgressProfile, domain.EgressProfilePolicyKey)
		})
	}
	t.Run("missing required set", func(t *testing.T) {
		check(t, map[string]string{domain.EgressProfilePolicyKey: "provider_registry"}, domain.ErrRegistrySetInvalid, domain.RegistrySetPolicyKey)
	})
	for name, value := range map[string]string{
		"invalid JSON": "[", "empty value": "", "null": "null", "empty array": "[]",
		"object": `{"hosts":["pypi.org"]}`, "non-string": `[1]`,
		"whitespace": `["pypi.org", "sum.golang.org"]`, "unsorted": `["sum.golang.org","pypi.org"]`,
		"duplicate": `["pypi.org","pypi.org"]`, "escape": `["pypi.or\u0067"]`,
		"disallowed host": `["registry.local"]`, "port": `["pypi.org:443"]`,
	} {
		for _, profile := range []string{"absent", "provider_only", "provider_registry"} {
			t.Run(name+"/"+profile, func(t *testing.T) {
				values := map[string]string{domain.RegistrySetPolicyKey: value}
				if profile != "absent" {
					values[domain.EgressProfilePolicyKey] = profile
				}
				check(t, values, domain.ErrRegistrySetInvalid, domain.RegistrySetPolicyKey)
			})
		}
	}
}

func TestSubmitTaskEgressPolicyBeforePersistence(t *testing.T) {
	t.Parallel()
	for name, values := range map[string]map[string]string{
		"unknown profile":   {domain.EgressProfilePolicyKey: "provider_unknown"},
		"missing set":       {domain.EgressProfilePolicyKey: "provider_registry"},
		"set alone":         {domain.RegistrySetPolicyKey: "null"},
		"provider only":     {domain.EgressProfilePolicyKey: "provider_only", domain.RegistrySetPolicyKey: "[]"},
		"provider registry": {domain.EgressProfilePolicyKey: "provider_registry", domain.RegistrySetPolicyKey: `["registry.local"]`},
		"valid defaults":    {},
		"valid only":        {domain.EgressProfilePolicyKey: "provider_only"},
		"valid set":         {domain.RegistrySetPolicyKey: `["pypi.org"]`},
		"valid only set":    {domain.EgressProfilePolicyKey: "provider_only", domain.RegistrySetPolicyKey: `["pypi.org"]`},
		"valid opt-in":      {domain.EgressProfilePolicyKey: "provider_registry", domain.RegistrySetPolicyKey: `["pypi.org"]`},
	} {
		t.Run(name, func(t *testing.T) {
			_, s, holder := newSubmitTaskHarness(t)
			blobs, err := signet.NewBlobStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			service := signet.NewService(s, signet.WithBlobStore(blobs), signet.WithTaskSubmitter(engine.NewTaskSubmitter(blobs, holder.lookup)))
			init := submissionInitiator("daemon/**")
			init.PolicyKeys = submittedEgressKeys(values)
			holder.set("project-1", init)
			const source = "Check the egress declaration."
			publication, err := json.Marshal(engine.ProductionPublication{Recipe: "freeside.client-publication/v2", CommitAuthor: init.CommitAuthor})
			if err != nil {
				t.Fatal(err)
			}
			keysDigest, err := (domain.ResolvedPolicy{Keys: init.PolicyKeys}).ComputeDigest()
			if err != nil {
				t.Fatal(err)
			}
			runID := engine.ManualSubmissionRunID("client:cmd-egress", "project-1", domain.Digest(contentaddr.Sum([]byte(source))), keysDigest, domain.Digest(contentaddr.Sum(publication)), "")
			specID, err := engine.SpecificationRunIDForImplementation(runID)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := domain.NewResolvedPolicy(specID, init.PolicyKeys)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Submit(t.Context(), submitCmd("cmd-egress", "project-1", source, ""))
			if strings.HasPrefix(name, "valid") {
				if err != nil || result.Submission == nil || result.Submission.SpecificationRunID != specID {
					t.Fatalf("valid submission = %+v, error %v", result, err)
				}
				if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
					stored, err := tx.GetResolvedPolicy(t.Context(), specID)
					if err == nil && !reflect.DeepEqual(stored, policy) {
						t.Fatal("submission changed policy keys or digest")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return
			}
			wantErr, wantKey := domain.ErrRegistrySetInvalid, domain.RegistrySetPolicyKey
			if name == "unknown profile" {
				wantErr, wantKey = domain.ErrInvalidEgressProfile, domain.EgressProfilePolicyKey
			}
			if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), wantKey) {
				t.Fatalf("submit = %v, want %v naming %s", err, wantErr, wantKey)
			}
			if err := s.Read(t.Context(), func(tx *store.ReadTx) error {
				if _, err := tx.GetTaskSubmission(t.Context(), "cmd-egress"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("submission record = %v, want absent", err)
				}
				for _, id := range []domain.RunID{runID, specID} {
					if _, err := tx.GetRun(t.Context(), id); !errors.Is(err, store.ErrNotFound) {
						t.Fatalf("run %s = %v, want absent", id, err)
					}
				}
				if _, err := tx.GetResolvedPolicy(t.Context(), specID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("policy = %v, want absent", err)
				}
				artifact, err := engine.SubmissionArtifact(domain.ArtifactKindPolicy, policy.Digest, domain.EvidenceMediaApplicationJSON, 1)
				if err != nil {
					return err
				}
				if _, err := tx.GetArtifact(t.Context(), artifact.ID); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("policy artifact = %v, want absent", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if found, err := blobs.Has(policy.Digest); err != nil || found {
				t.Fatalf("policy blob = %t, %v, want absent", found, err)
			}
		})
	}
}
