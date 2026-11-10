package claude_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
)

func usageFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // fixed fixture names
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestExtractUsage(t *testing.T) {
	observedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := []exec.UsageMeasurement{
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementBillableCost,
			Metric: "total_cost", Unit: "usd_micros", Quantity: 12346, Sequence: 1, ObservedAt: observedAt,
		},
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "input_tokens", Unit: "tokens", Quantity: 100, Sequence: 1, ObservedAt: observedAt,
		},
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "output_tokens", Unit: "tokens", Quantity: 50, Sequence: 1, ObservedAt: observedAt,
		},
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "cache_creation_input_tokens", Unit: "tokens", Quantity: 10, Sequence: 1, ObservedAt: observedAt,
		},
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "cache_read_input_tokens", Unit: "tokens", Quantity: 20, Sequence: 1, ObservedAt: observedAt,
		},
	}
	if got := claude.ExtractUsage(usageFixture(t, "usage_complete.jsonl"), observedAt); !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractUsage() = %#v, want %#v", got, want)
	}
	for i, measurement := range want {
		if err := measurement.Validate(); err != nil {
			t.Fatalf("measurement %d invalid: %v", i, err)
		}
	}

	encoded, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret result text") {
		t.Fatal("usage measurements retained a transcript text field")
	}
}

