package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestReadyReturnActionMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0062_")
	if got := rawVersion(t, db); got != 61 {
		t.Fatalf("prior schema version = %d, want 61", got)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE server_state SET revision = 11 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}

	production := legacyReadyItem(t, "run-production", true)
	fake := legacyReadyItem(t, "run-fake", false)
	seedLegacyReadyItem(t, ctx, db, production)
	seedLegacyReadyItem(t, ctx, db, fake)
	seedLegacyReadyBinding(t, ctx, db, production)

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 62 {
		t.Fatalf("schema version = %d, want 62", got)
	}

	got, snapshot, err := scanAttentionItemRecord(db.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture,
		        subject_run_id, readiness_summary, yield_history, entity_version,
		        as_of_revision, body
		 FROM attention_items WHERE id = ?`, production.ID))
	if err != nil {
		t.Fatalf("reconstruct migrated item: %v", err)
	}
	if !slices.Equal(got.RequestedDecision, productionReadyActions) {
		t.Fatalf("requested decisions = %v, want %v", got.RequestedDecision, productionReadyActions)
	}
	if got.ItemVersion != production.ItemVersion+1 {
		t.Fatalf("item version = %d, want %d", got.ItemVersion, production.ItemVersion+1)
	}
	if snapshot != (Snapshot{EntityVersion: 8, AsOfRevision: 12}) {
		t.Fatalf("snapshot = %+v, want entity version 8 at revision 12", snapshot)
	}
	readTx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	reader := ReadTx{tx: readTx}
	surface, err := reader.DecisionSurface(ctx, production.ID)
	if err != nil {
		t.Fatalf("read migrated surface: %v", err)
	}
	if surface.Epoch != 2 || !surface.Matches(got) ||
		got.DecisionSurface != (domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}) {
		t.Fatalf("migrated surface = %+v, item ref = %+v", surface, got.DecisionSurface)
	}
	if err := readTx.Rollback(); err != nil {
		t.Fatal(err)
	}

	fakeGot, fakeSnapshot, err := scanAttentionItemRecord(db.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture,
		        subject_run_id, readiness_summary, yield_history, entity_version,
		        as_of_revision, body
		 FROM attention_items WHERE id = ?`, fake.ID))
	if err != nil {
		t.Fatalf("reconstruct fake item: %v", err)
	}
	if !slices.Equal(fakeGot.RequestedDecision, legacyProductionReadyActions) ||
		fakeGot.ItemVersion != fake.ItemVersion ||
		fakeSnapshot != (Snapshot{EntityVersion: 7, AsOfRevision: 11}) {
		t.Fatalf("fake item changed: actions %v item version %d snapshot %+v",
			fakeGot.RequestedDecision, fakeGot.ItemVersion, fakeSnapshot)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("replay migration: %v", err)
	}
	var revision int64
	if err := db.QueryRowContext(ctx, `SELECT revision FROM server_state WHERE id = 1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 12 {
		t.Fatalf("revision after replay = %d, want 12", revision)
	}
}

func TestReadyReturnActionMigrationRejectsUnauthenticatedBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0062_")
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE server_state SET revision = 11 WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	item := legacyReadyItem(t, "run-corrupt-binding", true)
	seedLegacyReadyItem(t, ctx, db, item)
	seedLegacyReadyBinding(t, ctx, db, item)
	if _, err := db.ExecContext(ctx,
		`DELETE FROM execution_exports WHERE invocation_id = 'inv-production'`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	got, snapshot, err := scanAttentionItemRecord(db.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture,
		        subject_run_id, readiness_summary, yield_history, entity_version,
		        as_of_revision, body
		 FROM attention_items WHERE id = ?`, item.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.RequestedDecision, legacyProductionReadyActions) ||
		got.ItemVersion != item.ItemVersion || snapshot != (Snapshot{EntityVersion: 7, AsOfRevision: 11}) {
		t.Fatalf("unauthenticated ready item migrated: actions %v item version %d snapshot %+v",
			got.RequestedDecision, got.ItemVersion, snapshot)
	}
}

