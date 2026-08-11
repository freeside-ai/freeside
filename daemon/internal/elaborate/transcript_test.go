package elaborate_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
)

func TestTranscriptRoundTrip(t *testing.T) {
	want := elaborate.Output{Specification: &elaborate.Specification{
		Summary: "Ready.", Body: "# Specification", Addressals: []elaborate.Addressal{},
	}}
	body, err := elaborate.EncodeTranscript(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := elaborate.DecodeTranscript(strings.NewReader(
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
		{"missing", `{"type":"assistant"}` + "\n", elaborate.ErrTranscriptResultMissing},
		{"duplicate", `{"type":"result","subtype":"success","is_error":false,"result":"{}"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":"{}"}` + "\n", elaborate.ErrInvalidOutput},
		{"failed", `{"type":"result","subtype":"error","is_error":true,"result":"{}"}` + "\n", elaborate.ErrInvalidOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := elaborate.DecodeTranscript(strings.NewReader(tc.body))
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
