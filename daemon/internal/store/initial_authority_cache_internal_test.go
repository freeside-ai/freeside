package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func initialAuthorityFixture(t *testing.T, approved bool) (*Store, domain.ProductionAttempt) {
	t.Helper()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	attempt := testInitialProductionAttempt()
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "source", Type: domain.ArtifactKindSpecification, Digest: attempt.SourceDigest,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: "source-input", HeadBinding: domain.HeadIndependent,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	marker, err := json.Marshal(map[string]any{
		"specification_run_id": attempt.SpecificationRunID, "implementation_run_id": attempt.ImplementationRunID,
		"campaign_id": attempt.CampaignID, "attempt_number": 1,
		"publication_digest": attempt.PublicationDigest, "input_artifact_ids": []domain.ArtifactID{artifact.ID},
		"invocation_id": domain.SpecificationInvocationID(attempt.SpecificationRunID, 1), "iteration": 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(t.Context(), func(tx *WriteTx) error {
		if err := tx.PutArtifact(t.Context(), artifact); err != nil {
			return err
		}
		_, _, err := tx.RecordDispatchedOutbox(t.Context(),
			string(domain.SpecificationInvocationID(attempt.SpecificationRunID, 1)),
			string(domain.SpecificationInvocationRequestedKind), marker)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if approved {
		attempt.ApprovedSpecDigest = "sha256:approved-specification"
		invocation := domain.SpecificationInvocationID(attempt.SpecificationRunID, 1)
		specification, err := domain.NewArtifact(domain.ArtifactInput{
			ID:   domain.ArtifactID(fmt.Sprintf("spec-%s-1", attempt.ImplementationRunID)),
			Type: domain.ArtifactKindSpecification, Digest: attempt.ApprovedSpecDigest,
			Provenance: domain.Provenance{
				ProducerClass:        domain.ProducerAgent,
				ProducerInvocationID: invocation, HeadBinding: domain.HeadIndependent,
				SensitivityClass: domain.SensitivityNormal,
			},
			Metadata: runMeta(),
		}, nil)
		if err != nil {
			t.Fatal(err)
		}
		policy, err := domain.NewResolvedPolicy(attempt.SpecificationRunID, []domain.PolicyKey{{
			Key: "gates.spec_approval", Value: "false",
			Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:policy-source"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		terminal, err := json.Marshal(map[string]any{
			"invocation_id": invocation, "iteration": 1, "status": "completed",
			"spec_artifact_id": specification.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Write(t.Context(), func(tx *WriteTx) error {
			if err := tx.PutArtifact(t.Context(), specification); err != nil {
				return err
			}
			if err := tx.PutRun(t.Context(), domain.Run{
				ID: attempt.SpecificationRunID, ProjectID: "project-1",
				SpecDigest: attempt.SourceDigest, PolicyDigest: policy.Digest,
			}); err != nil {
				return err
			}
			if err := tx.PutResolvedPolicy(t.Context(), policy); err != nil {
				return err
			}
			_, _, err := tx.RecordInbox(t.Context(), string(invocation), "specification_stage_terminal", terminal)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return st, attempt
}

func TestInitialAuthorityReadCachePreservesInputAndCancellationChecks(t *testing.T) {
	t.Parallel()
	for _, approved := range []bool{false, true} {
		t.Run(fmt.Sprintf("approved=%t", approved), func(t *testing.T) {
			testInitialAuthorityReadCache(t, approved)
		})
	}
}

func testInitialAuthorityReadCache(t *testing.T, approved bool) {
	t.Helper()
	st, attempt := initialAuthorityFixture(t, approved)
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		for range 3 {
			if err := tx.authenticateInitialAttemptAuthority(t.Context(), attempt); err != nil {
				return err
			}
		}
		if len(tx.initialAttemptAuthorities) != 1 {
			t.Fatal("successful repeated authority read was not retained for this snapshot")
		}
		// Compare the original predicate with the cached route for all
		// combinations of changed authority coordinates. A cache key omission
		// must not let another tuple inherit the successful source proof.
		for mask := range 64 {
			changed := attempt
			if mask&1 != 0 {
				changed.CampaignID = "foreign-campaign"
			}
			if mask&2 != 0 {
				changed.SpecificationRunID = "foreign-specification"
			}
			if mask&4 != 0 {
				changed.ImplementationRunID = "foreign-implementation"
			}
			if mask&8 != 0 {
				changed.SourceDigest = "sha256:foreign-source"
			}
			if mask&16 != 0 {
				changed.PublicationDigest = "sha256:foreign-publication"
			}
			if mask&32 != 0 {
				changed.ApprovedSpecDigest = "sha256:foreign-approved-spec"
			}
			want := tx.authenticateInitialAttemptAuthorityUncached(t.Context(), changed)
			got := tx.authenticateInitialAttemptAuthority(t.Context(), changed)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatalf("authority tuple %d: cached=%v, uncached=%v", mask, got, want)
			}
		}
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		if err := tx.authenticateInitialAttemptAuthority(canceled, attempt); !errors.Is(err, context.Canceled) {
			t.Fatalf("cached authority ignored cancellation: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInitialAuthorityReadCacheDoesNotRetainFailures(t *testing.T) {
	t.Parallel()
	st, attempt := initialAuthorityFixture(t, true)
	attempt.SourceDigest = "sha256:forged"
	if err := st.Read(t.Context(), func(tx *ReadTx) error {
		for range 2 {
			if err := tx.authenticateInitialAttemptAuthority(t.Context(), attempt); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("forged source authenticated: %v", err)
			}
		}
		if len(tx.initialAttemptAuthorities) != 0 {
			t.Fatal("failed authority was cached")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInitialAuthorityCacheDoesNotCrossWrites(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"write", "internal", "next-read"} {
		t.Run(mode, func(t *testing.T) {
			st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
			attempt := testInitialProductionAttempt()
			check := func(tx *ReadTx, wantFailure bool) error {
				err := tx.authenticateInitialAttemptAuthority(t.Context(), attempt)
				if wantFailure && !errors.Is(err, domain.ErrParentKeyMismatch) {
					return fmt.Errorf("new invalid authority was not observed: %w", err)
				}
				if !wantFailure && err != nil {
					return err
				}
				return nil
			}
			mutate := func(tx *InternalTx) error {
				_, _, err := tx.EnqueueOutbox(t.Context(),
					string(domain.SpecificationInvocationID(attempt.SpecificationRunID, 1)),
					string(domain.SpecificationInvocationRequestedKind), []byte(`{}`))
				return err
			}
			var err error
			switch mode {
			case "write":
				err = st.Write(t.Context(), func(tx *WriteTx) error {
					if err := check(&tx.ReadTx, false); err != nil {
						return err
					}
					if err := mutate(&tx.InternalTx); err != nil {
						return err
					}
					return check(&tx.ReadTx, true)
				})
			case "internal":
				err = st.WriteInternal(t.Context(), func(tx *InternalTx) error {
					if err := check(&tx.ReadTx, false); err != nil {
						return err
					}
					if err := mutate(tx); err != nil {
						return err
					}
					return check(&tx.ReadTx, true)
				})
			case "next-read":
				if err = st.Read(t.Context(), func(tx *ReadTx) error { return check(tx, false) }); err != nil {
					t.Fatal(err)
				}
				if err = st.WriteInternal(t.Context(), mutate); err != nil {
					t.Fatal(err)
				}
				err = st.Read(t.Context(), func(tx *ReadTx) error { return check(tx, true) })
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
