package strictjson_test

import (
	"encoding/json"
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

type fixture struct {
	Known string `json:"known"`
}

func TestUTF8PolicyRegistration(t *testing.T) {
	want := []strictjson.UTF8Policy{
		strictjson.RejectInvalidUTF8,
		strictjson.TolerateInvalidUTF8,
	}
	if !slices.Equal(strictjson.AllUTF8Policies, want) {
		t.Fatalf("AllUTF8Policies = %v, want %v", strictjson.AllUTF8Policies, want)
	}
}

func TestDecodeContract(t *testing.T) {
	valid := `{"known":"value"}`
	tests := []struct {
		name         string
		input        string
		policy       strictjson.UTF8Policy
		limit        strictjson.Limit
		allowUnknown bool
		reader       bool
		want         fixture
		wantErr      error
	}{
		{name: "one value", input: valid, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, want: fixture{Known: "value"}},
		{name: "reader", input: valid, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, reader: true, want: fixture{Known: "value"}},
		{name: "trailing whitespace", input: valid + " \n\t", policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, want: fixture{Known: "value"}},
		{name: "second value", input: valid + ` {}`, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, wantErr: strictjson.ErrTrailingData},
		{name: "bare trailing bracket", input: valid + `]`, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, wantErr: strictjson.ErrTrailingData},
		{name: "bare trailing brace", input: valid + `}`, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, wantErr: strictjson.ErrTrailingData},
		{name: "trailing garbage", input: valid + `xyz`, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, wantErr: strictjson.ErrTrailingData},
		{name: "unknown field rejected", input: `{"known":"value","extra":true}`, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, wantErr: jsonv2.ErrUnknownName},
		{name: "unknown field allowed", input: `{"known":"value","extra":true}`, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, allowUnknown: true, want: fixture{Known: "value"}},
		{name: "reader unknown field allowed", input: `{"known":"value","extra":true}`, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, allowUnknown: true, reader: true, want: fixture{Known: "value"}},
		{name: "invalid UTF-8 rejected", input: "{\"known\":\"\xff\"}", policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, wantErr: strictjson.ErrInvalidUTF8},
		{name: "invalid UTF-8 tolerated", input: "{\"known\":\"\xff\"}", policy: strictjson.TolerateInvalidUTF8, limit: strictjson.NoLimit, want: fixture{Known: "�"}},
		{name: "duplicate keys rejected", input: `{"known":"first","known":"last"}`, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, wantErr: strictjson.ErrDuplicateName},
		{name: "empty input", input: "", policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, wantErr: io.EOF},
		{name: "truncated input", input: `{"known":"value"`, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, wantErr: io.ErrUnexpectedEOF},
		{name: "at byte limit", input: valid, policy: strictjson.RejectInvalidUTF8, limit: strictjson.Limit(len(valid)), want: fixture{Known: "value"}},
		{name: "over byte limit", input: valid, policy: strictjson.RejectInvalidUTF8, limit: strictjson.Limit(len(valid) - 1), wantErr: strictjson.ErrLimitExceeded},
		{name: "malformed prefix over byte limit", input: `{not-json`, policy: strictjson.RejectInvalidUTF8, limit: 1, wantErr: strictjson.ErrLimitExceeded},
		{name: "reader over byte limit", input: valid, policy: strictjson.RejectInvalidUTF8, limit: strictjson.Limit(len(valid) - 1), reader: true, wantErr: strictjson.ErrLimitExceeded},
		{name: "reader malformed prefix over byte limit", input: `{not-json`, policy: strictjson.RejectInvalidUTF8, limit: 1, reader: true, wantErr: strictjson.ErrLimitExceeded},
		{name: "explicit no limit", input: valid, policy: strictjson.RejectInvalidUTF8, limit: strictjson.NoLimit, reader: true, want: fixture{Known: "value"}},
		{name: "maximum explicit limit", input: valid, policy: strictjson.RejectInvalidUTF8, limit: strictjson.Limit(math.MaxInt64), reader: true, want: fixture{Known: "value"}},
		{name: "zero limit invalid", input: valid, policy: strictjson.RejectInvalidUTF8, limit: 0, wantErr: strictjson.ErrInvalidLimit},
		{name: "negative limit invalid", input: valid, policy: strictjson.RejectInvalidUTF8, limit: -2, wantErr: strictjson.ErrInvalidLimit},
		{name: "zero UTF-8 policy invalid", input: valid, policy: "", limit: strictjson.NoLimit, wantErr: strictjson.ErrInvalidUTF8Policy},
		{name: "unknown UTF-8 policy invalid", input: valid, policy: "unknown", limit: strictjson.NoLimit, wantErr: strictjson.ErrInvalidUTF8Policy},
		{name: "invalid UTF-8 policy precedes size gate", input: valid, policy: "", limit: 1, wantErr: strictjson.ErrInvalidUTF8Policy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got fixture
			var err error
			switch {
			case tt.reader && tt.allowUnknown:
				err = strictjson.DecodeReaderAllowingUnknownFields(strings.NewReader(tt.input), &got, tt.policy, tt.limit)
			case tt.reader:
				err = strictjson.DecodeReader(strings.NewReader(tt.input), &got, tt.policy, tt.limit)
			case tt.allowUnknown:
				err = strictjson.DecodeAllowingUnknownFields([]byte(tt.input), &got, tt.policy, tt.limit)
			default:
				err = strictjson.Decode([]byte(tt.input), &got, tt.policy, tt.limit)
			}

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("Decode() error = %v", err)
				}
				if got != tt.want {
					t.Fatalf("Decode() = %#v, want %#v", got, tt.want)
				}
				return
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Decode() error = %v, want errors.Is(_, %v)", err, tt.wantErr)
			}
		})
	}
}

