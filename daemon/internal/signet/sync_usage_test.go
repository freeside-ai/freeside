package signet_test

import (
	"context"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestBootstrapProjectsUsagePoolsFromTheStore drives the usage projection
// through a real store: a pool before its first record carries version 0 and
// its default collector, the record's write raises the revision the heartbeat
// reads and stamps the record with it, and the routing policy rides beside it.
func TestBootstrapProjectsUsagePoolsFromTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	service := signet.NewService(st,
		signet.WithClock(func() time.Time { return now }), signet.WithUsageRefreshAge(30*time.Minute))
	generation := seedUsageEnrollment(t, st, now.Add(-time.Hour))

	before, err := service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	if before.RoutingPolicy != nil {
		t.Fatalf("routing policy before any set = %+v, want nil", before.RoutingPolicy)
	}
	if len(before.UsagePools) != 2 {
		t.Fatalf("usage pools = %+v, want pool-a and pool-idle", before.UsagePools)
	}
	unrecorded, idle := before.UsagePools[0], before.UsagePools[1]
	if unrecorded.Pool.ID != "pool-a" || unrecorded.EntityVersion != 0 ||
		unrecorded.AsOfRevision != before.Revision ||
		unrecorded.Pool.Collector == nil || *unrecorded.Pool.Collector != "enrollment-usage-1" ||
		len(unrecorded.Pool.Enrollments) != 1 || unrecorded.Pool.Enrollments[0].Availability != nil {
		t.Fatalf("unrecorded pool = %+v", unrecorded)
	}
	if idle.Pool.ID != "pool-idle" || idle.Pool.Collector != nil || len(idle.Pool.Enrollments) != 0 {
		t.Fatalf("pool with no enrollment = %+v", idle)
	}

	observedAt := now.Add(-10 * time.Minute)
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordPoolUsageObservation(ctx, domain.PoolUsageObservation{
			Pool: "pool-a", Collector: "enrollment-usage-1",
			LastAttempt: &domain.CollectionAttempt{Status: domain.CollectionAttemptSucceeded, At: observedAt},
			Enrollments: []domain.EnrollmentAvailability{{
				EnrollmentID: "enrollment-usage-1", Answer: domain.AvailabilitySupported, ObservedAt: observedAt,
			}},
			Windows: []domain.WindowReading{{
				Window: "five_hour", EnrollmentID: "enrollment-usage-1", Generation: generation,
				Status: domain.WindowStatusAllowed, ObservedAt: observedAt,
			}},
		}, observedAt)
	}); err != nil {
		t.Fatalf("record observation: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.AppendRoutingPolicy(ctx, domain.RoutingPolicyInput{
			Roles: []domain.RoutingPolicyRole{{
				Role: domain.RoleImplementer, Enrollments: []domain.ClientEnrollmentID{"enrollment-usage-1"},
			}},
			Source: domain.RoutingPolicySourceClientSet, SetBy: "device:device-1",
			LineupRevision: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		}, now)
		return err
	}); err != nil {
		t.Fatalf("append routing policy: %v", err)
	}

	after, err := service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	heartbeat, err := service.Revision(ctx)
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if after.Revision != before.Revision+2 || heartbeat.Revision != after.Revision {
		t.Fatalf("revision %d -> %d (heartbeat %d), want two client-visible writes",
			before.Revision, after.Revision, heartbeat.Revision)
	}
	recorded := after.UsagePools[0]
	if recorded.EntityVersion != 1 || recorded.AsOfRevision != before.Revision+1 {
		t.Fatalf("recorded pool snapshot = rev %d version %d, want rev %d version 1",
			recorded.AsOfRevision, recorded.EntityVersion, before.Revision+1)
	}
	if len(recorded.Pool.Windows) != 1 || recorded.Pool.Windows[0].StaleAt == nil ||
		!recorded.Pool.Windows[0].StaleAt.Equal(observedAt.Add(30*time.Minute)) {
		t.Fatalf("recorded windows = %+v, want stale_at at the refresh age", recorded.Pool.Windows)
	}
	if answer := recorded.Pool.Enrollments[0].Availability; answer == nil || answer.Answer != domain.AvailabilitySupported {
		t.Fatalf("recorded availability = %+v", answer)
	}
	policy := after.RoutingPolicy
	if policy == nil || policy.EntityVersion != 1 || policy.AsOfRevision != after.Revision ||
		policy.Policy.Source != domain.RoutingPolicySourceClientSet || len(policy.Policy.Roles) != 1 ||
		!slices.Equal(policy.Policy.Roles[0].Enrollments, []domain.ClientEnrollmentID{"enrollment-usage-1"}) ||
		policy.Policy.Roles[0].Agents != nil {
		t.Fatalf("routing policy = %+v", policy)
	}
}

