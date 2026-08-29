package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestScheduleAuthorityMigrationBackfillsAndNormalizesOneShots proves existing
// workload schedules acquire independent authority while legacy one-shot
// expiry state converges onto the new two-outcome contract.
func TestScheduleAuthorityMigrationBackfillsAndNormalizesOneShots(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrationsBeforeScheduleAuthority(t)); err != nil {
		t.Fatal(err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}

	runID := domain.RunID("run-1")
	policy, err := domain.NewResolvedPolicy(runID, []domain.PolicyKey{{
		Key: "driver", Value: "claude",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: "sha256:migration-policy",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason: "ready", RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRReference: &domain.PRReference{Repo: "owner/repo", Number: 123},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		run := domain.Run{
			ID: runID, ProjectID: item.ProjectID,
			SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}
		body, err := encode(run)
		if err != nil {
			return err
		}
		if _, err := tx.tx.ExecContext(ctx, `INSERT INTO runs
			(id, project_id, policy_digest, entity_version, as_of_revision, body)
			VALUES (?, ?, ?, 1, ?, ?)`, run.ID, run.ProjectID, run.PolicyDigest,
			tx.asOfRevision, body); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	itemBody, err := encode(item)
	if err != nil {
		t.Fatal(err)
	}
	// Seed in the exact pre-0027 schema shape. The current Put includes
	// columns introduced by later migrations and therefore must not be used
	// to construct a historical migration fixture.
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
		(id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body)
		VALUES (?, ?, NULL, ?, ?, 1, 1, ?)`,
		item.ID, item.ProjectID, item.Type, item.Status, itemBody); err != nil {
		t.Fatal(err)
	}

	itemID, version := item.ID, item.ItemVersion
	// SQLite rounds %s at .9995; the migration must derive whole seconds from
	// the fraction-free timestamp before restoring the original nanoseconds.
	fireAt := time.Date(2026, 2, 3, 4, 30, 0, 999999999, time.UTC)
	expiresAt := fireAt.Add(time.Minute)
	legacy := domain.Schedule{
		ID: "schedule-pr_checks_deadline-item-1", ProjectID: item.ProjectID,
		Kind: domain.SchedulePRChecksDeadline,
		Subject: domain.ScheduleSubject{
			Type:   domain.ScheduleSubjectAttentionItem,
			ItemID: &itemID, ItemVersion: &version,
		},
		Generation: 1, CreatedAt: fireAt.Add(-time.Minute), FireAt: &fireAt,
		ExpiresAt: &expiresAt, Status: domain.ScheduleArmed,
	}
	legacyBody, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO schedules
    (id, project_id, kind, status, generation, fire_at, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, ?, ?, 1, 1, ?)`,
		legacy.ID, legacy.ProjectID, legacy.Kind, legacy.Status,
		legacy.Generation, fireAt.UnixNano(), string(legacyBody),
	); err != nil {
		t.Fatal(err)
	}
	expired := legacy
	expired.ID = "schedule-review_wait_threshold-item-1"
	expired.Kind = domain.ScheduleReviewWaitThreshold
	expired.Status = domain.ScheduleExpired
	expired.Resolution = &domain.ScheduleResolution{
		Reason: domain.ResolutionIntentExpired, RecordedAt: expiresAt,
	}
	expiredBody, err := json.Marshal(expired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO schedules
    (id, project_id, kind, status, generation, fire_at, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, ?, ?, 1, 1, ?)`,
		expired.ID, expired.ProjectID, expired.Kind, expired.Status,
		expired.Generation, fireAt.UnixNano(), string(expiredBody),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO schedule_occurrences
    (schedule_id, generation, nominal_fire_at, status, created_at, consumed_at, outcome)
VALUES (?, 1, ?, 'consumed', ?, ?, 'condition_no_longer_applies')`,
		expired.ID, fireAt.UnixNano(),
		expiresAt.Format(time.RFC3339Nano), expiresAt.Format(time.RFC3339Nano),
	); err != nil {
		t.Fatal(err)
	}
	before, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	after, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.SyncEpoch != before.SyncEpoch || after.Revision != before.Revision+1 {
		t.Fatalf("migration server state = %+v, before %+v", after, before)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		got, err := tx.GetSchedule(ctx, legacy.ID)
		if err != nil {
			return err
		}
		if got.RunID == nil || *got.RunID != runID ||
			got.PolicyDigest == nil || *got.PolicyDigest != policy.Digest {
			t.Fatalf("migrated authority = run %v policy %v", got.RunID, got.PolicyDigest)
		}
		if got.ExpiresAt != nil || got.Status != domain.ScheduleArmed || got.Generation != 1 {
			t.Fatalf("migrated armed one-shot = %+v", got)
		}
		reopened, err := tx.GetSchedule(ctx, expired.ID)
		if err != nil {
			return err
		}
		if reopened.RunID == nil || *reopened.RunID != runID ||
			reopened.PolicyDigest == nil || *reopened.PolicyDigest != policy.Digest ||
			reopened.ExpiresAt != nil || reopened.Status != domain.ScheduleArmed ||
			reopened.Generation != 2 || reopened.Resolution != nil {
			t.Fatalf("migrated expired one-shot = %+v", reopened)
		}
		occurrence, err := tx.GetScheduleOccurrence(ctx, expired.ID, 1, fireAt)
		if err != nil {
			return err
		}
		if occurrence.Status != domain.OccurrenceConsumed {
			t.Fatalf("legacy occurrence status = %s, want consumed", occurrence.Status)
		}
		rows, err := tx.tx.QueryContext(ctx, `
SELECT entity_version, as_of_revision FROM schedules ORDER BY id`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var entityVersion, asOfRevision int64
			if err := rows.Scan(&entityVersion, &asOfRevision); err != nil {
				return err
			}
			if entityVersion != 2 || asOfRevision != after.Revision {
				t.Fatalf("migrated sync metadata = v%d/r%d, want v2/r%d",
					entityVersion, asOfRevision, after.Revision)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestScheduleAuthorityMigrationRejectsInvalidLegacyRows proves normalization
// cannot launder column/body contradictions or timestamps the Go decoder would
// reject into valid rows.
func TestScheduleAuthorityMigrationRejectsInvalidLegacyRows(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"status contradiction",
		"loose expiry timestamp",
		"hour 24 expiry timestamp",
		"fire_at contradiction",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			db := openRaw(t)
			if err := migrate(ctx, db, migrationsBeforeScheduleAuthority(t)); err != nil {
				t.Fatal(err)
			}
			if err := seedEpoch(ctx, db); err != nil {
				t.Fatal(err)
			}

			itemID := domain.ItemID("item-1")
			version := 1
			fireAt := time.Date(2026, 2, 3, 4, 30, 0, 123456789, time.UTC)
			legacy := domain.Schedule{
				ID: "schedule-review_wait_threshold-item-1", ProjectID: "project-1",
				Kind: domain.ScheduleReviewWaitThreshold,
				Subject: domain.ScheduleSubject{
					Type:   domain.ScheduleSubjectAttentionItem,
					ItemID: &itemID, ItemVersion: &version,
				},
				Generation: 1, CreatedAt: fireAt.Add(-time.Minute), FireAt: &fireAt,
				Status: domain.ScheduleArmed,
			}
			columnStatus := legacy.Status
			columnFireAt := fireAt
			if name == "status contradiction" {
				columnStatus = domain.ScheduleExpired
			}
			if name == "loose expiry timestamp" || name == "hour 24 expiry timestamp" {
				expiresAt := fireAt.Add(time.Minute)
				legacy.Status = domain.ScheduleExpired
				legacy.ExpiresAt = &expiresAt
				legacy.Resolution = &domain.ScheduleResolution{
					Reason: domain.ResolutionIntentExpired, RecordedAt: expiresAt,
				}
				columnStatus = legacy.Status
			}
			if name == "fire_at contradiction" {
				columnFireAt = fireAt.Add(time.Nanosecond)
			}
			body, err := json.Marshal(legacy)
			if err != nil {
				t.Fatal(err)
			}
			if name == "loose expiry timestamp" || name == "hour 24 expiry timestamp" {
				var decoded map[string]any
				if err := json.Unmarshal(body, &decoded); err != nil {
					t.Fatal(err)
				}
				decoded["expires_at"] = "2026-02-03"
				if name == "hour 24 expiry timestamp" {
					decoded["expires_at"] = "2026-02-03T24:00:00Z"
				}
				body, err = json.Marshal(decoded)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := db.ExecContext(ctx, `
INSERT INTO schedules
    (id, project_id, kind, status, generation, fire_at, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, ?, ?, 1, 1, ?)`,
				legacy.ID, legacy.ProjectID, legacy.Kind, columnStatus,
				legacy.Generation, columnFireAt.UnixNano(), string(body),
			); err != nil {
				t.Fatal(err)
			}

			if err := migrate(ctx, db, migrations.FS); err == nil {
				t.Fatal("migration accepted an invalid legacy row")
			}
			if got := rawVersion(t, db); got != 26 {
				t.Fatalf("schema version after rejected migration = %d, want 26", got)
			}
			assertTableExists(t, db, "schedules", true)
			assertTableExists(t, db, "schedules_without_authority", false)
		})
	}
}

// TestJoinedRowSurfacesScanMismatchAsError proves the forwarding scanner that
// feeds the recurring due read reports an incompatible scan destination as an
// error, never the panic the removed positional type assertions would raise on
// a future schedule column-type change.
func TestJoinedRowSurfacesScanMismatchAsError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	ts := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	interval := int64(30)
	janitor, err := domain.NewSchedule(domain.ScheduleInput{
		ID: "schedule-janitor", ProjectID: "project-system",
		Kind:      domain.ScheduleJanitor,
		Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
		CreatedAt: ts, IntervalSeconds: &interval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		return tx.PutSchedule(ctx, janitor)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.SetScheduleTimer(ctx, janitor.ID, janitor.Generation, ts.Add(30*time.Second))
	}); err != nil {
		t.Fatal(err)
	}

	err = st.Read(ctx, func(tx *ReadTx) error {
		rows, err := tx.tx.QueryContext(ctx, listDueRecurringSQL, ts.Add(time.Minute).UnixNano())
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		if !rows.Next() {
			t.Fatal("expected one due recurring row")
		}
		// Correct destinations except the id column: it is TEXT, so scanning it
		// into *int64 is exactly the type drift the deleted assertions would
		// have panicked on. joinedRow forwards to sql.Rows.Scan, which reports
		// it as an error. A panic here would fail the test outright.
		var (
			wrongID              int64
			projectID, kind      string
			status               string
			generation           int64
			runID, policy        sql.NullString
			fireAt               sql.NullInt64
			ev, aor              int64
			body                 []byte
			timerGen, nextNomFAt int64
		)
		scanErr := joinedRow{rows: rows, extra: []any{&timerGen, &nextNomFAt}}.Scan(
			&wrongID, &projectID, &kind, &status, &generation, &runID, &policy,
			&fireAt, &ev, &aor, &body,
		)
		if scanErr == nil {
			t.Fatal("scan with a mismatched destination returned a nil error")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func migrationsBeforeScheduleAuthority(t *testing.T) fs.FS {
	t.Helper()
	prior := map[string]string{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "0027_schedule_authority.sql" ||
			entry.Name() == "0028_ready_item_pr_binding.sql" ||
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
			entry.Name() == "0050_attention_subject_run_binding.sql" ||
			entry.Name() == "0051_finding_adjudications.sql" ||
			entry.Name() == "0052_admitted_agents.sql" ||
			entry.Name() == "0053_shadow_review.sql" ||
			entry.Name() == "0054_attention_readiness_summary.sql" ||
			entry.Name() == "0055_attention_yield_history.sql" ||
			entry.Name() == "0056_shadow_review_configuration_approval.sql" ||
			entry.Name() == "0057_finding_adjudication_revisions.sql" ||
			entry.Name() == "0058_attention_decision_surfaces.sql" ||
			entry.Name() == "0059_attention_decision_surface_bodies.sql" ||
			entry.Name() == "0060_attention_recommendation_sources.sql" || entry.IsDir() {
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

// TestGetScheduleRejectsOneShotExpiry proves #462 at the reconstruction
// boundary. The malformed rows bypass PutSchedule, as only a damaged store or
// migration can present this shape after constructors and writes reject it.
func TestGetScheduleRejectsOneShotExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	ts := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-1", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	runBody, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO runs
    (id, project_id, policy_digest, entity_version, as_of_revision, body)
VALUES (?, ?, ?, 1, 1, ?)`,
		run.ID, run.ProjectID, run.PolicyDigest, string(runBody),
	); err != nil {
		t.Fatal(err)
	}

	for _, kind := range []domain.ScheduleKind{
		domain.SchedulePRChecksDeadline,
		domain.ScheduleReviewWaitThreshold,
	} {
		t.Run(string(kind), func(t *testing.T) {
			id := domain.ScheduleID("schedule-" + string(kind) + "-item-1")
			itemID := domain.ItemID("item-1")
			version := 1
			fireAt := ts.Add(time.Minute)
			expiresAt := ts.Add(30 * time.Second)
			runID := domain.RunID("run-1")
			policyDigest := domain.Digest("sha256:policy")
			schedule := domain.Schedule{
				ID: id, ProjectID: "project-1", Kind: kind,
				Subject: domain.ScheduleSubject{
					Type:   domain.ScheduleSubjectAttentionItem,
					ItemID: &itemID, ItemVersion: &version,
				},
				Generation: 1, CreatedAt: ts, FireAt: &fireAt,
				RunID: &runID, PolicyDigest: &policyDigest,
				ExpiresAt: &expiresAt, Status: domain.ScheduleArmed,
			}
			body, err := json.Marshal(schedule)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `
INSERT INTO schedules
	(id, project_id, kind, status, generation, run_id, policy_digest,
	 fire_at, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, 1, ?)`,
				id, schedule.ProjectID, kind, schedule.Status, schedule.Generation,
				runID, policyDigest, fireAt.UnixNano(), string(body),
			); err != nil {
				t.Fatal(err)
			}
			err = st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetSchedule(ctx, id)
				return err
			})
			if !errors.Is(err, domain.ErrScheduleDetailMismatch) {
				t.Fatalf("GetSchedule = %v, want ErrScheduleDetailMismatch", err)
			}
		})
	}
}
