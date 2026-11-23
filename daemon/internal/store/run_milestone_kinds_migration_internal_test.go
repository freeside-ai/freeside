package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindRunMilestoneKinds returns a migrated store to the schema 0147
// leaves: run_milestones rebuilt back to the baseline's kind CHECK list,
// keeping its rows, and 0148 forgotten.
func rewindRunMilestoneKinds(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindCordonedRunSettings(t, ctx, db)
	for _, statement := range []string{
		`CREATE TABLE run_milestones_0147 (
		    id            INTEGER PRIMARY KEY,
		    run_id        TEXT NOT NULL CHECK (run_id <> ''),
		    kind          TEXT NOT NULL CHECK (kind IN (
		                      'run_submitted', 'invocation_admitted',
		                      'invocation_started', 'execution_export_recorded',
		                      'execution_outcome_recorded', 'terminal_recorded',
		                      'publication_ready', 'publication_blocked',
		                      'work_unit_completed')),
		    invocation_id TEXT CHECK (invocation_id IS NULL OR invocation_id <> ''),
		    terminal      TEXT CHECK (terminal IS NULL OR terminal <> ''),
		    outcome       TEXT CHECK (outcome IS NULL OR outcome <> ''),
		    reason        TEXT CHECK (reason IS NULL OR reason <> ''),
		    recorded_at   TEXT NOT NULL CHECK (recorded_at <> '')
		) STRICT`,
		`INSERT INTO run_milestones_0147 SELECT * FROM run_milestones`,
		`DROP TABLE run_milestones`,
		`ALTER TABLE run_milestones_0147 RENAME TO run_milestones`,
		`CREATE UNIQUE INDEX run_milestones_identity
		    ON run_milestones (run_id, kind, COALESCE(invocation_id, ''))`,
		`DELETE FROM schema_migrations WHERE name = '0148_run_milestone_kinds.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestRunMilestoneKindsMigrationKeepsRows: milestones recorded at 0147 read
// back unchanged and in record order after 0148; the rebuilt table then
// stores every registered kind, keeps first observation wins, and leaves an
// unknown stored kind to the decode's refusal.
func TestRunMilestoneKindsMigrationKeepsRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawThrough(t, "0148_")
	// Ids out of insertion order: record order is the id, so the rebuild
	// must carry ids over, not renumber.
	if _, err := db.ExecContext(ctx, `INSERT INTO run_milestones
		(id, run_id, kind, invocation_id, terminal, outcome, reason, recorded_at)
		VALUES (7, 'run-1', 'publication_blocked', 'pub-1', NULL, NULL, 'trust_blocked', '2026-01-02T03:06:05Z'),
		       (3, 'run-1', 'run_submitted', 'inv-1', NULL, NULL, NULL, '2026-01-02T03:04:05Z'),
		       (5, 'run-1', 'terminal_recorded', 'inv-1', 'completed', NULL, NULL, '2026-01-02T03:05:05Z')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, db)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	write := &WriteTx{InternalTx: InternalTx{ReadTx: ReadTx{tx: tx}}, asOfRevision: 1}
	kept, err := write.ListRunMilestones(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	at := func(s string) time.Time {
		parsed, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return parsed
	}
	inv, pub := domain.InvocationID("inv-1"), domain.InvocationID("pub-1")
	completed, blocked := domain.ObservedStatusCompleted, domain.HoldTrustBlocked
	want := []domain.RunMilestone{
		{RunID: "run-1", Kind: domain.MilestoneRunSubmitted, InvocationID: &inv, RecordedAt: at("2026-01-02T03:04:05Z")},
		{RunID: "run-1", Kind: domain.MilestoneTerminalRecorded, InvocationID: &inv, Terminal: &completed, RecordedAt: at("2026-01-02T03:05:05Z")},
		{RunID: "run-1", Kind: domain.MilestonePublicationBlocked, InvocationID: &pub, Reason: &blocked, RecordedAt: at("2026-01-02T03:06:05Z")},
	}
	if !reflect.DeepEqual(kept, want) {
		t.Fatalf("kept milestones = %+v, want %+v", kept, want)
	}

	recorded := at("2026-01-02T04:00:00Z")
	for _, kind := range domain.AllRunMilestoneKinds {
		m := domain.RunMilestone{RunID: "run-2", Kind: kind, InvocationID: &inv, RecordedAt: recorded}
		switch kind {
		case domain.MilestoneTerminalRecorded:
			m.Terminal = &completed
		case domain.MilestoneExecutionOutcomeRecorded:
			failed := domain.ExecutionOutcomeFailed
			m.Outcome = &failed
		case domain.MilestonePublicationBlocked:
			m.Reason = &blocked
		case domain.MilestoneRunSubmitted, domain.MilestoneInvocationAdmitted,
			domain.MilestoneInvocationStarted, domain.MilestoneExecutionExportRecorded,
			domain.MilestonePublicationReady, domain.MilestoneWorkUnitCompleted,
			domain.MilestoneVerificationStarted, domain.MilestoneVerificationPassed,
			domain.MilestoneVerificationFailed:
		}
		if err := write.AppendRunMilestone(ctx, m); err != nil {
			t.Fatalf("append %s: %v", kind, err)
		}
	}
	// A replay of one identity converges on the stored row: the unique
	// index survived the rebuild.
	if err := write.AppendRunMilestone(ctx, domain.RunMilestone{
		RunID: "run-2", Kind: domain.MilestoneVerificationStarted, InvocationID: &inv,
		RecordedAt: recorded.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := write.ListRunMilestones(ctx, "run-2")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != len(domain.AllRunMilestoneKinds) {
		t.Fatalf("stored %d milestones, want one per kind (%d)", len(stored), len(domain.AllRunMilestoneKinds))
	}
	for i, m := range stored {
		if m.Kind != domain.AllRunMilestoneKinds[i] || !m.RecordedAt.Equal(recorded) {
			t.Fatalf("milestone %d = %s at %s, want %s at %s", i, m.Kind, m.RecordedAt, domain.AllRunMilestoneKinds[i], recorded)
		}
	}

	if _, err := tx.ExecContext(ctx, `UPDATE run_milestones SET kind = 'verification_skipped'
		WHERE run_id = 'run-1' AND kind = 'run_submitted'`); err != nil {
		t.Fatal(err)
	}
	if _, err := write.ListRunMilestones(ctx, "run-1"); !errors.Is(err, domain.ErrInvalidRunMilestoneKind) {
		t.Fatalf("unknown stored kind = %v, want %v", err, domain.ErrInvalidRunMilestoneKind)
	}
}
