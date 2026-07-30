package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestTamperedConformanceRowFailsClosed is the decode-side re-gate: a row
// edited underneath the store reconstructs through the same domain validation
// as the accept side, so a widened, disordered, or unregistered claim fails
// closed instead of reading back as a wider declaration.
func TestTamperedConformanceRowFailsClosed(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name         string
		capabilities string
		outcome      string
	}{
		{
			name:         "claim beyond the class ceiling",
			capabilities: `["supports_credential_volume_detach","supports_post_exit_export"]`,
			outcome:      "passed",
		},
		{
			name:         "unregistered capability name",
			capabilities: `["supports_time_travel"]`,
			outcome:      "passed",
		},
		{
			name:         "non-canonical duplicate claim",
			capabilities: `["supports_post_exit_export","supports_post_exit_export"]`,
			outcome:      "passed",
		},
		{
			name:         "unsorted claim",
			capabilities: `["supports_post_exit_export","supports_detachable_workspace"]`,
			outcome:      "passed",
		},
		{
			name:         "unknown outcome",
			capabilities: `null`,
			outcome:      "pending",
		},
		{
			name:         "failure retroactively granted capabilities",
			capabilities: `["supports_post_exit_export"]`,
			outcome:      "failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := seedAdmission(t, nil)
			record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
				Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
				Outcome:             domain.ConformancePassed,
				ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
				Capabilities:        domain.NewCapabilitySnapshot(domain.CapPostExitExport),
				ProvedAt:            time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			})
			if err != nil {
				t.Fatalf("NewBackendConformance: %v", err)
			}
			if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
				_, err := tx.RecordBackendConformance(ctx, record)
				return err
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
			if _, err := s.db.ExecContext(ctx,
				`UPDATE backend_conformance_records SET capabilities = ?, outcome = ?`,
				tc.capabilities, tc.outcome); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err = s.Read(ctx, func(tx *ReadTx) error {
				_, _, err := tx.LatestBackendConformance(ctx, domain.BackendFreshVMReadOnlyVolumeHandoff)
				return err
			})
			if err == nil {
				t.Fatal("tampered conformance row reconstructed as valid")
			}
			if errors.Is(err, ErrNotFound) {
				t.Fatalf("tampered row read as absence, want a loud refusal: %v", err)
			}
		})
	}
}

func TestConformanceConfigurationMigrationPreservesLegacyRowsAsUnbound(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0020_")
	if _, err := db.ExecContext(ctx, `
INSERT INTO backend_conformance_records
    (backend, outcome, capabilities, proved_at)
VALUES ('fresh_vm_read_only_volume_handoff', 'passed',
        '["supports_post_exit_export"]', '2026-07-27T12:00:00Z')`); err != nil {
		t.Fatalf("seed legacy conformance: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	var digest string
	if err := db.QueryRowContext(ctx,
		`SELECT configuration_digest FROM backend_conformance_records WHERE id = 1`).
		Scan(&digest); err != nil {
		t.Fatalf("read migrated digest: %v", err)
	}
	if digest != string(domain.UnboundBackendConfigurationDigest) {
		t.Fatalf("migrated digest = %q, want reserved unbound value", digest)
	}

	st := &Store{db: db}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		record, found, err := tx.LatestBackendConformance(
			ctx, domain.BackendFreshVMReadOnlyVolumeHandoff)
		if err != nil {
			return err
		}
		if !found || record.ConfigurationBound() {
			t.Fatalf("migrated record = %+v, want present and unbound", record)
		}
		return nil
	}); err != nil {
		t.Fatalf("reconstruct migrated record: %v", err)
	}
}
