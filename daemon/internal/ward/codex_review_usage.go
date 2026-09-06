package ward

import (
	"bytes"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// codexUsageMeasurements returns the reported-usage measurements from the last
// turn.completed event in a Codex JSONL transcript. It mirrors
// exec.ExtractClaudeUsage: malformed or missing telemetry is observation
// absence, not a stage error, so a transcript with no turn.completed usage
// yields nil, and a negative or non-integer count discards that event's
// measurements. Codex reports usage on turn.completed (no per-turn USD cost, so
// unlike the Claude transcript there is no billable-cost measurement here).
func codexUsageMeasurements(events []byte, observedAt time.Time) []exec.UsageMeasurement {
	var latest []exec.UsageMeasurement
	for line := range bytes.SplitSeq(events, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || RejectDuplicateJSONKeys(line) != nil {
			continue
		}
		var envelopeType struct {
			Type string `json:"type"`
		}
		if err := strictjson.DecodeAllowingUnknownFields(
			line, &envelopeType, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
		); err != nil || envelopeType.Type != "turn.completed" {
			continue
		}
		latest = decodeCodexUsageEvent(line, observedAt.UTC())
	}
	return latest
}

func decodeCodexUsageEvent(line []byte, observedAt time.Time) []exec.UsageMeasurement {
	var event struct {
		Usage *struct {
			InputTokens       *int64 `json:"input_tokens"`
			CachedInputTokens *int64 `json:"cached_input_tokens"`
			OutputTokens      *int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := strictjson.DecodeAllowingUnknownFields(
		line, &event, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
	); err != nil || event.Usage == nil {
		return nil
	}
	measurements := make([]exec.UsageMeasurement, 0, 3)
	for _, metric := range []struct {
		name     string
		quantity *int64
	}{
		{"input_tokens", event.Usage.InputTokens},
		{"cached_input_tokens", event.Usage.CachedInputTokens},
		{"output_tokens", event.Usage.OutputTokens},
	} {
		if metric.quantity == nil {
			continue
		}
		if *metric.quantity < 0 {
			return nil
		}
		measurements = append(measurements, exec.UsageMeasurement{
			Source: domain.UsageSourceReviewSource, Kind: domain.UsageMeasurementReportedUsage,
			Metric: metric.name, Unit: "tokens", Quantity: *metric.quantity,
			Sequence: 1, ObservedAt: observedAt,
		})
	}
	if len(measurements) == 0 {
		return nil
	}
	return measurements
}
