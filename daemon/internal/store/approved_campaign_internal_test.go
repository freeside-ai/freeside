package store

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// SeedApprovedCampaign writes, through the store's own write gates, the state
// an approved specification leaves behind: a task holding a campaign's
// completed specification run and the implementation run its approval
// reserved, with the initial production attempt approved at the
// specification's digest. Tests that need an authenticated campaign start
// from it rather than from a frozen database. It is the smallest state the
// store's reconstruction gates accept, not everything the engine writes: the
// specification run has no stages, its request no policy artifact or work
// unit, and its policy one key. It returns both runs as stored.
func SeedApprovedCampaign(
	t testing.TB, st *Store, projectID domain.ProjectID, implementationRunID domain.RunID,
) (specification, implementation domain.Run) {
	t.Helper()
	ctx := t.Context()
	const approvedSpec = domain.Digest("sha256:approved-specification")
	campaignID := derivedInitialCampaignID(implementationRunID)
	attempt := domain.ProductionAttempt{
		CampaignID: campaignID, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
		SourceDigest: "sha256:campaign-source", PublicationDigest: "sha256:campaign-publication",
		SpecificationRunID:  domain.SpecificationRunIDForImplementation(implementationRunID),
		ImplementationRunID: implementationRunID,
	}
	invocationID := domain.SpecificationInvocationID(attempt.SpecificationRunID, 1)
	artifact := func(id domain.ArtifactID, digest domain.Digest) domain.Artifact {
		t.Helper()
		made, err := domain.NewArtifact(domain.ArtifactInput{
			ID: id, Type: domain.ArtifactKindSpecification, Digest: digest,
			Provenance: domain.Provenance{
				ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocationID,
				HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
			},
			Metadata: domain.EvidenceMetadata{
				MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: 1,
				CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
				Source:    domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
			},
		}, map[domain.Digest]bool{})
		if err != nil {
			t.Fatal(err)
		}
		return made
	}
	source := artifact(domain.ArtifactID("source-"+string(implementationRunID)), attempt.SourceDigest)
	approved := artifact(domain.ArtifactID(fmt.Sprintf("spec-%s-1", implementationRunID)), approvedSpec)
	sourceRef := domain.SpecificationSource{
		Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: source.ID,
	}
	request, err := json.Marshal(map[string]any{
		"version":               "freeside.specification-request/v1",
		"specification_run_id":  attempt.SpecificationRunID,
		"implementation_run_id": implementationRunID, "project_id": projectID,
		"invocation_id": invocationID, "iteration": 1,
		"campaign_id": campaignID, "attempt_number": 1,
		"publication_digest": attempt.PublicationDigest,
		"input_artifact_ids": []domain.ArtifactID{source.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := json.Marshal(map[string]any{
		"invocation_id": invocationID, "iteration": 1, "status": "completed",
		"research_artifact_ids": []domain.ArtifactID{}, "spec_artifact_id": approved.ID,
	})
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
	specification = domain.Run{
		ID: attempt.SpecificationRunID, ProjectID: projectID,
		SpecDigest: attempt.SourceDigest, PolicyDigest: policy.Digest,
		CampaignID: campaignID, AttemptNumber: 1, Stages: []domain.Stage{},
	}
	implementation = domain.Run{
		ID: implementationRunID, ProjectID: projectID,
		SpecDigest: approvedSpec, PolicyDigest: "sha256:implementation-policy",
		CampaignID: campaignID, AttemptNumber: 1, Stages: []domain.Stage{},
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		for _, a := range []domain.Artifact{source, approved} {
			if err := tx.PutArtifact(ctx, a); err != nil {
				return err
			}
		}
		if err := tx.PutProductionAttempt(ctx, attempt); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, string(invocationID),
			string(domain.SpecificationInvocationRequestedKind), request); err != nil {
			return err
		}
		if err := tx.AssignTask(ctx, &specification, &sourceRef); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, specification); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, string(invocationID)); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(ctx, string(invocationID),
			"specification_stage_terminal", terminal); err != nil {
			return err
		}
		if _, err := tx.ApproveProductionAttempt(ctx, campaignID, 1, approvedSpec); err != nil {
			return err
		}
		if err := tx.AssignTask(ctx, &implementation, &sourceRef); err != nil {
			return err
		}
		return tx.PutRun(ctx, implementation)
	}); err != nil {
		t.Fatalf("seed approved campaign: %v", err)
	}
	return specification, implementation
}
