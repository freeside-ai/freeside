package store

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
			entry.Name() == "0033_publish_installation_mint_audit.sql" || entry.IsDir() {
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
		PRHeadSHA: "cafed00d", ItemVersion: 1,
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
		Identity: identity, InvocationID: publicationInvocationID,
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
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
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

	originalBody, err := encode(binding)
	if err != nil {
		t.Fatal(err)
	}
	foreignIdentity := domain.Digest("sha256:" + strings.Repeat("b", 64))
	foreignPublicationInvocationID := domain.InvocationID("publish-production-run-ready-foreign")
	foreignIntent, err := json.Marshal(readyPublicationIntent{
		Identity: foreignIdentity, InvocationID: foreignPublicationInvocationID,
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
