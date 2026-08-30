package elaborate_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func TestOutputGolden(t *testing.T) {
	out := elaborate.Output{Specification: &elaborate.Specification{
		Summary: "Add the missing lifecycle gate.",
		Body:    "# Objective\n\nRequire approval before implementation.",
		Addressals: []elaborate.Addressal{{
			Comment: "Show the refusal path.", Response: "Added refusal tests.",
		}},
	}}
	body, err := elaborate.EncodeOutput(out)
	if err != nil {
		t.Fatal(err)
	}
	var indented []byte
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	indented, err = json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "output", append(indented, '\n'))
}

func TestDecodeOutputStrictAndExclusive(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"fetch", `{"fetch_requests":[{"url":"https://example.com/a","purpose":"confirm behavior"}],"specification":null}`, nil},
		{"reply", `{"fetch_requests":[],"specification":null,"reply":"The approval remains open."}`, nil},
		{"unknown", `{"fetch_requests":[],"specification":null,"extra":true}`, elaborate.ErrInvalidOutput},
		{"trailing", `{"fetch_requests":[],"specification":null}{}`, strictjson.ErrTrailingData},
		{"neither", `{"fetch_requests":[],"specification":null}`, elaborate.ErrInvalidOutput},
		{"both", `{"fetch_requests":[{"url":"https://example.com","purpose":"research"}],"specification":{"summary":"s","body":"b","addressals":[]}}`, elaborate.ErrInvalidOutput},
		{"reply and spec", `{"fetch_requests":[],"specification":{"summary":"s","body":"b","addressals":[]},"reply":"answer"}`, elaborate.ErrInvalidOutput},
		{"duplicate", `{"fetch_requests":[{"url":"https://example.com","purpose":"one"},{"url":"https://example.com","purpose":"two"}],"specification":null}`, elaborate.ErrInvalidOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := elaborate.DecodeOutput([]byte(tc.body))
			if tc.want == nil && err != nil {
				t.Fatal(err)
			}
			if tc.want != nil && !errors.Is(err, tc.want) &&
				(tc.name != "unknown" || !strings.Contains(err.Error(), "unknown field")) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestOutputRejectsCardinalityAndPresentationOverflow(t *testing.T) {
	requests := make([]elaborate.FetchRequest, elaborate.MaxFetchRequests+1)
	for i := range requests {
		requests[i] = elaborate.FetchRequest{URL: "https://example.com/" + strings.Repeat("x", i+1), Purpose: "test"}
	}
	if err := (elaborate.Output{FetchRequests: requests}).Validate(); !errors.Is(err, elaborate.ErrInvalidOutput) {
		t.Fatalf("fetch cardinality error = %v", err)
	}
	if err := (elaborate.Output{Specification: &elaborate.Specification{
		Summary: strings.Repeat("s", elaborate.MaxSummaryBytes+1), Body: "body",
		Addressals: []elaborate.Addressal{},
	}}).Validate(); !errors.Is(err, elaborate.ErrInvalidOutput) {
		t.Fatalf("summary bound error = %v", err)
	}
	reply := strings.Repeat("r", elaborate.MaxReplyBytes+1)
	if err := (elaborate.Output{Reply: &reply}).Validate(); !errors.Is(err, elaborate.ErrInvalidOutput) {
		t.Fatalf("reply bound error = %v", err)
	}
	addressals := make([]elaborate.Addressal, elaborate.MaxAddressals)
	for i := range addressals {
		addressals[i] = elaborate.Addressal{
			Comment:  strings.Repeat("c", elaborate.MaxAddressalTextBytes),
			Response: strings.Repeat("r", elaborate.MaxAddressalTextBytes),
		}
	}
	if _, err := elaborate.EncodeOutput(elaborate.Output{Specification: &elaborate.Specification{
		Summary: strings.Repeat("s", elaborate.MaxSummaryBytes),
		Body:    strings.Repeat("b", 64<<10), Addressals: addressals,
	}}); !errors.Is(err, elaborate.ErrInvalidOutput) {
		t.Fatalf("aggregate output bound error = %v, want ErrInvalidOutput", err)
	}
}

// TestDecodeOutputToleratesSingleFence enumerates the fence-tolerance input
// space (issue #780): exactly one whole-payload Markdown fence pair decodes;
// every other presentation defect still fails strict decode and carries a
// bounded raw-output prefix for diagnosis.
func TestDecodeOutputToleratesSingleFence(t *testing.T) {
	const inner = `{"fetch_requests":[{"url":"https://example.com/a","purpose":"confirm behavior"}],"specification":null}`
	accepted := []struct{ name, body string }{
		{"bare fence", "```\n" + inner + "\n```"},
		{"json tag", "```json\n" + inner + "\n```"},
		{"uppercase tag", "```JSON\n" + inner + "\n```"},
		{"surrounding whitespace", "\n\n  ```json\n" + inner + "\n```  \n"},
		{"indented closing fence", "```json\n" + inner + "\n  ```"},
		{"unfenced unchanged", inner},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			out, err := elaborate.DecodeOutput([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if len(out.FetchRequests) != 1 {
				t.Fatalf("fetch_requests = %d, want 1", len(out.FetchRequests))
			}
		})
	}
	rejected := []struct{ name, body string }{
		{"prose before fence", "Here you go:\n```json\n" + inner + "\n```"},
		{"prose after fence", "```json\n" + inner + "\n```\nHope this helps!"},
		{"unclosed fence", "```json\n" + inner},
		{"closing fence only", inner + "\n```"},
		{"double-wrapped fence", "```\n```json\n" + inner + "\n```\n```"},
		{"tag with content", "```json {\"x\":1}\n" + inner + "\n```"},
		{"single line fence", "```" + inner + "```"},
		{"truncated inside fence", "```json\n" + inner[:40]},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			_, err := elaborate.DecodeOutput([]byte(tc.body))
			if err == nil {
				t.Fatal("decode accepted a payload outside the single-fence tolerance")
			}
			if !strings.Contains(err.Error(), "output begins") {
				t.Fatalf("error %q lacks the raw-output prefix", err)
			}
		})
	}
}
