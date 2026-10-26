package specify_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/specify"
)

func TestTranscriptToleratesBacktickEscape(t *testing.T) {
	// The transcript is valid JSON; only the model's nested payload is malformed.
	payload := "{\"specification\":{\"summary\":\"Validate header names.\",\"body\":\"RFC 7230 token characters include \\`.\",\"addressals\":[]}}"
	record, err := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := specify.DecodeTranscript(strings.NewReader(string(record) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if out.Specification == nil || out.Specification.Body != "RFC 7230 token characters include `." {
		t.Fatalf("specification = %+v", out.Specification)
	}
}

func TestTranscriptRoundTrip(t *testing.T) {
	want := specify.Output{Specification: &specify.Specification{
		Summary: "Ready.", Body: "# Specification", Addressals: []specify.Addressal{},
	}}
	body, err := specify.EncodeTranscript(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := specify.DecodeTranscript(strings.NewReader(
		`{"type":"system","session_id":"session"}` + "\n" + string(body),
	))
	if err != nil {
		t.Fatal(err)
	}
	if got.Specification == nil || got.Specification.Body != want.Specification.Body {
		t.Fatalf("output = %+v, want %+v", got, want)
	}
}

func TestTranscriptRejectsMissingDuplicateAndUnsuccessfulResults(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"missing", `{"type":"assistant"}` + "\n", specify.ErrTranscriptResultMissing},
		{"duplicate", `{"type":"result","subtype":"success","is_error":false,"result":"{}"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"{}"}` + "\n", specify.ErrInvalidOutput},
		{"failed", `{"type":"result","subtype":"error","is_error":true,"result":"{}"}` + "\n", specify.ErrInvalidOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := specify.DecodeTranscript(strings.NewReader(tc.body))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// A schema-constrained launch carries the decision in structured_output, so a
// prose preamble in result is not the payload (#2314).
func TestTranscriptDecodesStructuredOutput(t *testing.T) {
	cases := []struct {
		name, record string
	}{
		{"prose result", `{"type":"result","subtype":"success","is_error":false,` +
			`"result":"I need the issue first. Requesting it.",` +
			`"structured_output":{"fetch_requests":[{"url":"https://github.com/o/r/issues/1","purpose":"Read the issue."}]}}`},
		{"no result", `{"type":"result","subtype":"success","is_error":false,` +
			`"structured_output":{"fetch_requests":[{"url":"https://github.com/o/r/issues/1","purpose":"Read the issue."}]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := specify.DecodeTranscript(strings.NewReader(
				`{"type":"assistant","message":{"content":[{"type":"text","text":"I need the issue first."}]}}` + "\n" +
					tc.record + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			if len(out.FetchRequests) != 1 || out.FetchRequests[0].URL != "https://github.com/o/r/issues/1" {
				t.Fatalf("output = %+v", out)
			}
		})
	}
}

// structured_output passes the same gate as result text, and a record that
// carries no usable payload fails closed.
func TestTranscriptRejectsInvalidStructuredOutput(t *testing.T) {
	const prose = "The work item is only a URL. Requesting it."
	success := func(fields string) string {
		return `{"type":"result","subtype":"success","is_error":false` + fields + "}\n"
	}
	cases := []struct {
		name, body string
		want       error  // nil when only the bounded prefix is checked
		prefix     string // text the error must preserve
	}{
		{"null falls back to prose result", success(`,"result":"` + prose + `","structured_output":null`), nil, prose},
		{"string", success(`,"result":"{}","structured_output":"` + prose + `"`), nil, prose},
		{"array", success(`,"result":"{}","structured_output":[{"reply":"R"}]`), nil, `[{`},
		{"relative URL", success(`,"structured_output":{"fetch_requests":[{"url":"/o/r/issues/1","purpose":"P"}]}`), specify.ErrInvalidOutput, ""},
		{"two forms", success(`,"structured_output":{"reply":"R","fetch_requests":[{"url":"https://ex.com","purpose":"P"}]}`), specify.ErrInvalidOutput, ""},
		{"unknown field", success(`,"structured_output":{"reply":"R","note":"N"}`), nil, `{"reply"`},
		{"neither member", success(``), specify.ErrInvalidOutput, ""},
		{"failed with structured output", `{"type":"result","subtype":"error","is_error":true,"structured_output":{"reply":"R"}}` + "\n", specify.ErrInvalidOutput, ""},
		{"prose after result", success(`,"result":"{\"reply\":\"R\"} Done."`), nil, `{"reply":"R"} Done.`},
		{"two result objects", success(`,"result":"{\"reply\":\"R\"}{\"reply\":\"S\"}"`), nil, `{"reply":"R"}{"reply":"S"}`},
		{"truncated result", success(`,"result":"{\"reply\":\"R"`), nil, `{"reply":"R`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := specify.DecodeTranscript(strings.NewReader(tc.body))
			if err == nil {
				t.Fatal("decoded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			// The error quotes the prefix with %q.
			if quoted := strconv.Quote(tc.prefix); !strings.Contains(err.Error(), quoted[1:len(quoted)-1]) {
				t.Fatalf("error = %v, want prefix %q", err, tc.prefix)
			}
		})
	}
}

// A backslash-backtick is not valid JSON, so it cannot reach DecodeOutput's
// repair through structured_output: the record itself fails to decode.
func TestTranscriptStructuredOutputGetsNoBacktickRepair(t *testing.T) {
	body := `{"type":"result","subtype":"success","is_error":false,"structured_output":{"reply":"a \` + "`" + `b"}}` + "\n"
	if _, err := specify.DecodeTranscript(strings.NewReader(body)); err == nil {
		t.Fatal("decoded a structured_output with an invalid escape")
	}
}
