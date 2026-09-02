package specify

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const MaxTranscriptRecordBytes = strictjson.Limit(2 << 20)

var ErrTranscriptResultMissing = errors.New("specifier transcript has no successful result")

type transcriptRecord struct {
	Type    string  `json:"type"`
	Subtype string  `json:"subtype"`
	IsError *bool   `json:"is_error"`
	Result  *string `json:"result"`
}

// DecodeTranscript extracts the typed output from the successful terminal
// record in a Claude stream-json transcript. The human-readable StageResult
// summary is deliberately not an output channel.
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
	var result *string
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
		if result != nil {
			return Output{}, fmt.Errorf("decode specifier transcript: multiple result records: %w", ErrInvalidOutput)
		}
		if record.Subtype != "success" || record.IsError == nil || *record.IsError || record.Result == nil {
			return Output{}, fmt.Errorf("decode specifier transcript: unsuccessful result record: %w", ErrInvalidOutput)
		}
		result = record.Result
	}
	if err := scanner.Err(); err != nil {
		return Output{}, fmt.Errorf("decode specifier transcript: %w", err)
	}
	if result == nil {
		return Output{}, ErrTranscriptResultMissing
	}
	return decode([]byte(*result))
}

// EncodeTranscript emits the production transcript result shape for the
// deterministic fake, keeping fixtures on the same output channel as Claude.
func EncodeTranscript(out Output) ([]byte, error) {
	body, err := EncodeOutput(out)
	if err != nil {
		return nil, err
	}
	isError := false
	result := string(body)
	record, err := json.Marshal(transcriptRecord{
		Type: "result", Subtype: "success", IsError: &isError, Result: &result,
	})
	if err != nil {
		return nil, fmt.Errorf("encode specifier transcript: %w", err)
	}
	return append(record, '\n'), nil
}
