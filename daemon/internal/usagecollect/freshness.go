package usagecollect

import (
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// Freshness is how a window reading stands at a query time. A stale reading
// is shown as it was observed, never as replenished. The zero value is invalid
// by design.
type Freshness string

const (
	FreshnessFresh Freshness = "fresh"
	// FreshnessStaleReset means the window's reset has passed since the
	// reading: the provider has likely replenished it, but nothing observed so.
	FreshnessStaleReset Freshness = "stale_reset_passed"
	// FreshnessStaleAge means the reading is older than the refresh age.
	FreshnessStaleAge Freshness = "stale_refresh_age"
)

// AllFreshness is the single registration point for freshness values.
var AllFreshness = []Freshness{FreshnessFresh, FreshnessStaleReset, FreshnessStaleAge}

func (f Freshness) valid() bool {
	switch f {
	case FreshnessFresh, FreshnessStaleReset, FreshnessStaleAge:
		return true
	default:
		return false
	}
}

// WindowFreshness is one window reading's freshness, with the reading's own
// times so a stale reading shows when it was true.
type WindowFreshness struct {
	Window     string     `json:"window"`
	Freshness  Freshness  `json:"freshness"`
	ObservedAt time.Time  `json:"observed_at"`
	ResetsAt   *time.Time `json:"resets_at"`
}

// readingFreshness reports a reading stale once its reset has passed or it is
// older than refreshAge; a passed reset is reported first.
func readingFreshness(reading domain.WindowReading, now time.Time, refreshAge time.Duration) Freshness {
	switch {
	case reading.ResetsAt != nil && !reading.ResetsAt.After(now):
		return FreshnessStaleReset
	case now.Sub(reading.ObservedAt) > refreshAge:
		return FreshnessStaleAge
	default:
		return FreshnessFresh
	}
}

// windowsFreshness reports every window reading of record at now, in window
// order.
func windowsFreshness(record domain.PoolUsageObservation, now time.Time, refreshAge time.Duration) []WindowFreshness {
	windows := make([]WindowFreshness, 0, len(record.Windows))
	for _, reading := range record.Windows {
		windows = append(windows, WindowFreshness{
			Window: reading.Window, Freshness: readingFreshness(reading, now, refreshAge),
			ObservedAt: reading.ObservedAt, ResetsAt: reading.ResetsAt,
		})
	}
	return windows
}
