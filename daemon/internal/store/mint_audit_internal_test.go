package store

import (
	"context"
	"io/fs"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestMintAuditRegistrationMigrationPreservesLegacyRows proves the forward
// migration neither invents a registration identity for historical singleton
// mints nor makes those rows unreadable.
func TestMintAuditRegistrationMigrationPreservesLegacyRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	names, err := fs.Glob(migrations.FS, "000[1-9]_*.sql")
	if err != nil {
		t.Fatalf("glob pre-registration migrations: %v", err)
	}
	older := map[string]string{}
	for _, name := range names {
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		older[name] = string(body)
	}
	if err := migrate(ctx, db, mapFS(older)); err != nil {
		t.Fatalf("migrate old schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO publish_mint_audits (
    minted_at, installation_id, repo,
    requested_contents, requested_pull_requests, requested_metadata,
    granted_contents, granted_pull_requests, granted_metadata,
    requested_actions, requested_administration, requested_environments,
    granted_actions, granted_administration, granted_environments,
    expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"2026-07-17T12:00:00Z", 424242, "freeside-ai/legacy-repo",
		"write", "write", "read", "write", "write", "read",
		"read", "read", "read", "read", "read", "read",
		"2026-07-17T13:00:00Z"); err != nil {
		t.Fatalf("insert legacy audit: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate current schema: %v", err)
	}

	var registrationID int64
	if err := db.QueryRowContext(ctx,
		`SELECT registration_id FROM publish_mint_audits WHERE repo = ?`,
		"freeside-ai/legacy-repo").Scan(&registrationID); err != nil {
		t.Fatalf("read migrated audit: %v", err)
	}
	if registrationID != 0 {
		t.Fatalf("legacy registration_id = %d, want explicit unknown 0", registrationID)
	}
}

// TestMintAuditRepositoryMigrationPreservesLegacyRows proves the forward
// migration neither guesses a canonical repository ID from a mutable
// owner/name nor makes pre-ID mint rows unreadable.
func TestMintAuditRepositoryMigrationPreservesLegacyRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob pre-repository-ID migrations: %v", err)
	}
	older := map[string]string{}
	for _, name := range names {
		if name >= "0011_" {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			t.Fatalf("read migration %s: %v", name, err)
		}
		older[name] = string(body)
	}
	if err := migrate(ctx, db, mapFS(older)); err != nil {
		t.Fatalf("migrate old schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO publish_mint_audits (
    minted_at, registration_id, installation_id, repo,
    requested_contents, requested_pull_requests, requested_metadata,
    granted_contents, granted_pull_requests, granted_metadata,
    requested_actions, requested_administration, requested_environments,
    granted_actions, granted_administration, granted_environments,
    expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"2026-07-17T12:00:00Z", 4365457, 424242, "freeside-ai/legacy-repo",
		"write", "write", "read", "write", "write", "read",
		"read", "read", "read", "read", "read", "read",
		"2026-07-17T13:00:00Z"); err != nil {
		t.Fatalf("insert legacy audit: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate current schema: %v", err)
	}

	var repositoryID int64
	if err := db.QueryRowContext(ctx,
		`SELECT repository_id FROM publish_mint_audits WHERE repo = ?`,
		"freeside-ai/legacy-repo").Scan(&repositoryID); err != nil {
		t.Fatalf("read migrated audit: %v", err)
	}
	if repositoryID != 0 {
		t.Fatalf("legacy repository_id = %d, want explicit unknown 0", repositoryID)
	}
}

// TestMintAuditIssuesMigrationPreservesLegacyRows proves the forward
// migration applies to a populated ledger and records no issues scope for a
// mint that predates the column pair: the row requested none.
func TestMintAuditIssuesMigrationPreservesLegacyRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0092_")
	if _, err := db.ExecContext(ctx, `
INSERT INTO publish_mint_audits (
    minted_at, registration_id, installation_id, repository_id, repo,
    requested_contents, requested_pull_requests, requested_metadata,
    granted_contents, granted_pull_requests, granted_metadata,
    requested_actions, requested_administration, requested_environments,
    granted_actions, granted_administration, granted_environments,
    expires_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"2026-07-17T12:00:00Z", 4365457, 424242, 990011, "freeside-ai/legacy-repo",
		"write", "write", "read", "write", "write", "read",
		"read", "read", "read", "read", "read", "read",
		"2026-07-17T13:00:00Z"); err != nil {
		t.Fatalf("insert legacy audit: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate current schema: %v", err)
	}

	var requested, granted string
	if err := db.QueryRowContext(ctx,
		`SELECT requested_issues, granted_issues FROM publish_mint_audits WHERE repo = ?`,
		"freeside-ai/legacy-repo").Scan(&requested, &granted); err != nil {
		t.Fatalf("read migrated audit: %v", err)
	}
	if requested != "" || granted != "" {
		t.Fatalf("legacy issues scope = requested %q, granted %q, want both empty", requested, granted)
	}
}

// TestEncryptedRestoreRoundTripsMintAuditIssues proves a checkpoint carries a
// mint audit's issues scope and the daemon's restore brings it back. The
// restore copies each row by column position, so the two columns are read
// directly and hold different values: a copy that dropped or crossed them
// would show here. The audit written after the checkpoint must be gone.
func TestEncryptedRestoreRoundTripsMintAuditIssues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)

	record := func(repo, requested, granted string) {
		t.Helper()
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.RecordMintAudit(ctx, MintAudit{
				MintedAt:       time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC),
				RegistrationID: 4365457, InstallationID: 424242, RepositoryID: 990011,
				Repo:              repo,
				RequestedMetadata: "read", GrantedMetadata: "read",
				RequestedIssues: requested, GrantedIssues: granted,
				ExpiresAt: time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC),
			})
			return err
		}); err != nil {
			t.Fatalf("record %s: %v", repo, err)
		}
	}
	record("freeside-ai/checkpointed-repo", "write", "read")

	plaintext, err := serializeStoreCheckpoint(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	sourceDB, source, err := openDeserializedBackupDatabase(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDeserializedBackupDatabase(sourceDB, source)
	record("freeside-ai/post-checkpoint-repo", "", "")

	if _, err := s.restoreFromDatabase(ctx, source); err != nil {
		t.Fatalf("restore: %v", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT repo, requested_issues, granted_issues FROM publish_mint_audits`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var got [][3]string
	for rows.Next() {
		var row [3]string
		if err := rows.Scan(&row[0], &row[1], &row[2]); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := [3]string{"freeside-ai/checkpointed-repo", "write", "read"}
	if len(got) != 1 || got[0] != want {
		t.Fatalf("restored audits = %q, want only %q", got, want)
	}
}
