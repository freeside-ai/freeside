package store

import (
	"context"
	"testing"
)

// TestEffectProposalItemsCheckEnforcesAllOrNoneMerge proves the
// effect_proposal_items merge CHECK is all-or-none for real: a fully-null merge
// (task proposal) and a fully-populated merge (closure) are accepted, and a
// partially-null merge is rejected rather than slipping through because SQLite
// treats a NULL CHECK result as satisfied. Foreign keys are disabled on the
// probe connection so only the CHECK, not a missing parent row, can reject the
// insert.
func TestEffectProposalItemsCheckEnforcesAllOrNoneMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawTemplate(t)
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
