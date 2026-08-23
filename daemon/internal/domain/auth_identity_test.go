package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func authIdentity() domain.AuthIdentity {
	return domain.AuthIdentity{
		ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
		MaxParallelExecutions: 1,
		Interim:               domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
	}
}

func TestAuthIdentityValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.AuthIdentity)
		wantErr error
	}{
		{"valid", func(*domain.AuthIdentity) {}, nil},
		{"no id", func(i *domain.AuthIdentity) { i.ID = "" }, domain.ErrEmptyID},
		{"no provider", func(i *domain.AuthIdentity) { i.Provider = "" }, domain.ErrEmptyField},
		{"leased without auth-store volume", func(i *domain.AuthIdentity) {
			i.Interim.AuthStoreVolume = ""
		}, domain.ErrEmptyField},
		{"zero parallelism", func(i *domain.AuthIdentity) { i.MaxParallelExecutions = 0 }, domain.ErrNonPositive},
		{"unknown refresh strategy", func(i *domain.AuthIdentity) {
			i.Interim.RefreshStrategy = domain.RefreshStrategy("magic")
		}, domain.ErrInvalidRefreshStrategy},
		{"zero refresh strategy", func(i *domain.AuthIdentity) {
			i.Interim.RefreshStrategy = ""
		}, domain.ErrInvalidRefreshStrategy},
		{"negative budget", func(i *domain.AuthIdentity) { i.Budget = -1 }, domain.ErrNonPositive},
		{
			// A post-adoption identity carries no interim facts; the lease
			// then binds stores through enrollments, so no volume is required
			// here.
			"leased without interim facts",
			func(i *domain.AuthIdentity) { i.Interim = domain.InterimClientFacts{} },
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			identity := authIdentity()
			tc.mutate(&identity)
			if err := identity.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestValidateAuthIdentityTransition(t *testing.T) {
	old := authIdentity()
	cases := []struct {
		name    string
		mutate  func(*domain.AuthIdentity)
		wantErr error
	}{
		{"unchanged", func(*domain.AuthIdentity) {}, nil},
		{"remeasured parallelism", func(i *domain.AuthIdentity) { i.MaxParallelExecutions = 4 }, nil},
		{"gained snapshot support", func(i *domain.AuthIdentity) { i.Interim.SupportsReadOnlyAuthSnapshot = true }, domain.ErrImmutableTransition},
		{"restrategized refresh", func(i *domain.AuthIdentity) { i.Interim.RefreshStrategy = domain.RefreshExternal }, domain.ErrImmutableTransition},
		{"other identity", func(i *domain.AuthIdentity) { i.ID = "auth-2" }, domain.ErrImmutableTransition},
		{"other provider", func(i *domain.AuthIdentity) { i.Provider = "openai" }, domain.ErrImmutableTransition},
		{"other auth-store volume", func(i *domain.AuthIdentity) {
			i.Interim.AuthStoreVolume = "other-provider-cred"
		}, domain.ErrImmutableTransition},
		{
			// Retiring the serialization point while a holder still believes
			// it holds a lease is the failure this rule exists to refuse.
			"lease requirement dropped",
			func(i *domain.AuthIdentity) { i.AuthStoreMutationLease = false },
			domain.ErrImmutableTransition,
		},
		{"account bound from empty", func(i *domain.AuthIdentity) { i.AccountBinding = "acct-1" }, nil},
		{"usage pool set from empty", func(i *domain.AuthIdentity) { i.UsagePool = "pool-1" }, nil},
		{"disabled by the operator", func(i *domain.AuthIdentity) { i.Enabled = false }, nil},
		{"cost owner reassigned", func(i *domain.AuthIdentity) { i.CostOwner = "finance" }, nil},
		{"budget revised", func(i *domain.AuthIdentity) { i.Budget = 250_000 }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated := old
			tc.mutate(&updated)
			if err := domain.ValidateAuthIdentityTransition(old, updated); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateAuthIdentityTransition = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func lease() domain.AuthStoreMutationLease {
	acquired := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return domain.AuthStoreMutationLease{
		AuthIdentityID: "auth-1", Holder: "inv-1", Fence: 1,
		AcquiredAt: acquired, ExpiresAt: acquired.Add(time.Minute),
	}
}

func TestAuthStoreMutationLeaseValidate(t *testing.T) {
	acquired := lease().AcquiredAt
	earlier := acquired.Add(-time.Second)
	cases := []struct {
		name    string
		mutate  func(*domain.AuthStoreMutationLease)
		wantErr error
	}{
		{"valid", func(*domain.AuthStoreMutationLease) {}, nil},
		{"released", func(l *domain.AuthStoreMutationLease) {
			released := acquired.Add(time.Second)
			l.ReleasedAt = &released
		}, nil},
		{"no identity", func(l *domain.AuthStoreMutationLease) { l.AuthIdentityID = "" }, domain.ErrEmptyID},
		{"no holder", func(l *domain.AuthStoreMutationLease) { l.Holder = "" }, domain.ErrEmptyID},
		{"zero fence", func(l *domain.AuthStoreMutationLease) { l.Fence = 0 }, domain.ErrNonPositive},
		{"no acquired_at", func(l *domain.AuthStoreMutationLease) { l.AcquiredAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"no expires_at", func(l *domain.AuthStoreMutationLease) { l.ExpiresAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"expires at acquisition", func(l *domain.AuthStoreMutationLease) {
			l.ExpiresAt = l.AcquiredAt
		}, domain.ErrTimestampOutOfOrder},
		{"released before acquisition", func(l *domain.AuthStoreMutationLease) {
			l.ReleasedAt = &earlier
		}, domain.ErrTimestampOutOfOrder},
		{
			// A release outside the window it ends is incoherent, and a future
			// one is harmful: acquisition refuses instants preceding the
			// current generation's release, so an imported row carrying one
			// would block takeovers until it passed.
			"released after expiry", func(l *domain.AuthStoreMutationLease) {
				after := l.ExpiresAt.Add(24 * time.Hour)
				l.ReleasedAt = &after
			}, domain.ErrTimestampOutOfOrder,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := lease()
			tc.mutate(&l)
			if err := l.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestAuthStoreMutationLeaseHeldAt pins that liveness comes from the caller's
// clock, never from the row: a record claiming a window is not evidence the
// window is open.
func TestAuthStoreMutationLeaseHeldAt(t *testing.T) {
	l := lease()
	released := l.AcquiredAt.Add(time.Second)
	cases := []struct {
		name string
		now  time.Time
		l    domain.AuthStoreMutationLease
		want bool
	}{
		{"inside the window", l.AcquiredAt.Add(30 * time.Second), l, true},
		{"at acquisition", l.AcquiredAt, l, true},
		{
			// A regressed or stale clock names an instant the generation did
			// not exist at; holding there would let it be renewed without a
			// takeover.
			"before acquisition", l.AcquiredAt.Add(-time.Second), l, false,
		},
		{"at expiry", l.ExpiresAt, l, false},
		{"after expiry", l.ExpiresAt.Add(time.Nanosecond), l, false},
		{
			"released early",
			l.AcquiredAt.Add(30 * time.Second),
			func() domain.AuthStoreMutationLease { r := l; r.ReleasedAt = &released; return r }(),
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.l.HeldAt(tc.now); got != tc.want {
				t.Fatalf("HeldAt(%s) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestAuthIdentitySetOnceBindings(t *testing.T) {
	bound := authIdentity()
	bound.AccountBinding = "acct-1"
	bound.UsagePool = "pool-1"
	cases := []struct {
		name    string
		mutate  func(*domain.AuthIdentity)
		wantErr error
	}{
		{"unchanged", func(*domain.AuthIdentity) {}, nil},
		{
			// Rebinding would re-home recorded usage onto a different
			// account; the binding is set once, never moved.
			"account rebound",
			func(i *domain.AuthIdentity) { i.AccountBinding = "acct-2" },
			domain.ErrImmutableTransition,
		},
		{
			"account cleared",
			func(i *domain.AuthIdentity) { i.AccountBinding = "" },
			domain.ErrImmutableTransition,
		},
		{
			"usage pool moved",
			func(i *domain.AuthIdentity) { i.UsagePool = "pool-2" },
			domain.ErrImmutableTransition,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			updated := bound
			tc.mutate(&updated)
			if err := domain.ValidateAuthIdentityTransition(bound, updated); !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateAuthIdentityTransition = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLeaseGenerationBindingValidate(t *testing.T) {
	valid := domain.LeaseGenerationBinding{
		EnrollmentID: "enroll-1", Generation: 2, AuthStoreVolume: "store-1",
		StoreManifestDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	cases := []struct {
		name    string
		mutate  func(*domain.LeaseGenerationBinding)
		wantErr error
	}{
		{"valid", func(*domain.LeaseGenerationBinding) {}, nil},
		{"no enrollment", func(b *domain.LeaseGenerationBinding) { b.EnrollmentID = "" }, domain.ErrEmptyID},
		{"bootstrap generation", func(b *domain.LeaseGenerationBinding) { b.Generation = 0 }, nil},
		{"negative generation", func(b *domain.LeaseGenerationBinding) { b.Generation = -1 }, domain.ErrNonPositive},
		{"no volume", func(b *domain.LeaseGenerationBinding) { b.AuthStoreVolume = "" }, domain.ErrEmptyField},
		{"malformed digest", func(b *domain.LeaseGenerationBinding) { b.StoreManifestDigest = "manifest-1" }, domain.ErrInvalidDigest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binding := valid
			tc.mutate(&binding)
			if err := binding.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
			l := lease()
			l.GenerationBinding = &binding
			if err := l.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("lease Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
