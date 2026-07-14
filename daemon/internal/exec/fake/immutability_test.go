package fake_test

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

// The permanent fakes must re-deliver a committed result value-identically on
// every Collect/Poll (§5.3, issue #35). A result's slice-backed fields would
// alias the fake's committed snapshot without cloning, so a caller mutating a
// delivered slice, or mutating the slice it scripted, would change what the
// fake later hands back. These tests pin both mutation vectors for each
// interface.

func driveStageToResult(t *testing.T, d *fake.StageDriver, id domain.InvocationID) exec.StageResult {
	t.Helper()
	if err := d.Start(t.Context(), id, exec.StartSpec{RunID: "run-1", StageID: "stage-1"}); err != nil {
		t.Fatalf("start %s: %v", id, err)
	}
	inspectUntilTerminalOrGone(t, d, id)
	r, err := d.Collect(t.Context(), id)
	if err != nil {
		t.Fatalf("collect %s: %v", id, err)
	}
	return r
}

// TestStageDeliveredResultIsImmutable mutates the first delivered result and
// proves the next delivery is unchanged: the committed snapshot does not alias
// what Collect handed out.
func TestStageDeliveredResultIsImmutable(t *testing.T) {
	d := fake.NewStageDriver()
	d.Script("inv-1", fake.StageScript{
		RunningInspects: 1,
		Outcome:         fake.OutcomeComplete,
		Result: exec.StageResult{
			HeadSHA:   "cafebabe",
			Artifacts: []domain.Digest{"sha256:a", "sha256:b"},
			Summary:   "did the thing",
		},
	})

	first := driveStageToResult(t, d, "inv-1")
	first.Artifacts[0] = "sha256:TAMPERED"

	second, err := d.Collect(t.Context(), "inv-1")
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	want := []domain.Digest{"sha256:a", "sha256:b"}
	if !slices.Equal(second.Artifacts, want) {
		t.Errorf("redelivered artifacts = %v, want unchanged %v", second.Artifacts, want)
	}
}

// TestStageScriptInputIsImmutable mutates the caller's artifact slice after
// scripting and proves the delivered result reflects the value scripted, not
// the later mutation: the stored script does not alias the caller's slice.
func TestStageScriptInputIsImmutable(t *testing.T) {
	d := fake.NewStageDriver()
	artifacts := []domain.Digest{"sha256:a", "sha256:b"}
	d.Script("inv-1", fake.StageScript{
		RunningInspects: 1,
		Outcome:         fake.OutcomeComplete,
		Result: exec.StageResult{
			HeadSHA:   "cafebabe",
			Artifacts: artifacts,
			Summary:   "did the thing",
		},
	})
	artifacts[0] = "sha256:TAMPERED"

	result := driveStageToResult(t, d, "inv-1")
	want := []domain.Digest{"sha256:a", "sha256:b"}
	if !slices.Equal(result.Artifacts, want) {
		t.Errorf("delivered artifacts = %v, want scripted %v", result.Artifacts, want)
	}
}

func reviewFindings() []domain.Finding {
	return []domain.Finding{{
		ID:        "finding-1",
		RunID:     "run-1",
		Source:    "codex",
		Location:  "daemon/internal/exec/driver.go:12",
		Message:   "possible off-by-one",
		CreatedAt: fixedTime,
	}}
}

func driveReviewToResult(t *testing.T, s *fake.ReviewSource, id domain.InvocationID) exec.ReviewResult {
	t.Helper()
	if err := s.RequestReview(t.Context(), id, exec.ReviewRequest{RunID: "run-1", HeadSHA: "cafebabe"}); err != nil {
		t.Fatalf("request review %s: %v", id, err)
	}
	result, _ := pollUntilResult(t, s, id)
	return result
}

// TestReviewDeliveredResultIsImmutable mutates the first polled result and
// proves the next poll is unchanged.
func TestReviewDeliveredResultIsImmutable(t *testing.T) {
	s := fake.NewReviewSource()
	s.Script("inv-1", fake.ReviewScript{
		Result: exec.ReviewResult{HeadSHA: "cafebabe", Findings: reviewFindings()},
	})

	first := driveReviewToResult(t, s, "inv-1")
	first.Findings[0].Message = "TAMPERED"

	second, err := s.Poll(t.Context(), "inv-1")
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if !slices.Equal(second.Findings, reviewFindings()) {
		t.Errorf("redelivered findings = %v, want unchanged %v", second.Findings, reviewFindings())
	}
}

// TestReviewScriptInputIsImmutable mutates the caller's findings slice after
// scripting and proves the delivered result reflects the scripted findings.
func TestReviewScriptInputIsImmutable(t *testing.T) {
	s := fake.NewReviewSource()
	findings := reviewFindings()
	s.Script("inv-1", fake.ReviewScript{
		Result: exec.ReviewResult{HeadSHA: "cafebabe", Findings: findings},
	})
	findings[0].Message = "TAMPERED"

	result := driveReviewToResult(t, s, "inv-1")
	if !slices.Equal(result.Findings, reviewFindings()) {
		t.Errorf("delivered findings = %v, want scripted %v", result.Findings, reviewFindings())
	}
}
