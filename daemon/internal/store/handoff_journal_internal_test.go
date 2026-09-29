package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestHandoffJournalColumnsAreCrossChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{
			"ownership token body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.ownership_token', 'ffffffffffffffffffffffffffffffff')
			 WHERE run_id = 'journal-run'`,
		},
		{
			"writer complete column only",
			`UPDATE handoff_journal_records SET writer_complete = 1 WHERE run_id = 'journal-run'`,
		},
		{
			"cancellation column only",
			`UPDATE handoff_journal_records SET cancellation_requested = 1 WHERE run_id = 'journal-run'`,
		},
		{
			"writer failure body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.writer_failure_status', 7)
			 WHERE run_id = 'journal-run'`,
		},
		{
			"writer failure column only",
			`UPDATE handoff_journal_records SET writer_failure_status = 7 WHERE run_id = 'journal-run'`,
		},
		{
			"state binding body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.state', json('{
			   "config_root_fingerprint":"root",
			   "continuity_fingerprint":"continuity",
			   "session_scratch_fingerprint":"scratch",
			   "config_root_target":"/var/lib/freeside/claude-config",
			   "continuity_target":"/var/lib/freeside/claude-config/projects",
			   "session_scratch_target":"/var/lib/freeside/claude-config/session-env",
			   "config_root_read_only":true,
			   "continuity_read_only":false,
			   "session_scratch_read_only":false,
			   "config_root_digest":"aa",
			   "continuity_digest":"bb",
			   "session_scratch_digest":"cc"
			 }'))
			 WHERE run_id = 'journal-run'`,
		},
		{
			"state binding column only",
			`UPDATE handoff_journal_records
			 SET state_preparation = '{
			   "config_root_fingerprint":"root",
			   "continuity_fingerprint":"continuity",
			   "session_scratch_fingerprint":"scratch",
			   "config_root_target":"/var/lib/freeside/claude-config",
			   "continuity_target":"/var/lib/freeside/claude-config/projects",
			   "session_scratch_target":"/var/lib/freeside/claude-config/session-env",
			   "config_root_read_only":true,
			   "continuity_read_only":false,
			   "session_scratch_read_only":false,
			   "config_root_digest":"aa",
			   "continuity_digest":"bb",
			   "session_scratch_digest":"cc"
			 }'
			 WHERE run_id = 'journal-run'`,
		},
		{
			"instruction binding body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.instructions', json('{
			   "composition_version":"claude_explicit_bundle_v1",
			   "host_digest":"absent",
			   "repository_manifest_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			   "bundle_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			 }'))
			 WHERE run_id = 'journal-run'`,
		},
		{
			"instruction binding column only",
			`UPDATE handoff_journal_records
			 SET instruction_preparation = '{
			   "composition_version":"claude_explicit_bundle_v1",
			   "host_digest":"absent",
			   "repository_manifest_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			   "bundle_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			 }'
			 WHERE run_id = 'journal-run'`,
		},
		{
			"lease fence body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.lease.fence', 8) WHERE run_id = 'journal-run'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
			identity := domain.AuthIdentity{
				ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
				MaxParallelExecutions: 1,
				Interim:               domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
			}
			at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
			if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
				if err := tx.RecordAuthIdentity(ctx, identity, at); err != nil {
					return err
				}
				_, err := tx.BeginLeasedHandoffJournal(ctx, HandoffJournalRecord{
					RunID:          "journal-run",
					OwnershipToken: "00112233445566778899aabbccddeeff",
					SpecDigest:     strings.Repeat("ab", 32),
					OpenedAt:       at,
				}, identity.ID, "inv-1", at, at.Add(time.Hour))
				return err
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := s.db.ExecContext(ctx, tc.stmt); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetHandoffJournal(ctx, "journal-run")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("tampered read = %v, want %v", err, errRowInconsistent)
			}
		})
	}
}

