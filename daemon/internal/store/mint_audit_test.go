package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func mintAuditFixture() store.MintAudit {
	return store.MintAudit{
		MintedAt:                time.Date(2026, 7, 17, 12, 0, 0, 123456789, time.UTC),
		InstallationID:          424242,
		Repo:                    "freeside-ai/candidate-repo",
		RequestedActions:        "read",
		RequestedAdministration: "read",
		RequestedContents:       "write",
		RequestedEnvironments:   "read",
		RequestedPullRequests:   "write",
		RequestedMetadata:       "read",
		GrantedContents:         "write",
		GrantedActions:          "read",
		GrantedAdministration:   "read",
		GrantedEnvironments:     "read",
		GrantedPullRequests:     "write",
		GrantedMetadata:         "read",
		ExpiresAt:               time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC),
	}
}

// TestRecordMintAuditRoundTrip: a recorded mint reads back field-identical
// and in insertion order, with times normalized to UTC instants.
func TestRecordMintAuditRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})

	first := mintAuditFixture()
	second := mintAuditFixture()
	second.Repo = "freeside-ai/other-repo"
	// A non-UTC wall clock must round-trip as the same instant.
	second.MintedAt = second.MintedAt.In(time.FixedZone("PDT", -7*60*60))

	var recorded []store.MintAudit
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, rec := range []store.MintAudit{first, second} {
			got, err := tx.RecordMintAudit(ctx, rec)
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
	if recorded[0].ID <= 0 || recorded[1].ID <= recorded[0].ID {
		t.Fatalf("assigned IDs not ascending: %d, %d", recorded[0].ID, recorded[1].ID)
	}

	var listed []store.MintAudit
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		listed, err = tx.ListMintAudits(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d audits, want 2", len(listed))
	}
	for i, want := range []store.MintAudit{first, second} {
		got := listed[i]
		if got.ID != recorded[i].ID {
			t.Errorf("audit %d: ID %d, want %d", i, got.ID, recorded[i].ID)
		}
		if !got.MintedAt.Equal(want.MintedAt) || got.MintedAt.Location() != time.UTC {
			t.Errorf("audit %d: minted_at %v, want UTC instant %v", i, got.MintedAt, want.MintedAt)
		}
		if !got.ExpiresAt.Equal(want.ExpiresAt) || got.ExpiresAt.Location() != time.UTC {
			t.Errorf("audit %d: expires_at %v, want UTC instant %v", i, got.ExpiresAt, want.ExpiresAt)
		}
		got.ID, got.MintedAt, got.ExpiresAt = 0, time.Time{}, time.Time{}
		want.ID, want.MintedAt, want.ExpiresAt = 0, time.Time{}, time.Time{}
		if got != want {
			t.Errorf("audit %d round-trip mismatch:\ngot  %+v\nwant %+v", i, got, want)
		}
	}
}

// TestRecordMintAuditAppendOnly: two byte-identical mints are two real
// events; the ledger has no idempotency key and must not dedup them.
func TestRecordMintAuditAppendOnly(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})

	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for range 2 {
			if _, err := tx.RecordMintAudit(ctx, mintAuditFixture()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	var listed []store.MintAudit
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		listed, err = tx.ListMintAudits(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d audits, want 2 identical rows", len(listed))
	}
}

// TestRecordMintAuditRejections: the write method names invalid records
// before the schema CHECKs surface a bare constraint error.
func TestRecordMintAuditRejections(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})

	cases := []struct {
		name    string
		mutate  func(*store.MintAudit)
		wantErr string
	}{
		{"empty repo", func(r *store.MintAudit) { r.Repo = "" }, "empty repo"},
		{"zero installation id", func(r *store.MintAudit) { r.InstallationID = 0 }, "not positive"},
		{"negative installation id", func(r *store.MintAudit) { r.InstallationID = -1 }, "not positive"},
		{"zero minted at", func(r *store.MintAudit) { r.MintedAt = time.Time{} }, "zero mint or expiry time"},
		{"zero expires at", func(r *store.MintAudit) { r.ExpiresAt = time.Time{} }, "zero mint or expiry time"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := mintAuditFixture()
			tc.mutate(&rec)
			err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
				_, err := tx.RecordMintAudit(ctx, rec)
				return err
			})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want message containing %q", err, tc.wantErr)
			}
		})
	}

	var listed []store.MintAudit
	err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		listed, err = tx.ListMintAudits(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("rejected records left %d rows, want 0", len(listed))
	}
}

// TestRecordMintAuditInvisibleToSync: audit is not a synced entity (#107
// acceptance 1); recording a mint rides WriteInternal and must not bump the
// client-visible revision.
func TestRecordMintAuditInvisibleToSync(t *testing.T) {
	ctx := context.Background()
	s := openStore(t, store.Options{})

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("server state: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordMintAudit(ctx, mintAuditFixture())
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

// TestMintAuditPersistsAcrossReopen: the ledger survives a daemon restart.
func TestMintAuditPersistsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := tempDBPath(t)

	s, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordMintAudit(ctx, mintAuditFixture())
		return err
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openStoreAt(t, path, store.Options{})
	var listed []store.MintAudit
	err = reopened.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		listed, err = tx.ListMintAudits(ctx)
		return err
	})
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d audits after reopen, want 1", len(listed))
	}
	want := mintAuditFixture()
	if listed[0].Repo != want.Repo || !listed[0].MintedAt.Equal(want.MintedAt) {
		t.Fatalf("reopened audit mismatch: %+v", listed[0])
	}
}
