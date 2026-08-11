package publish

import (
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func internalDispositionHistory(t *testing.T, headSHA string) DispositionHistory {
	t.Helper()
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	review, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-size-1", RunID: "run-size", Round: 1,
		Provider: "openai", ModelConfiguration: "codex/high",
		ConfigurationDigest: digest, InstructionDigest: digest, CostOwner: "operator",
		BaseSHA: "base", HeadSHA: headSHA, CompletedAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		CompletionEvidence: digest, Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := newDispositionHistory(dispositionHistoryInput{
		runID: "run-size", headSHA: headSHA, expectedInstructionDigest: digest,
		reviews:   []domain.ReviewRecord{review},
		readiness: domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: digest},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return history
}

func TestDesiredPRContentBudgetsDispositionSectionWithoutTruncation(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	history := internalDispositionHistory(t, head)
	identity, err := DeriveIdentity(IdentityInput{
		Repo: "freeside-ai/repo", BaseRef: "main", SourceHeadSHA: head,
		ArtifactDigests: []domain.Digest{"sha256:" + domain.Digest(strings.Repeat("c", 64))},
	})
	if err != nil {
		t.Fatal(err)
	}
	section, err := RenderDispositionHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	proseBytes := maxPullRequestBodyBytes - len(section) - len(identity.Marker()) - 2*len("\n\n")
	candidate := Candidate{
		Title: "Sized body", Body: strings.Repeat("x", proseBytes),
		RunID: "run-size", HeadSHA: head, DispositionHistory: &history,
	}
	_, body, err := desiredPRContent(identity, candidate)
	if err != nil {
		t.Fatalf("exact body limit: %v", err)
	}
	if len(body) != maxPullRequestBodyBytes {
		t.Fatalf("composed body bytes = %d, want %d", len(body), maxPullRequestBodyBytes)
	}
	candidate.Body += "x"
	if _, _, err := desiredPRContent(identity, candidate); err == nil {
		t.Fatal("oversized composed body was truncated or accepted")
	}
}
