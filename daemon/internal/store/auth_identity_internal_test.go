package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestLeaseRowRequiresItsIdentity pins the foreign key: a lease row for an
// identity that does not exist is unrepresentable, so a forged row cannot
// invent the identity it claims to guard.
func TestLeaseRowRequiresItsIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawTemplate(t)
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
			"refresh strategy rewritten in the body alone",
			`UPDATE auth_identities SET body = json_set(body, '$.identity.interim.refresh_strategy', 'refresh_external') WHERE id = 'auth-1'`,
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
			 SET body = json_set(body, '$.identity.interim.supports_read_only_auth_snapshot', json('true')) WHERE id = 'auth-1'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
			identity := domain.AuthIdentity{
				ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
				Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
			}
			if err := s.Write(ctx, func(tx *WriteTx) error {
				return tx.RecordAuthIdentity(ctx, identity, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
			}); err != nil {
				t.Fatalf("record: %v", err)
			}
			if _, err := s.db.ExecContext(ctx, tc.stmt); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetAuthIdentity(ctx, "auth-1")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("read of a partially edited identity = %v, want %v", err, errRowInconsistent)
			}
		})
	}
}

// TestAuthIdentityProviderRowsDecode pins that AuthProvider's member values
// are the strings rows already store: a row in the shape RecordAuthIdentity
// writes, inserted by hand rather than through the typed encoder, reads back
// for every member. A provider outside the enum fails as an invalid stored
// row instead of reaching admission.
func TestAuthIdentityProviderRowsDecode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	insert := func(t *testing.T, s *Store, provider string) {
		t.Helper()
		body := `{"identity":{"id":"auth-1","provider":"` + provider + `",` +
			`"account_binding":"","usage_pool":"","budget":0,"auth_store_mutation_lease":true,` +
			`"enabled":true,"cost_owner":"","interim":{"auth_store_volume":"provider-cred",` +
			`"refresh_strategy":"refresh_on_demand","supports_read_only_auth_snapshot":false}},` +
			`"recorded_at":"2026-01-02T03:04:05Z"}`
		if _, err := s.db.ExecContext(ctx, `
INSERT INTO auth_identities
    (id, provider, account_binding, usage_pool, budget, auth_store_mutation_lease,
     auth_store_volume, refresh_strategy, supports_read_only_auth_snapshot, enabled,
     cost_owner, recorded_at, body)
VALUES ('auth-1', ?, '', '', 0, 1, 'provider-cred', 'refresh_on_demand', 0, 1, '',
        '2026-01-02T03:04:05Z', ?)`, provider, body); err != nil {
			t.Fatalf("insert identity row: %v", err)
		}
	}
	read := func(s *Store) (domain.AuthIdentity, error) {
		var identity domain.AuthIdentity
		err := s.Read(ctx, func(tx *ReadTx) error {
			var err error
			identity, err = tx.GetAuthIdentity(ctx, "auth-1")
			return err
		})
		return identity, err
	}
	for _, provider := range domain.AllAuthProviders {
		t.Run(string(provider), func(t *testing.T) {
			t.Parallel()
			s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
			insert(t, s, string(provider))
			identity, err := read(s)
			if err != nil {
				t.Fatalf("read %s row: %v", provider, err)
			}
			want := domain.AuthIdentity{
				ID: "auth-1", Provider: provider, AuthStoreMutationLease: true, Enabled: true,
				Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
			}
			if identity != want {
				t.Fatalf("read %s row = %+v, want %+v", provider, identity, want)
			}
		})
	}
	t.Run("unknown provider", func(t *testing.T) {
		t.Parallel()
		s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
		insert(t, s, "codex")
		if _, err := read(s); !errors.Is(err, domain.ErrInvalidAuthProvider) {
			t.Fatalf("read codex row = %v, want %v", err, domain.ErrInvalidAuthProvider)
		}
	})
}
