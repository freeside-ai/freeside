package claude

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestClassifyQuotaRefusalRecorded reads the pinned CLI's own refused turn:
// 2.1.319 under the file_v8 agent line with a synthetic setup token, against
// a loopback mock whose one POST /v1/messages answered 429 with unified
// headers rejecting five_hour (reset 1794400000) and allowing seven_day. The
// CLI made that one request, wrote one rejected rate_limit_event and an error
// result, and exited 1, with nothing on stderr. As in the other loopback
// fixtures, the init answer (local paths) is emptied, the system init and
// assistant lines (local time zone) are dropped, and the result keeps only
// its type, subtype, and is_error; the event and get_usage answer are as
// written.
func TestClassifyQuotaRefusalRecorded(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("testdata", "claude_quota_refusal_loopback_2_1_319.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	got := claudeProvider{}.ClassifyQuotaRefusal(body)
	want := &domain.QuotaRefusal{Window: "five_hour", ResetsAt: new(time.Date(2026, 11, 11, 12, 26, 40, 0, time.UTC))}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ClassifyQuotaRefusal() = %#v, want %#v", got, want)
	}
	if got.ResetsAt.Location() != time.UTC {
		t.Fatal("reset is not UTC")
	}
}

// rejectedEvent is one rate_limit_event line in the pinned CLI's shape.
func rejectedEvent(window, status, resetsAt string) string {
	return `{"type":"rate_limit_event","rate_limit_info":{"status":"` + status + `",` + resetsAt +
		`"rateLimitType":"` + window + `","isUsingOverage":false},"uuid":"u","session_id":"s"}`
}

// TestClassifyQuotaRefusal pins that only one readable rejected window
// classifies, and that every doubt gives nil rather than a guessed window or
// reset.
func TestClassifyQuotaRefusal(t *testing.T) {
	t.Parallel()
	reset := `"resetsAt":1794400000,`
	resetAt := time.Unix(1794400000, 0).UTC()
	initLine := `{"type":"system","subtype":"init","session_id":"s"}`
	result := `{"type":"result","subtype":"success","is_error":true}`
	lines := func(l ...string) string { return strings.Join(l, "\n") + "\n" }
	for _, tc := range []struct {
		name       string
		transcript string
		want       *domain.QuotaRefusal
	}{
		{
			"one rejected window", lines(initLine, rejectedEvent("five_hour", "rejected", reset), result),
			&domain.QuotaRefusal{Window: "five_hour", ResetsAt: &resetAt},
		},
		{
			"no result record", lines(initLine, rejectedEvent("seven_day", "rejected", reset)),
			&domain.QuotaRefusal{Window: "seven_day", ResetsAt: &resetAt},
		},
		{
			"no reset", lines(rejectedEvent("five_hour", "rejected", ""), result),
			&domain.QuotaRefusal{Window: "five_hour"},
		},
		{
			"null reset", lines(rejectedEvent("five_hour", "rejected", `"resetsAt":null,`), result),
			&domain.QuotaRefusal{Window: "five_hour"},
		},
		{
			"another window allowed", lines(rejectedEvent("seven_day", "allowed", reset),
				rejectedEvent("five_hour", "rejected", reset), result),
			&domain.QuotaRefusal{Window: "five_hour", ResetsAt: &resetAt},
		},
		{
			"the Fable window", lines(rejectedEvent("seven_day_overage_included", "rejected", reset), result),
			&domain.QuotaRefusal{Window: "seven_day_overage_included", ResetsAt: &resetAt},
		},
		{"empty transcript", "", nil},
		{"no rejected event", lines(initLine, rejectedEvent("five_hour", "allowed", reset), result), nil},
		{"a warning is not a refusal", lines(rejectedEvent("five_hour", "allowed_warning", reset), result), nil},
		{
			"two windows rejected", lines(rejectedEvent("five_hour", "rejected", reset),
				rejectedEvent("seven_day", "rejected", reset), result), nil,
		},
		{
			"a later allowed event clears it", lines(rejectedEvent("five_hour", "rejected", reset),
				rejectedEvent("five_hour", "allowed", reset), result), nil,
		},
		{"a window name Validate refuses", lines(rejectedEvent("Five Hour", "rejected", reset), result), nil},
		{"empty window name", lines(rejectedEvent("", "rejected", reset), result), nil},
		{"zero reset", lines(rejectedEvent("five_hour", "rejected", `"resetsAt":0,`), result), nil},
		{"negative reset", lines(rejectedEvent("five_hour", "rejected", `"resetsAt":-1,`), result), nil},
		{"unencodable reset", lines(rejectedEvent("five_hour", "rejected", `"resetsAt":253402300800,`), result), nil},
		{"fractional reset", lines(rejectedEvent("five_hour", "rejected", `"resetsAt":1794400000.5,`), result), nil},
		{"string reset", lines(rejectedEvent("five_hour", "rejected", `"resetsAt":"1794400000",`), result), nil},
		{"no window", lines(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}`, result), nil},
		{"null window", lines(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":null}}`, result), nil},
		{"no status", lines(`{"type":"rate_limit_event","rate_limit_info":{"rateLimitType":"five_hour"}}`, result), nil},
		{"no info", lines(`{"type":"rate_limit_event"}`, result), nil},
		{"unreadable line", lines(rejectedEvent("five_hour", "rejected", reset), "not json", result), nil},
		{"truncated line", lines(rejectedEvent("five_hour", "rejected", reset), result[:20]), nil},
		{
			"duplicate key", lines(strings.Replace(rejectedEvent("five_hour", "rejected", reset),
				`"status":"rejected"`, `"status":"allowed","status":"rejected"`, 1), result), nil,
		},
		{"invalid UTF-8", lines(rejectedEvent("five_hour", "rejected", reset), "{\"type\":\"x\xff\"}", result), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := claudeProvider{}.ClassifyQuotaRefusal([]byte(tc.transcript))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ClassifyQuotaRefusal() = %#v, want %#v", got, tc.want)
			}
			if got != nil {
				if err := got.Validate(); err != nil {
					t.Fatalf("refusal is invalid: %v", err)
				}
			}
		})
	}
}
