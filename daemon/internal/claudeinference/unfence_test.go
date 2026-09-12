package claudeinference

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// The first live judgment calls returned the requested object inside a
// ```json fence and after prose reasoning; both were refused as unavailable.
// The last site-valid JSON object in the completion is the answer; anything
// without one still fails.
func TestCompletionSelectsTheSiteValidObject(t *testing.T) {
	const answer = `{"materiality":"high","confidence":"high","note":"Concrete"}`
	const pretty = "{\n  \"materiality\": \"high\",\n  \"confidence\": \"high\",\n  \"note\": \"Concrete\"\n}"
	req := classifierRequest()
	site := inference.ClassifierSite(inference.Budget{})
	for _, tc := range []struct {
		name   string
		result string
		want   string
	}{
		{"bare object", answer, answer},
		{"json fence", "```json\n" + pretty + "\n```", pretty},
		{"bare fence", "```\n" + answer + "\n```", answer},
		{"prose before fence", "Here is the classification:\n```json\n" + answer + "\n```", answer},
		{"prose before and after", "Reasoning first.\n\n" + answer + "\n\nThat is my answer.", answer},
		{"nested example then answer", `Consider {"note":"partial"} first.` + "\n" + answer, answer},
		{"last object wins", `{"materiality":"low","confidence":"low","note":"Draft"}` + "\nFinal:\n" + answer, answer},
		{"unmatched braces before the answer", strings.Repeat("{ ", 4000) + "\n" + answer, answer},
		{"nested braces before the answer", strings.Repeat("{", 3000) + strings.Repeat("}", 3000) + "\n" + answer, answer},
		// Validating every span of a deeply nested completion is quadratic;
		// selection stops after maxSelectionPasses passes and fails closed,
		// even when an answer precedes the nesting.
		{"nested braces only", strings.Repeat("{", 3000) + strings.Repeat("}", 3000), ""},
		{"nested braces after the answer", answer + "\n" + strings.Repeat("{", 3000) + strings.Repeat("}", 3000), ""},
		{"brace inside a string", `{"materiality":"high","confidence":"high","note":"see { below"}`, `{"materiality":"high","confidence":"high","note":"see { below"}`},
		{"no object", "I cannot classify this finding.", ""},
		{"truncated object", `{"materiality":"high","confidence":"high","note":"Conc`, ""},
		{"object failing validation", `{"materiality":"critical","confidence":"high","note":"Concrete"}`, ""},
		{"only fences", "``````", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := completion()
			c["result"] = tc.result
			body, _ := json.Marshal(c)
			resp, err := decodeCompletion(body, "test-model", req, site)
			if (err == nil) != (tc.want != "") {
				t.Fatalf("decodeCompletion err=%v, want accepted=%v", err, tc.want != "")
			}
			if err == nil && string(resp.Output) != tc.want {
				t.Fatalf("output %q, want %q", resp.Output, tc.want)
			}
		})
	}
}
