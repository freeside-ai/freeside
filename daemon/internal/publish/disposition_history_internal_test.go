package publish

import (
	"fmt"
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

func TestDesiredPRContentBoundsOversizedFindingClaim(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	first, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-large-finding", RunID: "run-large-finding", Round: 1,
		Provider: "openai", ModelConfiguration: "codex/high",
		ConfigurationDigest: digest, InstructionDigest: digest, CostOwner: "operator",
		BaseSHA: "base", HeadSHA: head, CompletedAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		CompletionEvidence: digest, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"finding-large"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-clean", RunID: "run-large-finding", Round: 2,
		Provider: "openai", ModelConfiguration: "codex/high",
		ConfigurationDigest: digest, InstructionDigest: digest, CostOwner: "operator",
		BaseSHA: "base", HeadSHA: head, CompletedAt: time.Date(2026, 8, 11, 12, 1, 0, 0, time.UTC),
		CompletionEvidence: digest, Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := newDispositionHistory(dispositionHistoryInput{
		runID: "run-large-finding", headSHA: head, expectedInstructionDigest: digest,
		reviews: []domain.ReviewRecord{first, second},
		findings: []domain.Finding{{
			ID: "finding-large", RunID: "run-large-finding", Message: strings.Repeat("x", 1<<20),
			CreatedAt: time.Date(2026, 8, 11, 12, 0, 30, 0, time.UTC),
		}},
		dispositions: []domain.ReviewDispositionRecord{{
			FindingID: "finding-large", RunID: "run-large-finding", Round: 1,
			Disposition: domain.ReviewDispositionDeclined, Reason: "not reproducible",
			AdjudicationDigest: digest,
			CreatedAt:          time.Date(2026, 8, 11, 12, 0, 45, 0, time.UTC),
		}},
		readiness: domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: digest},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := DeriveIdentity(IdentityInput{
		Repo: "freeside-ai/repo", BaseRef: "main", SourceHeadSHA: head,
		ArtifactDigests: []domain.Digest{digest},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := desiredPRContent(identity, Candidate{
		Title: "Bound finding", RunID: "run-large-finding", HeadSHA: head, DispositionHistory: &history,
	})
	if err != nil {
		t.Fatalf("oversized finding claim blocked publication: %v", err)
	}
	if len(body) > maxPullRequestBodyBytes {
		t.Fatalf("body bytes = %d, want at most %d", len(body), maxPullRequestBodyBytes)
	}
	if !strings.Contains(body, "truncated; content digest <code>sha256:") {
		t.Fatal("bounded finding claim omitted its content digest")
	}
}

func TestDesiredPRContentBoundsAggregateDispositionHistory(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	claim := strings.Repeat("x", maxRenderedDispositionClaimBytes)
	findingIDs := make([]domain.FindingID, 8)
	findings := make([]domain.Finding, 8)
	dispositions := make([]domain.ReviewDispositionRecord, 8)
	for i := range findingIDs {
		findingID := domain.FindingID(fmt.Sprintf("finding-%d", i))
		findingIDs[i] = findingID
		findings[i] = domain.Finding{
			ID: findingID, RunID: "run-aggregate-findings", Location: &domain.FindingLocation{Path: claim}, Message: claim,
			CreatedAt: time.Date(2026, 8, 11, 13, 0, i, 0, time.UTC),
		}
		dispositions[i] = domain.ReviewDispositionRecord{
			FindingID: findingID, RunID: "run-aggregate-findings", Round: 1,
			Disposition: domain.ReviewDispositionDeclined, Reason: claim,
			AdjudicationDigest: digest,
			CreatedAt:          time.Date(2026, 8, 11, 13, 1, i, 0, time.UTC),
		}
	}
	first, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-aggregate-findings", RunID: "run-aggregate-findings", Round: 1,
		Provider: "openai", ModelConfiguration: "codex/high",
		ConfigurationDigest: digest, InstructionDigest: digest, CostOwner: "operator",
		BaseSHA: "base", HeadSHA: head, CompletedAt: time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC),
		CompletionEvidence: digest, Outcome: domain.ReviewFindings, FindingIDs: findingIDs,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-aggregate-clean", RunID: "run-aggregate-findings", Round: 2,
		Provider: "openai", ModelConfiguration: "codex/high",
		ConfigurationDigest: digest, InstructionDigest: digest, CostOwner: "operator",
		BaseSHA: "base", HeadSHA: head, CompletedAt: time.Date(2026, 8, 11, 13, 2, 0, 0, time.UTC),
		CompletionEvidence: digest, Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := newDispositionHistory(dispositionHistoryInput{
		runID: "run-aggregate-findings", headSHA: head, expectedInstructionDigest: digest,
		reviews: []domain.ReviewRecord{first, second}, findings: findings,
		dispositions: dispositions,
		readiness:    domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: digest},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	section, err := RenderDispositionHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	if len(section) > maxRenderedDispositionHistoryBytes {
		t.Fatalf("section bytes = %d, want at most %d", len(section), maxRenderedDispositionHistoryBytes)
	}
	if !strings.Contains(section, "Disposition history truncated; full rendered digest: <code>sha256:") {
		t.Fatal("aggregate bound omitted the full rendered digest")
	}
	if strings.Count(section, dispositionHistoryOpenMarker) != 1 ||
		strings.Count(section, dispositionHistoryCloseMarker) != 1 {
		t.Fatal("aggregate bound did not preserve one matched marker pair")
	}
	identity, err := DeriveIdentity(IdentityInput{
		Repo: "freeside-ai/repo", BaseRef: "main", SourceHeadSHA: head,
		ArtifactDigests: []domain.Digest{digest},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := desiredPRContent(identity, Candidate{
		Title: "Bound aggregate", RunID: "run-aggregate-findings", HeadSHA: head,
		DispositionHistory: &history,
	}); err != nil {
		t.Fatalf("aggregate finding claims blocked publication: %v", err)
	}
}
