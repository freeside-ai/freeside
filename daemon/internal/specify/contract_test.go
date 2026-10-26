package specify_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func TestSpecificationTitle(t *testing.T) {
	out, err := specify.DecodeOutput([]byte(`{"specification":{"title":"  Bound requests  ","summary":"Summary","body":"# Bound requests","addressals":[]}}`))
	if err != nil || out.Specification == nil || out.Specification.Title == nil || *out.Specification.Title != "Bound requests" {
		t.Fatalf("title decode = %+v, %v", out, err)
	}
	for _, title := range []string{"", " untrimmed ", "two\nlines", "two\rlines", "\xff", strings.Repeat("x", 4<<10+1)} {
		out.Specification.Title = &title
		if err := out.Validate(); !errors.Is(err, specify.ErrInvalidOutput) {
			t.Fatalf("invalid title accepted: %q, %v", title, err)
		}
	}
	long := strings.Repeat("界", 61)
	out.Specification.Title = &long
	if _, err := specify.EncodeOutput(out); err != nil {
		t.Fatalf("long title rejected the entire specification: %v", err)
	}
}

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

// The launch inlines the schema as one shell argument, and the model fills
// only the fields it names, so it must parse, stay on one line, and name
// exactly the JSON fields DecodeOutput accepts at every level.
func TestOutputJSONSchemaMirrorsOutput(t *testing.T) {
	if strings.ContainsAny(specify.OutputJSONSchema, "\n\r") {
		t.Fatal("schema carries a line break")
	}
	var schema map[string]any
	if err := json.Unmarshal([]byte(specify.OutputJSONSchema), &schema); err != nil {
		t.Fatalf("schema does not parse: %v", err)
	}
	assertSchemaMirrors(t, "$", schema, reflect.TypeFor[specify.Output]())
}

func assertSchemaMirrors(t *testing.T, at string, node map[string]any, typ reflect.Type) {
	t.Helper()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Slice:
		items, ok := node["items"].(map[string]any)
		if !schemaAllowsType(node, "array") || !ok {
			t.Fatalf("%s: %s needs an array schema with items, got %v", at, typ, node)
		}
		assertSchemaMirrors(t, at+"[]", items, typ.Elem())
	case reflect.Struct:
		properties, ok := node["properties"].(map[string]any)
		if !schemaAllowsType(node, "object") || !ok || node["additionalProperties"] != false {
			t.Fatalf("%s: %s needs a closed object schema, got %v", at, typ, node)
		}
		tags := map[string]reflect.Type{}
		for field := range typ.Fields() {
			tags[strings.Split(field.Tag.Get("json"), ",")[0]] = field.Type
		}
		if len(tags) != len(properties) {
			t.Fatalf("%s: schema names %d properties, %s has %d fields", at, len(properties), typ, len(tags))
		}
		for name, fieldType := range tags {
			property, ok := properties[name].(map[string]any)
			if !ok {
				t.Fatalf("%s: schema lacks %s's field %q", at, typ, name)
			}
			assertSchemaMirrors(t, at+"."+name, property, fieldType)
		}
		required, _ := node["required"].([]any)
		for _, name := range required {
			if _, ok := tags[name.(string)]; !ok {
				t.Fatalf("%s: required %v is not a field of %s", at, name, typ)
			}
		}
	case reflect.String:
		if !schemaAllowsType(node, "string") {
			t.Fatalf("%s: string field needs a string schema, got %v", at, node)
		}
	default:
		t.Fatalf("%s: no schema rule for kind %s", at, typ.Kind())
	}
}

