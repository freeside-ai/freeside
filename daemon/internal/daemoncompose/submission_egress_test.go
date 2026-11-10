package daemoncompose

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type submissionEgressCase struct {
	name    string
	values  map[string]string
	wantErr error
	wantKey string
}

// submissionEgressCases is one refusal per error kind and the accept path that
// needs both keys: each entry point only has to show it runs the egress gate
// and reports its refusal. The full matrix of profiles and sets is
// TestSubmittedEgressPolicy in internal/engine/submission_egress_test.go, over
// the domain parsers tested in internal/domain/registry_set_test.go.
func submissionEgressCases() []submissionEgressCase {
	return []submissionEgressCase{
		{"unknown profile", map[string]string{domain.EgressProfilePolicyKey: "provider_unknown"}, domain.ErrInvalidEgressProfile, domain.EgressProfilePolicyKey},
		{"malformed provider registry set", map[string]string{domain.EgressProfilePolicyKey: "provider_registry", domain.RegistrySetPolicyKey: `["registry.local"]`}, domain.ErrRegistrySetInvalid, domain.RegistrySetPolicyKey},
		{"provider registry set", map[string]string{domain.EgressProfilePolicyKey: "provider_registry", domain.RegistrySetPolicyKey: `["pypi.org"]`}, nil, ""},
	}
}

func withSubmissionEgress(keys []domain.PolicyKey, values map[string]string) []domain.PolicyKey {
	out := slices.Clone(keys)
	for key, value := range values {
		out = append(out, domain.PolicyKey{Key: key, Value: value, Provenance: keys[0].Provenance})
	}
	return out
}

func checkSubmissionEgressError(t *testing.T, err error, tc submissionEgressCase) {
	t.Helper()
	if !errors.Is(err, tc.wantErr) || (tc.wantErr != nil && !strings.Contains(err.Error(), tc.wantKey)) {
		t.Fatalf("error = %v, want %v naming %s", err, tc.wantErr, tc.wantKey)
	}
}

func assertEgressPolicyAbsent(t *testing.T, st *store.Store, blobs *signet.BlobStore, runID domain.RunID, digest domain.Digest) {
	t.Helper()
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		if _, err := tx.GetRun(t.Context(), runID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("run %s = %v, want absent", runID, err)
		}
		if _, err := tx.GetResolvedPolicy(t.Context(), runID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("resolved policy = %v, want absent", err)
		}
		artifact, err := engine.SubmissionArtifact(domain.ArtifactKindPolicy, digest, domain.EvidenceMediaApplicationJSON, 1)
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
	if found, err := blobs.Has(digest); err != nil || found {
		t.Fatalf("policy blob = %t, %v, want absent", found, err)
	}
}

func TestIntakeEgressPolicyBeforePersistence(t *testing.T) {
	t.Parallel()
	for _, tc := range submissionEgressCases() {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntakeFixture(t)
			init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
			init.PolicyKeys = withSubmissionEgress(init.PolicyKeys, tc.values)
			r := f.reconciler([]IntakeInitiator{init}, labeledOpen(7), nil)
			r.reconcile(t.Context(), nil)
			occurrence := f.latestOccurrence(t, 7)
			if tc.wantErr == nil {
				if occurrence.Admission == nil {
					t.Fatal("valid policy was not admitted")
				}
				return
			}
			if occurrence.Admission != nil {
				t.Fatal("malformed policy was admitted")
			}
			_, err := r.admit(t.Context(), init, occurrence)
			checkSubmissionEgressError(t, err, tc)
			specID, err := engine.SpecificationRunIDForImplementation(intakeImplementationRunID(occurrence))
			if err != nil {
				t.Fatal(err)
			}
			policy, err := domain.NewResolvedPolicy(specID, init.PolicyKeys)
			if err != nil {
				t.Fatal(err)
			}
			assertEgressPolicyAbsent(t, f.store, f.blobs, specID, policy.Digest)
		})
	}
}