// v1 decoded an escaped lone surrogate to U+FFFD under either policy. v2
// rejects it under RejectInvalidUTF8, which the raw-byte pre-check cannot
// see, and still replaces it under TolerateInvalidUTF8.
func TestDecodeEscapedLoneSurrogate(t *testing.T) {
	input := []byte(`{"known":"\ud800"}`)
	var rejected fixture
	err := strictjson.Decode(input, &rejected, strictjson.RejectInvalidUTF8, strictjson.NoLimit)
	var syntactic *jsontext.SyntacticError
	if !errors.As(err, &syntactic) {
		t.Fatalf("Decode(RejectInvalidUTF8) error = %v, want a jsontext.SyntacticError", err)
	}
	var tolerated fixture
	if err := strictjson.Decode(input, &tolerated, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		t.Fatalf("Decode(TolerateInvalidUTF8) error = %v", err)
	}
	if tolerated.Known != "\uFFFD" {
		t.Fatalf("Decode(TolerateInvalidUTF8) = %q, want U+FFFD", tolerated.Known)
	}
}

type duplicateFixture struct {
	Known string            `json:"known"`
	Map   map[string]string `json:"map"`
	Raw   json.RawMessage   `json:"raw"`
}

func TestDecodeRejectsDuplicateNames(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		allowUnknown bool
	}{
		{name: "exact duplicate", input: `{"known":"a","known":"b"}`},
		{name: "case variant of one field", input: `{"known":"a","Known":"b"}`},
		// U+212A KELVIN SIGN folds to "k" under v1's case-insensitive matching.
		{name: "Kelvin-sign variant of one field", input: "{\"known\":\"a\",\"\u212Anown\":\"b\"}"},
		{name: "duplicate inside a map", input: `{"map":{"a":"1","a":"2"}}`},
		{name: "duplicate inside a raw message", input: `{"raw":{"a":1,"a":2}}`},
		{name: "duplicate inside a skipped unknown member", input: `{"known":"a","extra":{"a":1,"a":2}}`, allowUnknown: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got duplicateFixture
			var err error
			if tt.allowUnknown {
				err = strictjson.DecodeAllowingUnknownFields([]byte(tt.input), &got, strictjson.RejectInvalidUTF8, strictjson.NoLimit)
			} else {
				err = strictjson.Decode([]byte(tt.input), &got, strictjson.RejectInvalidUTF8, strictjson.NoLimit)
			}
			if !errors.Is(err, strictjson.ErrDuplicateName) {
				t.Fatalf("Decode() error = %v, want errors.Is(_, strictjson.ErrDuplicateName)", err)
			}
			if !errors.Is(err, jsontext.ErrDuplicateName) {
				t.Fatalf("Decode() error = %v, want errors.Is(_, jsontext.ErrDuplicateName)", err)
			}
		})
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

type countingReader struct {
	reads int
}

func (r *countingReader) Read([]byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

func TestDecodeReaderValidatesPosturesBeforeRead(t *testing.T) {
	tests := []struct {
		name         string
		policy       strictjson.UTF8Policy
		limit        strictjson.Limit
		allowUnknown bool
		wantErr      error
	}{
		{name: "strict invalid UTF-8 policy", policy: "", limit: strictjson.NoLimit, wantErr: strictjson.ErrInvalidUTF8Policy},
		{name: "permissive invalid UTF-8 policy", policy: "", limit: strictjson.NoLimit, allowUnknown: true, wantErr: strictjson.ErrInvalidUTF8Policy},
		{name: "strict invalid byte limit", policy: strictjson.RejectInvalidUTF8, limit: 0, wantErr: strictjson.ErrInvalidLimit},
		{name: "permissive invalid byte limit", policy: strictjson.RejectInvalidUTF8, limit: 0, allowUnknown: true, wantErr: strictjson.ErrInvalidLimit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &countingReader{}
			var got fixture
			var err error
			if tt.allowUnknown {
				err = strictjson.DecodeReaderAllowingUnknownFields(reader, &got, tt.policy, tt.limit)
			} else {
				err = strictjson.DecodeReader(reader, &got, tt.policy, tt.limit)
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("DecodeReader() error = %v, want errors.Is(_, %v)", err, tt.wantErr)
			}
			if reader.reads != 0 {
				t.Fatalf("DecodeReader() performed %d reads before rejecting its posture", reader.reads)
			}
		})
	}
}

func TestDecodeReaderPropagatesReadError(t *testing.T) {
	var got fixture
	err := strictjson.DecodeReader(failingReader{}, &got, strictjson.RejectInvalidUTF8, strictjson.NoLimit)
	if err == nil || err.Error() != "read failed" {
		t.Fatalf("DecodeReader() error = %v, want read failed", err)
	}
}
