package specify_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/specify"
)

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
