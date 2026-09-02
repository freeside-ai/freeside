package specify_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func TestOutputGolden(t *testing.T) {
	out := specify.Output{Specification: &specify.Specification{
		Summary: "Add the missing lifecycle gate.",
		Body:    "# Objective\n\nRequire approval before implementation.",
		Addressals: []specify.Addressal{{
			CommentID: "request-refusal-tests", Response: "Added refusal tests.",
		}},
	}}
	body, err := specify.EncodeOutput(out)
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
		{"unknown", `{"fetch_requests":[],"specification":null,"extra":true}`, specify.ErrInvalidOutput},
		{"trailing", `{"fetch_requests":[],"specification":null}{}`, strictjson.ErrTrailingData},
		{"neither", `{"fetch_requests":[],"specification":null}`, specify.ErrInvalidOutput},
		{"both", `{"fetch_requests":[{"url":"https://example.com","purpose":"research"}],"specification":{"summary":"s","body":"b","addressals":[]}}`, specify.ErrInvalidOutput},
		{"reply and spec", `{"fetch_requests":[],"specification":{"summary":"s","body":"b","addressals":[]},"reply":"answer"}`, specify.ErrInvalidOutput},
		{"duplicate", `{"fetch_requests":[{"url":"https://example.com","purpose":"one"},{"url":"https://example.com","purpose":"two"}],"specification":null}`, specify.ErrInvalidOutput},
		{"decisions", `{"fetch_requests":[],"specification":null,"reply":null,"decisions":` + decisionsJSON(1) + `}`, nil},
		{"decisions and fetch", `{"fetch_requests":[{"url":"https://example.com","purpose":"research"}],"specification":null,"reply":null,"decisions":` + decisionsJSON(1) + `}`, specify.ErrInvalidOutput},
		{"decisions and spec", `{"fetch_requests":[],"specification":{"summary":"s","body":"b","addressals":[]},"reply":null,"decisions":` + decisionsJSON(1) + `}`, specify.ErrInvalidOutput},
		{"decisions and reply", `{"fetch_requests":[],"specification":null,"reply":"answer","decisions":` + decisionsJSON(1) + `}`, specify.ErrInvalidOutput},
		{"empty decisions", `{"fetch_requests":[],"specification":null,"reply":null,"decisions":[]}`, specify.ErrInvalidOutput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := specify.DecodeOutput([]byte(tc.body))
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

func TestDecodeLegacyAddressalOutputBindsAuthenticatedFeedback(t *testing.T) {
	body := []byte(`{"fetch_requests":[],"specification":{"summary":"Revised.","body":"# Specification","addressals":[{"comment":"Bound the request.","response":"Added a 1 MiB limit."}]},"reply":null}`)
	out, err := specify.DecodeLegacyAddressalOutput(body, map[string]string{
		"Bound the request.": "revise-spec",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Specification == nil || len(out.Specification.Addressals) != 1 ||
		out.Specification.Addressals[0].CommentID != "revise-spec" {
		t.Fatalf("legacy output = %+v, want authenticated comment id", out)
	}
	if _, err := specify.DecodeLegacyAddressalOutput(body, nil); !errors.Is(err, specify.ErrInvalidOutput) {
		t.Fatalf("unbound legacy output error = %v, want ErrInvalidOutput", err)
	}
	if _, err := specify.DecodeOutput(body); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("current decoder accepted legacy output: %v", err)
	}
}

func TestOutputRejectsCardinalityAndPresentationOverflow(t *testing.T) {
	requests := make([]specify.FetchRequest, specify.MaxFetchRequests+1)
	for i := range requests {
		requests[i] = specify.FetchRequest{URL: "https://example.com/" + strings.Repeat("x", i+1), Purpose: "test"}
	}
	if err := (specify.Output{FetchRequests: requests}).Validate(); !errors.Is(err, specify.ErrInvalidOutput) {
		t.Fatalf("fetch cardinality error = %v", err)
	}
	if err := (specify.Output{Specification: &specify.Specification{
		Summary: strings.Repeat("s", specify.MaxSummaryBytes+1), Body: "body",
		Addressals: []specify.Addressal{},
	}}).Validate(); !errors.Is(err, specify.ErrInvalidOutput) {
		t.Fatalf("summary bound error = %v", err)
	}
	reply := strings.Repeat("r", specify.MaxReplyBytes+1)
	if err := (specify.Output{Reply: &reply}).Validate(); !errors.Is(err, specify.ErrInvalidOutput) {
		t.Fatalf("reply bound error = %v", err)
	}
	addressals := make([]specify.Addressal, specify.MaxAddressals)
	for i := range addressals {
		addressals[i] = specify.Addressal{
			CommentID: strings.Repeat("c", specify.MaxAddressalTextBytes),
			Response:  strings.Repeat("r", specify.MaxAddressalTextBytes),
		}
	}
	if _, err := specify.EncodeOutput(specify.Output{Specification: &specify.Specification{
		Summary: strings.Repeat("s", specify.MaxSummaryBytes),
		Body:    strings.Repeat("b", 64<<10), Addressals: addressals,
	}}); !errors.Is(err, specify.ErrInvalidOutput) {
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
			out, err := specify.DecodeOutput([]byte(tc.body))
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
			_, err := specify.DecodeOutput([]byte(tc.body))
			if err == nil {
				t.Fatal("decode accepted a payload outside the single-fence tolerance")
			}
			if !strings.Contains(err.Error(), "output begins") {
				t.Fatalf("error %q lacks the raw-output prefix", err)
			}
		})
	}
}

func decisionFixture() domain.Decision {
	return domain.Decision{
		Question:    "Which retention period applies to exported logs?",
		WhyBlocking: "The specification cannot fix the schema without it.",
		Options: []domain.DecisionOption{
			{Label: "30 days", Tradeoffs: "Cheaper storage, shorter audit window."},
			{Label: "1 year", Tradeoffs: "Longer audit window, higher storage cost."},
		},
		Recommendation: "30 days",
	}
}

func decisionsJSON(count int) string {
	decisions := make([]domain.Decision, count)
	for i := range decisions {
		decisions[i] = decisionFixture()
	}
	body, err := json.Marshal(decisions)
	if err != nil {
		panic(err)
	}
	return string(body)
}

func TestOutputNeedsDecisionGolden(t *testing.T) {
	body, err := specify.EncodeOutput(specify.Output{Decisions: []domain.Decision{decisionFixture()}})
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatal(err)
	}
	indented, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "output_needs_decision", append(indented, '\n'))
}

