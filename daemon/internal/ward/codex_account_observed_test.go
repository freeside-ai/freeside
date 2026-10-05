package ward

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

func TestCodexAccountObservedFixtures(t *testing.T) {
	for _, mode := range []string{"fresh", "near_expiry", "control"} {
		t.Run(mode, func(t *testing.T) {
			body, err := os.ReadFile("testdata/codex_account_observed_" + mode + ".jsonl") //nolint:gosec // mode is one of three literal fixture names
			if err != nil {
				t.Fatal(err)
			}
			var evidence codexAccountEvidence
			if err := strictjson.Decode(body, &evidence, strictjson.RejectInvalidUTF8, strictjson.Limit(16<<10)); err != nil {
				t.Fatal(err)
			}
			capture, err := json.Marshal(evidence.Capture)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := codexAccountDecode(capture); err != nil {
				t.Fatal("fixture contains unreviewed capture fields")
			}
			want := "fail"
			if mode != "near_expiry" {
				want = "pass"
			}
			if evidence.Case != mode || evidence.Verdict != want || codexAccountAnalyze(evidence.Capture, mode == "control", evidence.Requests, evidence.Failures) != want {
				t.Fatal("pinned observation changed")
			}
		})
	}
}
