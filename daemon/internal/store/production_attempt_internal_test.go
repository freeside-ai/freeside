package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func testInitialProductionAttempt() domain.ProductionAttempt {
	implementationRunID := domain.RunID("run-1")
	return domain.ProductionAttempt{
		CampaignID: derivedInitialCampaignID(implementationRunID), AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
		SourceDigest: "sha256:source", PublicationDigest: "sha256:publication",
		SpecificationRunID:  derivedSpecificationRunID(implementationRunID),
		ImplementationRunID: implementationRunID,
	}
}

func TestProductionAttemptApprovalDecisionSetAcceptsHistoricalAndCurrentShapes(t *testing.T) {
	legacy := []domain.Action{domain.ActionApprove, domain.ActionRequestChanges, domain.ActionStop}
	current := []domain.Action{domain.ActionApprove, domain.ActionRequestChanges, domain.ActionDiscuss, domain.ActionStop}
	if !authenticSpecificationApprovalDecisionSet(legacy) || !authenticSpecificationApprovalDecisionSet(current) {
		t.Fatal("historical or current specification approval decision set was rejected")
	}
	if authenticSpecificationApprovalDecisionSet([]domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop}) {
		t.Fatal("unrecognized specification approval decision set was accepted")
	}
}

func TestProductionAttemptMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0048_")
	if got := rawVersion(t, db); got != 47 {
		t.Fatalf("prior schema version = %d, want 47", got)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	legacy := domain.Run{
		ID: "run-legacy", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
	}
	body, err := encode(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO runs
		(id, project_id, policy_digest, entity_version, as_of_revision, body)
		VALUES (?, ?, ?, 1, 1, ?)`, legacy.ID, legacy.ProjectID, legacy.PolicyDigest, body); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 63 {
		t.Fatalf("schema version = %d, want 63", got)
	}
	assertTableExists(t, db, "production_attempts", true)
	st := &Store{db: db}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetRun(ctx, legacy.ID)
		if err != nil {
			return err
		}
		if got.CampaignID != "" || got.AttemptNumber != 0 || got.ParentRunID != "" {
			t.Fatalf("migrated legacy run gained lineage: %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestProductionAttemptReconstructionRejectsTamperedLineage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	attempt := testInitialProductionAttempt()
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutProductionAttempt(ctx, attempt) }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `
UPDATE production_attempts SET source_digest = 'sha256:forged'
WHERE campaign_id = ? AND attempt_number = 1`, attempt.CampaignID); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetProductionAttempt(ctx, attempt.CampaignID, attempt.AttemptNumber)
		return err
	})
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("GetProductionAttempt() = %v, want errRowInconsistent", err)
	}
}

func TestProductionAttemptReconstructionRejectsStructurallyInvalidTuple(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	attempt := testInitialProductionAttempt()
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutProductionAttempt(ctx, attempt) }); err != nil {
		t.Fatal(err)
	}
	forged := attempt
	forged.AttemptNumber = 2
	body, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `
UPDATE production_attempts SET attempt_number = ?, body = ?
WHERE campaign_id = ? AND attempt_number = ?`,
		forged.AttemptNumber, string(body), forged.CampaignID, attempt.AttemptNumber); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.LatestProductionAttempt(ctx, attempt.CampaignID)
		return err
	})
	if err == nil {
		t.Fatal("LatestProductionAttempt() succeeded for a structurally invalid tuple")
	}
}

func TestProductionAttemptAuthenticationRejectsRetryAsSpecification(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	initial := testInitialProductionAttempt()
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutProductionAttempt(ctx, initial); err != nil {
			return err
		}
		if _, err := tx.ApproveProductionAttempt(ctx, initial.CampaignID, 1, "sha256:approved"); err != nil {
			return err
		}
		return tx.PutProductionAttempt(ctx, domain.ProductionAttempt{
			CampaignID: initial.CampaignID, AttemptNumber: 2, Kind: domain.ProductionAttemptRetry,
			Reason: "retry after repair", ParentRunID: initial.ImplementationRunID,
			SourceDigest: initial.SourceDigest, PublicationDigest: initial.PublicationDigest,
			ApprovedSpecDigest: "sha256:approved",
			SpecificationRunID: initial.SpecificationRunID, ImplementationRunID: derivedRetryImplementationRunID(initial.CampaignID, 2),
		})
	}); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		return tx.authenticateRunProductionLineage(ctx, domain.Run{
			ID: initial.SpecificationRunID, SpecDigest: initial.SourceDigest,
			CampaignID: initial.CampaignID, AttemptNumber: 2,
		})
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("authenticate retry as specification = %v, want ErrParentKeyMismatch", err)
	}
}

func TestInitialApprovalClaimsRequireMarkdownAndCanonicalDigests(t *testing.T) {
	text := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown, Content: "# Specification\n\nImplement the bounded workflow.",
	}
	provenance := domain.Provenance{
		ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-specify-run-1",
		HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
	}
	specification := domain.Artifact{
		ID: "spec-run-1-1", Type: domain.ArtifactKindSpecification,
		Digest: text.ComputeDigest(), Provenance: provenance,
		Metadata: runMeta(),
	}
	item := domain.AttentionItem{
		AgentClaims: []domain.AgentClaim{
			{Label: "Specification", Artifact: specification.ID, Digest: specification.Digest, Provenance: provenance, Metadata: claimMeta(domain.EvidenceMediaImagePNG)},
			{
				Label: summaryEvidenceLabel, Artifact: "spec-summary-run-1-1",
				Digest: text.ComputeDigest(), Provenance: provenance, Text: &text,
				Metadata: claimTextMeta(text),
			},
		},
		ArtifactDigests: []domain.Digest{specification.Digest},
	}
	summaryDigest := text.ComputeDigest()
	if !authenticatesInitialApprovalClaims(item, specification, &summaryDigest, "run-1", 1) {
		t.Fatal("canonical duplicate digest was rejected")
	}
	substituted := text
	substituted.Content = "# Specification\n\nSubstitute an unauthenticated summary."
	item.AgentClaims[1].Text = &substituted
	item.AgentClaims[1].Digest = substituted.ComputeDigest()
	if authenticatesInitialApprovalClaims(item, specification, &summaryDigest, "run-1", 1) {
		t.Fatal("summary not bound to the terminal digest was accepted")
	}
	item.AgentClaims[1].Text = &text
	item.AgentClaims[1].Digest = text.ComputeDigest()

	plain := text
	plain.MediaType = domain.MediaTypeTextPlain
	item.AgentClaims[1].Text = &plain
	if authenticatesInitialApprovalClaims(item, specification, &summaryDigest, "run-1", 1) {
		t.Fatal("plain-text reserved summary was accepted")
	}

	item.AgentClaims[1].Text = &text
	addressalsDigest := domain.Digest("sha256:addressals")
	item.AgentClaims = append(item.AgentClaims, domain.AgentClaim{
		Label: "Addressals", Artifact: "spec-addressals-run-1-1",
		Digest: addressalsDigest, Provenance: provenance,
		Metadata: claimMeta(domain.EvidenceMediaImagePNG),
	})
	item.SpecRevision = &domain.SpecRevisionFacts{AddressalsDigest: addressalsDigest}
	item.ArtifactDigests = append(item.ArtifactDigests, addressalsDigest)
	slices.Sort(item.ArtifactDigests)
	if !authenticatesInitialApprovalClaims(item, specification, &summaryDigest, "run-1", 1) {
		t.Fatal("canonical revised approval claims were rejected")
	}
	item.AgentClaims[2].Artifact = "spec-addressals-run-1-forged"
	if authenticatesInitialApprovalClaims(item, specification, &summaryDigest, "run-1", 1) {
		t.Fatal("substituted revised approval addressals were accepted")
	}
}

func TestProductionAttemptReconstructionRejectsCyclicParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	initial := testInitialProductionAttempt()
	retry := domain.ProductionAttempt{
		CampaignID: initial.CampaignID, AttemptNumber: 2, Kind: domain.ProductionAttemptRetry,
		Reason: "retry after repair", ParentRunID: initial.ImplementationRunID,
		SourceDigest: initial.SourceDigest, PublicationDigest: initial.PublicationDigest,
		ApprovedSpecDigest: "sha256:approved",
		SpecificationRunID: initial.SpecificationRunID, ImplementationRunID: derivedRetryImplementationRunID(initial.CampaignID, 2),
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutProductionAttempt(ctx, initial); err != nil {
			return err
		}
		if _, err := tx.ApproveProductionAttempt(ctx, initial.CampaignID, 1, retry.ApprovedSpecDigest); err != nil {
			return err
		}
		return tx.PutProductionAttempt(ctx, retry)
	}); err != nil {
		t.Fatal(err)
	}
	forged := retry
	forged.ParentRunID = retry.ImplementationRunID
	body, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE production_attempts SET parent_run_id = ?, body = ?
WHERE campaign_id = ? AND attempt_number = ?`, forged.ParentRunID, string(body), forged.CampaignID, forged.AttemptNumber); err != nil {
		t.Fatal(err)
	}
	err = st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetProductionAttemptByRun(ctx, forged.ImplementationRunID)
		return err
	})
	if !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("GetProductionAttemptByRun() = %v, want ErrParentKeyMismatch", err)
	}
}
