package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func installationMintAuditFixture() store.InstallationMintAudit {
	// The janitor mints only metadata:read, but the store surface is
	// scope-agnostic, so the round-trip fixture populates every scope column
	// with a distinguishing value (requested differs from granted; read
	// differs from write): a field-to-wrong-column misbinding then fails the
	// round-trip instead of surviving as two empty strings.
	expiresAt := time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	return store.InstallationMintAudit{
		MintedAt:                time.Date(2026, 7, 17, 12, 0, 0, 123456789, time.UTC),
		RegistrationID:          4365457,
		InstallationID:          424242,
		Outcome:                 "validated",
		RequestedActions:        "read",
		RequestedAdministration: "write",
		RequestedContents:       "read",
		RequestedEnvironments:   "write",
		RequestedPullRequests:   "read",
		RequestedMetadata:       "read",
		GrantedActions:          "write",
		GrantedAdministration:   "read",
		GrantedContents:         "write",
		GrantedEnvironments:     "read",
		GrantedPullRequests:     "write",
		GrantedMetadata:         "read",
		ExpiresAt:               &expiresAt,
	}
}

// installationMintAuditEqual compares two records for value equality, comparing
// the time fields as instants (so UTC and a shifted zone match) and the
// ExpiresAt pointers as their pointed-to instants (nil == nil).
func installationMintAuditEqual(a, b store.InstallationMintAudit) bool {
	if !a.MintedAt.Equal(b.MintedAt) {
		return false
	}
	ae, be := a.ExpiresAt, b.ExpiresAt
	switch {
	case ae == nil && be == nil:
	case ae == nil || be == nil:
		return false
	case !ae.Equal(*be):
		return false
	}
	a.MintedAt, b.MintedAt = time.Time{}, time.Time{}
	a.ExpiresAt, b.ExpiresAt = nil, nil
	return a == b
}

