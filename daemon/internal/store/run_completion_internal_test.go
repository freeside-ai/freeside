package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestWorkUnitCompletedMilestoneMigrationAppliesFromHead is the migration
// acceptance for 0066 (#1134): the rebuilt run_milestones keeps every durable
// row and the first-observation-wins identity index, and the widened kind
// CHECK accepts work_unit_completed where the prior head refused it.
func TestWorkUnitCompletedMilestoneMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0066_")
	if got := rawVersion(t, db); got != 65 {
		t.Fatalf("prior schema version = %d, want 65", got)
	}
	const at = "2026-01-02T03:04:05Z"
	seed := func(kind string) error {
		_, err := db.ExecContext(ctx, `INSERT INTO run_milestones
			   (run_id, kind, invocation_id, recorded_at)
			 VALUES ('run-1', ?, 'publish-production-run-1', ?)`, kind, at)
		return err
	}
	for _, kind := range []domain.RunMilestoneKind{
		domain.MilestoneRunSubmitted, domain.MilestonePublicationReady,
	} {
		if err := seed(string(kind)); err != nil {
			t.Fatalf("seed %s at prior head: %v", kind, err)
		}
	}
	if err := seed(string(domain.MilestoneWorkUnitCompleted)); err == nil {
		t.Fatal("prior head accepted work_unit_completed")
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 66 {
		t.Fatalf("schema version = %d, want 66", got)
	}
	var kept int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM run_milestones WHERE run_id = 'run-1' AND recorded_at = ?`, at,
	).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 2 {
		t.Fatalf("rows preserved across the rebuild = %d, want 2", kept)
	}
	for _, kind := range domain.AllRunMilestoneKinds {
		if kind == domain.MilestoneRunSubmitted || kind == domain.MilestonePublicationReady {
			continue
		}
		if err := seed(string(kind)); err != nil {
			t.Errorf("head refuses registered kind %s: %v", kind, err)
		}
	}
	if err := seed(string(domain.MilestoneWorkUnitCompleted)); err == nil {
		t.Fatal("identity index lost: a duplicate (run, kind, invocation) inserted")
	}
	if err := seed("shipped"); err == nil {
		t.Fatal("head accepts an unregistered kind")
	}
}

// TestRunSuccessor pins the supersession lookup (#1134): the run that retried
// a parent is its successor, a run nobody retried has none, when more than
// one attempt names the same parent the earliest attempt wins, a child with
// no attempt number never outranks a numbered one, and a candidate whose
// canonical body or production lineage does not stand fails the read closed.
//
// The retry attempts are written through the real allocation path, so the
// candidates the lookup reconstructs carry a lineage the run trust gate
// authenticates. The campaign's initial implementation run has no row, which
// is the admission-reserved shape the store authenticates without a
// specification marker; the forged rows are seeded raw because the gates that
// would refuse to write them are pinned by their own tests.
func TestRunSuccessor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	const (
		parentRunID  = domain.RunID("run-parent")
		retryReason  = "retry after repair"
		approvedSpec = domain.Digest("sha256:approved")
	)
	campaignID := derivedInitialCampaignID(parentRunID)
	initial := domain.ProductionAttempt{
		CampaignID: campaignID, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
		SourceDigest: "sha256:source", PublicationDigest: "sha256:publication",
		SpecificationRunID:  domain.SpecificationRunIDForImplementation(parentRunID),
		ImplementationRunID: parentRunID,
	}
	retryAttempt := func(number int) domain.ProductionAttempt {
		return domain.ProductionAttempt{
			CampaignID: campaignID, AttemptNumber: number, Kind: domain.ProductionAttemptRetry,
			Reason: retryReason, ParentRunID: parentRunID,
			SourceDigest: initial.SourceDigest, PublicationDigest: initial.PublicationDigest,
			ApprovedSpecDigest:  approvedSpec,
			SpecificationRunID:  initial.SpecificationRunID,
			ImplementationRunID: derivedRetryImplementationRunID(campaignID, number),
		}
	}
	specificationInvocationID := domain.SpecificationInvocationID(initial.SpecificationRunID, 1)
	specArtifactID := domain.ArtifactID(fmt.Sprintf("spec-%s-1", parentRunID))
	artifact := func(id domain.ArtifactID, digest domain.Digest) domain.Artifact {
		t.Helper()
		made, err := domain.NewArtifact(domain.ArtifactInput{
			ID: id, Type: domain.ArtifactKindSpecification, Digest: digest,
			Provenance: domain.Provenance{
				ProducerClass: domain.ProducerAgent, ProducerInvocationID: specificationInvocationID,
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
	source := artifact("artifact-source", initial.SourceDigest)
	specification := artifact(specArtifactID, approvedSpec)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutArtifact(ctx, source); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, specification); err != nil {
			return err
		}
		if err := tx.PutProductionAttempt(ctx, initial); err != nil {
			return err
		}
		request, err := json.Marshal(map[string]any{
			"version":               "freeside.specification-request/v1",
			"specification_run_id":  initial.SpecificationRunID,
			"implementation_run_id": parentRunID, "project_id": "proj-1",
			"invocation_id": specificationInvocationID, "iteration": 1,
			"campaign_id": campaignID, "attempt_number": 1,
			"publication_digest": initial.PublicationDigest,
			"input_artifact_ids": []domain.ArtifactID{source.ID},
		})
		if err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, string(specificationInvocationID),
			string(domain.SpecificationInvocationRequestedKind), request); err != nil {
			return err
		}
		policy, err := domain.NewResolvedPolicy(initial.SpecificationRunID, []domain.PolicyKey{{
			Key: "gates.spec_approval", Value: "false",
			Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:policy-source"},
		}})
		if err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: initial.SpecificationRunID, ProjectID: "proj-1",
			SpecDigest: initial.SourceDigest, PolicyDigest: policy.Digest,
			CampaignID: campaignID, AttemptNumber: 1,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, string(specificationInvocationID)); err != nil {
			return err
		}
		terminal, err := json.Marshal(map[string]any{
			"invocation_id": specificationInvocationID, "iteration": 1, "status": "completed",
			"research_artifact_ids": []domain.ArtifactID{}, "spec_artifact_id": specArtifactID,
		})
		if err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(ctx, string(specificationInvocationID),
			"specification_stage_terminal", terminal); err != nil {
			return err
		}
		if _, err := tx.ApproveProductionAttempt(ctx, campaignID, 1, approvedSpec); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: parentRunID, ProjectID: "proj-1",
			SpecDigest: approvedSpec, PolicyDigest: "sha256:policy",
			CampaignID: campaignID, AttemptNumber: 1,
		}); err != nil {
			return err
		}
		for _, number := range []int{2, 3, 4} {
			if err := tx.PutProductionAttempt(ctx, retryAttempt(number)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed campaign lineage: %v", err)
	}
	seed := func(run domain.Run, attempt any, parent any) {
		t.Helper()
		body, err := encode(run)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.ExecContext(ctx, `INSERT INTO runs
			(id, project_id, policy_digest, campaign_id, attempt_number, attempt_reason, parent_run_id,
			 entity_version, as_of_revision, body)
			VALUES (?, 'proj-1', 'sha256:policy', ?, ?, ?, ?, 1, 1, ?)`,
			run.ID, nullable(string(run.CampaignID)), attempt, nullable(run.AttemptReason),
			parent, body); err != nil {
			t.Fatalf("seed run %s: %v", run.ID, err)
		}
	}
	retryRun := func(number int) domain.Run {
		return domain.Run{
			ID: derivedRetryImplementationRunID(campaignID, number), ProjectID: "proj-1",
			SpecDigest: approvedSpec, PolicyDigest: "sha256:policy",
			CampaignID: campaignID, AttemptNumber: number,
			AttemptReason: retryReason, ParentRunID: parentRunID,
		}
	}
	legacyRun := func(id domain.RunID) domain.Run {
		return domain.Run{
			ID: id, ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		}
	}
	successor := func(runID domain.RunID) (domain.RunID, bool, error) {
		t.Helper()
		var (
			id domain.RunID
			ok bool
		)
		err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			id, ok, err = tx.RunSuccessor(ctx, runID)
			return err
		})
		return id, ok, err
	}
	if id, ok, err := successor(parentRunID); err != nil || ok || id != "" {
		t.Fatalf("RunSuccessor(unretried) = %q, %v, %v, want none", id, ok, err)
	}
	// The later attempt's row lands first so the answer comes from attempt
	// order, not insertion order. The fourth child's attempt column is null:
	// a child with no attempt number cannot carry a parent, so it must sort
	// after every numbered one, and selecting it would fail the read closed
	// instead of returning the real successor.
	seed(retryRun(3), 3, parentRunID)
	seed(retryRun(4), nil, parentRunID)
	seed(retryRun(2), 2, parentRunID)
	wantSuccessor := derivedRetryImplementationRunID(campaignID, 2)
	if id, ok, err := successor(parentRunID); err != nil || !ok || id != wantSuccessor {
		t.Fatalf("RunSuccessor(retried twice) = %q, %v, %v, want %q", id, ok, err, wantSuccessor)
	}
	if id, ok, err := successor(wantSuccessor); err != nil || ok || id != "" {
		t.Fatalf("RunSuccessor(leaf attempt) = %q, %v, %v, want none", id, ok, err)
	}
	// A candidate whose lineage column names the parent but whose canonical
	// body does not is the row inconsistency the cross-check refuses.
	seed(legacyRun("run-other"), nil, nil)
	seed(legacyRun("run-forged"), nil, "run-other")
	if _, _, err := successor("run-other"); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("RunSuccessor(forged column) = %v, want errRowInconsistent", err)
	}
	// A candidate whose row agrees with itself but whose production attempt
	// names another parent has no retry authority over this run: the run
	// reconstruction gate refuses it rather than minting a successor.
	misbound := retryRun(3)
	misbound.ID = derivedRetryImplementationRunID(campaignID, 3)
	misbound.ParentRunID = "run-victim"
	seed(legacyRun("run-victim"), nil, nil)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE runs SET parent_run_id = 'run-victim', body = ? WHERE id = ?`,
		mustEncode(t, misbound), misbound.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := successor("run-victim"); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("RunSuccessor(forged lineage) = %v, want ErrParentKeyMismatch", err)
	}
}

func mustEncode(t *testing.T, run domain.Run) string {
	t.Helper()
	body, err := encode(run)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}