func schemaAllowsType(node map[string]any, want string) bool {
	switch typ := node["type"].(type) {
	case string:
		return typ == want
	case []any:
		for _, member := range typ {
			if member == want {
				return true
			}
		}
	}
	return false
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

func TestDecodeOutputToleratesBacktickEscape(t *testing.T) {
	spec := func(body string) string {
		return "{\"specification\":{\"summary\":\"s\",\"body\":\"" + body + "\",\"addressals\":[]}}"
	}
	accepted := []struct{ name, body, want string }{
		{"body", "token \\` character", "token ` character"},
		{"escaped backslash", "token \\\\` character", "token \\` character"},
		{"escaped backslash and backtick", "token \\\\\\` character", "token \\` character"},
		{"escaped quote", "say \\\"\\`\\\"", "say \"`\""},
		{"unicode", "界\\`", "界`"},
		{"trailing whitespace", "token \\` \\n", "token `"},
		{"valid escapes", "\\\"\\\\\\/\\b\\f\\n\\r\\t\\u0061\\u0060x", "\"\\/\b\f\n\r\ta`x"},
	}
	for _, tc := range accepted {
		t.Run(tc.name, func(t *testing.T) {
			for _, fenced := range []bool{false, true} {
				payload := spec(tc.body)
				if fenced {
					payload = "```json\n" + payload + "\n```"
				}
				out, err := specify.DecodeOutput([]byte(payload))
				if err != nil {
					t.Fatal(err)
				}
				if out.Specification.Body != tc.want {
					t.Fatalf("body = %q, want %q", out.Specification.Body, tc.want)
				}
			}
		})
	}
	t.Run("all specification prose", func(t *testing.T) {
		payload := "{\"specification\":{\"title\":\"a\\`\",\"summary\":\"b\\`\",\"body\":\"c\\`\",\"addressals\":[{\"comment_id\":\"c1\",\"response\":\"d\\`\"}]}}"
		out, err := specify.DecodeOutput([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		s := out.Specification
		if *s.Title != "a`" || s.Summary != "b`" || s.Body != "c`" || s.Addressals[0].Response != "d`" {
			t.Fatalf("prose = %+v", s)
		}
	})
	t.Run("reply", func(t *testing.T) {
		out, err := specify.DecodeOutput([]byte("{\"reply\":\"Use \\`code\\`.\"}"))
		if err != nil || out.Reply == nil || *out.Reply != "Use `code`." {
			t.Fatalf("reply = %+v, %v", out, err)
		}
	})
	malformed := spec("token \\` character")
	rejected := []struct{ name, payload string }{
		{"truncated", malformed[:len(malformed)-1]},
		{"second object", malformed + "{}"},
		{"unknown field", strings.Replace(malformed, "\"summary\"", "\"unknown\"", 1)},
		{"invalid unicode", spec("\\`\\uZZZZ")},
		{"invalid UTF-8", spec("\\`\xff")},
		{"regex escape", spec("\\`\\d")},
		{"quote escape", spec("\\`\\'")},
		{"dollar escape", spec("\\`\\$")},
		{"outside string", malformed + "\\`"},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			_, err := specify.DecodeOutput([]byte(tc.payload))
			if err == nil || !strings.Contains(err.Error(), "output begins") {
				t.Fatalf("error = %v, want failure with raw-output prefix", err)
			}
			prefix := tc.payload
			if len(prefix) > 80 {
				prefix = prefix[:80] + "…"
			}
			if !strings.Contains(err.Error(), "output begins "+strconv.Quote(prefix)) {
				t.Fatalf("error lost the bounded raw prefix: %v", err)
			}
		})
	}
	for _, tc := range []struct{ name, payload string }{
		{"fetch URL", "{\"fetch_requests\":[{\"url\":\"https://example.com/\\`\",\"purpose\":\"research\"}]}"},
		{"comment ID", "{\"specification\":{\"summary\":\"s\",\"body\":\"b\",\"addressals\":[{\"comment_id\":\"c\\`\",\"response\":\"r\"}]}}"},
		{"bare URL with repaired purpose", "{\"fetch_requests\":[{\"url\":\"https://example.com/`\",\"purpose\":\"research \\`\"}]}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := specify.DecodeOutput([]byte(tc.payload)); !errors.Is(err, specify.ErrInvalidOutput) {
				t.Fatalf("identifier error = %v, want ErrInvalidOutput", err)
			}
		})
	}
}

