package domain

import "fmt"

// UsagePoolID names one provider quota pool. A characterized identity carries
// it in AuthIdentity.UsagePool; EffectiveUsagePool derives one for an identity
// that has none yet.
type UsagePoolID string

// UsagePool is the provider quota pool execution limits count against (plan
// §5.4, revision 72). The provider enforces quota and rate limits on the pool,
// not on one credential, so two identities on one pool share one limit. Each
// daemon instance counts only its own slots: instances that draw on one pool
// split its limit between them.
type UsagePool struct {
	ID       UsagePoolID `json:"id"`
	Provider string      `json:"provider"`
	// MaxParallelExecutions is how many ward launches may hold a slot on this
	// pool at once. It is at least one, since a pool that can run nothing
	// would hold its work forever.
	MaxParallelExecutions int `json:"max_parallel_executions"`
}

// Validate reports whether the pool declaration is well-formed.
func (p UsagePool) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("usage pool id: %w", ErrEmptyID)
	}
	if p.Provider == "" {
		return fmt.Errorf("usage pool %s provider: %w", p.ID, ErrEmptyField)
	}
	if p.MaxParallelExecutions < 1 {
		return fmt.Errorf("usage pool %s max_parallel_executions %d: %w",
			p.ID, p.MaxParallelExecutions, ErrNonPositive)
	}
	return nil
}

// UsagePoolExhaustedError is an ErrUsagePoolExhausted refusal with the fact the
// run's usage_pool_exhausted hold records: which pool is full.
type UsagePoolExhaustedError struct {
	Pool   UsagePoolID
	Active int
	Limit  int
}

func (e *UsagePoolExhaustedError) Error() string {
	return fmt.Sprintf("usage pool %q has %d active launches at limit %d: %s",
		e.Pool, e.Active, e.Limit, ErrUsagePoolExhausted)
}

// Is matches ErrUsagePoolExhausted.
func (e *UsagePoolExhaustedError) Is(target error) bool { return target == ErrUsagePoolExhausted }

// ValidateUsagePoolTransition reports whether updated is a legal successor to
// the stored pool old. A pool's id and provider are fixed; its limit may
// change freely.
func ValidateUsagePoolTransition(old, updated UsagePool) error {
	if updated.ID != old.ID {
		return fmt.Errorf("usage pool %s: id would change from %s: %w",
			updated.ID, old.ID, ErrImmutableTransition)
	}
	if updated.Provider != old.Provider {
		return fmt.Errorf("usage pool %s: provider would change from %q: %w",
			updated.ID, old.Provider, ErrImmutableTransition)
	}
	return nil
}
