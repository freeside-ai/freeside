package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/domain/domaintest"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestRetireBlockedItemsMigrationClosesOpenRows proves migration 0096 closes
// an open blocked item in lockstep across the status column and the body,
// advances the item version and both sync cursors so a client receives the
// close, leaves a closed blocked item byte-identical, deletes nothing, and
// leaves both rows reading through the store's trust gates.
func TestRetireBlockedItemsMigrationClosesOpenRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/retire-blocked.db", Options{})

	open := plantClosedBlockedWait(t, ctx, st, "open")
	closed := plantClosedBlockedWait(t, ctx, st, "closed")
	// A pre-retirement row is planted closed and reopened in SQL, so this test
	// does not depend on the store accepting an open blocked item.
	if _, err := st.db.ExecContext(ctx, `UPDATE attention_items
		SET status = 'open', body = json_set(body, '$.status', 'open') WHERE id = ?`, open); err != nil {
		t.Fatal(err)
	}
	// 0096 changes no schema, so forgetting it leaves exactly the database an
	// earlier daemon would hand the migration runner.
	forgetRetireBlockedItems(t, ctx, st.db)

	beforeOpen := readBlockedRow(t, ctx, st.db, open)
	beforeClosed := readBlockedRow(t, ctx, st.db, closed)
	beforeSync := serverRevision(t, ctx, st.db)
	beforeCount := attentionItemCount(t, ctx, st.db)
	if beforeOpen.status != "open" || beforeClosed.status != "superseded" {
		t.Fatalf("planted statuses = %q and %q, want open and superseded", beforeOpen.status, beforeClosed.status)
	}

	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)

	afterOpen := readBlockedRow(t, ctx, st.db, open)
	afterSync := serverRevision(t, ctx, st.db)
	if afterOpen.status != "superseded" || afterOpen.bodyStatus != "superseded" {
		t.Fatalf("migrated status = column %q body %q, want superseded in both", afterOpen.status, afterOpen.bodyStatus)
	}
	if afterOpen.itemVersion != beforeOpen.itemVersion+1 {
		t.Fatalf("item_version = %d, want %d (advanced once)", afterOpen.itemVersion, beforeOpen.itemVersion+1)
	}
	if afterOpen.entityVersion != beforeOpen.entityVersion+1 {
		t.Fatalf("entity_version = %d, want %d (advanced once)", afterOpen.entityVersion, beforeOpen.entityVersion+1)
	}
	if afterSync != beforeSync+1 {
		t.Fatalf("server revision = %d, want %d (advanced once)", afterSync, beforeSync+1)
	}
	if afterOpen.asOfRevision != afterSync || afterOpen.asOfRevision <= beforeOpen.asOfRevision {
		t.Fatalf("as_of_revision = %d, want new server revision %d (was %d)",
			afterOpen.asOfRevision, afterSync, beforeOpen.asOfRevision)
	}
	if afterClosed := readBlockedRow(t, ctx, st.db, closed); afterClosed != beforeClosed {
		t.Fatalf("closed row changed:\n got %+v\nwant %+v", afterClosed, beforeClosed)
	}
	if got := attentionItemCount(t, ctx, st.db); got != beforeCount {
		t.Fatalf("attention item count = %d, want %d (no row deleted)", got, beforeCount)
	}

	// Both rows still reconstruct: the read re-runs the blocked-wait binding
	// and decision-surface gates against the rewritten body.
	if err := st.Read(ctx, func(tx *ReadTx) error {
		for _, id := range []domain.ItemID{open, closed} {
			item, err := tx.GetAttentionItem(ctx, id)
			if err != nil {
				return err
			}
			if item.Type != domain.AttentionBlocked || item.Status != domain.StatusSuperseded || item.BlockedOn == nil {
				t.Fatalf("item %s after migration = %+v", id, item)
			}
		}
		stillOpen, err := tx.ListOpenAttentionItems(ctx, domain.AttentionBlocked)
		if err != nil {
			return err
		}
		if len(stillOpen) != 0 {
			t.Fatalf("open blocked items after migration = %+v", stillOpen)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// With no open row left the migration is a no-op: a second application
	// advances neither the cursor nor any row.
	forgetRetireBlockedItems(t, ctx, st.db)
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if again := readBlockedRow(t, ctx, st.db, open); again != afterOpen {
		t.Fatalf("second application changed the row:\n got %+v\nwant %+v", again, afterOpen)
	}
	if got := serverRevision(t, ctx, st.db); got != afterSync {
		t.Fatalf("server revision after a second application = %d, want %d", got, afterSync)
	}
}

// forgetRetireBlockedItems returns the database to the one a daemon at 0095
// holds. 0096 changes no schema, so its record is all there is to forget; the
// later migrations only add tables and columns, which are dropped with their
// records so the recorded history stays contiguous.
func forgetRetireBlockedItems(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if _, err := db.ExecContext(ctx, `ALTER TABLE execution_outcomes DROP COLUMN cause`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE run_hold_observations DROP COLUMN detail_check;
		ALTER TABLE run_hold_observations DROP COLUMN detail_agent;
		ALTER TABLE run_hold_observations DROP COLUMN detail_launch_stage;
		ALTER TABLE run_hold_observations DROP COLUMN detail_launch_digest`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE work_unit_rebase_proofs`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE task_workflow_closures; DROP TABLE task_attention_workflow_origins`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE publish_installation_mint_holds`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE review_dispute_rows`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE review_admissions`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE admitted_image_pointers`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE run_hold_observations DROP COLUMN upkeep_pending`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE run_hold_observations DROP COLUMN upkeep_refusal_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE run_hold_observations DROP COLUMN upkeep_item_id`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE run_hold_observations DROP COLUMN upkeep_task_id`); err != nil {
		t.Fatal(err)
	}
	rewindUsagePools(t, ctx, db)
	if _, err := db.ExecContext(ctx, `DROP TABLE repository_tip_observations`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `ALTER TABLE task_lines DROP COLUMN provenance`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE image_policy_adoptions`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DROP TABLE image_upkeep_refusals`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM schema_migrations WHERE name IN
		('0096_retire_blocked_items.sql', '0097_review_admissions.sql', '0098_review_dispute_rows.sql',
		'0099_publish_installation_mint_holds.sql', '0100_task_workflow_closures.sql',
		'0101_work_unit_rebase_proofs.sql',
		'0102_usage_observations_review.sql', '0103_run_hold_admission_detail.sql',
		'0104_execution_outcome_cause.sql',
		'0105_admitted_image_pointers.sql',
		'0106_schedules_image_upkeep.sql',
		'0107_run_hold_image_upkeep.sql',
		'0108_image_upkeep.sql',
		'0109_task_submission_origin.sql',
		'0110_image_policy_adoption.sql',
		'0111_task_line_provenance.sql')`); err != nil {
		t.Fatal(err)
	}
}

// plantClosedBlockedWait stores a run, its specification approval, and a
// superseded blocked item waiting on that approval, and returns the blocked
// item's ID. The name keeps one fixture's rows apart from another's.
func plantClosedBlockedWait(t *testing.T, ctx context.Context, st *Store, name string) domain.ItemID {
	t.Helper()
	at := time.Date(2026, 10, 10, 9, 26, 0, 0, time.UTC)
	run := domain.Run{
		ID: domain.RunID("run-retire-blocked-" + name), ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	runID := run.ID
	subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID}
	// The store's specification-approval gate binds these identities: the
	// artifact and the approval are named for the implementation run and
	// iteration, and the specification run's terminal record names them both.
	invocationID := domain.SpecificationInvocationID(run.ID, 1)
	implementation := "implementation-retire-blocked-" + name + "-1"
	specification, err := domain.NewArtifact(domain.ArtifactInput{
		ID: domain.ArtifactID("spec-" + implementation), Type: domain.ArtifactKindSpecification,
		Digest: domain.Digest("sha256:specification-" + name),
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: invocationID,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID("spec-approval-" + implementation), ProjectID: run.ProjectID,
		Subject: subject,
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason: "approve the specification", RequestedDecision: []domain.Action{domain.ActionApprove},
		AgentClaims: []domain.AgentClaim{{
			Label: "Specification", Artifact: specification.ID, Digest: specification.Digest,
			Provenance: specification.Provenance,
			Metadata:   claimMeta(domain.EvidenceMediaImagePNG),
		}},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		CreatedAt: &at, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	approvalID := approval.ID
	raised := at.Add(7 * time.Minute)
	blocked, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "blocked-" + approval.ID, ProjectID: run.ProjectID,
		Subject: subject,
		Type:    domain.AttentionBlocked, Priority: domain.PriorityNormal,
		// The characters Go's JSON encoder escapes prove json_set's
		// re-serialization keeps the body decodable.
		Reason: "Specification approval has been waiting & <still> is.", RequestedDecision: []domain.Action{},
		BlockedOn: &domain.BlockedWait{
			Kind: domain.BlockedWaitSpecApproval, Since: at, ItemID: &approvalID,
		},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: &raised, Status: domain.StatusSuperseded,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := json.Marshal(blockedWaitSpecificationTerminal{
		InvocationID: invocationID, Iteration: 1, Status: "completed",
		ResearchArtifactIDs: []domain.ArtifactID{},
		SpecArtifactID:      &specification.ID, ApprovalItemID: &approvalID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		_, _, err := tx.RecordInbox(ctx, string(invocationID), blockedWaitSpecificationTerminalKind, terminal)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, specification); err != nil {
			return err
		}
		if err := putTestAttentionItem(ctx, tx, &approval); err != nil {
			return err
		}
		blocked = domaintest.MustConclude(blocked)
		return putTestAttentionItem(ctx, tx, &blocked)
	}); err != nil {
		t.Fatal(err)
	}
	return blocked.ID
}

type blockedRow struct {
	status, bodyStatus          string
	itemVersion                 int64
	entityVersion, asOfRevision int64
	body                        string
}

func readBlockedRow(t *testing.T, ctx context.Context, db *sql.DB, id domain.ItemID) blockedRow {
	t.Helper()
	var row blockedRow
	if err := db.QueryRowContext(ctx, `SELECT status, json_extract(body, '$.status'),
		json_extract(body, '$.item_version'), entity_version, as_of_revision, body
		FROM attention_items WHERE id = ?`, id).Scan(
		&row.status, &row.bodyStatus, &row.itemVersion, &row.entityVersion, &row.asOfRevision, &row.body,
	); err != nil {
		t.Fatal(err)
	}
	return row
}

func attentionItemCount(t *testing.T, ctx context.Context, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_items`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestOpenBlockedItemSurvivesTheDecodingBackfills is the upgrade acceptance
// for a store older than the backfills that decode attention items (schema
// 58, 59, and 70). Each skips a row the domain refuses, so without the
// retirement running first an open blocked item would reach the head with no
// decision surface or task binding and fail every read, taking the inbox
// listing with it.
func TestOpenBlockedItemSurvivesTheDecodingBackfills(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, before string
		// surface plants the schema 58 record. bound also binds it in the body
		// as schema 59 does, and spells the display names the way schema 69
		// stored them, which only the schema 70 backfill rewrites.
		surface, bound bool
	}{
		{name: "from schema 57", before: "0058_"},
		{name: "from schema 58", before: "0059_", surface: true},
		{name: "from schema 69", before: "0070_", surface: true, bound: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "store.db")
			db, err := openDB(path, Options{})
			if err != nil {
				t.Fatalf("openDB: %v", err)
			}
			migrateThrough(t, ctx, db, tc.before)
			if err := seedEpoch(ctx, db); err != nil {
				t.Fatal(err)
			}
			legacy := decisionSurfaceItem(t, "item-blocked-legacy")
			legacy.Subject = domain.Subject{Type: domain.SubjectProject, ID: "proj-1"}
			legacy.Type = domain.AttentionBlocked
			legacy.RequestedDecision = []domain.Action{}
			legacy.Status = domain.StatusSuperseded
			surface, err := domain.NewDecisionSurface(legacy)
			if err != nil {
				t.Fatal(err)
			}
			if tc.bound {
				legacy.DecisionSurface = domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}
				legacy.DisplayNames = &domain.DisplayNames{
					Project: domain.DisplayName{Text: "proj-1", Source: domain.DisplayNameSourceIdentifier},
					Task:    domain.DisplayName{Text: "proj-1", Source: domain.DisplayNameSourceIdentifier},
				}
			}
			body, err := encode(legacy)
			if err != nil {
				t.Fatal(err)
			}
			// The row is encoded closed and opened in SQL: the domain no longer
			// encodes the open blocked item an earlier daemon stored.
			stored := `json_set(?2, '$.status', 'open')`
			if tc.bound {
				stored = `json_remove(json_set(` + stored + `, '$.display_names.work_unit',
					json_extract(?2, '$.display_names.task')), '$.display_names.task')`
			}
			if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
				(id, project_id, conversation_id, item_type, status, subject_run_id, entity_version, as_of_revision, body)
				VALUES (?1, 'proj-1', NULL, 'blocked', 'open', NULL, 1, 1, `+stored+`)`,
				legacy.ID, body); err != nil {
				t.Fatalf("seed open blocked item: %v", err)
			}
			if tc.surface {
				surfaceBody, err := encode(surface)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ExecContext(ctx, insertDecisionSurfaceSQL,
					surface.ItemID, surface.Epoch, surface.Digest, surfaceBody); err != nil {
					t.Fatalf("seed decision surface: %v", err)
				}
			}
			if err := migrate(ctx, db, migrations.FS); err != nil {
				t.Fatalf("migrate to head: %v", err)
			}
			assertAtHead(t, db)
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			st, err := Open(ctx, path, Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Read(ctx, func(tx *ReadTx) error {
				item, err := tx.GetAttentionItem(ctx, legacy.ID)
				if err != nil {
					return err
				}
				if item.Status != domain.StatusSuperseded {
					t.Fatalf("status after upgrade = %q, want superseded", item.Status)
				}
				if item.ItemVersion != legacy.ItemVersion+1 {
					t.Fatalf("item_version after upgrade = %d, want %d (closed once)",
						item.ItemVersion, legacy.ItemVersion+1)
				}
				items, err := tx.ListAttentionItems(ctx)
				if err != nil {
					return err
				}
				if len(items) != 1 {
					t.Fatalf("listed %d items, want 1", len(items))
				}
				return nil
			}); err != nil {
				t.Fatalf("read the upgraded item: %v", err)
			}
		})
	}
}
