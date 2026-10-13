package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestLocalBackupProducerEffectProposalEvidence(t *testing.T) {
	for _, forged := range []bool{false, true} {
		name := "durable instance"
		if forged {
			name = "missing instance"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fixture := newSchemaCheckpointTestStore(t)
			ctx, st := fixture.ctx, fixture.live
			policy, err := domain.NewResolvedPolicy("proposal-policy-run", []domain.PolicyKey{{
				Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
					Source: domain.ProvenanceOverride,
					Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
				},
			}})
			if err != nil {
				t.Fatal(err)
			}
			proposal, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
				SubjectHandle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID)),
				Intent:        domain.TaskProposalIntentImplement, ExpectedCostUnits: 10,
				Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
			}, policy)
			if err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
			err = st.Write(ctx, func(tx *WriteTx) error {
				if err := tx.PutRun(ctx, domain.Run{
					ID: policy.RunID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
				}); err != nil {
					return err
				}
				if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
					return err
				}
				declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
					CompletionCriterion: domain.CompletionBoundPRMerged,
					DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
				}, policy.RunID, "project-1", at)
				if err != nil {
					return err
				}
				if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
					return err
				}
				instance, _, err := tx.AllocateProposalInstance(ctx,
					domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "event-1"},
					"batch-1", proposal, at)
				if err != nil {
					return err
				}
				if forged {
					instance.ID = "unallocated-instance"
				}
				artifact, err := instance.EvidenceArtifact()
				if err != nil {
					return err
				}
				if !forged {
					return tx.PutArtifact(ctx, artifact)
				}
				// Bypass the write gate to test reconstruction of a forged row.
				body, err := encode(artifact)
				if err != nil {
					return err
				}
				_, err = tx.tx.ExecContext(ctx, putArtifactSQL, artifact.ID, artifact.Digest, 1, body)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			err = fixture.producer.Maintain(ctx)
			if forged {
				if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "proposal artifact instance") {
					t.Fatalf("Maintain with forged carrier = %v, want missing proposal instance", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Maintain with proposal evidence: %v", err)
			}
			current := schemaTestBackupHealthContext(t, ctx, st)
			snapshot, _, found, err := inspectEncryptedCheckpoint(
				ctx, current.SchemaVersion, fixture.files, true, fixture.files.approvedRecipes, nil)
			if err != nil || !found {
				t.Fatalf("inspect sealed checkpoint: found=%v, err=%v", found, err)
			}
			if !slices.Contains(snapshot.digests, proposal.Digest) {
				t.Fatal("sealed checkpoint omits proposal evidence from its artifact closure")
			}
			if snapshot.restoredAt.IsZero() {
				t.Fatal("sealed checkpoint has no successful restore test")
			}
			scoped, err := fixture.files.NewScopedCheckpointHealthSource(nil)
			if err != nil {
				t.Fatal(err)
			}
			// The fixture verifies all blobs; this tests recipe gating independently
			// of proposal-body blob persistence.
			for _, source := range []BackupHealthSource{fixture.source, scoped} {
				health, err := source.BackupHealth(ctx, current)
				if err != nil || health.ArtifactClosure != domain.BackupHealthHealthy ||
					health.RestoreTestAge != domain.BackupHealthHealthy {
					t.Fatalf("proposal checkpoint health = %+v, err=%v", health, err)
				}
			}
			if err := fixture.producer.Maintain(ctx); err != nil {
				t.Fatalf("Maintain with installed checkpoint: %v", err)
			}
		})
	}
}
