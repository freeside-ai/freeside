package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

const enrollmentManifest = domain.Digest(
	"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")

// staleManifest is a well-formed digest no generation records: the bytes a
// stale or fabricated lease binding claims to mutate from.
const staleManifest = domain.Digest(
	"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")

func enrollmentIdentity() domain.AuthIdentity {
	// The post-adoption shape: account-bound, leased, no interim facts — the
	// enrollment and its generations carry the store.
	return domain.AuthIdentity{
		ID: "auth-1", Provider: "openai", AccountBinding: "acct-1",
		UsagePool: "pool-1", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1, Enabled: true,
	}
}

func codexEnrollment() domain.ClientEnrollment {
	return domain.ClientEnrollment{
		ID: "enroll-1", AuthIdentityID: "auth-1",
		HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
		AuthMethod:      domain.AuthMethodOAuth,
		CredentialMode:  domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: "acct-1",
	}
}

func enrollmentEntry(fence int64) domain.EnrollmentGeneration {
	expiry := time.Date(2026, 1, 3, 3, 4, 5, 0, time.UTC)
	return domain.EnrollmentGeneration{
		EnrollmentID: "enroll-1", AuthStoreVolume: "codex-store",
		StoreManifestDigest: enrollmentManifest, LeaseFence: fence,
		AccountBinding: "acct-1", TokenExpiry: &expiry,
		RecordedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func openEnrollmentStore(t *testing.T) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func recordEnrollmentFixtures(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordAuthIdentity(ctx, enrollmentIdentity(),
			time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)); err != nil {
			return err
		}
		return tx.RecordClientEnrollment(ctx, codexEnrollment(),
			time.Date(2026, 1, 2, 3, 1, 0, 0, time.UTC))
	}); err != nil {
		t.Fatalf("record fixtures: %v", err)
	}
}

// TestAdmittedAgentsMigrationNarrowsIdentities proves 0052 rewrites a
// pre-revision identity row into the narrowed shape: client facts under
// interim, enabled backfilled true, account fields empty, and the row
// readable through the cross-checked reconstruction.
func TestAdmittedAgentsMigrationNarrowsIdentities(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	prior := map[string]string{}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "0052_admitted_agents.sql" ||
			entry.Name() == "0053_shadow_review.sql" ||
			entry.Name() == "0054_attention_readiness_summary.sql" ||
			entry.Name() == "0055_attention_yield_history.sql" ||
			entry.Name() == "0056_shadow_review_configuration_approval.sql" ||
			entry.Name() == "0057_finding_adjudication_revisions.sql" ||
			entry.Name() == "0058_attention_decision_surfaces.sql" ||
			entry.Name() == "0059_attention_decision_surface_bodies.sql" ||
			entry.Name() == "0060_attention_recommendation_sources.sql" || entry.IsDir() {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		prior[entry.Name()] = string(body)
	}
	if err := migrate(ctx, db, mapFS(prior)); err != nil {
		t.Fatalf("migrate to 0051: %v", err)
	}
	legacyBody := `{"identity":{"id":"auth-legacy","provider":"claude",` +
		`"auth_store_mutation_lease":true,"auth_store_volume":"claude-cred",` +
		`"max_parallel_executions":2,"refresh_strategy":"refresh_external",` +
		`"supports_read_only_auth_snapshot":true},` +
		`"recorded_at":"2026-01-02T03:04:05Z"}`
	if _, err := db.ExecContext(ctx, `
INSERT INTO auth_identities
    (id, provider, auth_store_mutation_lease, auth_store_volume, max_parallel_executions,
     refresh_strategy, supports_read_only_auth_snapshot, recorded_at, body)
VALUES ('auth-legacy', 'claude', 1, 'claude-cred', 2, 'refresh_external', 1,
        '2026-01-02T03:04:05Z', ?)`, legacyBody); err != nil {
		t.Fatalf("insert legacy identity: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}
	s, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var identity domain.AuthIdentity
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		identity, err = tx.GetAuthIdentity(ctx, "auth-legacy")
		return err
	}); err != nil {
		t.Fatalf("read migrated identity: %v", err)
	}
	want := domain.AuthIdentity{
		ID: "auth-legacy", Provider: "claude", AuthStoreMutationLease: true,
		MaxParallelExecutions: 2, Enabled: true,
		Interim: domain.InterimClientFacts{
			AuthStoreVolume: "claude-cred", RefreshStrategy: domain.RefreshExternal,
			SupportsReadOnlyAuthSnapshot: true,
		},
	}
	if identity != want {
		t.Fatalf("migrated identity = %+v, want %+v", identity, want)
	}
}