// TestBootstrapExhaustedMarksHoldBetweenRevisions reads one revision on both
// sides of a reset. A window holds an older refusal resetting at T2 and a newer
// exhausted reading resetting at T1 < T2. Passing T1 writes nothing, so the
// projected marks must not change: both stay, and the client lapses each by
// its own resets_at.
func TestBootstrapExhaustedMarksHoldBetweenRevisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	refusedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	observedAt := refusedAt.Add(10 * time.Minute)
	readingReset, refusalReset := refusedAt.Add(2*time.Hour), refusedAt.Add(3*time.Hour)
	now := refusedAt.Add(time.Hour)
	service := signet.NewService(st, signet.WithClock(func() time.Time { return now }))
	generation := seedUsageEnrollment(t, st, refusedAt.Add(-time.Hour))

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordWindowRefusalMark(ctx, "pool-a", domain.RefusalMark{
			Window: "five_hour", InvocationID: "inv-refused", ResetsAt: &refusalReset, SetAt: refusedAt,
		}, refusedAt)
	}); err != nil {
		t.Fatalf("record refusal: %v", err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		observation, err := tx.GetPoolUsageObservation(ctx, "pool-a")
		if err != nil {
			return err
		}
		observation.LastAttempt = &domain.CollectionAttempt{
			Status: domain.CollectionAttemptSucceeded, At: observedAt,
		}
		observation.Windows = []domain.WindowReading{{
			Window: "five_hour", EnrollmentID: "enrollment-usage-1", Generation: generation,
			Status: domain.WindowStatusExhausted, ResetsAt: &readingReset, ObservedAt: observedAt,
		}}
		return tx.RecordPoolUsageObservation(ctx, observation, observedAt)
	}); err != nil {
		t.Fatalf("record reading: %v", err)
	}

	before, err := service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap before T1: %v", err)
	}
	now = readingReset.Add(30 * time.Minute)
	after, err := service.Bootstrap(ctx)
	if err != nil {
		t.Fatalf("Bootstrap after T1: %v", err)
	}

	if before.Revision != after.Revision {
		t.Fatalf("revision %d -> %d, want one revision", before.Revision, after.Revision)
	}
	beforePool, afterPool := before.UsagePools[0], after.UsagePools[0]
	if beforePool.EntityVersion != afterPool.EntityVersion {
		t.Fatalf("entity version %d -> %d, want one", beforePool.EntityVersion, afterPool.EntityVersion)
	}
	if !reflect.DeepEqual(beforePool.Pool.RecordedExhaustedMarks, afterPool.Pool.RecordedExhaustedMarks) {
		t.Fatalf("marks moved at one revision:\nbefore %+v\nafter  %+v",
			beforePool.Pool.RecordedExhaustedMarks, afterPool.Pool.RecordedExhaustedMarks)
	}
	marks := afterPool.Pool.RecordedExhaustedMarks
	if len(marks) != 2 ||
		marks[0].Source != domain.ExhaustedMarkProviderRefusal || !marks[0].ResetsAt.Equal(refusalReset) ||
		marks[1].Source != domain.ExhaustedMarkUsageReading || !marks[1].ResetsAt.Equal(readingReset) {
		t.Fatalf("marks = %+v, want the refusal then the reading", marks)
	}
}

