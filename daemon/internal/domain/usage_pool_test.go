package domain_test

import (
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestEffectiveUsagePool(t *testing.T) {
	t.Parallel()
	identity := domain.AuthIdentity{ID: "auth-1"}
	if got := identity.EffectiveUsagePool(); got != "identity:auth-1" {
		t.Fatalf("no-pool identity = %q, want its own pool", got)
	}
	identity.UsagePool = "pool-shared"
	if got := identity.EffectiveUsagePool(); got != "pool-shared" {
		t.Fatalf("characterized identity = %q, want its pool", got)
	}
}

func TestUsagePoolValidate(t *testing.T) {
	t.Parallel()
	valid := domain.UsagePool{ID: "pool-1", Provider: "claude", MaxParallelExecutions: 1}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid pool: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate  func(*domain.UsagePool)
		wantErr error
	}{
		"no id":       {func(p *domain.UsagePool) { p.ID = "" }, domain.ErrEmptyID},
		"no provider": {func(p *domain.UsagePool) { p.Provider = "" }, domain.ErrEmptyField},
		"zero limit":  {func(p *domain.UsagePool) { p.MaxParallelExecutions = 0 }, domain.ErrNonPositive},
	} {
		pool := valid
		tc.mutate(&pool)
		if err := pool.Validate(); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: %v, want %v", name, err, tc.wantErr)
		}
	}
}

func TestValidateUsagePoolTransition(t *testing.T) {
	t.Parallel()
	old := domain.UsagePool{ID: "pool-1", Provider: "claude", MaxParallelExecutions: 2}
	for name, tc := range map[string]struct {
		mutate  func(*domain.UsagePool)
		wantErr error
	}{
		"raised limit":   {func(p *domain.UsagePool) { p.MaxParallelExecutions = 8 }, nil},
		"lowered limit":  {func(p *domain.UsagePool) { p.MaxParallelExecutions = 1 }, nil},
		"other pool":     {func(p *domain.UsagePool) { p.ID = "pool-2" }, domain.ErrImmutableTransition},
		"other provider": {func(p *domain.UsagePool) { p.Provider = "openai" }, domain.ErrImmutableTransition},
	} {
		updated := old
		tc.mutate(&updated)
		if err := domain.ValidateUsagePoolTransition(old, updated); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: %v, want %v", name, err, tc.wantErr)
		}
	}
}

func TestUsagePoolExhaustedErrorMatchesItsSentinel(t *testing.T) {
	t.Parallel()
	var err error = &domain.UsagePoolExhaustedError{Pool: "pool-1", Active: 2, Limit: 2}
	if !errors.Is(err, domain.ErrUsagePoolExhausted) {
		t.Fatalf("%v does not match its sentinel", err)
	}
}

// TestRunHoldUsagePoolPairsWithItsReason holds the pool to its reason in both
// directions: a pool hold that names none can't say what is full, and a pool
// beside another reason isn't a fact that reason carries. A stored
// identity_parallelism hold has no pool and stays valid.
func TestRunHoldUsagePoolPairsWithItsReason(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	pool, empty := domain.UsagePoolID("pool-1"), domain.UsagePoolID("")
	for name, tc := range map[string]struct {
		reason  domain.RunHoldReason
		pool    *domain.UsagePoolID
		wantErr error
	}{
		"pool hold":           {domain.HoldUsagePoolExhausted, &pool, nil},
		"stored identity":     {domain.HoldIdentityParallelism, nil, nil},
		"pool hold, no pool":  {domain.HoldUsagePoolExhausted, nil, domain.ErrHoldDetailMismatch},
		"pool hold, empty":    {domain.HoldUsagePoolExhausted, &empty, domain.ErrEmptyID},
		"pool, another cause": {domain.HoldIdentityParallelism, &pool, domain.ErrHoldDetailMismatch},
	} {
		hold := domain.RunHoldObservation{
			RunID: "run-1", Reason: tc.reason, UsagePool: tc.pool,
			FirstObservedAt: at, LastObservedAt: at,
		}
		if err := hold.Validate(); !errors.Is(err, tc.wantErr) {
			t.Errorf("%s: %v, want %v", name, err, tc.wantErr)
		}
	}
}