// TestRecordInstallationMintAuditRoundTrip: recorded installation-scope mints
// read back field-identical and in insertion order, with times normalized to
// UTC instants and a rejected mint's nil expiry surviving as NULL.
func TestRecordInstallationMintAuditRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})

	first := installationMintAuditFixture()
	second := installationMintAuditFixture()
	second.InstallationID = 525252
	// A non-UTC wall clock must round-trip as the same instant.
	shifted := second.ExpiresAt.In(time.FixedZone("PDT", -7*60*60))
	second.ExpiresAt = &shifted
	second.MintedAt = second.MintedAt.In(time.FixedZone("PDT", -7*60*60))
	// A rejected grant carries no validated expiry: nil must persist as NULL,
	// and the granted scopes the daemon does not vouch for stay empty.
	third := installationMintAuditFixture()
	third.InstallationID = 626262
	third.Outcome = "grant_rejected"
	third.ExpiresAt = nil
	third.GrantedActions, third.GrantedAdministration, third.GrantedContents = "", "", ""
	third.GrantedEnvironments, third.GrantedPullRequests, third.GrantedMetadata = "", "", ""

	inputs := []store.InstallationMintAudit{first, second, third}
	var recorded []store.InstallationMintAudit
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, rec := range inputs {
			got, err := tx.RecordInstallationMint(ctx, rec)
			if err != nil {
				return err
			}
			recorded = append(recorded, got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if recorded[0].ID <= 0 || recorded[1].ID <= recorded[0].ID || recorded[2].ID <= recorded[1].ID {
		t.Fatalf("assigned IDs not ascending: %d, %d, %d", recorded[0].ID, recorded[1].ID, recorded[2].ID)
	}

	var listed []store.InstallationMintAudit
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		listed, err = tx.ListInstallationMintAudits(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != len(inputs) {
		t.Fatalf("listed %d audits, want %d", len(listed), len(inputs))
	}
	for i, want := range inputs {
		got := listed[i]
		if got.ID != recorded[i].ID {
			t.Errorf("audit %d: ID %d, want %d", i, got.ID, recorded[i].ID)
		}
		if got.MintedAt.Location() != time.UTC {
			t.Errorf("audit %d: minted_at %v is not a UTC instant", i, got.MintedAt)
		}
		if (want.ExpiresAt == nil) != (got.ExpiresAt == nil) {
			t.Errorf("audit %d: expires_at presence = %v, want %v", i, got.ExpiresAt != nil, want.ExpiresAt != nil)
		}
		if got.ExpiresAt != nil && got.ExpiresAt.Location() != time.UTC {
			t.Errorf("audit %d: expires_at %v is not a UTC instant", i, got.ExpiresAt)
		}
		got.ID = 0
		want.ID = 0
		if !installationMintAuditEqual(got, want) {
			t.Errorf("audit %d round-trip mismatch:\ngot  %+v\nwant %+v", i, got, want)
		}
	}
}

// TestRecordInstallationMintAuditAppendOnly: two byte-identical mints are two
// real events; the ledger has no idempotency key and must not dedup them.
func TestRecordInstallationMintAuditAppendOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for range 2 {
			if _, err := tx.RecordInstallationMint(ctx, installationMintAuditFixture()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	var listed []store.InstallationMintAudit
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		listed, err = tx.ListInstallationMintAudits(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d audits, want 2 identical rows", len(listed))
	}
}

// TestRecordInstallationMintAuditRejections: the write method names invalid
// records before the schema CHECKs surface a bare constraint error.
func TestRecordInstallationMintAuditRejections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})

	zero := time.Time{}
	cases := []struct {
		name    string
		mutate  func(*store.InstallationMintAudit)
		wantErr string
	}{
		{"zero registration id", func(r *store.InstallationMintAudit) { r.RegistrationID = 0 }, "not positive"},
		{"negative registration id", func(r *store.InstallationMintAudit) { r.RegistrationID = -1 }, "not positive"},
		{"zero installation id", func(r *store.InstallationMintAudit) { r.InstallationID = 0 }, "not positive"},
		{"negative installation id", func(r *store.InstallationMintAudit) { r.InstallationID = -1 }, "not positive"},
		{"empty outcome", func(r *store.InstallationMintAudit) { r.Outcome = "" }, "empty outcome"},
		{"zero minted at", func(r *store.InstallationMintAudit) { r.MintedAt = time.Time{} }, "zero mint time"},
		{"present but zero expiry", func(r *store.InstallationMintAudit) { r.ExpiresAt = &zero }, "present but zero expiry"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := installationMintAuditFixture()
			tc.mutate(&rec)
			err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				_, err := tx.RecordInstallationMint(ctx, rec)
				return err
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want message containing %q", err, tc.wantErr)
			}
		})
	}

	var listed []store.InstallationMintAudit
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		listed, err = tx.ListInstallationMintAudits(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("rejected records left %d rows, want 0", len(listed))
	}
}

// TestRecordInstallationMintAuditInvisibleToSync: audit is not a synced entity
// (#545, mirroring #107 acceptance 1); recording a mint rides WriteInternal
// and must not bump the client-visible revision.
func TestRecordInstallationMintAuditInvisibleToSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("server state: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordInstallationMint(ctx, installationMintAuditFixture())
		return err
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("server state: %v", err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("revision moved %d -> %d; mint audit must be invisible to sync", before.Revision, after.Revision)
	}
}

// TestInstallationMintAuditPersistsAcrossReopen: the ledger survives a daemon
// restart.
func TestInstallationMintAuditPersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := tempDBPath(t)

	s, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordInstallationMint(ctx, installationMintAuditFixture())
		return err
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openStoreAt(t, path, store.Options{})
	var listed []store.InstallationMintAudit
	err = reopened.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		listed, err = tx.ListInstallationMintAudits(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d audits after reopen, want 1", len(listed))
	}
	want := installationMintAuditFixture()
	if listed[0].InstallationID != want.InstallationID || !listed[0].MintedAt.Equal(want.MintedAt) {
		t.Fatalf("reopened audit mismatch: %+v", listed[0])
	}
}