// TestDecodeOutputNeedsDecisionLimits pins the decision limits at the
// decoder: the counts and bounds the specifier prompt states.
func TestDecodeOutputNeedsDecisionLimits(t *testing.T) {
	withOptions := func(n int) domain.Decision {
		d := decisionFixture()
		d.Options = nil
		for i := 0; i < n; i++ {
			d.Options = append(d.Options, domain.DecisionOption{
				Label: strings.Repeat("x", i+1), Tradeoffs: "tradeoffs",
			})
		}
		d.Recommendation = "x"
		return d
	}
	noMatch := decisionFixture()
	noMatch.Recommendation = "never"
	over := decisionFixture()
	over.Question = strings.Repeat("q", domain.MaxDecisionTextBytes+1)

	cases := []struct {
		name      string
		decisions []domain.Decision
		accepted  bool
	}{
		{"one decision", []domain.Decision{decisionFixture()}, true},
		{"eight decisions", make([]domain.Decision, domain.MaxDecisionsPerResult), true},
		{"nine decisions", make([]domain.Decision, domain.MaxDecisionsPerResult+1), false},
		{"zero options", []domain.Decision{withOptions(0)}, false},
		{"one option", []domain.Decision{withOptions(1)}, false},
		{"six options", []domain.Decision{withOptions(domain.MaxDecisionOptions)}, true},
		{"seven options", []domain.Decision{withOptions(domain.MaxDecisionOptions + 1)}, false},
		{"recommendation matches no option", []domain.Decision{noMatch}, false},
		{"text over 4 KiB", []domain.Decision{over}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i := range tc.decisions {
				if tc.decisions[i].Question == "" {
					tc.decisions[i] = decisionFixture()
				}
			}
			body, err := json.Marshal(specify.Output{Decisions: tc.decisions})
			if err != nil {
				t.Fatal(err)
			}
			out, err := specify.DecodeOutput(body)
			if tc.accepted {
				if err != nil {
					t.Fatalf("DecodeOutput() = %v, want accepted", err)
				}
				if len(out.Decisions) != len(tc.decisions) {
					t.Fatalf("decoded %d decisions, want %d", len(out.Decisions), len(tc.decisions))
				}
				return
			}
			if !errors.Is(err, specify.ErrInvalidOutput) || !errors.Is(err, domain.ErrDecisionInvalid) {
				t.Fatalf("DecodeOutput() = %v, want ErrInvalidOutput wrapping ErrDecisionInvalid", err)
			}
		})
	}
}