func TestDecodeLegacyAddressalOutputToleratesBacktickEscape(t *testing.T) {
	payload := "{\"specification\":{\"summary\":\"s\",\"body\":\"Use \\`code\\`.\",\"addressals\":[{\"comment\":\"Use \\`code\\`.\",\"response\":\"Added \\`code\\`.\"}]}}"
	out, err := specify.DecodeLegacyAddressalOutput([]byte(payload), map[string]string{"Use `code`.": "c`"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Specification.Body != "Use `code`." || out.Specification.Addressals[0].CommentID != "c`" ||
		out.Specification.Addressals[0].Response != "Added `code`." {
		t.Fatalf("legacy specification = %+v", out.Specification)
	}
	for _, payload := range []string{
		"{\"fetch_requests\":[{\"url\":\"https://example.com/\\`\",\"purpose\":\"research\"}]}",
		"{\"reply\":\"\\`\\d\"}",
		"{\"reply\":\"\\`\"}{}",
	} {
		if _, err := specify.DecodeLegacyAddressalOutput([]byte(payload), nil); err == nil {
			t.Fatalf("legacy decoder accepted %q", payload)
		}
	}
}

func TestBacktickEscapePreservesInputLimitAndValidIdentifiers(t *testing.T) {
	for _, decode := range []func([]byte) (specify.Output, error){
		specify.DecodeOutput,
		func(data []byte) (specify.Output, error) {
			return specify.DecodeLegacyAddressalOutput(data, nil)
		},
	} {
		payload := "{\"reply\":\"\\`\"}"
		payload += strings.Repeat(" ", int(specify.MaxOutputBytes)+1-len(payload))
		if _, err := decode([]byte(payload)); !errors.Is(err, strictjson.ErrLimitExceeded) {
			t.Fatalf("oversized input error = %v, want ErrLimitExceeded", err)
		}
		payload = "{\"fetch_requests\":[{\"url\":\"https://example.com/`\",\"purpose\":\"research\"}]}"
		out, err := decode([]byte(payload))
		if err != nil || len(out.FetchRequests) != 1 || out.FetchRequests[0].URL != "https://example.com/`" {
			t.Fatalf("valid identifier = %+v, %v", out, err)
		}
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

// TestDecodeOutputTrimsProse pins the whitespace tolerance (issue #1123): the
// prose fields decode trimmed, an all-whitespace field is still empty and
// rejected, and the identifier fields stay strict.
func TestDecodeOutputTrimsProse(t *testing.T) {
	t.Run("specification prose", func(t *testing.T) {
		out, err := specify.DecodeOutput([]byte(`{"fetch_requests":[],"specification":{"summary":"  Gate the lifecycle.  ","body":"# Objective\n\nRequire approval.\n","addressals":[{"comment_id":"c1","response":"Addressed.\n"}]}}`))
		if err != nil {
			t.Fatal(err)
		}
		if out.Specification.Summary != "Gate the lifecycle." {
			t.Fatalf("summary = %q", out.Specification.Summary)
		}
		if out.Specification.Body != "# Objective\n\nRequire approval." {
			t.Fatalf("body = %q", out.Specification.Body)
		}
		if out.Specification.Addressals[0].Response != "Addressed." {
			t.Fatalf("response = %q", out.Specification.Addressals[0].Response)
		}
	})
	t.Run("reply", func(t *testing.T) {
		out, err := specify.DecodeOutput([]byte(`{"fetch_requests":[],"specification":null,"reply":"\n Not yet. \n"}`))
		if err != nil {
			t.Fatal(err)
		}
		if *out.Reply != "Not yet." {
			t.Fatalf("reply = %q", *out.Reply)
		}
	})
	rejected := []struct{ name, body string }{
		{"whitespace-only body", `{"fetch_requests":[],"specification":{"summary":"s","body":" \n ","addressals":[]}}`},
		{"whitespace-only reply", `{"fetch_requests":[],"specification":null,"reply":"  "}`},
		{"padded comment id", `{"fetch_requests":[],"specification":{"summary":"s","body":"b","addressals":[{"comment_id":" c1","response":"r"}]}}`},
		{"padded fetch url", `{"fetch_requests":[{"url":" https://example.com/a","purpose":"p"}],"specification":null}`},
	}
	for _, tc := range rejected {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := specify.DecodeOutput([]byte(tc.body)); !errors.Is(err, specify.ErrInvalidOutput) {
				t.Fatalf("err = %v, want ErrInvalidOutput", err)
			}
		})
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
