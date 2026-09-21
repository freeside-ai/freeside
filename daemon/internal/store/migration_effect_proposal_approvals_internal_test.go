package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestEffectProposalApprovalsMigrationPreservesRows proves migration 0079
// rebuilds effect_proposal_decisions and effect_proposal_items without losing
// data: an existing task_proposal start decision and its item binding survive
// the rebuild, and the item binding gains four null merge columns (all null for
// a task proposal). The fixture carries a real start decision and item row from
// before 0070.
func TestEffectProposalApprovalsMigrationPreservesRows(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)

	migrateThrough(t, ctx, db, "0070_")
	dump, err := os.ReadFile(filepath.Join("testdata", "pre_tasks_started_label_intake.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(dump)); err != nil {
		t.Fatal(err)
	}

	const instanceID = "proposal-dcd53aaaa960fd8e8fbe2339a17c02f3"
	const wantDigest = "sha256:666dec2d7a4c1f760936b0a1d427f6ba3b3137e49d816430463d7c4c2cd4fcb4"

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 79 {
		t.Fatalf("schema version = %d, want 79", got)
	}

	var action, selectedDigest string
	if err := db.QueryRowContext(ctx,
		`SELECT action, selected_digest FROM effect_proposal_decisions WHERE instance_id = ?`,
		instanceID).Scan(&action, &selectedDigest); err != nil {
		t.Fatalf("read preserved decision: %v", err)
	}
	if action != "start" || selectedDigest != wantDigest {
		t.Fatalf("preserved decision = action %q digest %q, want start / %q", action, selectedDigest, wantDigest)
	}

	var digest string
	var publicationIdentity, candidateHeadSHA, baseRef, baseSHA sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT content_digest, publication_identity, candidate_head_sha, base_ref, base_sha
		 FROM effect_proposal_items WHERE item_id = ?`, instanceID).Scan(
		&digest, &publicationIdentity, &candidateHeadSHA, &baseRef, &baseSHA); err != nil {
		t.Fatalf("read preserved item binding: %v", err)
	}
	if digest != wantDigest {
		t.Fatalf("preserved item content_digest = %q, want %q", digest, wantDigest)
	}
	if publicationIdentity.Valid || candidateHeadSHA.Valid || baseRef.Valid || baseSHA.Valid {
		t.Fatalf("task-proposal item binding gained non-null merge columns: %v %v %v %v",
			publicationIdentity, candidateHeadSHA, baseRef, baseSHA)
	}
}

// TestEffectProposalItemsCheckEnforcesAllOrNoneMerge proves the
// effect_proposal_items merge CHECK is all-or-none for real: a fully-null merge
// (task proposal) and a fully-populated merge (closure) are accepted, and a
// partially-null merge is rejected rather than slipping through because SQLite
// treats a NULL CHECK result as satisfied. Foreign keys are disabled on the
// probe connection so only the CHECK, not a missing parent row, can reject the
// insert.
func TestEffectProposalItemsCheckEnforcesAllOrNoneMerge(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}

	const digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := conn.ExecContext(ctx, `INSERT INTO effect_proposal_items
		(item_id, instance_id, content_digest) VALUES ('item-null', 'inst-null', ?)`, digest); err != nil {
		t.Fatalf("all-null merge binding rejected: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO effect_proposal_items
		(item_id, instance_id, content_digest,
		 publication_identity, candidate_head_sha, base_ref, base_sha)
		VALUES ('item-full', 'inst-full', ?, 'pub', 'head', 'main', 'base')`, digest); err != nil {
		t.Fatalf("fully-populated merge binding rejected: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO effect_proposal_items
		(item_id, instance_id, content_digest,
		 publication_identity, candidate_head_sha, base_ref, base_sha)
		VALUES ('item-partial', 'inst-partial', ?, 'pub', 'head', 'main', NULL)`, digest); err == nil {
		t.Fatal("partial merge binding (null base_sha) accepted; the CHECK does not enforce all-or-none")
	}
}
