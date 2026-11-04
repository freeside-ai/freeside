package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindRoutingPolicyDrainFirst returns a store at head to the shape a daemon
// at 0120 holds.
func rewindRoutingPolicyDrainFirst(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE routing_policies DROP COLUMN drain_first_account`,
		`DELETE FROM schema_migrations WHERE name = '0121_routing_policy_drain_first.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRoutingPolicyDrainFirstMigrationKeepsRecordedRevisions: a revision
// recorded before 0121, whose body has no drain_first_account, reads back
// after it as a revision that names no account.
func TestRoutingPolicyDrainFirstMigrationKeepsRecordedRevisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, t.TempDir()+"/routing-drain-first.db", Options{})
	rewindRoutingPolicyDrainFirst(t, ctx, st.db)
	for _, statement := range []string{
		`INSERT INTO routing_policies (revision, source, set_by, set_at, lineup_revision, body)
		VALUES (1, 'cli_routing_set', 'cli:operator', '2026-01-02T03:04:05Z',
			'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',
			'{"revision":1,"roles":[{"role":"implementer","agents":["claude-a"]}],` +
			`"source":"cli_routing_set","set_by":"cli:operator","set_at":"2026-01-02T03:04:05Z",` +
			`"lineup_revision":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}')`,
	} {
		if _, err := st.db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)

	var policy domain.RoutingPolicy
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		policy, err = tx.CurrentRoutingPolicy(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if policy.Revision != 1 || policy.DrainFirstAccount != nil {
		t.Fatalf("policy = %+v, want revision 1 with no drain-first account", policy)
	}
}
