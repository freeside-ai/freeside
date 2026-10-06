package ward

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// The pinned observations of one complete live run and the verdict each
// produced.
var codexUsageObserved = map[string]string{
	"synthetic_fresh":       "pass",
	"synthetic_near_expiry": "pass",
	"real_idle":             "pass",
	"real_turn":             "pass",
}

const codexUsageObservedOverall = "pass"

func TestCodexUsageObservedFixtures(t *testing.T) {
	verdicts := map[string]string{}
	for name, want := range codexUsageObserved {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile("testdata/codex_usage_observed_" + name + ".jsonl") //nolint:gosec // name is one of the literal fixture names above
			if err != nil {
				t.Fatal(err)
			}
			var evidence codexUsageEvidence
			if err := strictjson.Decode(body, &evidence, strictjson.RejectInvalidUTF8, strictjson.Limit(16<<10)); err != nil {
				t.Fatal(err)
			}
			if evidence.Capture != nil {
				capture, err := json.Marshal(evidence.Capture)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := codexUsageDecode(capture); err != nil {
					t.Fatal("fixture contains unreviewed capture fields")
				}
			}
			if evidence.Case != name || evidence.Verdict != want || codexUsageAnalyze(evidence) != want {
				t.Fatal("pinned observation changed")
			}
			verdicts[name] = evidence.Verdict
		})
	}
	if got := codexUsageOverall(verdicts); got != codexUsageObservedOverall {
		t.Fatalf("pinned observations give %s", got)
	}
}