// TestRecordClientEnrollment pins the account rule at the write boundary and
// the write-once convergence contract.
func TestRecordClientEnrollment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	recordedAt := time.Date(2026, 1, 2, 3, 1, 0, 0, time.UTC)

	t.Run("identity without account binding refuses", func(t *testing.T) {
		s := openEnrollmentStore(t)
		unbound := enrollmentIdentity()
		unbound.AccountBinding = ""
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			if err := tx.RecordAuthIdentity(ctx, unbound, recordedAt); err != nil {
				return err
			}
			return tx.RecordClientEnrollment(ctx, codexEnrollment(), recordedAt)
		})
		if !errors.Is(err, domain.ErrAccountBindingMismatch) {
			t.Fatalf("RecordClientEnrollment = %v, want %v", err, domain.ErrAccountBindingMismatch)
		}
	})

	t.Run("credential for another account refuses", func(t *testing.T) {
		s := openEnrollmentStore(t)
		foreign := codexEnrollment()
		foreign.AccountBinding = "acct-2"
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			if err := tx.RecordAuthIdentity(ctx, enrollmentIdentity(), recordedAt); err != nil {
				return err
			}
			return tx.RecordClientEnrollment(ctx, foreign, recordedAt)
		})
		if !errors.Is(err, domain.ErrAccountBindingMismatch) {
			t.Fatalf("RecordClientEnrollment = %v, want %v", err, domain.ErrAccountBindingMismatch)
		}
	})

	t.Run("replay converges and a rewrite fails", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.RecordClientEnrollment(ctx, codexEnrollment(),
				time.Date(2026, 1, 2, 3, 1, 0, 0, time.UTC))
		}); err != nil {
			t.Fatalf("byte-identical replay = %v, want nil", err)
		}
		rewritten := codexEnrollment()
		rewritten.Route = "openai_platform"
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.RecordClientEnrollment(ctx, rewritten, recordedAt)
		})
		if !errors.Is(err, ErrImmutableConflict) {
			t.Fatalf("rewrite = %v, want %v", err, ErrImmutableConflict)
		}
	})

	t.Run("read re-gates the identity binding", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		// Tamper the identity's account binding coherently (column and body),
		// bypassing the set-once transition rule: reconstruction must still
		// refuse the enrollment, because its recorded account no longer
		// matches its identity's.
		if _, err := s.db.ExecContext(ctx, `
UPDATE auth_identities
SET account_binding = 'acct-stolen',
    body = json_set(body, '$.identity.account_binding', 'acct-stolen')
WHERE id = 'auth-1'`); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		err := s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetClientEnrollment(ctx, "enroll-1")
			return err
		})
		if !errors.Is(err, domain.ErrAccountBindingMismatch) {
			t.Fatalf("re-gated read = %v, want %v", err, domain.ErrAccountBindingMismatch)
		}
	})

	t.Run("column and body cross-checked", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		if _, err := s.db.ExecContext(ctx, `
UPDATE client_enrollments
SET body = json_set(body, '$.enrollment.route', 'openai_platform')
WHERE id = 'enroll-1'`); err != nil {
			t.Fatalf("tamper: %v", err)
		}
		err := s.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetClientEnrollment(ctx, "enroll-1")
			return err
		})
		if !errors.Is(err, errRowInconsistent) {
			t.Fatalf("cross-checked read = %v, want %v", err, errRowInconsistent)
		}
	})
}

// TestAccountBindingUnique pins the §5.4 one-account-one-identity rule: a
// second identity claiming a bound account is a typed refusal.
func TestAccountBindingUnique(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openEnrollmentStore(t)
	recordedAt := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	second := enrollmentIdentity()
	second.ID = "auth-2"
	err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordAuthIdentity(ctx, enrollmentIdentity(), recordedAt); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, second, recordedAt)
	})
	if !errors.Is(err, domain.ErrAccountBindingTaken) {
		t.Fatalf("second identity for one account = %v, want %v", err, domain.ErrAccountBindingTaken)
	}
}

