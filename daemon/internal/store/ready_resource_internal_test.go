package store

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func migrationsBeforeReadyResource(t *testing.T) fs.FS {
	t.Helper()
	prior := map[string]string{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "0028_ready_item_pr_binding.sql" ||
			entry.Name() == "0029_review_stage.sql" ||
			entry.Name() == "0030_native_review_observation.sql" ||
			entry.Name() == "0031_review_retry.sql" ||
			entry.Name() == "0032_review_recovery.sql" ||
			entry.Name() == "0033_publish_installation_mint_audit.sql" ||
			entry.Name() == "0034_review_configuration_recovery.sql" ||
			entry.Name() == "0035_attention_health_posture.sql" ||
			entry.Name() == "0036_attention_pr_reference.sql" ||
			entry.Name() == "0037_finding_dispositions.sql" ||
			entry.Name() == "0038_verification_readiness.sql" ||
			entry.Name() == "0039_outbox_payload_authentication.sql" ||
			entry.Name() == "0040_codex_reenrollment.sql" ||
			entry.Name() == "0041_effect_proposals.sql" ||
			entry.Name() == "0042_execution_identity_parallelism.sql" ||
			entry.Name() == "0043_intake_occurrences.sql" ||
			entry.Name() == "0044_agent_claims.sql" ||
			entry.Name() == "0045_projects.sql" ||
			entry.Name() == "0046_export_rejections.sql" ||
			entry.Name() == "0047_current_import_starts.sql" ||
			entry.Name() == "0048_production_attempts.sql" ||
			entry.Name() == "0049_production_attempt_publication_digest.sql" ||
			entry.Name() == "0050_attention_subject_run_binding.sql" || entry.IsDir() {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		prior[entry.Name()] = string(body)
	}
	return mapFS(prior)
}

// TestAttentionPRReferenceMigrationAppliesFromHead proves the synchronized
// ready item is backfilled from the durable first-party PR binding. The body
// rewrite advances both sync cursors, so a client cannot retain the old
// prose-only item as current after upgrade.
func TestAttentionPRReferenceMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0036_")
	if got := rawVersion(t, db); got != 35 {
		t.Fatalf("prior schema version = %d, want 35", got)
	}

	runID := domain.RunID("run-legacy-ready")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "legacy-ready", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "owner/repo#450 is published and ready for final review.",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA:         "cafebabe", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	legacyBody := strings.Replace(
		body, `"pr_reference":{"repo":"owner/repo","number":450},`, "", 1)
	if legacyBody == body {
		t.Fatalf("legacy PR reference strip did not apply: %s", body)
	}
	if _, err := db.ExecContext(ctx, `UPDATE server_state SET revision = 11 WHERE id = 1`); err != nil {
		t.Fatalf("seed server revision: %v", err)
	}
	// Only the two rows consumed by this migration matter here. Disable FK
	// checks while seeding their pre-upgrade shape instead of reconstructing
	// the publication workflow that migration 0028 already tests.
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
	   (id, project_id, conversation_id, item_type, status, health_posture,
	    entity_version, as_of_revision, body)
	 VALUES (?, ?, NULL, ?, ?, NULL, 3, 11, ?)`,
		item.ID, item.ProjectID, item.Type, item.Status, legacyBody); err != nil {
		t.Fatalf("seed legacy item: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO ready_item_pr_bindings
	   (item_id, run_id, producing_invocation_id, publication_invocation_id,
	    publication_identity, repository_id, pr_number, body, recorded_at)
	 VALUES (?, ?, 'inv-legacy', 'publish-legacy', 'sha256:publication',
	         84958515, 450, ?, '2026-08-09T12:00:00Z')`,
		item.ID, runID,
		`{"item_id":"legacy-ready","run_id":"run-legacy-ready","producing_invocation_id":"inv-legacy","publication_invocation_id":"publish-legacy","publication_identity":"sha256:publication","repo":"owner/repo","repository_id":84958515,"pr_number":450,"base_ref":"main","head_sha":"cafebabe","recorded_at":"2026-08-09T12:00:00Z"}`); err != nil {
		t.Fatalf("seed ready binding: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 50 {
		t.Fatalf("schema version = %d, want 50", got)
	}
	got, snapshot, err := scanAttentionItemRecord(db.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id,
		        entity_version, as_of_revision, body
		 FROM attention_items WHERE id = ?`, item.ID))
	if err != nil {
		t.Fatalf("reconstruct backfilled item: %v", err)
	}
	if got.PRReference == nil || *got.PRReference != (domain.PRReference{Repo: "owner/repo", Number: 450}) {
		t.Fatalf("backfilled PR reference = %+v", got.PRReference)
	}
	if got.RequestedDecision[0] != domain.ActionOpenPR {
		t.Fatalf("wire action = %q, want compatibility token %q", got.RequestedDecision[0], domain.ActionOpenPR)
	}
	if snapshot != (Snapshot{EntityVersion: 4, AsOfRevision: 12}) {
		t.Fatalf("backfilled snapshot = %+v, want entity version 4 at revision 12", snapshot)
	}
	var anchoredItemID, anchoredRepo, anchoredBody string
	var anchoredNumber int
	if err := db.QueryRowContext(ctx, `SELECT item_id, repo, pr_number, body
		FROM attention_item_pr_references WHERE item_id = ?`, item.ID).Scan(
		&anchoredItemID, &anchoredRepo, &anchoredNumber, &anchoredBody,
	); err != nil {
		t.Fatalf("read migrated pr-reference anchor: %v", err)
	}
	if anchoredItemID != string(item.ID) || anchoredRepo != "owner/repo" ||
		anchoredNumber != 450 || anchoredBody != `{"repo":"owner/repo","number":450}` {
		t.Fatalf("migrated pr-reference anchor = %q %q %d %q",
			anchoredItemID, anchoredRepo, anchoredNumber, anchoredBody)
	}
}

// TestAttentionPRReferenceMigrationBackfillsLegacyFakePublication proves the
// pre-binding fake lane is recovered from its immutable task, dispatched
// publication intent, and recorded outcome rather than from reason prose.
func TestAttentionPRReferenceMigrationBackfillsLegacyFakePublication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0036_")

	runID := domain.RunID("run-legacy-fake-ready")
	fixedTime := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	task := fakepublication.Task{
		Version: fakepublication.TaskVersion, RunID: runID, ProjectID: "proj-1",
		StoreEpoch: "epoch-legacy", WorkspaceDir: "/tmp/legacy-workspace",
		HandoffDir:    "/tmp/legacy-handoff",
		HandoffDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)),
		Repo:          "owner/repo", BaseRef: "main", BaseSHA: strings.Repeat("1", 40),
		AllowedPaths:             []string{"daemon/**"},
		RecipeDigest:             domain.Digest("sha256:" + strings.Repeat("e", 64)),
		RecipePath:               ".freeside/verification.yaml",
		TrustProfileDigest:       domain.Digest("sha256:" + strings.Repeat("f", 64)),
		VerificationInvocationID: "verify-fake-legacy",
		PublicationInvocationID:  "publish-fake-legacy",
		Title:                    "Legacy publication", Body: "Legacy publication body",
		CommitDate: fixedTime, CommitDateExplicit: true, StartedAt: fixedTime,
		OperatingMode: fakepublication.OperatingModeAttended,
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: fakepublication.ReadyItemID(runID), ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "presentation text is not the migration authority",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA:         "cafebabe", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 451},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	legacyDigest, err := fakepublication.TerminalDigestBeforePRReference(task, item)
	if err != nil {
		t.Fatalf("legacy terminal digest: %v", err)
	}
	item.Reason += "\n\n<!-- freeside:fake-publication-terminal=" + string(legacyDigest) + " -->"
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	legacyBody := strings.Replace(
		body, `"pr_reference":{"repo":"owner/repo","number":451},`, "", 1)
	if legacyBody == body {
		t.Fatalf("legacy PR reference strip did not apply: %s", body)
	}
	identity := "sha256:" + strings.Repeat("a", 64)
	taskPayload, err := fakepublication.EncodeTask(task)
	if err != nil {
		t.Fatalf("encode fake publication task: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE server_state SET revision = 20 WHERE id = 1`); err != nil {
		t.Fatalf("seed server revision: %v", err)
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("disable foreign keys: %v", err)
	}
	for _, seed := range []struct {
		query string
		args  []any
	}{
		{
			`INSERT INTO attention_items
		   (id, project_id, conversation_id, item_type, status, health_posture,
		    entity_version, as_of_revision, body)
			 VALUES (?, ?, NULL, ?, ?, NULL, 1, 20, ?)`,
			[]any{item.ID, item.ProjectID, item.Type, item.Status, legacyBody},
		},
		{
			`INSERT INTO outbox (idempotency_key, kind, payload, status, created_at)
		 VALUES (?, 'engine.fake_publication', ?, 'dispatched', '2026-08-09T12:00:00Z')`,
			[]any{fakepublication.TaskKey(runID), taskPayload},
		},
		{
			`INSERT INTO outbox (idempotency_key, kind, payload, status, created_at)
		 VALUES ('publish/publish-fake-legacy/publish.publication', 'publish.publication', ?, 'dispatched', '2026-08-09T12:01:00Z')`,
			[]any{[]byte(`{"identity":"` + identity + `","invocation_id":"publish-fake-legacy","repo":"owner/repo","base_ref":"main","source_head_sha":"cafebabe","authorization_id":"sha256:` + strings.Repeat("c", 64) + `"}`)},
		},
		{
			`INSERT INTO inbox (idempotency_key, kind, payload, created_at)
		 VALUES (?, 'publish.outcome', ?, '2026-08-09T12:02:00Z')`,
			[]any{"publish.outcome/" + identity, []byte(`{"identity":"` + identity + `","repo":"owner/repo","base_ref":"main","head_sha":"cafebabe","branch":"freeside/publish/aaaaaaaaaaaaaaaa","pr_number":451,"evidence_eligible":true}`)},
		},
	} {
		if _, err := db.ExecContext(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed legacy fake publication: %v", err)
		}
	}
	mismatchRunID := domain.RunID("run-legacy-fake-mismatch")
	mismatch := item
	mismatch.ID = fakepublication.ReadyItemID(mismatchRunID)
	mismatch.Subject = domain.Subject{
		Type: domain.SubjectRun, ID: domain.SubjectID(mismatchRunID), RunID: &mismatchRunID,
	}
	mismatch.PRHeadSHA = "deadbeef"
	mismatchTask := task
	mismatchTask.RunID = mismatchRunID
	mismatchTask.PublicationInvocationID = "publish-fake-mismatch"
	mismatchTask.VerificationInvocationID = "verify-fake-mismatch"
	mismatchTaskPayload, err := fakepublication.EncodeTask(mismatchTask)
	if err != nil {
		t.Fatalf("encode mismatched task: %v", err)
	}
	mismatchBody, err := encode(mismatch)
	if err != nil {
		t.Fatalf("encode mismatched item: %v", err)
	}
	mismatchBody = strings.Replace(
		mismatchBody, `"pr_reference":{"repo":"owner/repo","number":451},`, "", 1)
	mismatchIdentity := "sha256:" + strings.Repeat("b", 64)
	for _, seed := range []struct {
		query string
		args  []any
	}{
		{
			`INSERT INTO attention_items
		   (id, project_id, conversation_id, item_type, status, health_posture,
		    entity_version, as_of_revision, body)
			 VALUES (?, ?, NULL, ?, ?, NULL, 1, 20, ?)`,
			[]any{mismatch.ID, mismatch.ProjectID, mismatch.Type, mismatch.Status, mismatchBody},
		},
		{
			`INSERT INTO outbox (idempotency_key, kind, payload, status, created_at)
		 VALUES (?, 'engine.fake_publication', ?, 'dispatched', '2026-08-09T12:00:00Z')`,
			[]any{fakepublication.TaskKey(mismatchRunID), mismatchTaskPayload},
		},
		{
			`INSERT INTO outbox (idempotency_key, kind, payload, status, created_at)
		 VALUES ('publish/publish-fake-mismatch/publish.publication', 'publish.publication', ?, 'dispatched', '2026-08-09T12:01:00Z')`,
			[]any{[]byte(`{"identity":"` + mismatchIdentity + `","invocation_id":"publish-fake-mismatch","repo":"owner/repo","base_ref":"main","source_head_sha":"deadbeef","authorization_id":"sha256:` + strings.Repeat("c", 64) + `"}`)},
		},
		{
			`INSERT INTO inbox (idempotency_key, kind, payload, created_at)
		 VALUES (?, 'publish.outcome', ?, '2026-08-09T12:02:00Z')`,
			[]any{"publish.outcome/" + mismatchIdentity, []byte(`{"identity":"` + mismatchIdentity + `","repo":"owner/repo","base_ref":"main","head_sha":"foreign-head","branch":"freeside/publish/bbbbbbbbbbbbbbbb","pr_number":452,"evidence_eligible":true}`)},
		},
	} {
		if _, err := db.ExecContext(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed mismatched fake publication: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		t.Fatalf("enable foreign keys: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	got, snapshot, err := scanAttentionItemRecord(db.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id,
		        entity_version, as_of_revision, body
		 FROM attention_items WHERE id = ?`, item.ID))
	if err != nil {
		t.Fatalf("reconstruct backfilled fake item: %v", err)
	}
	if got.PRReference == nil || *got.PRReference != (domain.PRReference{Repo: "owner/repo", Number: 451}) {
		t.Fatalf("backfilled fake PR reference = %+v", got.PRReference)
	}
	if snapshot != (Snapshot{EntityVersion: 2, AsOfRevision: 21}) {
		t.Fatalf("backfilled fake snapshot = %+v, want entity version 2 at revision 21", snapshot)
	}
	var anchoredBody string
	if err := db.QueryRowContext(ctx,
		`SELECT body FROM attention_item_pr_references WHERE item_id = ?`, item.ID,
	).Scan(&anchoredBody); err != nil {
		t.Fatalf("read migrated fake pr-reference anchor: %v", err)
	}
	if anchoredBody != `{"repo":"owner/repo","number":451}` {
		t.Fatalf("migrated fake pr-reference anchor = %q", anchoredBody)
	}
	var mismatchReferenceType string
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(json_type(body, '$.pr_reference'), 'absent')
		 FROM attention_items WHERE id = ?`, mismatch.ID,
	).Scan(&mismatchReferenceType); err != nil {
		t.Fatalf("read mismatched fake item: %v", err)
	}
	if mismatchReferenceType != "absent" {
		t.Fatalf("mismatched fake item pr_reference type = %q, want absent", mismatchReferenceType)
	}
	var mismatchAnchors int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM attention_item_pr_references WHERE item_id = ?`, mismatch.ID,
	).Scan(&mismatchAnchors); err != nil {
		t.Fatalf("count mismatched fake anchors: %v", err)
	}
	if mismatchAnchors != 0 {
		t.Fatalf("mismatched fake anchors = %d, want 0", mismatchAnchors)
	}
}

// TestReadyItemPRReferenceAnchorRegatesWithoutProductionBinding proves the
// fake-publication and development-fixture path: its creating Put stamps an
// immutable coordinate anchor even though no production binding exists, and
// both synchronized read shapes reject a later body-only retarget.
func TestReadyItemPRReferenceAnchorRegatesWithoutProductionBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runID := domain.RunID("run-fake-ready-anchor")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "fake-ready-anchor", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "fake publication ready", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA: "cafed00d", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetReadyItemPRBinding(ctx, item.ID)
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("production binding error = %v, want ErrNotFound", err)
	}
	itemBody, err := encode(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"repo", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.repo', 'other/repo') WHERE id = ?`},
		{"pr number", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.number', 999) WHERE id = ?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.sql, item.ID); err != nil {
				t.Fatal(err)
			}
			for name, read := range map[string]func(*ReadTx) error{
				"get": func(tx *ReadTx) error {
					_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
					return err
				},
				"list": func(tx *ReadTx) error {
					_, err := tx.ListAttentionItems(ctx)
					return err
				},
			} {
				t.Run(name, func(t *testing.T) {
					err := st.Read(ctx, read)
					if !errors.Is(err, errRowInconsistent) {
						t.Fatalf("retargeted read error = %v, want errRowInconsistent", err)
					}
				})
			}
			if _, err := st.db.ExecContext(ctx,
				`UPDATE attention_items SET body = ? WHERE id = ?`, itemBody, item.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestReadyItemBindingRegatesEveryResourceCoordinate corrupts the binding as
// though a reconstructed row or internal caller had retargeted it. Matching
// extracted columns are changed with the JSON, so only the immutable
// admission/export/outcome anchors can reject each forged coordinate.
func TestReadyItemBindingRegatesEveryResourceCoordinate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
		domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	runID := domain.RunID("run-ready-anchor")
	invocationID := domain.InvocationID("inv-ready-anchor")
	stageID := domain.StageID("stage-ready-anchor")
	attemptID := domain.AttemptID("attempt-ready-anchor")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run := domain.Run{
		ID: runID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: runID, Name: "implementation",
			Attempts: []domain.Attempt{{ID: attemptID, StageID: stageID, Number: 1, InvocationID: invocationID}},
		}},
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-ready-anchor", ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "published", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA: "cafed00d", PRReference: &domain.PRReference{Repo: "owner/repo", Number: 450},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatal(err)
	}
	admittedAt := time.Date(2026, 8, 2, 11, 0, 0, 0, time.UTC)
	admission, err := domain.NewExecutionAdmission(domain.ExecutionAdmissionInput{
		InvocationID: invocationID, RunID: runID, StageID: stageID, AttemptID: attemptID,
		Backend: "ready-anchor-test", Capabilities: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
		EgressProfile: domain.EgressCleanVerification,
		ImageRef:      domain.ImageRef("ghcr.io/freeside-ai/agent@sha256:" + strings.Repeat("a", 64)),
		SpecDigest:    run.SpecDigest, PolicyDigest: run.PolicyDigest, InputDigest: "sha256:input",
		Base:      domain.BaseRevision{Repo: "owner/repo", RepositoryID: 424242, BaseRef: "main", BaseSHA: "deadbeef"},
		Workspace: "workspace", AdmittedAt: admittedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	export, err := domain.NewExecutionExport(domain.ExecutionExportInput{
		InvocationID: invocationID, AdmissionID: admission.ID,
		ObservedBaseSHA: admission.Base.BaseSHA, HeadSHA: item.PRHeadSHA,
		ManifestDigest: "sha256:manifest", RecordedAt: admittedAt.Add(time.Minute),
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.Digest("sha256:" + strings.Repeat("a", 64))
	publicationInvocationID := domain.InvocationID("publish-production-" + string(runID))
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
	payload, err := json.Marshal(readyPublicationOutcome{
		Identity: identity, Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		HeadSHA: export.HeadSHA, Branch: "freeside/publish/" + strings.Repeat("a", 16),
		PRNumber: 450, EvidenceEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := domain.ReadyItemPRBinding{
		ItemID: item.ID, RunID: runID, ProducingInvocationID: invocationID,
		PublicationInvocationID: publicationInvocationID,
		PublicationIdentity:     identity, Repo: admission.Base.Repo,
		RepositoryID: admission.Base.RepositoryID, PRNumber: 450,
		BaseRef: admission.Base.BaseRef, HeadSHA: export.HeadSHA,
		RecordedAt: admittedAt.Add(2 * time.Minute),
	}
	intentKey := "publish/" + string(publicationInvocationID) + "/" + readyPublicationIntentKind
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RecordExecutionAdmission(ctx, admission); err != nil {
			return err
		}
		if err := tx.RecordExecutionExport(ctx, export); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, readyPublicationIntentKind, intentPayload); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		if _, _, err := tx.RecordInbox(ctx, "publish.outcome/"+string(identity), "publish.outcome", payload); err != nil {
			return err
		}
		return tx.RecordReadyItemPRBinding(ctx, binding)
	}); err != nil {
		t.Fatal(err)
	}

	assertAttentionReadRejected := func(t *testing.T) {
		t.Helper()
		for _, read := range []struct {
			name string
			fn   func(*ReadTx) error
		}{
			{"get", func(tx *ReadTx) error {
				_, _, err := tx.GetAttentionItemSnapshot(ctx, item.ID)
				return err
			}},
			{"list", func(tx *ReadTx) error {
				_, err := tx.ListAttentionItems(ctx)
				return err
			}},
		} {
			t.Run(read.name, func(t *testing.T) {
				err := st.Read(ctx, read.fn)
				if !errors.Is(err, errRowInconsistent) {
					t.Fatalf("retargeted attention item error = %v, want errRowInconsistent", err)
				}
			})
		}
	}
	itemBody, err := encode(item)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"repo", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.repo', 'other/repo') WHERE id = ?`},
		{"pr number", `UPDATE attention_items SET body = json_set(body, '$.pr_reference.number', 451) WHERE id = ?`},
	} {
		t.Run("attention item "+tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.sql, item.ID); err != nil {
				t.Fatal(err)
			}
			assertAttentionReadRejected(t)
			if _, err := st.db.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, itemBody, item.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	originalBody, err := encode(binding)
	if err != nil {
		t.Fatal(err)
	}
	foreignIdentity := domain.Digest("sha256:" + strings.Repeat("b", 64))
	foreignPublicationInvocationID := domain.InvocationID("publish-production-run-ready-foreign")
	foreignIntent, err := json.Marshal(readyPublicationIntent{
		FormatVersion: publicationrecord.IntentFormatCurrent,
		Identity:      foreignIdentity, InvocationID: foreignPublicationInvocationID,
		Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		SourceHeadSHA:         export.HeadSHA,
		AuthorizationID:       domain.Digest("sha256:" + strings.Repeat("d", 64)),
		ProducingInvocationID: "inv-ready-foreign", ReservationRunID: "run-ready-foreign",
	})
	if err != nil {
		t.Fatal(err)
	}
	foreignOutcome, err := json.Marshal(readyPublicationOutcome{
		Identity: foreignIdentity, Repo: admission.Base.Repo, BaseRef: admission.Base.BaseRef,
		HeadSHA: export.HeadSHA, Branch: "freeside/publish/" + strings.Repeat("b", 16),
		PRNumber: 451, EvidenceEligible: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		intentKey := "publish/" + string(foreignPublicationInvocationID) + "/" + readyPublicationIntentKind
		if _, _, err := tx.EnqueueOutbox(ctx, intentKey, readyPublicationIntentKind, foreignIntent); err != nil {
			return err
		}
		if err := tx.MarkOutboxDispatched(ctx, intentKey); err != nil {
			return err
		}
		_, _, err := tx.RecordInbox(ctx, "publish.outcome/"+string(foreignIdentity), "publish.outcome", foreignOutcome)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{name: "repo", sql: `UPDATE ready_item_pr_bindings SET body = json_set(body, '$.repo', 'other/repo') WHERE item_id = ?`},
		{name: "repository id", sql: `UPDATE ready_item_pr_bindings SET repository_id = 515151, body = json_set(body, '$.repository_id', 515151) WHERE item_id = ?`},
		{name: "pr number", sql: `UPDATE ready_item_pr_bindings SET pr_number = 451, body = json_set(body, '$.pr_number', 451) WHERE item_id = ?`},
		{name: "base ref", sql: `UPDATE ready_item_pr_bindings SET body = json_set(body, '$.base_ref', 'release') WHERE item_id = ?`},
		{name: "publication identity", sql: `UPDATE ready_item_pr_bindings SET publication_identity = ?, body = json_set(body, '$.publication_identity', ?) WHERE item_id = ?`, args: []any{"sha256:" + strings.Repeat("b", 64), "sha256:" + strings.Repeat("b", 64)}},
		{name: "cross-run publication", sql: `UPDATE ready_item_pr_bindings
			SET publication_invocation_id = ?, publication_identity = ?, pr_number = ?,
			body = json_set(body, '$.publication_invocation_id', ?, '$.publication_identity', ?, '$.pr_number', ?)
			WHERE item_id = ?`, args: []any{
			foreignPublicationInvocationID, foreignIdentity, 451,
			foreignPublicationInvocationID, foreignIdentity, 451,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(tc.args, item.ID)
			if _, err := st.db.ExecContext(ctx, tc.sql, args...); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetReadyItemPRBinding(ctx, item.ID)
				return err
			})
			if err == nil {
				t.Fatalf("retargeted binding read error = %v", err)
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE ready_item_pr_bindings
				SET repository_id = ?, pr_number = ?, publication_invocation_id = ?,
				publication_identity = ?, body = ? WHERE item_id = ?`,
				binding.RepositoryID, binding.PRNumber, binding.PublicationInvocationID,
				binding.PublicationIdentity, originalBody, item.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		sql  string
		args []any
	}{
		{name: "kind", sql: `UPDATE outbox SET kind = 'other.kind' WHERE idempotency_key = ?`},
		{name: "dispatch state", sql: `UPDATE outbox SET status = 'pending' WHERE idempotency_key = ?`},
		{name: "identity", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.identity', ?) AS BLOB) WHERE idempotency_key = ?`, args: []any{foreignIdentity}},
		{name: "publication invocation", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.invocation_id', ?) AS BLOB) WHERE idempotency_key = ?`, args: []any{foreignPublicationInvocationID}},
		{name: "repo", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.repo', 'other/repo') AS BLOB) WHERE idempotency_key = ?`},
		{name: "base ref", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.base_ref', 'release') AS BLOB) WHERE idempotency_key = ?`},
		{name: "head", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.source_head_sha', 'feedface') AS BLOB) WHERE idempotency_key = ?`},
		{name: "authorization", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.authorization_id', 'bad') AS BLOB) WHERE idempotency_key = ?`},
		{name: "producing invocation", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.producing_invocation_id', 'inv-ready-foreign') AS BLOB) WHERE idempotency_key = ?`},
		{name: "reservation run", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.reservation_run_id', 'run-ready-foreign') AS BLOB) WHERE idempotency_key = ?`},
		{name: "unknown field", sql: `UPDATE outbox SET payload = CAST(json_set(payload, '$.unexpected', true) AS BLOB) WHERE idempotency_key = ?`},
	} {
		t.Run("publication intent "+tc.name, func(t *testing.T) {
			args := append(tc.args, intentKey)
			if _, err := st.db.ExecContext(ctx, tc.sql, args...); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetReadyItemPRBinding(ctx, item.ID)
				return err
			})
			if err == nil {
				t.Fatal("corrupt publication intent was accepted")
			}
			if _, err := st.db.ExecContext(ctx, `UPDATE outbox
				SET kind = ?, status = ?, payload = ? WHERE idempotency_key = ?`,
				readyPublicationIntentKind, outboxStatusDispatched, intentPayload, intentKey); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestReadyResourceMigrationBackfillsOnlyExactHistory proves restart recovery
// for ready items created before #463 and the fail-closed authority rule.
func TestReadyResourceMigrationBackfillsOnlyExactHistory(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		extraOutcome bool
		omitIntent   bool
		wantRows     int
	}{
		{name: "exact", wantRows: 1},
		{name: "unrelated outcome ignored", extraOutcome: true, wantRows: 1},
		{name: "missing intent", omitIntent: true, wantRows: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db := openRaw(t)
			if err := migrate(ctx, db, migrationsBeforeReadyResource(t)); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO runs
				(id, project_id, policy_digest, entity_version, as_of_revision, body)
				VALUES ('run-1', 'project-1', 'sha256:policy', 1, 1, '{}')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
				(id, project_id, conversation_id, entity_version, as_of_revision, body, item_type, status)
				VALUES ('item-ready-1', 'project-1', NULL, 1, 1,
				'{"subject":{"subject_type":"run","run_id":"run-1"},"pr_head_sha":"cafed00d"}',
				'ready_for_final_review', 'open')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO execution_admissions
				(invocation_id, id, run_id, stage_id, attempt_id, operating_mode,
				 auth_identity_id, admitted_at, body)
				VALUES ('inv-1', 'sha256:admission', 'run-1', 'stage-1', 'attempt-1',
					'attended_dev', NULL, '2026-08-02T11:00:00Z',
					'{"base":{"repo":"owner/repo","repository_id":424242,"base_ref":"main"}}')`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO execution_exports
				(invocation_id, admission_id, observed_base_sha, head_sha, manifest_digest,
				 evidence_manifest_digest, commit_plan_present, recorded_at, body)
				VALUES ('inv-1', 'sha256:admission', 'deadbeef', 'cafed00d', 'sha256:manifest',
				 NULL, 0, '2026-08-02T11:15:00Z', '{}')`); err != nil {
				t.Fatal(err)
			}
			if !tc.omitIntent {
				identity := "sha256:" + strings.Repeat("a", 64)
				if _, err := db.ExecContext(ctx, `INSERT INTO outbox
					(idempotency_key, kind, payload, status, created_at)
					VALUES ('publish/publish-production-run-1/publish.publication',
					'publish.publication', CAST(json_object(
						'identity', ?, 'invocation_id', 'publish-production-run-1',
						'repo', 'owner/repo', 'base_ref', 'main', 'source_head_sha', 'cafed00d',
						'authorization_id', ?, 'producing_invocation_id', 'inv-1',
						'reservation_run_id', 'run-1') AS BLOB),
					'dispatched', '2026-08-02T11:20:00Z')`,
					identity, "sha256:"+strings.Repeat("c", 64)); err != nil {
					t.Fatal(err)
				}
			}
			insertOutcome := func(identity string, number int) {
				t.Helper()
				if _, err := db.ExecContext(ctx, `INSERT INTO inbox
					(idempotency_key, kind, payload, status, created_at)
					VALUES (?, 'publish.outcome', CAST(json_object(
						'identity', ?,
						'repo', 'owner/repo', 'base_ref', 'main',
						'head_sha', 'cafed00d', 'branch', 'freeside/publish/' || substr(?, 8, 16),
						'pr_number', ?, 'evidence_eligible', json('true')) AS BLOB),
						'pending', '2026-08-02T11:30:00Z')`,
					"publish.outcome/"+identity, identity, identity, number); err != nil {
					t.Fatal(err)
				}
			}
			insertOutcome("sha256:"+strings.Repeat("a", 64), 450)
			if tc.extraOutcome {
				insertOutcome("sha256:"+strings.Repeat("b", 64), 451)
			}
			if err := migrate(ctx, db, migrations.FS); err != nil {
				t.Fatal(err)
			}
			var count int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM ready_item_pr_bindings`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != tc.wantRows {
				t.Fatalf("binding rows = %d, want %d", count, tc.wantRows)
			}
			if count == 0 {
				return
			}
			var itemID, runID, invocationID, publicationInvocationID, identity, repo, baseRef, headSHA string
			var repositoryID int64
			var prNumber int
			if err := db.QueryRowContext(ctx, `SELECT item_id, run_id, producing_invocation_id,
				publication_invocation_id, publication_identity, repository_id,
				pr_number, json_extract(body, '$.repo'), json_extract(body, '$.base_ref'),
				json_extract(body, '$.head_sha') FROM ready_item_pr_bindings`).Scan(
				&itemID, &runID, &invocationID, &publicationInvocationID, &identity, &repositoryID,
				&prNumber, &repo, &baseRef, &headSHA,
			); err != nil {
				t.Fatal(err)
			}
			if itemID != "item-ready-1" || runID != "run-1" || repositoryID != 424242 ||
				invocationID != "inv-1" || publicationInvocationID != "publish-production-run-1" ||
				identity != "sha256:"+strings.Repeat("a", 64) ||
				prNumber != 450 || repo != "owner/repo" || baseRef != "main" || headSHA != "cafed00d" {
				t.Fatalf("backfill = %s %s %s %s %s %d %d %s %s %s",
					itemID, runID, invocationID, publicationInvocationID, identity,
					repositoryID, prNumber, repo, baseRef, headSHA)
			}
		})
	}
}
