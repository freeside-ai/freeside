// Package strictjson centralizes the daemon's single-value JSON decoding
// contract. It accepts exactly one JSON value, optionally rejects invalid
// UTF-8 before encoding/json can replace it with U+FFFD, optionally rejects
// unknown object fields, and enforces the caller's explicit byte limit.
//
// Duplicate object keys retain encoding/json's last-value-wins behavior.
// Boundaries that reject duplicates or require canonical bytes keep those
// stronger gates around this package. The package is a standard-library-only
// leaf so every daemon lane can share the same delimiting decision without
// importing another lane's policy.
package strictjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

// UTF8Policy states whether the boundary rejects invalid UTF-8 before decode.
// The zero value is invalid so a caller cannot select a posture by omission.
type UTF8Policy string

const (
	RejectInvalidUTF8   UTF8Policy = "reject_invalid"
	TolerateInvalidUTF8 UTF8Policy = "tolerate_invalid"
)

// AllUTF8Policies is the single registration point for valid UTF-8 postures.
var AllUTF8Policies = []UTF8Policy{RejectInvalidUTF8, TolerateInvalidUTF8}

func (p UTF8Policy) valid() bool {
	switch p {
	case RejectInvalidUTF8, TolerateInvalidUTF8:
		return true
	default:
		return false
	}
}

// Limit is the maximum accepted encoded size in bytes. Positive values are
// inclusive. NoLimit is the only unbounded value; zero is invalid so every
// call site must state its size posture explicitly.
type Limit int64

const NoLimit Limit = -1

var (
	ErrInvalidUTF8       = errors.New("strictjson: input is not valid UTF-8")
	ErrLimitExceeded     = errors.New("strictjson: input exceeds the byte limit")
	ErrInvalidLimit      = errors.New("strictjson: invalid byte limit")
	ErrInvalidUTF8Policy = errors.New("strictjson: invalid UTF-8 policy")
	ErrTrailingData      = errors.New("strictjson: trailing data after the JSON value")
)

// Decode decodes exactly one JSON value from data and rejects unknown object
// fields. The UTF-8 and size postures are required positional arguments.
func Decode(data []byte, dst any, policy UTF8Policy, max Limit) error {
	return decodeBytes(data, dst, policy, max, false)
}

// DecodeReader buffers and decodes exactly one JSON value from r and rejects
// unknown object fields. A bounded read consumes at most max+1 bytes so an
// over-limit input is rejected rather than silently truncated.
func DecodeReader(r io.Reader, dst any, policy UTF8Policy, max Limit) error {
	if err := validateArguments(policy, max); err != nil {
		return err
	}
	data, err := readAll(r, max)
	if err != nil {
		return err
	}
	return decodeBytes(data, dst, policy, NoLimit, false)
}

// DecodeAllowingUnknownFields decodes exactly one JSON value from data while
// retaining encoding/json's forward-compatible unknown-field behavior.
func DecodeAllowingUnknownFields(data []byte, dst any, policy UTF8Policy, max Limit) error {
	return decodeBytes(data, dst, policy, max, true)
}

// DecodeReaderAllowingUnknownFields is DecodeAllowingUnknownFields for a
// reader input. It exists for external response formats that may add fields.
func DecodeReaderAllowingUnknownFields(r io.Reader, dst any, policy UTF8Policy, max Limit) error {
	if err := validateArguments(policy, max); err != nil {
		return err
	}
	data, err := readAll(r, max)
	if err != nil {
		return err
	}
	return decodeBytes(data, dst, policy, NoLimit, true)
}

func decodeBytes(data []byte, dst any, policy UTF8Policy, max Limit, allowUnknown bool) error {
	if err := validateArguments(policy, max); err != nil {
		return err
	}
	if max != NoLimit && Limit(len(data)) > max {
		return fmt.Errorf("%w: got %d bytes, limit %d", ErrLimitExceeded, len(data), max)
	}
	if policy == RejectInvalidUTF8 && !utf8.Valid(data) {
		return ErrInvalidUTF8
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	if !allowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(new(json.RawMessage)); err == nil {
		return ErrTrailingData
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: %w", ErrTrailingData, err)
	}
	return nil
}

func readAll(r io.Reader, max Limit) ([]byte, error) {
	if err := validateLimit(max); err != nil {
		return nil, err
	}
	if max == NoLimit {
		return io.ReadAll(r)
	}
	if max == Limit(math.MaxInt64) {
		// A Go byte slice cannot contain more than MaxInt64 bytes. Treat the
		// largest valid explicit bound like an ordinary unbounded read rather
		// than overflowing the max+1 probe below.
		return io.ReadAll(r)
	}
	limited := &io.LimitedReader{R: r, N: int64(max) + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if Limit(len(data)) > max {
		return nil, fmt.Errorf("%w: limit %d", ErrLimitExceeded, max)
	}
	return data, nil
}

func validateLimit(max Limit) error {
	if max == NoLimit || max > 0 {
		return nil
	}
	return fmt.Errorf("%w: %d", ErrInvalidLimit, max)
}

func validateArguments(policy UTF8Policy, max Limit) error {
	if !policy.valid() {
		return fmt.Errorf("%w: %q", ErrInvalidUTF8Policy, policy)
	}
	if err := validateLimit(max); err != nil {
		return err
	}
	return nil
}
