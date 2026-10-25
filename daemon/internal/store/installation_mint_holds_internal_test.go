package store

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestInstallationMintHoldColumns pins the hold table's shape: no token and
// no outcome column, so no read of it can leak a credential (issue #2004).
func TestInstallationMintHoldColumns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawTemplate(t)
	rows, err := db.QueryContext(ctx,
		`SELECT name FROM pragma_table_info('publish_installation_mint_holds') ORDER BY cid`)
	if err != nil {
		t.Fatalf("table info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan: %v", err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []string{"installation_id", "registration_id", "requested_at", "held_until"}
	if !slices.Equal(columns, want) {
		t.Fatalf("columns = %v, want %v", columns, want)
	}
}

// TestInstallationMintHoldReadRejectsInconsistentRow: a row edited outside
// the store so that it breaks a write-side invariant reads as an error, not
// as a hold. A deadline at or before its request would otherwise read as
// expired and let the janitor mint while an earlier token may be live.
func TestInstallationMintHoldReadRejectsInconsistentRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		stmt string
		want string
	}{
		{
			"deadline equal to the request",
			`UPDATE publish_installation_mint_holds SET held_until = requested_at`,
			"deadline is not after the request",
		},
		{
			"deadline before the request",
			`UPDATE publish_installation_mint_holds SET held_until = '2026-10-14T11:00:00Z'`,
			"deadline is not after the request",
		},
		{
			"zero request time",
			`UPDATE publish_installation_mint_holds SET requested_at = '0001-01-01T00:00:00Z'`,
			"zero time",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
			requestedAt := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
			if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
				return tx.RecordInstallationMintHold(ctx, InstallationMintHold{
					InstallationID: 424242, RegistrationID: 4365457,
					RequestedAt: requestedAt, HeldUntil: requestedAt.Add(69 * time.Minute),
				})
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
			if _, err := s.db.ExecContext(ctx, tc.stmt); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, found, err := tx.InstallationMintHold(ctx, 424242)
				if found {
					t.Error("inconsistent row read as found")
				}
				return err
			})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("read of an inconsistent hold = %v, want %q", err, tc.want)
			}
		})
	}
}
