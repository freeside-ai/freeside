package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// rewindWorkflowPolicyRevisions returns a migrated store to the schema 0137
// leaves: no workflow policy revisions.
func rewindWorkflowPolicyRevisions(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE workflow_policy_revisions`,
		`DELETE FROM schema_migrations WHERE name = '0138_workflow_policy_revisions.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestWorkflowPolicyRevisionsMigration: a store at 0137 migrates to an empty
// revision table, which reads as revision 0 with every key at its default.
func TestWorkflowPolicyRevisionsMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	copySnapshotThrough(t, path, "0138_")
	st := openTemplateStoreAt(t, path, Options{})
	assertAtHead(t, st.db)
	if err := st.Read(ctx, func(tx *ReadTx) error {
		layers, err := tx.WorkflowPolicyLayers(ctx)
		if err == nil && layers.Revision() != 0 {
			t.Errorf("workflow policy revision after 0138 = %d, want 0", layers.Revision())
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