func TestExtractUsageAbsenceIsNotAnError(t *testing.T) {
	observedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"missing telemetry", usageFixture(t, "usage_missing.jsonl")},
		{"malformed line", usageFixture(t, "usage_malformed.jsonl")},
		{"no result", usageFixture(t, "usage_no_result.jsonl")},
		{"missing transcript", nil},
		{"negative telemetry", []byte(`{"type":"result","usage":{"input_tokens":-1}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claude.ExtractUsage(tc.body, observedAt); got != nil {
				t.Fatalf("ExtractUsage() = %#v, want nil", got)
			}
		})
	}
}

func TestExtractUsageUsesLastResultLine(t *testing.T) {
	observedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	body := append(usageFixture(t, "usage_complete.jsonl"),
		[]byte("\n{\"type\":\"result\",\"result\":\"later without telemetry\"}\n")...)
	if got := claude.ExtractUsage(body, observedAt); got != nil {
		t.Fatalf("ExtractUsage() = %#v, want last result's absent telemetry", got)
	}
}

func TestExtractUsageRejectsSchemaEscapes(t *testing.T) {
	observedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"duplicate type", `{"type":"result","type":"assistant","usage":{"input_tokens":1}}`},
		{"duplicate usage metric", `{"type":"result","usage":{"input_tokens":1,"input_tokens":2}}`},
		{"fractional token count", `{"type":"result","usage":{"input_tokens":1.5}}`},
		{"trailing document", `{"type":"result","usage":{"input_tokens":1}} {}`},
		{"invalid UTF-8", string(append([]byte(`{"type":"result","usage":{"input_tokens":1},"text":"`), 0xff))},
		{"cost overflow", `{"type":"result","total_cost_usd":9223372036855}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claude.ExtractUsage([]byte(tc.body), observedAt); got != nil {
				t.Fatalf("ExtractUsage() = %#v, want nil", got)
			}
		})
	}
}

func TestExtractUsageAcceptsFieldReorderingAndZero(t *testing.T) {
	observedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	body := []byte(`{"usage":{"output_tokens":0},"result":"ignored","type":"result","total_cost_usd":0}`)
	got := claude.ExtractUsage(body, observedAt)
	want := []exec.UsageMeasurement{
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementBillableCost,
			Metric: "total_cost", Unit: "usd_micros", Quantity: 0, Sequence: 1, ObservedAt: observedAt,
		},
		{
			Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "output_tokens", Unit: "tokens", Quantity: 0, Sequence: 1, ObservedAt: observedAt,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractUsage() = %#v, want %#v", got, want)
	}
}

func TestExtractUsageCapture(t *testing.T) {
	t.Parallel()
	observedAt := time.Date(2026, 11, 4, 9, 0, 0, 0, time.FixedZone("CET", 3600))
	sevenDayReset := time.Date(2026, 11, 6, 3, 0, 0, 0, time.UTC)
	fiveHourReset := time.Unix(1791072000, 0).UTC()
	for _, tc := range []struct {
		fixture string
		want    *exec.ClaudeUsageCapture
	}{
		{"claude_usage_observed_turn_seeded.jsonl", &exec.ClaudeUsageCapture{
			ObservedAt: observedAt.UTC(),
			Windows:    []exec.ClaudeUsageWindow{{Window: "seven_day", UsedPercent: 57, ResetsAt: &sevenDayReset}},
			Events:     []exec.ClaudeRateLimitEvent{{Window: "seven_day", Status: "allowed", ResetsAt: &sevenDayReset}},
		}},
		// A plain -p turn sends no get_usage, so only its event is captured.
		{"claude_usage_observed_turn.jsonl", &exec.ClaudeUsageCapture{
			ObservedAt: observedAt.UTC(),
			Windows:    []exec.ClaudeUsageWindow{},
			Events:     []exec.ClaudeRateLimitEvent{{Window: "five_hour", Status: "allowed", ResetsAt: &fiveHourReset}},
		}},
		// No turn finished: the null answer seeds nothing.
		{"claude_usage_observed_idle.jsonl", nil},
		{"claude_usage_error.jsonl", nil},
		// The 2.1.282 CLI under the driver against a loopback mock API
		// that sent both windows' headers: its answer adds session and
		// behaviors fields and its event adds unifiedWindows, all ignored.
		{"claude_usage_loopback_turn.jsonl", &exec.ClaudeUsageCapture{
			ObservedAt: observedAt.UTC(),
			Windows: []exec.ClaudeUsageWindow{
				{Window: "five_hour", UsedPercent: 4, ResetsAt: new(time.Date(2026, 11, 4, 16, 28, 26, 0, time.UTC))},
				{Window: "seven_day", UsedPercent: 57, ResetsAt: new(time.Date(2026, 11, 5, 15, 28, 26, 0, time.UTC))},
			},
			Events: []exec.ClaudeRateLimitEvent{{Window: "seven_day", Status: "allowed", ResetsAt: new(time.Unix(1793892506, 0).UTC())}},
		}},
		// The same launch whose turn the mock refused: a null share and no
		// event.
		{"claude_usage_loopback_error.jsonl", nil},
		// The 2.1.319 CLI under the same launch, against a mock that sent no
		// overall reset: the answer keeps both windows, and the event adds a
		// status to each unified window, which is ignored.
		{"claude_usage_loopback_turn_2_1_319.jsonl", &exec.ClaudeUsageCapture{
			ObservedAt: observedAt.UTC(),
			Windows: []exec.ClaudeUsageWindow{
				{Window: "five_hour", UsedPercent: 4, ResetsAt: new(time.Date(2026, 11, 8, 23, 59, 48, 0, time.UTC))},
				{Window: "seven_day", UsedPercent: 57, ResetsAt: new(time.Date(2026, 11, 9, 22, 59, 48, 0, time.UTC))},
			},
			Events: []exec.ClaudeRateLimitEvent{{Window: "seven_day", Status: "allowed"}},
		}},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()
			got := claude.ExtractUsageCapture(usageFixture(t, tc.fixture), observedAt)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ExtractUsageCapture() = %#v, want %#v", got, tc.want)
			}
			if got != nil {
				if err := got.Validate(); err != nil {
					t.Fatalf("capture is invalid: %v", err)
				}
			}
		})
	}
}

// TestExtractUsageCaptureNilOnDoubt pins that every malformed or
// out-of-range answer or event gives no capture, never a stage error, and
// that a refused or empty answer keeps the turn's events.
func TestExtractUsageCaptureNilOnDoubt(t *testing.T) {
	t.Parallel()
	seeded := string(usageFixture(t, "claude_usage_observed_turn_seeded.jsonl"))
	answer := `"rate_limits":{"seven_day":{"utilization":56.99999999999999,"resets_at":"2026-11-06T03:00:00.000Z"}}`
	if !strings.Contains(seeded, answer) {
		t.Fatal("fixture answer moved")
	}
	event := `"resetsAt":1793934000,"rateLimitType":"seven_day"`
	if !strings.Contains(seeded, event) {
		t.Fatal("fixture event moved")
	}
	observedAt := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, old, new string
		windows        bool
	}{
		{"string utilization", `"utilization":56.99999999999999`, `"utilization":"57"`, false},
		{"null utilization", `"utilization":56.99999999999999`, `"utilization":null`, false},
		{"share above 100", `"utilization":56.99999999999999`, `"utilization":140`, false},
		{"share rounding above 100", `"utilization":56.99999999999999`, `"utilization":100.5`, false},
		{"negative share", `"utilization":56.99999999999999`, `"utilization":-3`, false},
		{"huge share", `"utilization":56.99999999999999`, `"utilization":1e400`, false},
		{"unparsable reset", `"resets_at":"2026-11-06T03:00:00.000Z"`, `"resets_at":"next thursday"`, false},
		{"numeric reset", `"resets_at":"2026-11-06T03:00:00.000Z"`, `"resets_at":1793934000`, false},
		// Both parse, but their UTC year is one JSON cannot encode.
		{"window reset past year 9999", `"resets_at":"2026-11-06T03:00:00.000Z"`, `"resets_at":"9999-12-31T23:30:00-05:00"`, false},
		{"window reset before year 0", `"resets_at":"2026-11-06T03:00:00.000Z"`, `"resets_at":"0000-01-01T00:30:00+05:00"`, false},
		{"uppercase window", `{"seven_day":{`, `{"Seven_day":{`, false},
		{"window with a space", `{"seven_day":{`, `{"seven day":{`, false},
		{"rate limits as a list", answer, `"rate_limits":[]`, false},
		{"null window", answer, `"rate_limits":{"seven_day":null}`, false},
		{"unknown subtype", `"request_id":"usage-read","subtype":"success"`, `"request_id":"usage-read","subtype":"partial"`, false},
		{"answer without a body", `"subtype":"success","response":{"subscription_type"`, `"subtype":"success","other":{"subscription_type"`, false},
		{"event reset as text", `"resetsAt":1793934000`, `"resetsAt":"1793934000"`, false},
		{"event reset past year 9999", `"resetsAt":1793934000`, `"resetsAt":253402300800`, false},
		{"event without a window", `,"rateLimitType":"seven_day"`, ``, false},
		{"event status not a token", `"status":"allowed"`, `"status":"Allowed!"`, false},
		// A token past 64 bytes drops the capture.
		{"65-byte window", `{"seven_day":{`, `{"` + strings.Repeat("w", 65) + `":{`, false},
		{"65-byte event window", `"rateLimitType":"seven_day"`, `"rateLimitType":"` + strings.Repeat("w", 65) + `"`, false},
		{"65-byte event status", `"status":"allowed"`, `"status":"` + strings.Repeat("s", 65) + `"`, false},
		{"no result", `{"type":"result","subtype":"success","is_error":false}`, `{"type":"assistant"}`, false},
		{"duplicate key", `"subtype":"success","response":{"subscription_type"`, `"subtype":"success","subtype":"success","response":{"subscription_type"`, false},
		{"plain text line", `{"type":"result","subtype":"success","is_error":false}`, "{\"type\":\"result\"}\nwarning: not json", false},
		// A refused or empty answer has no windows, but the turn's event stays.
		{"null rate limits", answer, `"rate_limits":null`, true},
		{"rate limits unavailable", `"rate_limits_available":true`, `"rate_limits_available":false`, true},
		{"error answer", `"subtype":"success","response":{"subscription_type"`, `"subtype":"error","error":"no","x":{"subscription_type"`, true},
		// An answer to another request id is not the usage answer.
		{"answer to another request", `"request_id":"usage-read"`, `"request_id":"usage-other"`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(seeded, tc.old) {
				t.Fatalf("fixture lacks %s", tc.old)
			}
			body := []byte(strings.Replace(seeded, tc.old, tc.new, 1))
			got := claude.ExtractUsageCapture(body, observedAt)
			if !tc.windows {
				if got != nil {
					t.Fatalf("ExtractUsageCapture() = %#v, want nil", got)
				}
				return
			}
			if got == nil || len(got.Windows) != 0 || len(got.Events) != 1 {
				t.Fatalf("ExtractUsageCapture() = %#v, want the event and no window", got)
			}
		})
	}
}

// TestExtractUsageCaptureKeepsTheLastEventPerWindow pins that a turn's
// repeated events reduce to each window's latest, and that more windows
// than a capture holds is doubt.
func TestExtractUsageCaptureKeepsTheLastEventPerWindow(t *testing.T) {
	t.Parallel()
	observedAt := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	event := func(window, status string, resets int64) string {
		return fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"status":%q,"resetsAt":%d,"rateLimitType":%q}}`+"\n", status, resets, window)
	}
	const result = `{"type":"result","subtype":"success","is_error":false}` + "\n"
	transcript := event("seven_day", "allowed", 1793934000) + event("five_hour", "allowed", 1793800000) +
		event("seven_day", "allowed_warning", 1793934600) + result
	got := claude.ExtractUsageCapture([]byte(transcript), observedAt)
	want := []exec.ClaudeRateLimitEvent{
		{Window: "five_hour", Status: "allowed", ResetsAt: new(time.Unix(1793800000, 0).UTC())},
		{Window: "seven_day", Status: "allowed_warning", ResetsAt: new(time.Unix(1793934600, 0).UTC())},
	}
	if got == nil || !reflect.DeepEqual(got.Events, want) {
		t.Fatalf("ExtractUsageCapture() = %#v, want events %#v", got, want)
	}
	var many strings.Builder
	for i := range 17 {
		many.WriteString(event(fmt.Sprintf("w%02d", i), "allowed", 1793934000))
	}
	many.WriteString(result)
	if got := claude.ExtractUsageCapture([]byte(many.String()), observedAt); got != nil {
		t.Fatalf("seventeen windows gave %#v, want nil", got)
	}
}

// TestDuplexTranscriptKeepsUsage pins that the control records a duplex
// launch adds leave the result telemetry unchanged.
func TestDuplexTranscriptKeepsUsage(t *testing.T) {
	t.Parallel()
	observedAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	plain := usageFixture(t, "usage_complete.jsonl")
	seeded := strings.Split(strings.TrimSpace(string(usageFixture(t, "claude_usage_observed_turn_seeded.jsonl"))), "\n")
	result := strings.Split(strings.TrimSpace(string(plain)), "\n")
	// initialize answer, event, the plain transcript, then the usage answer.
	duplex := []byte(strings.Join(append(append(seeded[:2:2], result...), seeded[3]), "\n") + "\n")
	want := claude.ExtractUsage(plain, observedAt)
	if want == nil {
		t.Fatal("plain fixture carries no usage")
	}
	if got := claude.ExtractUsage(duplex, observedAt); !reflect.DeepEqual(got, want) {
		t.Fatalf("duplex ExtractUsage() = %#v, want %#v", got, want)
	}
	capture := claude.ExtractUsageCapture(duplex, observedAt)
	if capture == nil || len(capture.Windows) != 1 || capture.Windows[0].UsedPercent != 57 {
		t.Fatalf("duplex capture = %#v, want the seven_day share", capture)
	}
}