// TestAppendEnrollmentGeneration pins the §5.4 fencing: every append happens
// under the identity's live lease, names its fence, and receives the next
// contiguous store-assigned ordinal.
func TestAppendEnrollmentGeneration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	leaseStart := time.Date(2026, 1, 2, 3, 2, 0, 0, time.UTC)
	leaseEnd := leaseStart.Add(10 * time.Minute)
	inWindow := leaseStart.Add(time.Minute)

	acquire := func(t *testing.T, s *Store, binding *domain.LeaseGenerationBinding) domain.AuthStoreMutationLease {
		t.Helper()
		var lease domain.AuthStoreMutationLease
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			var err error
			lease, err = tx.AcquireAuthStoreMutationLeaseBound(
				ctx, "auth-1", "inv-refresh", binding, leaseStart, leaseEnd)
			return err
		}); err != nil {
			t.Fatalf("acquire lease: %v", err)
		}
		return lease
	}

	t.Run("ordinal is store-assigned and contiguous", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		acquire(t, s, &domain.LeaseGenerationBinding{
			EnrollmentID: "enroll-1", Generation: 0,
			AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
		})
		var stamped domain.EnrollmentGeneration
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			var err error
			stamped, err = tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(1), inWindow)
			return err
		}); err != nil {
			t.Fatalf("bootstrap append: %v", err)
		}
		if stamped.Ordinal != 1 {
			t.Fatalf("bootstrap append stamped ordinal %d", stamped.Ordinal)
		}
		// The binding was consumed by its own append: the store moved to
		// generation 1, so the same fence cannot append a second mutation
		// computed from the pre-append bytes.
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(1), inWindow)
			return err
		})
		if !errors.Is(err, ErrEnrollmentLeaseBindingStale) {
			t.Fatalf("append under a consumed binding = %v, want %v", err, ErrEnrollmentLeaseBindingStale)
		}
		// Release and re-acquire against the state the next mutation starts
		// from; the takeover bumps the fence to 2.
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.ReleaseAuthStoreMutationLease(
				ctx, "auth-1", "inv-refresh", 1, inWindow.Add(time.Minute))
		}); err != nil {
			t.Fatalf("release lease: %v", err)
		}
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AcquireAuthStoreMutationLeaseBound(
				ctx, "auth-1", "inv-refresh", &domain.LeaseGenerationBinding{
					EnrollmentID: "enroll-1", Generation: 1,
					AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
				}, inWindow.Add(2*time.Minute), leaseEnd)
			return err
		}); err != nil {
			t.Fatalf("re-acquire lease: %v", err)
		}
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			var err error
			stamped, err = tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(2), inWindow.Add(3*time.Minute))
			return err
		}); err != nil {
			t.Fatalf("second append: %v", err)
		}
		if stamped.Ordinal != 2 {
			t.Fatalf("second append stamped ordinal %d", stamped.Ordinal)
		}
		var current domain.EnrollmentGeneration
		if err := s.Read(ctx, func(tx *ReadTx) error {
			var err error
			current, err = tx.CurrentEnrollmentGeneration(ctx, "enroll-1")
			return err
		}); err != nil {
			t.Fatalf("current generation: %v", err)
		}
		if current.Ordinal != 2 {
			t.Fatalf("current ordinal = %d, want 2", current.Ordinal)
		}
		var first domain.EnrollmentGeneration
		if err := s.Read(ctx, func(tx *ReadTx) error {
			var err error
			first, err = tx.GetEnrollmentGeneration(ctx, "enroll-1", 1)
			return err
		}); err != nil {
			t.Fatalf("get generation 1: %v", err)
		}
		if first.Ordinal != 1 {
			t.Fatalf("generation 1 ordinal = %d", first.Ordinal)
		}
	})

	t.Run("caller-supplied ordinal refused", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		acquire(t, s, nil)
		entry := enrollmentEntry(1)
		entry.Ordinal = 1
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, entry, inWindow)
			return err
		})
		if !errors.Is(err, ErrEnrollmentOrdinalSupplied) {
			t.Fatalf("append = %v, want %v", err, ErrEnrollmentOrdinalSupplied)
		}
	})

	t.Run("no lease refuses", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(1), inWindow)
			return err
		})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("append without lease = %v, want %v", err, ErrNotFound)
		}
	})

	t.Run("stale fence refuses", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		acquire(t, s, nil)
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(7), inWindow)
			return err
		})
		if !errors.Is(err, ErrEnrollmentLeaseNotHeld) {
			t.Fatalf("append with stale fence = %v, want %v", err, ErrEnrollmentLeaseNotHeld)
		}
	})

	t.Run("expired window refuses", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		acquire(t, s, nil)
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(1), leaseEnd.Add(time.Second))
			return err
		})
		if !errors.Is(err, ErrEnrollmentLeaseNotHeld) {
			t.Fatalf("append past expiry = %v, want %v", err, ErrEnrollmentLeaseNotHeld)
		}
	})

	t.Run("bound lease guards its exact store", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		lease := acquire(t, s, &domain.LeaseGenerationBinding{
			EnrollmentID: "enroll-1", Generation: 0,
			AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
		})
		if lease.GenerationBinding == nil {
			t.Fatal("acquired lease carries no generation binding")
		}
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(lease.Fence), inWindow)
			return err
		}); err != nil {
			t.Fatalf("append under matching binding: %v", err)
		}
		foreign := enrollmentEntry(lease.Fence)
		foreign.AuthStoreVolume = "other-store"
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, foreign, inWindow)
			return err
		})
		if !errors.Is(err, ErrEnrollmentLeaseBindingMismatch) {
			t.Fatalf("append against another store = %v, want %v", err, ErrEnrollmentLeaseBindingMismatch)
		}
	})

	t.Run("unbound fence refuses", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		acquire(t, s, nil)
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(1), inWindow)
			return err
		})
		if !errors.Is(err, ErrEnrollmentLeaseBindingMismatch) {
			t.Fatalf("append under an unbound fence = %v, want %v", err, ErrEnrollmentLeaseBindingMismatch)
		}
	})

	t.Run("binding not naming the current generation refuses", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		// Fabricated coordinates: the fence claims to start from generation 3
		// of a store whose history holds no generation at all.
		acquire(t, s, &domain.LeaseGenerationBinding{
			EnrollmentID: "enroll-1", Generation: 3,
			AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
		})
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(1), inWindow)
			return err
		})
		if !errors.Is(err, ErrEnrollmentLeaseBindingStale) {
			t.Fatalf("append from a fabricated generation = %v, want %v",
				err, ErrEnrollmentLeaseBindingStale)
		}
	})

	t.Run("binding naming a superseded manifest refuses", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		acquire(t, s, &domain.LeaseGenerationBinding{
			EnrollmentID: "enroll-1", Generation: 0,
			AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
		})
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(1), inWindow)
			return err
		}); err != nil {
			t.Fatalf("bootstrap append: %v", err)
		}
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			return tx.ReleaseAuthStoreMutationLease(
				ctx, "auth-1", "inv-refresh", 1, inWindow.Add(time.Minute))
		}); err != nil {
			t.Fatalf("release lease: %v", err)
		}
		// The right ordinal with the wrong bytes: the manifest names a store
		// state generation 1 did not record.
		if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AcquireAuthStoreMutationLeaseBound(
				ctx, "auth-1", "inv-refresh", &domain.LeaseGenerationBinding{
					EnrollmentID: "enroll-1", Generation: 1,
					AuthStoreVolume: "codex-store", StoreManifestDigest: staleManifest,
				}, inWindow.Add(2*time.Minute), leaseEnd)
			return err
		}); err != nil {
			t.Fatalf("re-acquire lease: %v", err)
		}
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, enrollmentEntry(2), inWindow.Add(3*time.Minute))
			return err
		})
		if !errors.Is(err, ErrEnrollmentLeaseBindingStale) {
			t.Fatalf("append from superseded bytes = %v, want %v",
				err, ErrEnrollmentLeaseBindingStale)
		}
	})

	t.Run("expiry the method cannot observe refuses", func(t *testing.T) {
		s := openEnrollmentStore(t)
		recordEnrollmentFixtures(t, s)
		acquire(t, s, &domain.LeaseGenerationBinding{
			EnrollmentID: "enroll-1", Generation: 0,
			AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
		})
		entry := enrollmentEntry(1)
		entry.TokenExpiry = nil
		err := s.WriteInternal(ctx, func(tx *InternalTx) error {
			_, err := tx.AppendEnrollmentGeneration(ctx, entry, inWindow)
			return err
		})
		if !errors.Is(err, domain.ErrGenerationExpiryInconsistent) {
			t.Fatalf("append without observable expiry = %v, want %v",
				err, domain.ErrGenerationExpiryInconsistent)
		}
	})
}