func TestHandoffMigrationLeavesLegacyLeasedIdentitiesUnboundAndRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0019_")
	recordedAt := "2026-07-28T12:00:00Z"
	identityBody := `{"identity":{"id":"auth-legacy","provider":"claude","auth_store_mutation_lease":true,"max_parallel_executions":1,"refresh_strategy":"refresh_on_demand","supports_read_only_auth_snapshot":false},"recorded_at":"2026-07-28T12:00:00Z"}`
	if _, err := db.ExecContext(ctx, `
INSERT INTO auth_identities
    (id, provider, auth_store_mutation_lease, max_parallel_executions,
     refresh_strategy, supports_read_only_auth_snapshot, recorded_at, body)
VALUES ('auth-legacy', 'claude', 1, 1, 'refresh_on_demand', 0, ?, ?)`,
		recordedAt, identityBody); err != nil {
		t.Fatalf("seed legacy identity: %v", err)
	}
	leaseBody := `{"auth_identity_id":"auth-legacy","holder":"inv-legacy","fence":1,"acquired_at":"2026-07-28T12:00:00Z","expires_at":"2026-07-28T13:00:00Z","released_at":null}`
	if _, err := db.ExecContext(ctx, `
INSERT INTO auth_store_mutation_leases
    (auth_identity_id, holder, fence, acquired_at, expires_at,
     expires_at_unix_nano, released_at, body)
VALUES ('auth-legacy', 'inv-legacy', 1, '2026-07-28T12:00:00Z',
        '2026-07-28T13:00:00Z', ?, NULL, ?)`,
		time.Date(2026, 7, 28, 13, 0, 0, 0, time.UTC).UnixNano(), leaseBody); err != nil {
		t.Fatalf("seed legacy lease: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	var (
		identityRows int
		leaseRows    int
		volume       sql.NullString
	)
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), auth_store_volume FROM auth_identities WHERE id = 'auth-legacy'`).
		Scan(&identityRows, &volume); err != nil {
		t.Fatalf("read migrated identity: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM auth_store_mutation_leases WHERE auth_identity_id = 'auth-legacy'`).
		Scan(&leaseRows); err != nil {
		t.Fatalf("read migrated lease: %v", err)
	}
	if identityRows != 1 || leaseRows != 1 || volume.Valid {
		t.Fatalf("migrated legacy rows: identity=%d lease=%d volume=%v, want 1/1/NULL",
			identityRows, leaseRows, volume)
	}

	s := &Store{db: db}
	err := s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetAuthIdentity(ctx, "auth-legacy")
		return err
	})
	if err == nil {
		t.Fatal("legacy leased identity without a volume binding reconstructed")
	}
	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetAuthStoreMutationLease(ctx, "auth-legacy")
		return err
	})
	if err == nil {
		t.Fatal("legacy lease behind an unbound identity reconstructed")
	}
}

func TestFailedHandoffOutcomeMigrationAppliesFromPriorHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0022_")

	const (
		runID = "journal-before-failed-outcome"
		body  = `{"version":1,"outcome":"completed"}`
	)
	if _, err := db.ExecContext(ctx, `
INSERT INTO handoff_journal_records (
    run_id, ownership_token, spec_digest, observed_base_sha,
    credential_pre_digest, writer_complete, export_dir, outcome,
    opened_at, body
) VALUES (?, '00112233445566778899aabbccddeeff', ?, ?, ?, 1, ?, 'completed', ?, ?)`,
		runID, strings.Repeat("ab", 32), strings.Repeat("c", 40),
		strings.Repeat("de", 32), "/tmp/export",
		"2026-07-29T12:00:00Z", body); err != nil {
		t.Fatalf("seed prior-head handoff journal: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var (
		gotOutcome      string
		gotInstructions string
		gotBody         string
	)
	if err := db.QueryRowContext(ctx, `
SELECT outcome, instruction_preparation, body
FROM handoff_journal_records
WHERE run_id = ?`, runID).Scan(&gotOutcome, &gotInstructions, &gotBody); err != nil {
		t.Fatalf("read migrated handoff journal: %v", err)
	}
	if gotOutcome != "completed" || gotInstructions != "" || gotBody != body {
		t.Fatalf(
			"migrated row = outcome %q instructions %q body %q, want completed, empty, original",
			gotOutcome,
			gotInstructions,
			gotBody,
		)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE handoff_journal_records SET outcome = 'failed' WHERE run_id = ?`,
		runID); err != nil {
		t.Fatalf("set failed outcome under migrated constraint: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE handoff_journal_records SET outcome = 'unknown' WHERE run_id = ?`,
		runID); err == nil {
		t.Fatal("invalid outcome accepted under migrated constraint")
	}
}

