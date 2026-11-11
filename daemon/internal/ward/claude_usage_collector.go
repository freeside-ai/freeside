package ward

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// ClaudeUsageCollector answers a Claude pool's usage collection from the usage
// answer its collector enrollment's last writer turn reported (plan §10, usage
// collection rules). Under a setup token the only read that carries the share
// is the writer turn's own get_usage answer
// (devlog/2026-11-03-1457-claude-usage-share-spike.md), so this collector makes
// no request of its own: it opens no connection, starts no process, and reads
// no credential. It answers from memory, which a daemon restart empties.
//
// It holds at most one capture per enrollment: the newest under the highest
// generation recorded. Captures come only from the engine's terminal
// collection of a writer turn admitted under that enrollment and generation.
type ClaudeUsageCollector struct {
	mu   sync.Mutex
	held map[domain.ClientEnrollmentID]heldClaudeUsage
}

type heldClaudeUsage struct {
	generation int
	capture    exec.ClaudeUsageCapture
}

// claudeUsageWindowLengths are the windows the pinned CLI (2.1.319) names and
// their lengths. The router reads a seven-day length as weekly
// (engine.weeklyWindowLength). A name outside this table is left out of the
// answer, since its length is unknown.
var claudeUsageWindowLengths = map[string]time.Duration{
	"five_hour":       5 * time.Hour,
	"seven_day":       7 * 24 * time.Hour,
	claudeFableWindow: 7 * 24 * time.Hour,
}

// claudeFableWindow is exec.ClaudeFableWindow, the Fable weekly bucket. It is
// spelled here because the collector names only the capture's types from
// internal/exec (TestClaudeUsageCollectorMakesNoRequest); a test pins the two
// equal.
const claudeFableWindow = "seven_day_overage_included"

// claudeFableRouteModelIDs are the route model ids the Fable window limits:
// the CLI's alias and every Fable model id it accepts. Every collection maps
// the window to each, whether or not this turn reported it, so its reading or
// a refusal mark on it ranks and holds only Fable work. Unmapped, the record
// would treat it as general (domain.ModelWindow) and an exhausted Fable bucket
// would hold every model on the pool. An id missing
// here is not held by the window; the provider's refusal still parks its
// attempt (devlog/2026-11-10-1830-claude-fable-window.md).
var claudeFableRouteModelIDs = []string{"claude-fable-5", "claude-fable-5-1", "claude-fable-5-2", "fable"}

// NewClaudeUsageCollector returns a collector with no captures.
func NewClaudeUsageCollector() *ClaudeUsageCollector {
	return &ClaudeUsageCollector{held: map[domain.ClientEnrollmentID]heldClaudeUsage{}}
}

// Record holds a writer turn's capture for the enrollment and generation it
// ran under, and reports whether it was kept. It drops an invalid capture, one
// under a generation lower than the one held, and one no newer than the
// capture held for the same generation, so a replayed terminal collection
// can't roll a reading back. A capture under a higher generation replaces the
// held one.
//
// It also drops a capture with no get_usage answer, which carries rate-limit
// events alone: a specification turn, which sends no get_usage, or a writer
// turn whose answer was refused or null. A collection is a full report that
// replaces the pool's window set, so answering from events alone would erase
// the weekly reading and every share the last answer gave.
func (c *ClaudeUsageCollector) Record(
	enrollment domain.ClientEnrollmentID, generation int, capture exec.ClaudeUsageCapture,
) bool {
	if enrollment == "" || generation < 1 || len(capture.Windows) == 0 || capture.Validate() != nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if held, ok := c.held[enrollment]; ok {
		if held.generation > generation ||
			(held.generation == generation && !capture.ObservedAt.After(held.capture.ObservedAt)) {
			return false
		}
	}
	c.held[enrollment] = heldClaudeUsage{generation: generation, capture: capture}
	return true
}

// CollectUsage answers from the capture held for the request's enrollment and
// generation. With none, the attempt fails, which keeps the pool's stored
// readings.
func (c *ClaudeUsageCollector) CollectUsage(_ context.Context, request UsageCollectionRequest) (UsageCollection, error) {
	c.mu.Lock()
	held, ok := c.held[request.Enrollment.ID]
	c.mu.Unlock()
	if !ok || held.generation != request.Generation {
		return UsageCollection{}, fmt.Errorf(
			"no writer turn has reported usage under enrollment %s generation %d since the daemon started",
			request.Enrollment.ID, request.Generation)
	}
	return claudeUsageCollection(held.capture, request.Enrollment.ID, request.Generation)
}