// TestBoundLeaseSurvivesRenewal pins that a renewal extends the window
// without dropping or rewriting the fence's generation binding.
func TestBoundLeaseSurvivesRenewal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openEnrollmentStore(t)
	recordEnrollmentFixtures(t, s)
	start := time.Date(2026, 1, 2, 3, 2, 0, 0, time.UTC)
	binding := &domain.LeaseGenerationBinding{
		EnrollmentID: "enroll-1", Generation: 1,
		AuthStoreVolume: "codex-store", StoreManifestDigest: enrollmentManifest,
	}
	if err := s.WriteInternal(ctx, func(tx *InternalTx) error {
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(
			ctx, "auth-1", "inv-refresh", binding, start, start.Add(5*time.Minute))
		if err != nil {
			return err
		}
		renewed, err := tx.RenewAuthStoreMutationLease(
			ctx, "auth-1", "inv-refresh", lease.Fence,
			start.Add(time.Minute), start.Add(10*time.Minute))
		if err != nil {
			return err
		}
		if renewed.GenerationBinding == nil || *renewed.GenerationBinding != *binding {
			return fmt.Errorf("renewed lease binding = %+v, want %+v",
				renewed.GenerationBinding, binding)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var got domain.AuthStoreMutationLease
	if err := s.Read(ctx, func(tx *ReadTx) error {
		var err error
		got, err = tx.GetAuthStoreMutationLease(ctx, "auth-1")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.GenerationBinding == nil || *got.GenerationBinding != *binding {
		t.Fatalf("reconstructed lease binding = %+v, want %+v", got.GenerationBinding, binding)
	}
}
