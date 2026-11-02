package ward

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// UsageCollector asks a provider what it reports about a usage pool's
// allowance under one enrollment's own credential (plan §10, usage collection
// rules). The lifecycle in internal/usagecollect calls it outside any store
// transaction and decides what reaches the record. An implementation reads
// only: it never redeems a reset credit, refreshes a token, or writes the auth
// store, and it never answers for an enrollment other than the one asked.
type UsageCollector interface {
	// CollectUsage returns the provider's full report. An error means the
	// attempt failed and says nothing about the windows; a *UsageThrottleError
	// also says when the provider allows the next attempt.
	CollectUsage(ctx context.Context, request UsageCollectionRequest) (UsageCollection, error)
}

// UsageCollectionRequest names the collector enrollment and the generation
// read just before the call. Every reading the collector returns is stamped
// with both, so a reading taken under a generation replaced during the call is
// discarded at write.
type UsageCollectionRequest struct {
	Enrollment domain.ClientEnrollment
	Generation int
}

// UsageCollection is one full report. A full report replaces the pool's
// window set, so a window it doesn't name is dropped.
type UsageCollection struct {
	// Availability is supported or unsupported; a failed attempt is an error,
	// and a failed answer here is treated as one.
	Availability domain.AvailabilityAnswer
	// Plan is the provider's plan name where known.
	Plan *string
	// Windows holds one reading per window the provider reported, each
	// stamped with the request's enrollment and generation.
	Windows      []domain.WindowReading
	ModelWindows []domain.ModelWindow
	// Detail is a short human-readable note on the answer.
	Detail string
}

// UsageThrottleError is a failed attempt the provider throttled. The lifecycle
// waits at least RetryAfter before the next attempt.
type UsageThrottleError struct {
	RetryAfter time.Duration
	Detail     string
}

func (e *UsageThrottleError) Error() string {
	return fmt.Sprintf("usage collection throttled for %s: %s", e.RetryAfter, e.Detail)
}