// claudeUsageCollection maps a capture to a full report: one reading per
// window in claudeUsageWindowLengths that the get_usage answer or a
// rate-limit event names, and the Fable window's model scope. An event's status sets the reading's status and its
// reset wins over the answer's; a window with no event reads allowed, and one
// the answer leaves out but an event names carries no share. Unknown windows and event statuses
// are left out and named in the detail. A capture that yields no reading
// fails.
func claudeUsageCollection(
	capture exec.ClaudeUsageCapture, enrollment domain.ClientEnrollmentID, generation int,
) (UsageCollection, error) {
	readings := map[string]*domain.WindowReading{}
	reading := func(window string) *domain.WindowReading {
		if held := readings[window]; held != nil {
			return held
		}
		length := claudeUsageWindowLengths[window]
		held := &domain.WindowReading{
			Window: window, EnrollmentID: enrollment, Generation: generation,
			Status: domain.WindowStatusAllowed, Length: &length, ObservedAt: capture.ObservedAt,
		}
		readings[window] = held
		return held
	}
	var unknownWindows, ignoredEvents []string
	for _, window := range capture.Windows {
		if _, known := claudeUsageWindowLengths[window.Window]; !known {
			unknownWindows = append(unknownWindows, window.Window)
			continue
		}
		held := reading(window.Window)
		used := window.UsedPercent
		held.UsedPercent = &used
		held.ResetsAt = cloneTime(window.ResetsAt)
	}
	for _, event := range capture.Events {
		if _, known := claudeUsageWindowLengths[event.Window]; !known {
			unknownWindows = append(unknownWindows, event.Window)
			continue
		}
		status, known := claudeEventStatus(event.Status)
		if !known {
			ignoredEvents = append(ignoredEvents, event.Window+" status "+event.Status)
			continue
		}
		held := reading(event.Window)
		held.Status = status
		if event.ResetsAt != nil {
			held.ResetsAt = cloneTime(event.ResetsAt)
		}
	}
	slices.Sort(unknownWindows)
	unknownWindows = slices.Compact(unknownWindows)
	var notes []string
	if len(unknownWindows) > 0 {
		notes = append(notes, "left out unknown windows "+strings.Join(unknownWindows, ", "))
	}
	if len(ignoredEvents) > 0 {
		notes = append(notes, "ignored rate-limit events "+strings.Join(ignoredEvents, ", "))
	}
	if len(readings) == 0 {
		return UsageCollection{}, fmt.Errorf("writer turn's usage answer named no known window: %s",
			strings.Join(notes, "; "))
	}
	windows := make([]domain.WindowReading, 0, len(readings))
	for _, window := range slices.Sorted(maps.Keys(readings)) {
		windows = append(windows, *readings[window])
	}
	modelWindows := make([]domain.ModelWindow, 0, len(claudeFableRouteModelIDs))
	for _, model := range claudeFableRouteModelIDs {
		modelWindows = append(modelWindows, domain.ModelWindow{RouteModelID: model, Window: claudeFableWindow})
	}
	detail := "from the writer turn observed at " + capture.ObservedAt.Format(time.RFC3339)
	if len(notes) > 0 {
		detail += "; " + strings.Join(notes, "; ")
	}
	return UsageCollection{
		Availability: domain.AvailabilitySupported, Windows: windows, ModelWindows: modelWindows, Detail: detail,
	}, nil
}

// claudeEventStatus maps the pinned CLI's rate_limit_event status to a window
// status. Only rejected means the window refused work; allowed_warning still
// allows it. Any other token is not understood, so the caller ignores it.
func claudeEventStatus(status string) (domain.WindowStatus, bool) {
	switch status {
	case "rejected":
		return domain.WindowStatusExhausted, true
	case "allowed", "allowed_warning":
		return domain.WindowStatusAllowed, true
	default:
		return "", false
	}
}

func cloneTime(at *time.Time) *time.Time {
	if at == nil {
		return nil
	}
	clone := *at
	return &clone
}
