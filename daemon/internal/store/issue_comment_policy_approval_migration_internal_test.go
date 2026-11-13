package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindIssueCommentPolicyApprovals returns a migrated store to the schema
// 0140 leaves: it drops the table 0141 creates and forgets 0141.
func rewindIssueCommentPolicyApprovals(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE issue_comment_policy_approvals`,
		`DELETE FROM schema_migrations WHERE name = '0141_issue_comment_policy_approvals.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestIssueCommentPolicyApprovalsMigrationCreatesTable proves migration 0141
// creates issue_comment_policy_approvals with the shape the policy approval
// relies on: one row per instance and every column non-empty. Foreign keys
// are off on the probe connection, so only the table's own constraints can
// reject an insert.
func TestIssueCommentPolicyApprovalsMigrationCreatesTable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawThrough(t, "0141_")
	if got := rawVersion(t, db); got != 140 {
		t.Fatalf("schema version before 0141 = %d, want 140", got)
	}
	assertTableExists(t, db, "issue_comment_policy_approvals", false)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, db)
	assertTableExists(t, db, "issue_comment_policy_approvals", true)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	insert := `INSERT INTO issue_comment_policy_approvals
		(instance_id, proposal_digest, policy_digest, approved_at) VALUES (?, ?, ?, ?)`
	if _, err := conn.ExecContext(ctx, insert, "inst-a", digest, digest, "2026-11-13T00:00:00Z"); err != nil {
		t.Fatalf("well-formed policy approval rejected: %v", err)
	}
	if _, err := conn.ExecContext(ctx, insert, "inst-a", digest, digest, "2026-11-14T00:00:00Z"); err == nil {
		t.Fatal("second policy approval for one instance accepted; the primary key does not hold")
	}
	for _, row := range [][]any{
		{"inst-b", "", digest, "2026-11-13T00:00:00Z"},
		{"inst-c", digest, "", "2026-11-13T00:00:00Z"},
		{"inst-d", digest, digest, ""},
	} {
		if _, err := conn.ExecContext(ctx, insert, row...); err == nil {
			t.Fatalf("blank column accepted in %v; the non-empty CHECK does not hold", row)
		}
	}
}
