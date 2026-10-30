package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindUsagePools returns a store at head to the shape a daemon at 0111 holds,
// for the tests that seed an older schema and migrate forward again.
func rewindUsagePools(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`ALTER TABLE run_hold_observations DROP COLUMN usage_pool`,
		`DROP TABLE usage_pools`,
		`ALTER TABLE auth_identities ADD COLUMN max_parallel_executions INTEGER NOT NULL DEFAULT 1
			CHECK (max_parallel_executions >= 1)`,
		`DELETE FROM schema_migrations WHERE name = '0112_usage_pools.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
}

// TestUsagePoolsMigrationNeverRaisesConcurrency seeds identities the way a
// daemon at 0111 holds them: a shared pool takes its lowest limit, a no-pool
// identity keeps its own in a pool of its own, the limit leaves the identity
// row and body, and an upgraded store reads every pool and identity back.
func TestUsagePoolsMigrationNeverRaisesConcurrency(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := t.TempDir() + "/usage-pools.db"
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	migrateThrough(t, ctx, db, "0111_")
	const recorded = "2026-01-02T03:04:05Z"
	seeds := []struct {
		id, provider, pool string
		limit              int
		want               string
		wantLimit          int
	}{
		{"auth-shared-high", "claude", "pool-shared", 6, "pool-shared", 2},
		{"auth-shared-low", "claude", "pool-shared", 2, "pool-shared", 2},
		{"auth-solo-wide", "claude", "", 4, "identity:auth-solo-wide", 4},
		{"auth-solo-serial", "openai", "", 1, "identity:auth-solo-serial", 1},
	}
	for _, seed := range seeds {
		body, err := json.Marshal(map[string]any{
			"identity": map[string]any{
				"id": seed.id, "provider": seed.provider, "usage_pool": seed.pool,
				"auth_store_mutation_lease": true, "max_parallel_executions": seed.limit,
				"enabled": true,
				"interim": map[string]any{
					"auth_store_volume": "volume-" + seed.id, "refresh_strategy": "refresh_on_demand",
					"supports_read_only_auth_snapshot": false,
				},
			},
			"recorded_at": recorded,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO auth_identities
			(id, provider, usage_pool, auth_store_mutation_lease, auth_store_volume,
			 max_parallel_executions, refresh_strategy, supports_read_only_auth_snapshot,
			 enabled, recorded_at, body)
			VALUES (?, ?, ?, 1, ?, ?, 'refresh_on_demand', 0, 1, ?, ?)`,
			seed.id, seed.provider, seed.pool, "volume-"+seed.id, seed.limit, recorded, string(body)); err != nil {
			t.Fatalf("seed %s: %v", seed.id, err)
		}
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM auth_identities
		WHERE json_extract(body, '$.identity.max_parallel_executions') IS NOT NULL`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("%d identity bodies keep the limit", remaining)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Read(ctx, func(tx *ReadTx) error {
		for _, seed := range seeds {
			identity, err := tx.GetAuthIdentity(ctx, domain.AuthIdentityID(seed.id))
			if err != nil {
				return err
			}
			pool, err := tx.GetUsagePool(ctx, identity.EffectiveUsagePool())
			if err != nil {
				return err
			}
			if string(pool.ID) != seed.want || pool.MaxParallelExecutions != seed.wantLimit ||
				pool.Provider != seed.provider {
				t.Errorf("%s pool = %#v, want %s at %d", seed.id, pool, seed.want, seed.wantLimit)
			}
			if seed.wantLimit > seed.limit {
				t.Errorf("%s rose from %d to %d", seed.id, seed.limit, seed.wantLimit)
			}
		}
		pools, err := tx.ListUsagePools(ctx)
		if err != nil {
			return err
		}
		if len(pools) != 3 {
			t.Errorf("pools = %#v, want the shared pool and two of their own", pools)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
