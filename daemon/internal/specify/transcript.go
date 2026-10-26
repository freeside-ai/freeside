package specify

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// MaxTranscriptRecordBytes bounds one stream-json line. A schema-constrained
// result record carries the output twice, as structured_output and escaped in
// result, so the bound leaves room for both at MaxOutputBytes.
const MaxTranscriptRecordBytes = strictjson.Limit(4 << 20)

var ErrTranscriptResultMissing = errors.New("specifier transcript has no successful result")

// transcriptRecord is one stream-json line. A launch that passes the CLI a
// JSON Schema gets the model's answer in StructuredOutput, and Result then
// holds that object serialized; any prose the model wrote before it stays in
// the assistant records, which are not an output channel.
type transcriptRecord struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          *bool           `json:"is_error"`
	Result           *string         `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
}

// DecodeTranscript extracts the typed output from the successful terminal
// record in a Claude stream-json transcript: its structured_output when the
// record carries one, otherwise its result text, which is all a launch without
// a schema produces. Either payload passes DecodeOutput unchanged. The
// human-readable StageResult summary is deliberately not an output channel.
func DecodeTranscript(reader io.Reader) (Output, error) {
	return decodeTranscript(reader, DecodeOutput)
}

// DecodeLegacyAddressalTranscript decodes the durable transcript emitted by
// the prompt contract immediately preceding comment_id addressals.
func DecodeLegacyAddressalTranscript(reader io.Reader, commentIDs map[string]string) (Output, error) {
	return decodeTranscript(reader, func(data []byte) (Output, error) {
		return DecodeLegacyAddressalOutput(data, commentIDs)
	})
}

func decodeTranscript(reader io.Reader, decode func([]byte) (Output, error)) (Output, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), int(MaxTranscriptRecordBytes))
	var payload []byte
	found := false
	for line := 1; scanner.Scan(); line++ {
		var record transcriptRecord
		if err := strictjson.DecodeAllowingUnknownFields(
			scanner.Bytes(), &record, strictjson.RejectInvalidUTF8, MaxTranscriptRecordBytes,
		); err != nil {
			return Output{}, fmt.Errorf("decode specifier transcript line %d: %w", line, err)
		}
		if record.Type != "result" {
			continue
		}
		if found {
			return Output{}, fmt.Errorf("decode specifier transcript: multiple result records: %w", ErrInvalidOutput)
		}
		if record.Subtype != "success" || record.IsError == nil || *record.IsError {
			return Output{}, fmt.Errorf("decode specifier transcript: unsuccessful result record: %w", ErrInvalidOutput)
		}
		// A null structured_output is no structured answer, so the record's
		// result text is the payload, as for a launch without a schema.
		switch {
		case len(record.StructuredOutput) > 0 && string(record.StructuredOutput) != "null":
			payload = record.StructuredOutput
		case record.Result != nil:
			payload = []byte(*record.Result)
		default:
			return Output{}, fmt.Errorf("decode specifier transcript: unsuccessful result record: %w", ErrInvalidOutput)
		}
		found = true
	}
	if err := scanner.Err(); err != nil {
		return Output{}, fmt.Errorf("decode specifier transcript: %w", err)
	}
	if !found {
		return Output{}, ErrTranscriptResultMissing
	}
	return decode(payload)
}

// EncodeTranscript emits the production transcript result shape for the
// deterministic fake, keeping fixtures on the same output channel as Claude:
// a schema-constrained launch's record, which carries the object in
// structured_output and its serialization in result.
func EncodeTranscript(out Output) ([]byte, error) {
	body, err := EncodeOutput(out)
	if err != nil {
		return nil, err
	}
	isError := false
	result := string(body)
	record, err := json.Marshal(transcriptRecord{
		Type: "result", Subtype: "success", IsError: &isError, Result: &result,
		StructuredOutput: body,
	})
	if err != nil {
		return nil, fmt.Errorf("encode specifier transcript: %w", err)
	}
	return append(record, '\n'), nil
}
