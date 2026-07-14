package exec_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// TestGolden covers the serialized shape of the two committed result
// contracts (the shapes store and api will carry); interfaces and fakes have
// no serialized form. Each fixture is a fixed, valid value, so the goldens
// double as validation-positive cases (the domain golden convention).
// Regenerate with: go test ./internal/exec -run TestGolden -update.
func TestGolden(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	stage := exec.StageResult{
		InvocationID: "inv-1",
		Status:       exec.StatusCompleted,
		HeadSHA:      "cafebabe",
		Artifacts:    []domain.Digest{"sha256:transcript", "sha256:diff"},
		Summary:      "implemented the fix and its regression test",
	}
	review := exec.ReviewResult{
		InvocationID: "inv-2",
		HeadSHA:      "cafebabe",
		Findings: []domain.Finding{{
			ID:        "finding-1",
			RunID:     "run-1",
			Source:    "codex",
			Location:  "daemon/internal/exec/driver.go:12",
			Message:   "possible off-by-one in retry ordinal",
			RawText:   "P2: possible off-by-one in retry ordinal",
			CreatedAt: ts,
		}},
	}

	cases := []struct {
		name  string
		value interface {
			Validate() error
		}
	}{
		{"stage_result", stage},
		{"review_result", review},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.value.Validate(); err != nil {
				t.Fatalf("fixture must be valid: %v", err)
			}
			got, err := json.MarshalIndent(tc.value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, tc.name, append(got, '\n'))
		})
	}
}
