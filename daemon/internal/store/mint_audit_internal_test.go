package store

import (
	"context"
	"io/fs"
	"testing"

	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestMintAuditRegistrationMigrationPreservesLegacyRows proves the forward
// migration neither invents a registration identity for historical singleton
// mints nor makes those rows unreadable.
func TestMintAuditRegistrationMigrationPreservesLegacyRows(t *testing.T) {
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
