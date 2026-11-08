// Package strictjson centralizes the daemon's single-value JSON decoding
// contract. It accepts exactly one JSON value, optionally rejects invalid
// UTF-8 before the decoder can replace it with U+FFFD, optionally rejects
// unknown object members, and enforces the caller's explicit byte limit.
// Under RejectInvalidUTF8 the decoder also refuses an escaped lone surrogate,
// which the byte check cannot see; that error is a jsontext.SyntacticError,
// not ErrInvalidUTF8.
//
// Decoding rejects duplicate object member names, including names that
// differ only in case but match one struct field. It runs on
// encoding/json/v2 with v1's options for name matching, merging, and
// encodings, and v2's error reporting, which stops at the first error and
// may leave the destination partly written. Boundaries that require
// canonical bytes keep that stronger gate around this package. The package
// is a standard-library-only leaf so every daemon lane can share the same
// delimiting decision without importing another lane's policy.
//
// Decision record: devlog/2026-11-08-1212-json-v2-strict-decoding.md.
package strictjson

import (
	"bytes"
	jsonv1 "encoding/json"
	"encoding/json/jsontext"
	json "encoding/json/v2"
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
	// ErrDuplicateName marks a duplicate object member name. The returned
	// error wraps both it and jsontext.ErrDuplicateName, so errors.Is matches
	// either.
	ErrDuplicateName = errors.New("strictjson: duplicate object member name")
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

// DecodeAllowingUnknownFields decodes exactly one JSON value from data and
// skips unknown object members, for formats that may add fields. A skipped
// member is still checked for duplicate names.
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

	// DefaultOptionsV1 must come first: it sets AllowDuplicateNames(true) and
	// AllowInvalidUTF8(true) itself, and a later option wins.
	options := json.JoinOptions(
		jsonv1.DefaultOptionsV1(),
		// v2 error reporting keeps io.ErrUnexpectedEOF on truncation and
		// exposes jsontext.ErrDuplicateName.
		jsonv1.ReportErrorsWithLegacySemantics(false),
		jsontext.AllowDuplicateNames(false),
		jsontext.AllowInvalidUTF8(policy == TolerateInvalidUTF8),
		json.RejectUnknownMembers(!allowUnknown),
	)
	decoder := jsontext.NewDecoder(bytes.NewReader(data), options)
	if err := json.UnmarshalDecode(decoder, dst, options); err != nil {
		if errors.Is(err, jsontext.ErrDuplicateName) {
			return fmt.Errorf("%w: %w", ErrDuplicateName, err)
		}
		return err
	}
	if _, err := decoder.ReadToken(); err == nil {
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