// TestReadHoldMigrationAppliesFromPriorHead covers 0082: a leased journal row
// recorded before the rebuild survives unchanged, and the new constraints hold
// a row to at most one window, each all-present or all-absent.
func TestReadHoldMigrationAppliesFromPriorHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0082_")
	if _, err := db.ExecContext(ctx, `
INSERT INTO auth_identities (id, provider, auth_store_mutation_lease, auth_store_volume,
    max_parallel_executions, refresh_strategy, supports_read_only_auth_snapshot,
    enabled, cost_owner, recorded_at, body)
VALUES ('auth-1', 'claude', 1, 'vol', 1, 'on_demand', 0, 1, '', '2026-07-29T12:00:00Z', '{}')`); err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	const (
		runID = "journal-before-read-holds"
		body  = `{"version":1}`
	)
	if _, err := db.ExecContext(ctx, `
INSERT INTO handoff_journal_records (
    run_id, ownership_token, spec_digest, observed_base_sha,
    credential_pre_digest, writer_complete, cancellation_requested,
    state_preparation, instruction_preparation,
    lease_auth_identity_id, lease_holder, lease_fence, lease_acquired_at, lease_expires_at,
    export_dir, opened_at, body
) VALUES (?, '00112233445566778899aabbccddeeff', ?, '', '', 0, 0, '', '',
    'auth-1', 'inv-1', 3, '2026-07-29T12:00:00Z', '2026-07-29T13:00:00Z', '', ?, ?)`,
		runID, strings.Repeat("ab", 32), "2026-07-29T12:00:00Z", body); err != nil {
		t.Fatalf("seed prior-head leased journal: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	var (
		fence   int64
		holdID  sql.NullString
		gotBody string
	)
	if err := db.QueryRowContext(ctx, `
SELECT lease_fence, read_hold_auth_identity_id, body FROM handoff_journal_records WHERE run_id = ?`,
		runID).Scan(&fence, &holdID, &gotBody); err != nil {
		t.Fatalf("read migrated journal: %v", err)
	}
	if fence != 3 || holdID.Valid || gotBody != body {
		t.Fatalf("migrated row = fence %d read hold %v body %q, want 3, null, original", fence, holdID, gotBody)
	}
	assertTableExists(t, db, "auth_store_read_holds", true)
	if _, err := db.ExecContext(ctx, `
UPDATE handoff_journal_records
SET read_hold_auth_identity_id = 'auth-1', read_hold_holder = 'inv-1',
    read_hold_acquired_at = '2026-07-29T12:00:00Z', read_hold_expires_at = '2026-07-29T13:00:00Z'
WHERE run_id = ?`, runID); err == nil {
		t.Fatal("a row carrying both a lease and a read hold was accepted")
	}
	if _, err := db.ExecContext(ctx, `
UPDATE handoff_journal_records
SET lease_auth_identity_id = NULL, lease_holder = NULL, lease_fence = NULL,
    lease_acquired_at = NULL, lease_expires_at = NULL,
    read_hold_auth_identity_id = 'auth-1'
WHERE run_id = ?`, runID); err == nil {
		t.Fatal("a partial read hold reference was accepted")
	}
}
