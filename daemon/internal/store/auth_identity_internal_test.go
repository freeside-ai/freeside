package store

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// migrateThrough brings a raw database to the head immediately before the
// named migration, so an "applies from the current head" test runs the new
// migration against a database that was actually at the real prior head.
func migrateThrough(t *testing.T, ctx context.Context, db *sql.DB, before string) {
	t.Helper()
	files, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	prefix := fstest.MapFS{}
	for _, name := range files {
		if name >= before {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		prefix[name] = &fstest.MapFile{Data: body}
	}
	if err := migrate(ctx, db, prefix); err != nil {
		t.Fatalf("migrate through the head before %s: %v", before, err)
	}
}

// TestAuthIdentityMigrationAppliesFromHead is the migration acceptance: the
// new tables land on a database sitting at the real prior head, carrying
// existing rows, and nothing is backfilled into them.
func TestAuthIdentityMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0013_")

	// A run recorded under the old schema must survive untouched: the
	// migration adds tables and rewrites nothing.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body)
		 VALUES ('run-1', 'proj-1', 'sha256:policy', 1, 1, '{}')`); err != nil {
		t.Fatalf("seed run: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var runs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs WHERE id = 'run-1'`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("pre-migration run count = %d, want 1", runs)
	}
	for _, table := range []string{"auth_identities", "auth_store_mutation_leases"} {
		var rows int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&rows); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if rows != 0 {
			t.Errorf("%s = %d rows after migration, want 0 (nothing is backfilled)", table, rows)
		}
	}
}

// TestLeaseRowRequiresItsIdentity pins the foreign key: a lease row for an
// identity that does not exist is unrepresentable, so a forged row cannot
// invent the identity it claims to guard.
func TestLeaseRowRequiresItsIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO auth_store_mutation_leases
		   (auth_identity_id, holder, fence, acquired_at, expires_at, expires_at_unix_nano, body)
		 VALUES ('auth-ghost', 'inv-1', 1, '2026-01-02T03:04:05Z', '2026-01-02T03:05:05Z', 0, '{}')`)
	if err == nil {
		t.Fatal("a lease naming an unknown identity was accepted")
	}
}

// TestAuthIdentityFieldsCrossChecked pins that every declared field is
// authenticated on reconstruction. The identity carries no content address, so
// a column is what stops a partially edited row reading back as a larger
// parallelism limit than anyone measured.
func TestAuthIdentityFieldsCrossChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{
			"limit raised in the body alone",
			`UPDATE auth_identities SET body = json_set(body, '$.identity.max_parallel_executions', 8) WHERE id = 'auth-1'`,
		},
		{
			"refresh strategy rewritten in the body alone",
			`UPDATE auth_identities SET body = json_set(body, '$.identity.refresh_strategy', 'refresh_external') WHERE id = 'auth-1'`,
		},
		{
			// recorded_at orders revisions, so moving it is how a superseded
			// declaration would come back; it is authenticated like the rest.
			"revision instant moved backward in the body alone",
			`UPDATE auth_identities SET body = json_set(body, '$.recorded_at', '2020-01-01T00:00:00Z') WHERE id = 'auth-1'`,
		},
		{
			"revision instant moved backward in the column alone",
			`UPDATE auth_identities SET recorded_at = '2020-01-01T00:00:00Z' WHERE id = 'auth-1'`,
		},
		{
			"snapshot support claimed in the body alone",
			`UPDATE auth_identities
			 SET body = json_set(body, '$.identity.supports_read_only_auth_snapshot', json('true')) WHERE id = 'auth-1'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			t.Cleanup(func() { _ = s.Close() })
			identity := domain.AuthIdentity{
				ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
				AuthStoreVolume:       "provider-cred",
				MaxParallelExecutions: 1, RefreshStrategy: domain.RefreshOnDemand,
			}
			if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
				return tx.RecordAuthIdentity(ctx, identity, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
			if _, err := s.db.ExecContext(ctx, tc.stmt); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err = s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetAuthIdentity(ctx, "auth-1")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("read of a partially edited identity = %v, want %v", err, errRowInconsistent)
			}
		})
	}
}
