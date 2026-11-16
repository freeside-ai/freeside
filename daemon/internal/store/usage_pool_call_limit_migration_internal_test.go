package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindUsagePoolCallLimit returns a migrated store to the state 0142 leaves:
// it takes the call limit back out of every pool body and forgets 0143.
func rewindUsagePoolCallLimit(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	rewindDaemonReservations(t, ctx, db)
	for _, statement := range []string{
		`UPDATE usage_pools SET body = json_remove(body, '$.pool.max_calls_per_site')`,
		`DELETE FROM schema_migrations WHERE name = '0143_usage_pool_call_limit.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestUsagePoolCallLimitMigrationWritesOne proves migration 0143 gives every
// stored pool a call limit of one and changes nothing else in its body or
// columns: a pool written before the field fails validation on read, and one
// read after the migration is the same declaration with the limit added.
func TestUsagePoolCallLimitMigrationWritesOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawThrough(t, "0143_")
	if got := rawVersion(t, db); got != 142 {
		t.Fatalf("schema version before 0143 = %d, want 142", got)
	}
	const recordedAt = "2026-11-01T00:00:00Z"
	seeds := []struct {
		id, provider string
		limit        int
	}{
		{id: "pool-one", provider: "claude", limit: 1},
		{id: "pool-four", provider: "openai", limit: 4},
	}
	bodies := map[string]string{}
	for _, seed := range seeds {
		body := fmt.Sprintf(`{"pool":{"id":%q,"provider":%q,"max_parallel_executions":%d},"recorded_at":%q}`,
			seed.id, seed.provider, seed.limit, recordedAt)
		bodies[seed.id] = body
		if _, err := db.ExecContext(ctx, `INSERT INTO usage_pools
			(id, provider, max_parallel_executions, recorded_at, body) VALUES (?, ?, ?, ?, ?)`,
			seed.id, seed.provider, seed.limit, recordedAt, body); err != nil {
			t.Fatalf("seed %s: %v", seed.id, err)
		}
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, db)

	for _, seed := range seeds {
		var provider, at, body string
		var limit int
		if err := db.QueryRowContext(ctx, `SELECT provider, max_parallel_executions, recorded_at, body
			FROM usage_pools WHERE id = ?`, seed.id).Scan(&provider, &limit, &at, &body); err != nil {
			t.Fatalf("read %s: %v", seed.id, err)
		}
		if provider != seed.provider || limit != seed.limit || at != recordedAt {
			t.Errorf("%s columns = %s, %d, %s; want them unchanged", seed.id, provider, limit, at)
		}
		var got, want map[string]any
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(bodies[seed.id]), &want); err != nil {
			t.Fatal(err)
		}
		want["pool"].(map[string]any)["max_calls_per_site"] = float64(1)
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s body = %s, want %s", seed.id, gotJSON, wantJSON)
		}
		record, err := decode[usagePoolRecord]([]byte(body))
		if err != nil {
			t.Fatalf("%s decodes: %v", seed.id, err)
		}
		wantPool := domain.UsagePool{
			ID: domain.UsagePoolID(seed.id), Provider: domain.AuthProvider(seed.provider),
			MaxParallelExecutions: seed.limit, MaxCallsPerSite: 1,
		}
		if record.Pool != wantPool {
			t.Errorf("%s pool = %#v, want %#v", seed.id, record.Pool, wantPool)
		}
	}
}
