package claude_test

import (
	"encoding/json"
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
		{Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementBillableCost,
			Metric: "total_cost", Unit: "usd_micros", Quantity: 12346, Sequence: 1, ObservedAt: observedAt},
		{Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "input_tokens", Unit: "tokens", Quantity: 100, Sequence: 1, ObservedAt: observedAt},
		{Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "output_tokens", Unit: "tokens", Quantity: 50, Sequence: 1, ObservedAt: observedAt},
		{Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "cache_creation_input_tokens", Unit: "tokens", Quantity: 10, Sequence: 1, ObservedAt: observedAt},
		{Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "cache_read_input_tokens", Unit: "tokens", Quantity: 20, Sequence: 1, ObservedAt: observedAt},
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
		{Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementBillableCost,
			Metric: "total_cost", Unit: "usd_micros", Quantity: 0, Sequence: 1, ObservedAt: observedAt},
		{Source: domain.UsageSourceAdapterTranscript, Kind: domain.UsageMeasurementReportedUsage,
			Metric: "output_tokens", Unit: "tokens", Quantity: 0, Sequence: 1, ObservedAt: observedAt},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractUsage() = %#v, want %#v", got, want)
	}
}
