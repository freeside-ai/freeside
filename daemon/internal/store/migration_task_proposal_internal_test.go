package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestTaskProposalVocabularyMigrationAppliesFromHead proves migration 0074
// renames a persisted run_proposal attention item to task_proposal in lockstep
// across the item_type column and the body's $.type, advances both sync
// cursors so a client resyncs the item, and leaves the digest-bound
// effect-proposal encoding untouched so the proposal binding, decision surface,
// and effect-proposal instance all still reconstruct.
func TestTaskProposalVocabularyMigrationAppliesFromHead(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)

	// The fixture is dumped against the schema immediately before 0070; load it
	// there, then bring it to the real head just before 0074 to read the
	// pre-rename state, exactly as the fixture's other migration tests do.
	migrateThrough(t, ctx, db, "0070_")
	dump, err := os.ReadFile(filepath.Join("testdata", "pre_tasks_started_label_intake.sql"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, string(dump)); err != nil {
		t.Fatal(err)
	}

	migrateThrough(t, ctx, db, "0074_")
	if got := rawVersion(t, db); got != 73 {
		t.Fatalf("schema version before 0074 = %d, want 73", got)
	}

	const itemID = "proposal-dcd53aaaa960fd8e8fbe2339a17c02f3"
	beforeType, beforeEV, beforeRev := readItemRow(t, ctx, db, itemID)
	beforeSync := serverRevision(t, ctx, db)
	beforeSurface := decisionSurfaceDigest(t, ctx, db, itemID)
	if beforeType != "run_proposal" {
		t.Fatalf("pre-migration item_type = %q, want run_proposal", beforeType)
	}

	// Seed a second run_proposal row whose body carries the characters Go's JSON
	// encoder escapes (& < >). json_set re-serializes the whole body, so this
	// proves the rewrite preserves those bytes through a decode rather than
	// corrupting an escaped field. It clones the fixture row's columns at the
	// current head, so it stays valid whatever the exact schema shape is, and it
	// is seeded after the earlier migrations so its duplicate subject cannot
	// disturb their backfill of the real item.
	const seededID = "seeded-escape-task-proposal"
	const seededReason = "Ampersand & less-than < greater-than > kept"
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
		(id, project_id, item_type, status, entity_version, as_of_revision, body)
		SELECT ?, project_id, item_type, status, entity_version, as_of_revision,
			json_set(body, '$.id', ?, '$.reason', ?)
		FROM attention_items WHERE id = ?`,
		seededID, seededID, seededReason, itemID); err != nil {
		t.Fatal(err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 79 {
		t.Fatalf("schema version = %d, want 79", got)
	}

	afterType, afterEV, afterRev := readItemRow(t, ctx, db, itemID)
	afterSync := serverRevision(t, ctx, db)

	if afterType != string(domain.AttentionTaskProposal) {
		t.Fatalf("migrated item_type = %q, want task_proposal", afterType)
	}
	if bodyType := itemBodyType(t, ctx, db, itemID); bodyType != string(domain.AttentionTaskProposal) {
		t.Fatalf("migrated body $.type = %q, want task_proposal", bodyType)
	}
	if afterSync != beforeSync+1 {
		t.Fatalf("server revision = %d, want %d (advanced once)", afterSync, beforeSync+1)
	}
	if afterEV != beforeEV+1 {
		t.Fatalf("entity_version = %d, want %d (advanced once)", afterEV, beforeEV+1)
	}
	if afterRev != afterSync || afterRev <= beforeRev {
		t.Fatalf("as_of_revision = %d, want new server revision %d (was %d)", afterRev, afterSync, beforeRev)
	}

	// The seeded escaped body survives the rewrite: type flipped, and the & < >
	// reason decodes back byte-for-byte.
	if seedType, _, _ := readItemRow(t, ctx, db, seededID); seedType != string(domain.AttentionTaskProposal) {
		t.Fatalf("seeded item_type = %q, want task_proposal", seedType)
	}
	var seedReason string
	if err := db.QueryRowContext(ctx,
		`SELECT json_extract(body, '$.reason') FROM attention_items WHERE id = ?`, seededID).Scan(&seedReason); err != nil {
		t.Fatal(err)
	}
	if seedReason != seededReason {
		t.Fatalf("seeded reason = %q, want %q (escaped characters preserved)", seedReason, seededReason)
	}

	// The decision surface is unchanged: its preimage hashes item id, epoch,
	// subject, requested decisions, PR head SHA, and presented digests, not the
	// item type, so the type rename cannot invalidate it.
	if afterSurface := decisionSurfaceDigest(t, ctx, db, itemID); afterSurface != beforeSurface {
		t.Fatalf("decision surface digest = %q, want unchanged %q", afterSurface, beforeSurface)
	}

	// The bound effect-proposal instance still reconstructs against the retained
	// run_proposal encoding, and the item->instance binding still resolves: the
	// migration touched only the attention_items vocabulary, not the digest-bound
	// proposal encoding the binding addresses.
	st := &Store{db: db}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		instance, err := tx.GetProposalInstance(ctx, domain.ProposalInstanceID(itemID))
		if err != nil {
			return err
		}
		if instance.Proposal.Kind != domain.EffectTaskProposal {
			t.Fatalf("proposal kind = %q, want the retained run_proposal encoding", instance.Proposal.Kind)
		}
		var boundInstance string
		var boundDigest domain.Digest
		if err := tx.tx.QueryRowContext(ctx,
			`SELECT instance_id, content_digest FROM effect_proposal_items WHERE item_id = ?`, itemID).
			Scan(&boundInstance, &boundDigest); err != nil {
			return err
		}
		if boundInstance != itemID || boundDigest != instance.Proposal.Digest {
			t.Fatalf("proposal binding = instance %q digest %q, want instance %q digest %q",
				boundInstance, boundDigest, itemID, instance.Proposal.Digest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func decisionSurfaceDigest(t *testing.T, ctx context.Context, db *sql.DB, id string) string {
	t.Helper()
	var digest string
	if err := db.QueryRowContext(ctx,
		`SELECT digest FROM attention_decision_surfaces WHERE item_id = ?`, id).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	return digest
}

func readItemRow(t *testing.T, ctx context.Context, db *sql.DB, id string) (itemType string, entityVersion, asOfRevision int64) {
	t.Helper()
	if err := db.QueryRowContext(ctx,
		`SELECT item_type, entity_version, as_of_revision FROM attention_items WHERE id = ?`, id).
		Scan(&itemType, &entityVersion, &asOfRevision); err != nil {
		t.Fatal(err)
	}
	return itemType, entityVersion, asOfRevision
}

func itemBodyType(t *testing.T, ctx context.Context, db *sql.DB, id string) string {
	t.Helper()
	var bodyType string
	if err := db.QueryRowContext(ctx,
		`SELECT json_extract(body, '$.type') FROM attention_items WHERE id = ?`, id).Scan(&bodyType); err != nil {
		t.Fatal(err)
	}
	return bodyType
}

func serverRevision(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var revision int64
	if err := db.QueryRowContext(ctx,
		`SELECT revision FROM server_state WHERE id = 1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	return revision
}