// TestHeartbeatRevisionCoversPoolAndEnrollmentWrites: a pool limit change and
// a new enrollment each change what usage_pools carries, so each raises the
// heartbeat revision, and the bootstrap at that revision shows the change. A
// client holding the earlier revision therefore refreshes (plan §5.14).
func TestHeartbeatRevisionCoversPoolAndEnrollmentWrites(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
	})
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	service := signet.NewService(st, signet.WithClock(func() time.Time { return now }))
	seedUsageEnrollment(t, st, now.Add(-time.Hour))
	heartbeat := func() int64 {
		t.Helper()
		state, err := service.Revision(ctx)
		if err != nil {
			t.Fatalf("Revision: %v", err)
		}
		return state.Revision
	}
	// poolA reads pool-a from a bootstrap taken at the heartbeat's revision.
	poolA := func(want int64) signet.UsagePoolUsage {
		t.Helper()
		bootstrap, err := service.Bootstrap(ctx)
		if err != nil {
			t.Fatalf("Bootstrap: %v", err)
		}
		if bootstrap.Revision != want {
			t.Fatalf("bootstrap revision = %d, want %d", bootstrap.Revision, want)
		}
		for _, snapshot := range bootstrap.UsagePools {
			if snapshot.Pool.ID == "pool-a" {
				return snapshot.Pool
			}
		}
		t.Fatalf("usage pools = %+v, want pool-a", bootstrap.UsagePools)
		return signet.UsagePoolUsage{}
	}
	start := heartbeat()
	if pool := poolA(start); pool.MaxParallelExecutions != 1 || len(pool.Enrollments) != 1 {
		t.Fatalf("seeded pool-a = %+v, want limit 1 and one enrollment", pool)
	}

	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordUsagePool(ctx, domain.UsagePool{
			ID: "pool-a", Provider: "openai", MaxParallelExecutions: 3,
		}, now)
	}); err != nil {
		t.Fatalf("set pool-a limit: %v", err)
	}
	if got := heartbeat(); got != start+1 {
		t.Fatalf("heartbeat after limit change = %d, want %d", got, start+1)
	}
	if pool := poolA(start + 1); pool.MaxParallelExecutions != 3 {
		t.Fatalf("pool-a after limit change = %+v, want limit 3", pool)
	}

	second := domain.AuthIdentity{
		ID: "auth-usage-2", Provider: "openai", AccountBinding: "acct-usage-2",
		UsagePool: "pool-a", AuthStoreMutationLease: true, Enabled: true,
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordAuthIdentity(ctx, second, now); err != nil {
			return err
		}
		return tx.RecordClientEnrollment(ctx, domain.ClientEnrollment{
			ID: "enrollment-usage-2", AuthIdentityID: second.ID,
			HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
			AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
			RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
			AccountBinding: second.AccountBinding,
		}, now)
	}); err != nil {
		t.Fatalf("enroll a second identity on pool-a: %v", err)
	}
	if got := heartbeat(); got != start+2 {
		t.Fatalf("heartbeat after enrollment = %d, want %d", got, start+2)
	}
	pool := poolA(start + 2)
	if len(pool.Enrollments) != 2 || pool.Enrollments[1].EnrollmentID != "enrollment-usage-2" {
		t.Fatalf("pool-a enrollments after enrollment = %+v, want enrollment-usage-2 added", pool.Enrollments)
	}
}

// seedUsageEnrollment records an identity on pool-a with one enrollment at its
// first generation, and a declared pool-idle no identity meters; it returns
// the generation a reading must name.
func seedUsageEnrollment(t *testing.T, st *store.Store, at time.Time) int {
	t.Helper()
	ctx := context.Background()
	identity := domain.AuthIdentity{
		ID: "auth-usage-1", Provider: "openai", AccountBinding: "acct-usage-1",
		UsagePool: "pool-a", AuthStoreMutationLease: true, Enabled: true,
	}
	enrollment := domain.ClientEnrollment{
		ID: "enrollment-usage-1", AuthIdentityID: identity.ID,
		HarnessClient: domain.HarnessClientCodexCLI, Route: "openai_chatgpt_codex",
		AuthMethod: domain.AuthMethodOAuth, CredentialMode: domain.CredentialSubscriptionContained,
		RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
		AccountBinding: identity.AccountBinding,
	}
	manifest := domain.Digest("sha256:" + strings.Repeat("e", 64))
	expiry := at.Add(24 * time.Hour)
	var generation domain.EnrollmentGeneration
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.RecordUsagePool(ctx, domain.UsagePool{
			ID: "pool-idle", Provider: domain.AuthProviderClaude, MaxParallelExecutions: 1,
		}, at); err != nil {
			return err
		}
		if err := tx.RecordAuthIdentity(ctx, identity, at); err != nil {
			return err
		}
		if err := tx.RecordClientEnrollment(ctx, enrollment, at.Add(time.Minute)); err != nil {
			return err
		}
		lease, err := tx.AcquireAuthStoreMutationLeaseBound(ctx, identity.ID, "inv-store-usage-1",
			&domain.LeaseGenerationBinding{
				EnrollmentID: enrollment.ID, Generation: 0,
				AuthStoreVolume: "codex-usage-store", StoreManifestDigest: manifest,
			}, at.Add(2*time.Minute), at.Add(10*time.Minute))
		if err != nil {
			return err
		}
		generation, err = tx.AppendEnrollmentGeneration(ctx, domain.EnrollmentGeneration{
			EnrollmentID: enrollment.ID, AuthStoreVolume: "codex-usage-store",
			StoreManifestDigest: manifest, LeaseFence: lease.Fence,
			AccountBinding: identity.AccountBinding, TokenExpiry: &expiry, RecordedAt: at.Add(3 * time.Minute),
		}, at.Add(3*time.Minute))
		if err != nil {
			return err
		}
		return tx.ReleaseAuthStoreMutationLease(ctx, identity.ID, "inv-store-usage-1", lease.Fence, at.Add(4*time.Minute))
	}); err != nil {
		t.Fatalf("seed enrollment: %v", err)
	}
	return generation.Ordinal
}
