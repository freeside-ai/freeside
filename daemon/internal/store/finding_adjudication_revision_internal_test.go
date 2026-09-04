package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestFindingAdjudicationPreCommitmentBodyReconstructs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	body, err := os.ReadFile(filepath.Join("testdata", "finding_adjudication.golden"))
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.DecodeFindingAdjudication(body)
	if err != nil {
		t.Fatal(err)
	}
	if artifact.DecisionSurfaceDigest != "" {
		t.Fatalf("fixture decision surface digest = %q, want empty", artifact.DecisionSurfaceDigest)
	}
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	findings := make([]domain.Finding, 0, len(artifact.Entries))
	ids := make([]domain.FindingID, 0, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		ids = append(ids, entry.FindingID)
		findings = append(findings, domain.Finding{
			ID: entry.FindingID, RunID: artifact.RunID, Source: "codex_local",
			Location:  &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1},
			Message:   "finding",
			RawText:   "finding",
			CreatedAt: artifact.CreatedAt.Add(-time.Minute),
		})
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-pre-commitment", RunID: artifact.RunID, Round: artifact.Round,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: migrationAdjudicationDigest("c"),
		InstructionDigest:   artifact.InstructionSnapshotDigest,
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head",
		CompletedAt:        artifact.CreatedAt.Add(-time.Minute),
		CompletionEvidence: migrationAdjudicationDigest("e"),
		Outcome:            domain.ReviewFindings,
		FindingIDs:         ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: artifact.RunID, ProjectID: "project-1",
			SpecDigest: artifact.ApprovedSpecDigest, PolicyDigest: artifact.ResolvedPolicyDigest,
		}); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, findings)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.ExecContext(ctx, `INSERT INTO finding_adjudications
        (run_id, round, revision, predecessor_digest, content_digest,
         finding_batch_digest, approved_spec_digest, instruction_snapshot_digest,
         resolved_policy_digest, created_at, body_digest, body)
        VALUES (?, ?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.RunID, artifact.Round, artifact.Revision, artifact.Digest,
		artifact.FindingBatchDigest, artifact.ApprovedSpecDigest,
		artifact.InstructionSnapshotDigest, artifact.ResolvedPolicyDigest,
		formatTime(artifact.CreatedAt), reviewBodyDigest(string(body)), string(body),
	); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetFindingAdjudication(ctx, artifact.Digest)
		if err != nil {
			return err
		}
		if got.Digest != artifact.Digest || got.DecisionSurfaceDigest != "" {
			t.Fatalf("reconstructed digest = %q, decision surface = %q", got.Digest, got.DecisionSurfaceDigest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func migrationAdjudicationDigest(component string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(component, 64/len(component)))
}

func seedMigrationAdjudication(
	t *testing.T, ctx context.Context, st *Store,
) domain.FindingAdjudication {
	t.Helper()
	at := time.Date(2026, 8, 25, 13, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-migrated-adjudication", ProjectID: "project-1",
		SpecDigest: migrationAdjudicationDigest("a"), PolicyDigest: migrationAdjudicationDigest("b"),
	}
	finding := domain.Finding{
		ID: "finding-migrated-adjudication", RunID: run.ID, Source: "codex_local",
		Location: &domain.FindingLocation{Path: "daemon/a.go", StartLine: 1, EndLine: 1},
		Message:  "finding", RawText: "finding", CreatedAt: at,
	}
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-migrated-adjudication", RunID: run.ID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: migrationAdjudicationDigest("c"),
		InstructionDigest:   migrationAdjudicationDigest("d"),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head", CompletedAt: at,
		CompletionEvidence: migrationAdjudicationDigest("e"),
		Outcome:            domain.ReviewFindings, FindingIDs: []domain.FindingID{finding.ID},
	})
	if err != nil {
		t.Fatalf("review record: %v", err)
	}
	compatibility := domain.CompatibilityAllowed
	entry, err := domain.NewEngineAdjudicationEntry(
		finding.ID, domain.GoalRequired, &compatibility,
		domain.RouteRemediate, "in scope", nil, nil, nil, nil, nil,
	)
	if err != nil {
		t.Fatalf("adjudication entry: %v", err)
	}
	artifact, err := domain.NewFindingAdjudication(
		run.ID, 1, run.SpecDigest, record.InstructionDigest, run.PolicyDigest,
		[]domain.FindingAdjudicationEntry{entry}, "",

		at.Add(time.Minute))
	if err != nil {
		t.Fatalf("adjudication: %v", err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		return tx.PutReviewRecord(ctx, record, []domain.Finding{finding})
	}); err != nil {
		t.Fatalf("seed review: %v", err)
	}
	return artifact
}

func TestMigrateFindingAdjudicationRevisionsPreservesInitialRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := openDB(filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	migrateThrough(t, ctx, db, "0057_")
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seed epoch: %v", err)
	}
	requirementSets, err := resolveTrustedRequirementSets(
		domain.CurrentVerificationFloorRegistryGeneration, nil,
	)
	if err != nil {
		t.Fatalf("requirement sets: %v", err)
	}
	st := newStore(db, Options{}, domain.CurrentVerificationFloorRegistryGeneration, requirementSets)
	t.Cleanup(func() { _ = st.Close() })
	artifact := seedMigrationAdjudication(t, ctx, st)
	body, err := encode(artifact)
	if err != nil {
		t.Fatalf("encode legacy body: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO finding_adjudications
        (run_id, round, content_digest, finding_batch_digest, approved_spec_digest,
         instruction_snapshot_digest, resolved_policy_digest, created_at, body_digest, body)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		artifact.RunID, artifact.Round, artifact.Digest, artifact.FindingBatchDigest,
		artifact.ApprovedSpecDigest, artifact.InstructionSnapshotDigest,
		artifact.ResolvedPolicyDigest, formatTime(artifact.CreatedAt), reviewBodyDigest(body), body,
	); err != nil {
		t.Fatalf("insert legacy adjudication: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	var (
		revision     int
		predecessor  sql.NullString
		migratedBody string
		digest       string
	)
	if err := db.QueryRowContext(ctx, `SELECT revision, predecessor_digest, body, content_digest
        FROM finding_adjudications WHERE run_id = ? AND round = ?`, artifact.RunID, artifact.Round).
		Scan(&revision, &predecessor, &migratedBody, &digest); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if revision != 1 || predecessor.Valid || migratedBody != body || digest != string(artifact.Digest) {
		t.Fatalf("migrated row = revision %d predecessor %+v digest %q body-equal %t",
			revision, predecessor, digest, migratedBody == body)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		byRound, err := tx.GetFindingAdjudicationForRound(ctx, artifact.RunID, artifact.Round)
		if err != nil {
			return err
		}
		byDigest, err := tx.GetFindingAdjudication(ctx, artifact.Digest)
		if err != nil {
			return err
		}
		if byRound.Revision != 1 || byRound.Digest != artifact.Digest || byDigest.Digest != artifact.Digest {
			t.Fatalf("migrated reconstruction = by-round %+v, by-digest %+v", byRound, byDigest)
		}
		return nil
	}); err != nil {
		t.Fatalf("reconstruct migrated row: %v", err)
	}
}

func TestFindingAdjudicationRevisionColumnsAreCrossChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	artifact := seedMigrationAdjudication(t, ctx, st)
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutFindingAdjudication(ctx, artifact)
	}); err != nil {
		t.Fatalf("put adjudication: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE finding_adjudications
        SET revision = 2, predecessor_digest = ? WHERE content_digest = ?`,
		migrationAdjudicationDigest("f"), artifact.Digest); err != nil {
		t.Fatalf("corrupt revision columns: %v", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetFindingAdjudication(ctx, artifact.Digest)
		return err
	}); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("corrupt revision read = %v, want errRowInconsistent", err)
	}
}
