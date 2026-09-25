package store

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestEffectProposalPolicyApprovalsMigrationCreatesTable proves migration 0080
// creates effect_proposal_policy_approvals with the schema the policy-approval
// write relies on: one row per instance (a second row for one instance is
// rejected by the primary key) and every column non-empty (a blank column is
// rejected by its CHECK). Foreign keys are off on the probe connection so only
// the table's own constraints, not a missing parent row, can reject an insert.
func TestEffectProposalPolicyApprovalsMigrationCreatesTable(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)

	migrateThrough(t, ctx, db, "0080_")
	if got := rawVersion(t, db); got != 79 {
		t.Fatalf("schema version before head = %d, want 79", got)
	}
	assertTableExists(t, db, "effect_proposal_policy_approvals", false)

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 81 {
		t.Fatalf("schema version = %d, want 81", got)
	}
	assertTableExists(t, db, "effect_proposal_policy_approvals", true)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}

	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	insert := `INSERT INTO effect_proposal_policy_approvals
		(instance_id, proposal_digest, publication_identity, candidate_head_sha, base_ref, base_sha, approved_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`

	if _, err := conn.ExecContext(ctx, insert,
		"inst-a", digest, "pub", "head", "main", "base", "2026-01-01T00:00:00Z"); err != nil {
		t.Fatalf("well-formed policy approval rejected: %v", err)
	}

	// One row per instance: a second row for inst-a violates the primary key.
	if _, err := conn.ExecContext(ctx, insert,
		"inst-a", digest, "pub", "head2", "main", "base2", "2026-01-02T00:00:00Z"); err == nil {
		t.Fatal("second policy approval for one instance accepted; the primary key does not enforce one row")
	}

	// Every column is non-empty: a blank base_sha is rejected by its CHECK.
	if _, err := conn.ExecContext(ctx, insert,
		"inst-b", digest, "pub", "head", "main", "", "2026-01-01T00:00:00Z"); err == nil {
		t.Fatal("blank base_sha accepted; the non-empty CHECK does not hold")
	}
	// A blank approved_at is rejected too.
	if _, err := conn.ExecContext(ctx, insert,
		"inst-c", digest, "pub", "head", "main", "base", ""); err == nil {
		t.Fatal("blank approved_at accepted; the non-empty CHECK does not hold")
	}
}
