package exec

import (
	"bytes"
	"encoding/json"
	"maps"
	"math"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// ExtractClaudeUsage returns the measurements from the last trusted terminal
// result envelope in a Claude JSON or JSONL transcript. Malformed or missing
// telemetry is observation absence, not a stage error.
func ExtractClaudeUsage(
	transcript []byte,
	observedAt time.Time,
	source domain.UsageSource,
	rejectDuplicateKeys func([]byte) error,
) []UsageMeasurement {
	var latest []UsageMeasurement
	for line := range bytes.SplitSeq(transcript, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || rejectDuplicateKeys(line) != nil {
			continue
		}
		var envelopeType struct {
			Type string `json:"type"`
		}
		if err := strictjson.DecodeAllowingUnknownFields(
			line, &envelopeType, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
		); err != nil || envelopeType.Type != "result" {
			continue
		}
		latest = decodeClaudeUsageResult(line, observedAt.UTC(), source)
	}
	return latest
}

func decodeClaudeUsageResult(
	line []byte, observedAt time.Time, source domain.UsageSource,
) []UsageMeasurement {
	var result struct {
		TotalCostUSD *json.Number `json:"total_cost_usd"`
		Usage        *struct {
			InputTokens              *int64 `json:"input_tokens"`
			OutputTokens             *int64 `json:"output_tokens"`
			CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
			CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := strictjson.DecodeAllowingUnknownFields(
		line, &result, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return nil
	}

	measurements := make([]UsageMeasurement, 0, 5)
	if result.TotalCostUSD != nil {
		quantity, ok := roundUSDMicros(*result.TotalCostUSD)
		if !ok {
			return nil
		}
		measurements = append(measurements, UsageMeasurement{
			Source: source, Kind: domain.UsageMeasurementBillableCost,
			Metric: "total_cost", Unit: "usd_micros",
			Quantity: quantity,
			Sequence: 1, ObservedAt: observedAt,
		})
	}
	if result.Usage != nil {
		for _, metric := range []struct {
			name     string
			quantity *int64
		}{
			{"input_tokens", result.Usage.InputTokens},
			{"output_tokens", result.Usage.OutputTokens},
			{"cache_creation_input_tokens", result.Usage.CacheCreationInputTokens},
			{"cache_read_input_tokens", result.Usage.CacheReadInputTokens},
		} {
			if metric.quantity == nil {
				continue
			}
			if *metric.quantity < 0 {
				return nil
			}
			measurements = append(measurements, UsageMeasurement{
				Source: source, Kind: domain.UsageMeasurementReportedUsage,
				Metric: metric.name, Unit: "tokens", Quantity: *metric.quantity,
				Sequence: 1, ObservedAt: observedAt,
			})
		}
	}
	if len(measurements) == 0 {
		return nil
	}
	return measurements
}

func roundUSDMicros(number json.Number) (int64, bool) {
	value, ok := new(big.Rat).SetString(number.String())
	if !ok || value.Sign() < 0 {
		return 0, false
	}
	value.Mul(value, big.NewRat(1_000_000, 1))
	quantity, remainder := new(big.Int), new(big.Int)
	quantity.QuoRem(value.Num(), value.Denom(), remainder)
	remainder.Lsh(remainder, 1)
	if remainder.Cmp(value.Denom()) >= 0 {
		quantity.Add(quantity, big.NewInt(1))
	}
	if !quantity.IsInt64() {
		return 0, false
	}
	return quantity.Int64(), true
}

// ClaudeUsageRequestID is the control request id the writer's in-container
// driver sends get_usage under. Only the answer to that request is read, so
// an initialize response or any other control record is ignored.
const ClaudeUsageRequestID = "usage-read"

// maxUnixResetSeconds is the last second of year 9999, the largest instant a
// time.Time can still marshal as JSON. A larger reset would make the stage
// result unwritable, so it is refused as malformed.
const maxUnixResetSeconds = 253402300799

// ExtractClaudeUsageCapture reads what the writer's turn told the CLI about
// the token's provider windows: the last rate_limit_event record for each
// window, and the last answer to the driver's get_usage request. The
// transcript is agent output, so the capture is untrusted. Any doubt gives
// nil, never an error: no result record (the turn did not finish), an
// unreadable line, a malformed event or answer, a share outside 0 to 100, a
// capture Validate refuses, or nothing captured. An answer whose rate_limits
// is null, one that says rate limits are unavailable, or an error answer
// gives no windows but keeps the events; an answer with no response body is
// malformed.
func ExtractClaudeUsageCapture(
	transcript []byte,
	observedAt time.Time,
	rejectDuplicateKeys func([]byte) error,
) *ClaudeUsageCapture {
	capture := ClaudeUsageCapture{
		ObservedAt: observedAt.UTC(),
		Windows:    []ClaudeUsageWindow{},
		Events:     []ClaudeRateLimitEvent{},
	}
	var answer []byte
	events := map[string]ClaudeRateLimitEvent{}
	resulted := false
	for line := range bytes.SplitSeq(transcript, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		// Unlike the telemetry reader, an unreadable line is doubt, not a
		// line to skip: it may be the answer or an event.
		if rejectDuplicateKeys(line) != nil {
			return nil
		}
		var record struct {
			Type     string `json:"type"`
			Response struct {
				RequestID string `json:"request_id"`
			} `json:"response"`
		}
		if err := strictjson.DecodeAllowingUnknownFields(
			line, &record, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
		); err != nil {
			return nil
		}
		switch record.Type {
		case "result":
			resulted = true
		case "rate_limit_event":
			event, ok := decodeClaudeRateLimitEvent(line)
			if !ok {
				return nil
			}
			// A long turn repeats the event on every request; the last one
			// is the window's latest status.
			events[event.Window] = event
		case "control_response":
			if record.Response.RequestID == ClaudeUsageRequestID {
				answer = line
			}
		}
	}
	if !resulted {
		return nil
	}
	for _, window := range slices.Sorted(maps.Keys(events)) {
		capture.Events = append(capture.Events, events[window])
	}
	if answer != nil {
		windows, ok := decodeClaudeUsageAnswer(answer)
		if !ok {
			return nil
		}
		capture.Windows = windows
	}
	if capture.Validate() != nil {
		return nil
	}
	return &capture
}

// claudeRejectedStatus is the rate_limit_event status the pinned CLI reports
// for a window the provider refused, as the Claude collector reads it.
const claudeRejectedStatus = "rejected"

// ClassifyClaudeQuotaRefusal reads a failed writer turn's transcript for the
// provider's quota refusal: the one window whose last rate_limit_event is
// rejected, with that event's reset. The transcript is agent output and the
// refusal only feeds scheduling. Any doubt gives nil, never a guess: an
// unreadable line, a malformed event, no rejected window, more than one, or
// a refusal Validate refuses. A rejected event with no reset is still a
// refusal, with a nil reset. Unlike ExtractClaudeUsageCapture it requires no
// result record, because a refused turn need not finish.
func ClassifyClaudeQuotaRefusal(
	transcript []byte, rejectDuplicateKeys func([]byte) error,
) *domain.QuotaRefusal {
	events := map[string]ClaudeRateLimitEvent{}
	for line := range bytes.SplitSeq(transcript, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if rejectDuplicateKeys(line) != nil {
			return nil
		}
		var record struct {
			Type string `json:"type"`
		}
		if err := strictjson.DecodeAllowingUnknownFields(
			line, &record, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
		); err != nil {
			return nil
		}
		if record.Type != "rate_limit_event" {
			continue
		}
		event, ok := decodeClaudeRateLimitEvent(line)
		if !ok {
			return nil
		}
		// The last event is the window's latest status, so a later allowed
		// event clears an earlier rejection.
		events[event.Window] = event
	}
	var refusal *domain.QuotaRefusal
	for _, event := range events {
		if event.Status != claudeRejectedStatus {
			continue
		}
		if refusal != nil {
			return nil
		}
		refusal = &domain.QuotaRefusal{Window: event.Window, ResetsAt: event.ResetsAt}
	}
	if refusal == nil || refusal.Validate() != nil {
		return nil
	}
	return refusal
}

func decodeClaudeRateLimitEvent(line []byte) (ClaudeRateLimitEvent, bool) {
	var record struct {
		Info *struct {
			RateLimitType *string `json:"rateLimitType"`
			Status        *string `json:"status"`
			ResetsAt      *int64  `json:"resetsAt"`
		} `json:"rate_limit_info"`
	}
	if err := strictjson.DecodeAllowingUnknownFields(
		line, &record, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
	); err != nil || record.Info == nil || record.Info.RateLimitType == nil || record.Info.Status == nil {
		return ClaudeRateLimitEvent{}, false
	}
	event := ClaudeRateLimitEvent{Window: *record.Info.RateLimitType, Status: *record.Info.Status}
	if seconds := record.Info.ResetsAt; seconds != nil {
		if *seconds <= 0 || *seconds > maxUnixResetSeconds {
			return ClaudeRateLimitEvent{}, false
		}
		event.ResetsAt = new(time.Unix(*seconds, 0).UTC())
	}
	return event, true
}

// ClaudeFableWindow is the window the pinned CLI names the Fable weekly
// bucket: the seed it builds from a turn's anthropic-ratelimit-unified-7d_oi
// headers and the rateLimitType of a rate_limit_event that bucket binds. Its
// get_usage answer carries the same bucket as rate_limits.model_scoped, which
// decodeClaudeUsageAnswer reads under this name so the answer, the events, and
// a refusal all describe one window
// (devlog/2026-11-10-1830-claude-fable-window.md).
const ClaudeFableWindow = "seven_day_overage_included"

// claudeModelScopedKey is the rate_limits key the pinned CLI (2.1.319) puts
// per-model weekly buckets under, as an array rather than a named window.
const claudeModelScopedKey = "model_scoped"

// decodeClaudeUsageAnswer reads one get_usage control response. ok is false
// only for a malformed answer; a refused or empty answer is ok with no
// windows.
func decodeClaudeUsageAnswer(line []byte) ([]ClaudeUsageWindow, bool) {
	var record struct {
		Response struct {
			Subtype  string `json:"subtype"`
			Response *struct {
				RateLimitsAvailable *bool                      `json:"rate_limits_available"`
				RateLimits          map[string]json.RawMessage `json:"rate_limits"`
			} `json:"response"`
		} `json:"response"`
	}
	if err := strictjson.DecodeAllowingUnknownFields(
		line, &record, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return nil, false
	}
	switch record.Response.Subtype {
	case "error":
		return []ClaudeUsageWindow{}, true
	case "success":
	default:
		return nil, false
	}
	answer := record.Response.Response
	if answer == nil {
		return nil, false
	}
	if answer.RateLimitsAvailable != nil && !*answer.RateLimitsAvailable {
		return []ClaudeUsageWindow{}, true
	}
	windows := make([]ClaudeUsageWindow, 0, len(answer.RateLimits))
	for name, raw := range answer.RateLimits {
		if name == claudeModelScopedKey {
			window, found, ok := decodeClaudeFableWindow(raw)
			if !ok {
				return nil, false
			}
			if found {
				windows = append(windows, window)
			}
			continue
		}
		var limit *claudeUsageLimit
		if err := strictjson.DecodeAllowingUnknownFields(
			raw, &limit, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
		); err != nil || limit == nil || limit.Utilization == nil {
			return nil, false
		}
		window, ok := claudeUsageWindow(name, *limit.Utilization, limit.ResetsAt)
		if !ok {
			return nil, false
		}
		windows = append(windows, window)
	}
	slices.SortFunc(windows, func(a, b ClaudeUsageWindow) int { return strings.Compare(a.Window, b.Window) })
	return windows, true
}

// claudeUsageLimit is one named window of a get_usage answer.
type claudeUsageLimit struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

// decodeClaudeFableWindow reads rate_limits.model_scoped. On the setup-token
// path the CLI builds it from the turn's 7d_oi headers alone, so it holds at
// most one entry, named by the CLI's overage-included-models allowlist
// ("Fable" by default). That one entry, when it names Fable and carries a
// share, is the ClaudeFableWindow reading; found is false for anything else
// the CLI may report there (no entry, no share, another model, or several
// buckets from an account read), which this reader can't name, so it is left
// out rather than sinking the answer's other windows. ok is false only when
// the value is not an array of entries, or the Fable entry's share or reset is
// malformed.
func decodeClaudeFableWindow(raw json.RawMessage) (window ClaudeUsageWindow, found, ok bool) {
	var entries []struct {
		DisplayName string   `json:"display_name"`
		Utilization *float64 `json:"utilization"`
		ResetsAt    *string  `json:"resets_at"`
	}
	if err := strictjson.DecodeAllowingUnknownFields(
		raw, &entries, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return ClaudeUsageWindow{}, false, false
	}
	if len(entries) != 1 || !namesFable(entries[0].DisplayName) || entries[0].Utilization == nil {
		return ClaudeUsageWindow{}, false, true
	}
	window, ok = claudeUsageWindow(ClaudeFableWindow, *entries[0].Utilization, entries[0].ResetsAt)
	return window, ok, ok
}

// namesFable reports whether a model_scoped display name is Fable: "Fable"
// or a versioned "Fable 5.1", in any case. The CLI takes the name from its
// allowlist, whose entries include both.
func namesFable(displayName string) bool {
	name := strings.ToLower(displayName)
	return name == "fable" || strings.HasPrefix(name, "fable ")
}

// claudeUsageWindow builds one window from a share in percent and an
// optional RFC 3339 reset. ok is false for a share that rounds outside 0 to
// 100 or a reset that does not parse.
func claudeUsageWindow(name string, utilization float64, resetsAt *string) (ClaudeUsageWindow, bool) {
	share := math.Round(utilization)
	if share < 0 || share > 100 {
		return ClaudeUsageWindow{}, false
	}
	window := ClaudeUsageWindow{Window: name, UsedPercent: int(share)}
	if resetsAt != nil {
		resets, err := time.Parse(time.RFC3339Nano, *resetsAt)
		if err != nil {
			return ClaudeUsageWindow{}, false
		}
		window.ResetsAt = new(resets.UTC())
	}
	return window, true
}