func legacyReadyItem(t *testing.T, runID domain.RunID, production bool) domain.AttentionItem {
	t.Helper()
	id := domain.ItemID("fake-ready-" + string(runID))
	if production {
		id = domain.ProductionReadyItemID(runID)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "Published owner/repo#450 and completed production verification.",
		RequestedDecision: slices.Clone(legacyProductionReadyActions),
		PRHeadSHA:         strings.Repeat("a", 40),
		PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion:       3, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	return item
}

func seedLegacyReadyItem(t *testing.T, ctx context.Context, db execer, item domain.AttentionItem) {
	t.Helper()
	body, err := encode(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
		(id, project_id, conversation_id, item_type, status, health_posture,
		 subject_run_id, readiness_summary, yield_history, entity_version,
		 as_of_revision, body)
		VALUES (?, ?, NULL, ?, ?, NULL, ?, NULL, NULL, 7, 11, ?)`,
		item.ID, item.ProjectID, item.Type, item.Status, *item.Subject.RunID, body); err != nil {
		t.Fatal(err)
	}
	referenceBody, err := encode(*item.PRReference)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, insertAttentionItemPRReferenceSQL,
		item.ID, item.PRReference.Repo, item.PRReference.Number, referenceBody); err != nil {
		t.Fatal(err)
	}
	seedDecisionSurface(t, ctx, db, item)
}

func seedLegacyReadyBinding(t *testing.T, ctx context.Context, db *sql.DB, item domain.AttentionItem) {
	t.Helper()
	runID := *item.Subject.RunID
	invocationID := domain.InvocationID("inv-production")
	stageID := domain.StageID("stage-production")
	attemptID := domain.AttemptID("attempt-production")
	run := domain.Run{
		ID: runID, ProjectID: item.ProjectID,
		SpecDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		PolicyDigest: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: "implementation",
			Attempts: []domain.Attempt{{
				ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID,
			}},
		}},
	}
	admittedAt := time.Date(2026, 8, 31, 11, 0, 0, 0, time.UTC)
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend: "migration-test", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("agent@sha256:" + strings.Repeat("f", 64)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest,
		InputDigest: domain.Digest("sha256:" + strings.Repeat("1", 64)),
		Base: domain.BaseRevision{
			Repo: item.PRReference.Repo, RepositoryID: 84958515,
			BaseRef: "main", BaseSHA: strings.Repeat("2", 40),
		},
		Workspace: "workspace-production", AdmittedAt: admittedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: item.PRHeadSHA,
		ManifestDigest: domain.Digest("sha256:" + strings.Repeat("3", 64)),
		RecordedAt:     admittedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.Digest("sha256:" + strings.Repeat("b", 64))
	publicationInvocationID := domain.InvocationID("publish-production")
	intentPayload, err := json.Marshal(readyPublicationIntent{
		FormatVersion: publicationrecord.IntentFormatCurrent,
		Identity:      identity, InvocationID: publicationInvocationID,
		Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		SourceHeadSHA:         export.HeadSHA,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("c", 64)),
		ProducingInvocationID: invocationID, ReservationRunID: runID,
	})
	if err != nil {
		t.Fatal(err)
	}
	outcomePayload, err := json.Marshal(readyPublicationOutcome{
		Identity: identity, Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		HeadSHA: export.HeadSHA, Branch: "freeside/publish/" + strings.Repeat("b", 16),
		PRNumber: item.PRReference.Number, EvidenceEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.ReadyItemPRBinding{
		ItemID: item.ID, RunID: runID,
		ProducingInvocationID: invocationID, PublicationInvocationID: publicationInvocationID,
		PublicationIdentity: identity,
		Repo:                item.PRReference.Repo,
		RepositoryID:        84958515,
		PRNumber:            item.PRReference.Number,
		BaseRef:             "main",
		HeadSHA:             item.PRHeadSHA,
		RecordedAt:          time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC),
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck // committed below
	writer := &WriteTx{
		InternalTx: InternalTx{ReadTx: ReadTx{
			tx: tx,
			admissionPolicy: domain.AdmissionPolicy{Floors: map[domain.OperatingMode]domain.CapabilitySnapshot{
				domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
			}},
		}},
		asOfRevision: 11,
	}
	if err := writer.PutRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordExecutionAdmission(ctx, admission); err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordExecutionExportRecord(ctx, export); err != nil {
		t.Fatal(err)
	}
	intentKey := "publish/" + string(publicationInvocationID) + "/" + readyPublicationIntentKind
	if _, _, err := writer.EnqueueOutbox(ctx, intentKey, readyPublicationIntentKind, intentPayload); err != nil {
		t.Fatal(err)
	}
	if err := writer.MarkOutboxDispatched(ctx, intentKey); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writer.RecordInbox(
		ctx, "publish.outcome/"+string(identity), "publish.outcome", outcomePayload,
	); err != nil {
		t.Fatal(err)
	}
	if err := writer.RecordReadyItemPRBinding(ctx, binding); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
