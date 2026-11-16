package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestGetScheduleRejectsOneShotExpiry proves #462 at the reconstruction
// boundary. The malformed rows bypass PutSchedule, as only a damaged store or
// migration can present this shape after constructors and writes reject it.
func TestGetScheduleRejectsOneShotExpiry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawTemplate(t)
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	st := &Store{db: db}
	ts := time.Date(2026, 2, 3, 4, 0, 0, 0, time.UTC)
	run := domain.Run{
		ID: "run-1", ProjectID: "project-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	}
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
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
