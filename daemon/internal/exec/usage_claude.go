package exec

import (
	"bytes"
	"encoding/json"
	"math/big"
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
