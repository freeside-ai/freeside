package exec

import (
	"fmt"
	"time"
)

// ClaudeUsageCapture is the Claude writer's view of its token's provider
// windows after its own turn: the CLI's get_usage answer and the turn's
// rate_limit_event records. It keeps the CLI's own window names and status
// tokens; mapping them to the usage record's windows is the collector's job.
type ClaudeUsageCapture struct {
	// ObservedAt is the export stamp the transcript was read under, the
	// same instant Usage carries.
	ObservedAt time.Time `json:"observed_at"`
	// Windows is the get_usage answer, one entry per window the CLI named,
	// sorted by window. Empty when the answer was absent, null, or an error.
	Windows []ClaudeUsageWindow `json:"windows"`
	// Events are the turn's rate_limit_event records, the last one for each
	// window, sorted by window.
	Events []ClaudeRateLimitEvent `json:"events"`
}

// ClaudeUsageWindow is one window of the CLI's get_usage answer.
type ClaudeUsageWindow struct {
	Window      string     `json:"window"`       // the CLI's name, e.g. seven_day
	UsedPercent int        `json:"used_percent"` // utilization rounded to the nearest integer, 0 to 100
	ResetsAt    *time.Time `json:"resets_at"`    // UTC; null when the answer gave none
}

// ClaudeRateLimitEvent is one rate_limit_event record of the writer's turn.
type ClaudeRateLimitEvent struct {
	Window   string     `json:"window"`    // rate_limit_info.rateLimitType
	Status   string     `json:"status"`    // rate_limit_info.status, the CLI's token, e.g. allowed
	ResetsAt *time.Time `json:"resets_at"` // rate_limit_info.resetsAt (unix seconds) as UTC
}

// maxClaudeUsageWindows bounds the windows and the events a capture holds.
// The CLI names a handful of windows; the bound keeps a transcript the agent
// wrote from growing the stage result it rides on.
const maxClaudeUsageWindows = 16

// maxClaudeUsageTokenBytes bounds each window name and status token. The
// pinned CLI's tokens are under 20 bytes (five_hour, seven_day,
// allowed_warning); the bound leaves room for new ones while keeping a
// transcript the agent wrote from growing the capture, the stage result, and
// the collection detail that names an unknown window. A CLI token past it
// drops the whole capture, so raise it when the CLI adopts a longer name.
const maxClaudeUsageTokenBytes = 64

// Validate checks the capture's shape: a non-nil capture is observed at a
// UTC instant and carries at least one window or event, at most
// maxClaudeUsageWindows of each, windows and events are each sorted by
// window and unique, every name and status is a lowercase usage token of at
// most maxClaudeUsageTokenBytes, every share is a percentage, and every
// reset is a UTC instant JSON can encode (years 0 to 9999), so a valid
// capture always marshals.
func (c ClaudeUsageCapture) Validate() error {
	if c.ObservedAt.IsZero() || c.ObservedAt.Location() != time.UTC {
		return fmt.Errorf("claude usage observed_at must be a non-zero UTC instant: %w", ErrInvalidClaudeUsage)
	}
	if len(c.Windows) == 0 && len(c.Events) == 0 {
		return fmt.Errorf("claude usage carries no window or event: %w", ErrInvalidClaudeUsage)
	}
	if len(c.Windows) > maxClaudeUsageWindows || len(c.Events) > maxClaudeUsageWindows {
		return fmt.Errorf("claude usage carries %d windows and %d events, more than %d: %w",
			len(c.Windows), len(c.Events), maxClaudeUsageWindows, ErrInvalidClaudeUsage)
	}
	for i, w := range c.Windows {
		if err := checkUsageToken(fmt.Sprintf("windows[%d] window", i), w.Window); err != nil {
			return err
		}
		if i > 0 && c.Windows[i-1].Window >= w.Window {
			return fmt.Errorf("claude usage windows[%d] %q is not after %q: %w",
				i, w.Window, c.Windows[i-1].Window, ErrInvalidClaudeUsage)
		}
		if w.UsedPercent < 0 || w.UsedPercent > 100 {
			return fmt.Errorf("claude usage windows[%d] used_percent %d: %w", i, w.UsedPercent, ErrInvalidClaudeUsage)
		}
		if !resetValid(w.ResetsAt) {
			return fmt.Errorf("claude usage windows[%d] resets_at is not an encodable UTC instant: %w", i, ErrInvalidClaudeUsage)
		}
	}
	for i, e := range c.Events {
		if err := checkUsageToken(fmt.Sprintf("events[%d] window", i), e.Window); err != nil {
			return err
		}
		if err := checkUsageToken(fmt.Sprintf("events[%d] status", i), e.Status); err != nil {
			return err
		}
		if i > 0 && c.Events[i-1].Window >= e.Window {
			return fmt.Errorf("claude usage events[%d] %q is not after %q: %w",
				i, e.Window, c.Events[i-1].Window, ErrInvalidClaudeUsage)
		}
		if !resetValid(e.ResetsAt) {
			return fmt.Errorf("claude usage events[%d] resets_at is not an encodable UTC instant: %w", i, ErrInvalidClaudeUsage)
		}
	}
	return nil
}

// checkUsageToken refuses value, the capture's field, unless it is a usage
// token. An over-long value is reported by its length, not its text, so the
// error stays small.
func checkUsageToken(field, value string) error {
	if len(value) > maxClaudeUsageTokenBytes {
		return fmt.Errorf("claude usage %s is %d bytes, more than %d: %w",
			field, len(value), maxClaudeUsageTokenBytes, ErrInvalidClaudeUsage)
	}
	if !usageToken(value) {
		return fmt.Errorf("claude usage %s %q: %w", field, value, ErrInvalidClaudeUsage)
	}
	return nil
}

// usageToken is the window-name rule domain.QuotaRefusal applies
// ([a-z0-9_]+), which keeps a CLI token storable as a usage window later.
func usageToken(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
}

// resetValid accepts a missing reset or a non-zero UTC instant whose year
// time.Time.MarshalJSON can encode.
func resetValid(at *time.Time) bool {
	return at == nil || (!at.IsZero() && at.Location() == time.UTC && at.Year() >= 0 && at.Year() <= 9999)
}
