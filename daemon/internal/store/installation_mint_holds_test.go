package store_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func installationMintHoldFixture() store.InstallationMintHold {
	requestedAt := time.Date(2026, 10, 14, 12, 0, 0, 123456789, time.UTC)
	return store.InstallationMintHold{
		InstallationID: 424242,
		RegistrationID: 4365457,
		RequestedAt:    requestedAt,
		HeldUntil:      requestedAt.Add(69 * time.Minute),
	}
}

func recordMintHold(t *testing.T, s *store.Store, hold store.InstallationMintHold) error {
	t.Helper()
	return s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordInstallationMintHold(context.Background(), hold)
	})
}

func readMintHold(t *testing.T, s *store.Store, installationID int64) (store.InstallationMintHold, bool) {
	t.Helper()
	var (
		hold  store.InstallationMintHold
		found bool
	)
	err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		hold, found, err = tx.InstallationMintHold(context.Background(), installationID)
		return err
	})
	if err != nil {
		t.Fatalf("read hold: %v", err)
	}
	return hold, found
}

func mintHoldEqual(a, b store.InstallationMintHold) bool {
	return a.InstallationID == b.InstallationID && a.RegistrationID == b.RegistrationID &&
		a.RequestedAt.Equal(b.RequestedAt) && a.HeldUntil.Equal(b.HeldUntil)
}

// TestInstallationMintHoldLifecycle: a recorded hold reads back as the same
// instants, a second record replaces the deadline, release removes it, and an
// installation never held reads as absent.
func TestInstallationMintHoldLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})

	if _, found := readMintHold(t, s, 424242); found {
		t.Fatal("hold found before any record")
	}

	first := installationMintHoldFixture()
	// A non-UTC wall clock must round-trip as the same instant.
	first.RequestedAt = first.RequestedAt.In(time.FixedZone("PDT", -7*60*60))
	if err := recordMintHold(t, s, first); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, found := readMintHold(t, s, first.InstallationID)
	if !found || !mintHoldEqual(got, first) {
		t.Fatalf("read = %+v, %v; want %+v", got, found, first)
	}
	if got.RequestedAt.Location() != time.UTC {
		t.Fatalf("requested_at location = %v, want UTC", got.RequestedAt.Location())
	}

	replacement := installationMintHoldFixture()
	replacement.RegistrationID = 5476568
	replacement.RequestedAt = first.RequestedAt.Add(time.Hour)
	replacement.HeldUntil = replacement.RequestedAt.Add(69 * time.Minute)
	if err := recordMintHold(t, s, replacement); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got, found := readMintHold(t, s, first.InstallationID); !found || !mintHoldEqual(got, replacement) {
		t.Fatalf("after replace read = %+v, %v; want %+v", got, found, replacement)
	}

	other := installationMintHoldFixture()
	other.InstallationID = 525252
	if err := recordMintHold(t, s, other); err != nil {
		t.Fatalf("record other: %v", err)
	}
	err := s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseInstallationMintHold(ctx, first.InstallationID)
	})
	if err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, found := readMintHold(t, s, first.InstallationID); found {
		t.Fatal("hold found after release")
	}
	if got, found := readMintHold(t, s, other.InstallationID); !found || !mintHoldEqual(got, other) {
		t.Fatalf("release removed another installation's hold: %+v, %v", got, found)
	}
	// Releasing an absent hold is not an error.
	err = s.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseInstallationMintHold(ctx, first.InstallationID)
	})
	if err != nil {
		t.Fatalf("release absent: %v", err)
	}
}

// TestInstallationMintHoldRejections: malformed holds fail before the write
// and name the problem.
func TestInstallationMintHoldRejections(t *testing.T) {
	t.Parallel()
	s := openStore(t, store.Options{})
	cases := map[string]struct {
		mutate func(*store.InstallationMintHold)
		want   string
	}{
		"installation": {func(h *store.InstallationMintHold) { h.InstallationID = 0 }, "installation id 0 is not positive"},
		"registration": {func(h *store.InstallationMintHold) { h.RegistrationID = -1 }, "registration id -1 is not positive"},
		"requested":    {func(h *store.InstallationMintHold) { h.RequestedAt = time.Time{} }, "zero time"},
		"deadline":     {func(h *store.InstallationMintHold) { h.HeldUntil = time.Time{} }, "zero time"},
		"order":        {func(h *store.InstallationMintHold) { h.HeldUntil = h.RequestedAt }, "deadline is not after the request"},
	}
	for name, tc := range cases {
		hold := installationMintHoldFixture()
		tc.mutate(&hold)
		err := recordMintHold(t, s, hold)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", name, err, tc.want)
		}
	}
	if _, found := readMintHold(t, s, installationMintHoldFixture().InstallationID); found {
		t.Fatal("a rejected hold was written")
	}
}

// TestInstallationMintHoldInvisibleToSync: the hold is internal bookkeeping
// and never moves the sync revision.
func TestInstallationMintHoldInvisibleToSync(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("server state: %v", err)
	}
	if err := recordMintHold(t, s, installationMintHoldFixture()); err != nil {
		t.Fatalf("record: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("server state: %v", err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("revision moved %d -> %d; mint hold must be invisible to sync", before.Revision, after.Revision)
	}
}

// TestInstallationMintHoldPersistsAcrossReopen: the hold survives a daemon
// restart, which is the point of persisting it.
func TestInstallationMintHoldPersistsAcrossReopen(t *testing.T) {
	t.Parallel()
	path := tempDBPath(t)
	s := storetest.Open(t, path, store.Options{})
	hold := installationMintHoldFixture()
	if err := recordMintHold(t, s, hold); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openStoreAt(t, path, store.Options{})
	if got, found := readMintHold(t, reopened, hold.InstallationID); !found || !mintHoldEqual(got, hold) {
		t.Fatalf("after reopen read = %+v, %v; want %+v", got, found, hold)
	}
}
