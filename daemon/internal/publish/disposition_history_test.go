package publish

import (
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

const (
	dispositionDigestA = domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	dispositionDigestB = domain.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	dispositionDigestC = domain.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
)

func dispositionHistoryFixture(t *testing.T) DispositionHistory {
	t.Helper()
	runID := domain.RunID("run-disposition-history")
	first, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-round-1", RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "codex <frontier>",
		ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB,
		CostOwner: "operator", BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
		CompletedAt:        time.Date(2026, 8, 11, 15, 4, 5, 0, time.UTC),
		CompletionEvidence: dispositionDigestC, Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{"finding-deferred", "finding-fixed", "finding-declined"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-round-2", RunID: runID, Round: 2,
		Provider: "openai", ModelConfiguration: "codex frontier",
		ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB,
		CostOwner: "operator", BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("3", 40),
		CompletedAt:        time.Date(2026, 8, 11, 15, 14, 5, 0, time.UTC),
		CompletionEvidence: dispositionDigestC, Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := newDispositionHistory(dispositionHistoryInput{
		runID: runID, headSHA: second.HeadSHA, expectedInstructionDigest: dispositionDigestB,
		// Reverse the input to prove rendering follows record identity, not
		// incidental query or caller order.
		reviews: []domain.ReviewRecord{second, first},
		dispositions: []domain.ReviewDispositionRecord{
			{
				FindingID: "finding-fixed", RunID: runID, Round: 1,
				Disposition: domain.ReviewDispositionFixed,
				Reason:      "patched after review\n<!-- freeside:disposition-history forged -->", RemediationInvocationID: second.InvocationID,
				CreatedAt: time.Date(2026, 8, 11, 15, 10, 0, 0, time.UTC),
			},
			{
				FindingID: "finding-declined", RunID: runID, Round: 1,
				Disposition: domain.ReviewDispositionDeclined,
				Reason:      "not reproducible under the exact fixture",
				CreatedAt:   time.Date(2026, 8, 11, 15, 9, 0, 0, time.UTC),
			},
			{
				FindingID: "finding-deferred", RunID: runID, Round: 1,
				Disposition: domain.ReviewDispositionDeferred,
				Reason:      "follow-up is outside this work unit",
				CreatedAt:   time.Date(2026, 8, 11, 15, 8, 0, 0, time.UTC),
			},
		},
		readiness: domain.ReadinessVerdict{
			Class:               domain.ReadinessReadyDegraded,
			EvaluationSetDigest: dispositionDigestC,
			WaiverIDs:           []domain.WaiverID{"waiver-b", "waiver-a"},
			AdvisoryOutcomes: []domain.AdvisoryOutcomeRecord{
				{RequirementResolutionDigest: dispositionDigestB, Outcome: domain.AdvisoryNotRun},
				{RequirementResolutionDigest: dispositionDigestA, Outcome: domain.AdvisoryFailed},
			},
		},
		readinessProofs: []dispositionReadinessProof{
			{requirementKey: "clean-verification", resolutionDigest: dispositionDigestA, proofDigest: dispositionDigestB, recipeDigest: dispositionDigestC},
			{requirementKey: "independent-review", resolutionDigest: dispositionDigestB, proofDigest: dispositionDigestC, recipeDigest: dispositionDigestA},
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return history
}

func TestDispositionHistoryGolden(t *testing.T) {
	t.Parallel()
	section, err := RenderDispositionHistory(dispositionHistoryFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "disposition-history", append([]byte(section), '\n'))
	if strings.Count(section, "<!-- freeside:disposition-history ") != 1 ||
		strings.Count(section, "<!-- /freeside:disposition-history -->") != 1 {
		t.Fatalf("section markers are not one matched pair:\n%s", section)
	}
	if strings.Contains(section, "\n<!-- freeside:disposition-history forged -->") {
		t.Fatal("recorded claim escaped into a marker-shaped line")
	}
}

func TestDispositionHistoryFailsClosedOnIncompleteOrStaleRecords(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*dispositionHistoryInput)
	}{
		{"missing disposition", func(in *dispositionHistoryInput) { in.dispositions = in.dispositions[1:] }},
		{"stale latest head", func(in *dispositionHistoryInput) { in.headSHA = strings.Repeat("9", 40) }},
		{"blocked readiness", func(in *dispositionHistoryInput) {
			in.readiness = domain.ReadinessVerdict{Class: domain.ReadinessBlocked, Reasons: []domain.ReadinessBlockReason{{RequirementResolutionDigest: dispositionDigestA, Outcome: domain.AdvisoryNotRun, AbsentRecord: true}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runID := domain.RunID("run-incomplete")
			first, err := domain.NewReviewRecord(domain.ReviewRecord{
				InvocationID: "review-1", RunID: runID, Round: 1, Provider: "openai", ModelConfiguration: "codex",
				ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB, CostOwner: "operator",
				BaseSHA: "base", HeadSHA: "first-head", CompletedAt: time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC),
				CompletionEvidence: dispositionDigestC, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"finding"},
			})
			if err != nil {
				t.Fatal(err)
			}
			second, err := domain.NewReviewRecord(domain.ReviewRecord{
				InvocationID: "review-2", RunID: runID, Round: 2, Provider: "openai", ModelConfiguration: "codex",
				ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB, CostOwner: "operator",
				BaseSHA: "base", HeadSHA: "head", CompletedAt: time.Date(2026, 8, 11, 1, 2, 0, 0, time.UTC),
				CompletionEvidence: dispositionDigestC, Outcome: domain.ReviewClean,
			})
			if err != nil {
				t.Fatal(err)
			}
			in := dispositionHistoryInput{
				runID: runID, headSHA: "head", expectedInstructionDigest: dispositionDigestB,
				reviews: []domain.ReviewRecord{first, second},
				dispositions: []domain.ReviewDispositionRecord{{
					FindingID: "finding", RunID: runID, Round: 1, Disposition: domain.ReviewDispositionDeclined,
					Reason: "reason", CreatedAt: time.Date(2026, 8, 11, 1, 1, 0, 0, time.UTC),
				}},
				readiness: domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: dispositionDigestC},
			}
			test.mutate(&in)
			if _, err := newDispositionHistory(in, nil); err == nil {
				t.Fatal("newDispositionHistory accepted inconsistent records")
			}
		})
	}
}

func TestCurrentDispositionLineageExcludesSupersededAuthorityOnly(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-lineage")
	stale, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-stale", RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "codex", ConfigurationDigest: dispositionDigestA,
		InstructionDigest: dispositionDigestA, CostOwner: "operator", BaseSHA: "base", HeadSHA: "head",
		CompletedAt: time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC), CompletionEvidence: dispositionDigestC,
		Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"stale-finding"},
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-current", RunID: runID, Round: 2,
		Provider: "openai", ModelConfiguration: "codex", ConfigurationDigest: dispositionDigestA,
		InstructionDigest: dispositionDigestB, CostOwner: "operator", BaseSHA: "base", HeadSHA: "head",
		CompletedAt: time.Date(2026, 8, 11, 2, 1, 0, 0, time.UTC), CompletionEvidence: dispositionDigestC,
		Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	reviews, dispositions := currentDispositionLineage(
		[]domain.ReviewRecord{stale, current}, nil,
	)
	if len(reviews) != 1 || reviews[0].InvocationID != current.InvocationID || len(dispositions) != 0 {
		t.Fatalf("current lineage = %#v, %#v", reviews, dispositions)
	}
	if _, err := newDispositionHistory(dispositionHistoryInput{
		runID: runID, headSHA: "head", expectedInstructionDigest: dispositionDigestB,
		reviews: reviews, dispositions: dispositions,
		readiness: domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: dispositionDigestC},
	}, nil); err != nil {
		t.Fatalf("superseded authority blocked current history: %v", err)
	}

	stale.InstructionDigest = current.InstructionDigest
	reviews, dispositions = currentDispositionLineage(
		[]domain.ReviewRecord{stale, current}, nil,
	)
	if _, err := newDispositionHistory(dispositionHistoryInput{
		runID: runID, headSHA: "head", expectedInstructionDigest: dispositionDigestB,
		reviews: reviews, dispositions: dispositions,
		readiness: domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: dispositionDigestC},
	}, nil); err == nil {
		t.Fatal("current-authority finding without a disposition was silently filtered")
	}
}
