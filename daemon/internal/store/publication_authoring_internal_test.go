package store

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestPublicationAuthoringMigrationAppliesFromHead proves 0078 applies on top of
// the prior schema and creates the authoring table at schema version 78.
func TestPublicationAuthoringMigrationAppliesFromHead(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0077_")
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 78 {
		t.Fatalf("schema version = %d, want 78", got)
	}
	assertTableExists(t, db, "publication_authorings", true)
}
