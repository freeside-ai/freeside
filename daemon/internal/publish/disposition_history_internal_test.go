package publish

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

func oversizedDispositionHistory(t *testing.T, headSHA string) DispositionHistory {
	t.Helper()
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	const findingCount = 300
	claim := "x"
	findingIDs := make([]domain.FindingID, findingCount)
	findings := make([]domain.Finding, findingCount)
	dispositions := make([]domain.ReviewDispositionRecord, findingCount)
	for i := range findingIDs {
		findingID := domain.FindingID(fmt.Sprintf("finding-%d", i))
		findingIDs[i] = findingID
		findings[i] = domain.Finding{
			ID: findingID, RunID: "run-aggregate-findings",
			Location:  &domain.FindingLocation{Path: claim},
			Message:   claim,
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
		Admission:           fake.RecordAdmission(domain.RoleReviewer, "run-aggregate-findings", "review-aggregate-findings"),
		ConfigurationDigest: digest, InstructionDigest: digest,
		BaseSHA: "base", HeadSHA: headSHA, CompletedAt: time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC),
		CompletionEvidence: digest, Outcome: domain.ReviewFindings, FindingIDs: findingIDs,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-aggregate-clean", RunID: "run-aggregate-findings", Round: 2,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, "run-aggregate-findings", "review-aggregate-clean"),
		ConfigurationDigest: digest, InstructionDigest: digest,
		BaseSHA: "base", HeadSHA: headSHA, CompletedAt: time.Date(2026, 8, 11, 13, 2, 0, 0, time.UTC),
		CompletionEvidence: digest, Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := newDispositionHistory(dispositionHistoryInput{
		runID: "run-aggregate-findings", headSHA: headSHA, expectedInstructionDigest: digest,
		reviews: []domain.ReviewRecord{first, second}, findings: findings,
		dispositions: dispositions,
		readiness:    domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: digest},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return history
}

func TestDesiredPRContentFitsEveryReservedPublisherSection(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	history := oversizedDispositionHistory(t, head)
	identity, err := DeriveIdentity(IdentityInput{
		Repo: "freeside-ai/repo", BaseRef: "main", SourceHeadSHA: head,
		ArtifactDigests: []domain.Digest{"sha256:" + domain.Digest(strings.Repeat("c", 64))},
	})
	if err != nil {
		t.Fatal(err)
	}
	advisories := make([]domain.CandidateFinding, maxRenderedAdvisories+5)
	for i := range advisories {
		advisory := advisoryFinding("")
		advisory.Path = fmt.Sprintf("%s/%d/AGENTS.md", strings.Repeat("&", 4096), i)
		advisory.PathHex = strings.Repeat("&", 4096)
		advisory.Kind = strings.Repeat("&", 4096)
		advisory.Detail = strings.Repeat("&", 4096)
		advisories[i] = advisory
	}
	candidate := Candidate{
		Title: "Sized body", Body: strings.Repeat("x", maxCandidateBodyBytes),
		RunID: "run-aggregate-findings", HeadSHA: head, DispositionHistory: &history,
		Advisories: advisories,
	}
	if err := ValidateCandidateBody(candidate.Body); err != nil {
		t.Fatalf("exact candidate body budget: %v", err)
	}
	_, body, err := desiredPRContent(identity, candidate, closureResolution{})
	if err != nil {
		t.Fatalf("compose every reserved section: %v", err)
	}
	if len(body) > maxPullRequestBodyBytes {
		t.Fatalf("composed body bytes = %d, want at most %d", len(body), maxPullRequestBodyBytes)
	}
	start := strings.Index(body, verificationOpenMarker)
	end := strings.Index(body, verificationCloseMarker)
	if start < 0 || end < start {
		t.Fatal("composed body omitted the verification marker pair")
	}
	section := body[start : end+len(verificationCloseMarker)]
	if got := strings.Count(section, "  - Declined"); got != maxRenderedVerificationFindings {
		t.Fatalf("rendered findings = %d, want the %d-finding cap", got, maxRenderedVerificationFindings)
	}
	if want := fmt.Sprintf("  - %d more declined or deferred findings are recorded in Freeside.",
		300-maxRenderedVerificationFindings); !strings.Contains(section, want) {
		t.Fatalf("section omitted the count of findings left out:\n%s", section)
	}
	if !strings.HasSuffix(section, verificationClosingLine+"\n\n"+verificationCloseMarker) {
		t.Fatal("bounded section lost its closing line")
	}
	if err := ValidateCandidateBody(candidate.Body + "x"); err == nil {
		t.Fatal("candidate body consumed a publisher-owned section reserve")
	}
}

// TestDesiredPRContentOmitsDispositionHistory pins #3022 criterion 5's body
// half: no composed body carries the history section, while the canonical
// rendering the intent binds is still produced unchanged.
func TestDesiredPRContentOmitsDispositionHistory(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	history := oversizedDispositionHistory(t, head)
	identity, err := DeriveIdentity(IdentityInput{
		Repo: "freeside-ai/repo", BaseRef: "main", SourceHeadSHA: head,
		ArtifactDigests: []domain.Digest{"sha256:" + domain.Digest(strings.Repeat("c", 64))},
	})
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := RenderDispositionHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	_, body, err := desiredPRContent(identity, Candidate{
		Title: "No history", Body: "Short operator prose.",
		RunID: "run-aggregate-findings", HeadSHA: head, DispositionHistory: &history,
	}, closureResolution{})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{dispositionHistoryOpenMarker, dispositionHistoryCloseMarker, "Freeside Disposition History", canonical} {
		if strings.Contains(body, absent) {
			t.Fatalf("composed body still carries the disposition history (%q)", absent)
		}
	}
	if !strings.Contains(body, "- Passed: independent review, 2 rounds, 300 findings (0 fixed, 300 declined, 0 deferred)") {
		t.Fatalf("composed body omitted the review outcome:\n%s", body)
	}
}

func TestDesiredPRContentKeepsFailClosedBodyCeiling(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	identity, err := DeriveIdentity(IdentityInput{
		Repo: "freeside-ai/repo", BaseRef: "main", SourceHeadSHA: head,
		ArtifactDigests: []domain.Digest{"sha256:" + domain.Digest(strings.Repeat("c", 64))},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := desiredPRContent(identity, Candidate{
		Title: "Oversized body", Body: strings.Repeat("x", maxPullRequestBodyBytes),
	}, closureResolution{}); err == nil {
		t.Fatal("final composition guard accepted a body above the forge ceiling")
	}
}

func TestDesiredPRContentBoundsOversizedFindingClaim(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	first, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-large-finding", RunID: "run-large-finding", Round: 1,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, "run-large-finding", "review-large-finding"),
		ConfigurationDigest: digest, InstructionDigest: digest,
		BaseSHA: "base", HeadSHA: head, CompletedAt: time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC),
		CompletionEvidence: digest, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"finding-large"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-clean", RunID: "run-large-finding", Round: 2,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, "run-large-finding", "review-clean"),
		ConfigurationDigest: digest, InstructionDigest: digest,
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
	}, closureResolution{})
	if err != nil {
		t.Fatalf("oversized finding claim blocked publication: %v", err)
	}
	if len(body) > maxPullRequestBodyBytes {
		t.Fatalf("body bytes = %d, want at most %d", len(body), maxPullRequestBodyBytes)
	}
	if !strings.Contains(body, "</code> (truncated)") || strings.Contains(body, "content digest") {
		t.Fatal("bounded finding claim is not marked truncated, or names a digest")
	}
}

func TestDesiredPRContentBoundsAggregateDispositionHistory(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	history := oversizedDispositionHistory(t, head)
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
	}, closureResolution{}); err != nil {
		t.Fatalf("aggregate finding claims blocked publication: %v", err)
	}
}
