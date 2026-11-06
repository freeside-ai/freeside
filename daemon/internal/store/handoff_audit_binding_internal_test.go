package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindHandoffAuditBinding returns a store at head to the shape a daemon at
// 0128 holds: audits record no ref and handoff checks name no audit.
func rewindHandoffAuditBinding(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE publication_handoff_checks DROP COLUMN workflow_audit_id`,
		`ALTER TABLE workflow_audits DROP COLUMN audited_ref`,
		`DELETE FROM schema_migrations WHERE name = '0129_bind_handoff_check_to_audit.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

// TestHandoffAuditBindingMigration: a daemon at 0128 holding an audit and a
// sealed handoff check migrates to a store that still reads both, the audit
// with no ref and the check naming no audit, which the publication decision
// then refuses.
func TestHandoffAuditBindingMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, check, _ := handoffCheckFixture(t)
	rewindHandoffAuditBinding(t, ctx, st.db)
	// The fixture's audit body names a ref; a body written at 0128 has no
	// such key, so strip it to hold what a real legacy row holds.
	var (
		raw  []byte
		body map[string]any
	)
	if err := st.db.QueryRowContext(ctx, `SELECT body FROM workflow_audits WHERE id = ?`,
		check.WorkflowAuditID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	delete(body, "audited_ref")
	legacy, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`UPDATE workflow_audits SET body = ? WHERE id = ?`, []any{string(legacy), check.WorkflowAuditID}},
		{
			`INSERT INTO publication_handoff_checks
            (run_id, publication_id, head_sha, evidence_base_sha, tip_sha, resolved_at)
            VALUES (?, ?, ?, ?, ?, '2026-11-05T12:00:00Z')`,
			[]any{check.RunID, check.PublicationID, check.HeadSHA, check.EvidenceBaseSHA, check.TipSHA},
		},
	} {
		if _, err := st.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)
	if err := st.Read(ctx, func(tx *ReadTx) error {
		audit, err := tx.GetWorkflowAudit(ctx, check.WorkflowAuditID)
		if err != nil {
			return err
		}
		if audit.Audit.AuditedRef != "" {
			t.Fatalf("legacy audit ref = %q, want empty", audit.Audit.AuditedRef)
		}
		sealed, err := tx.GetHandoffCheck(ctx, check.RunID, check.PublicationID)
		if err != nil {
			return err
		}
		if sealed.WorkflowAuditID != 0 || sealed.TipSHA != check.TipSHA {
			t.Fatalf("legacy check = %+v, want tip %s naming no audit", sealed, check.TipSHA)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestWorkflowAuditRefRoundTrips: every audit reader returns the ref an
// audit was recorded with, and refuses a row whose ref column disagrees with
// its body, as it refuses a disagreeing repository, commit, or digest.
func TestWorkflowAuditRefRoundTrips(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/audit-ref.db", Options{})
	t.Cleanup(func() { _ = st.Close() })
	id := recordHandoffAudit(t, st, "o/r", "release", "tip-1")
	var digest string
	if err := st.db.QueryRowContext(ctx, `SELECT workflow_audit_digest FROM workflow_audits WHERE id = ?`,
		id).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	readers := map[string]func(tx *ReadTx) ([]WorkflowAuditRecord, error){
		"list": func(tx *ReadTx) ([]WorkflowAuditRecord, error) { return tx.ListWorkflowAudits(ctx, "o/r") },
		"by id": func(tx *ReadTx) ([]WorkflowAuditRecord, error) {
			rec, err := tx.GetWorkflowAudit(ctx, id)
			return []WorkflowAuditRecord{rec}, err
		},
		"latest": func(tx *ReadTx) ([]WorkflowAuditRecord, error) {
			rec, err := tx.scanWorkflowAuditRecord(ctx, latestWorkflowAuditSQL, "o/r")
			return []WorkflowAuditRecord{rec}, err
		},
		"latest for digest": func(tx *ReadTx) ([]WorkflowAuditRecord, error) {
			rec, err := tx.scanWorkflowAuditRecord(ctx, latestWorkflowAuditForDigestSQL, "o/r", digest)
			return []WorkflowAuditRecord{rec}, err
		},
	}
	read := func(name string) ([]WorkflowAuditRecord, error) {
		var recs []WorkflowAuditRecord
		err := st.Read(ctx, func(tx *ReadTx) error {
			var err error
			recs, err = readers[name](tx)
			return err
		})
		return recs, err
	}
	for name := range readers {
		recs, err := read(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(recs) != 1 || recs[0].ID != id || recs[0].Audit.AuditedRef != "release" {
			t.Fatalf("%s = %+v, want audit %d of release", name, recs, id)
		}
	}
	for _, column := range []any{"main", nil} {
		if _, err := st.db.ExecContext(ctx, `UPDATE workflow_audits SET audited_ref = ? WHERE id = ?`,
			column, id); err != nil {
			t.Fatal(err)
		}
		for name := range readers {
			if _, err := read(name); !errors.Is(err, errRowInconsistent) {
				t.Fatalf("%s with ref column %v error = %v, want errRowInconsistent", name, column, err)
			}
		}
	}
}
