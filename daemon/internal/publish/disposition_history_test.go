package publish

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

const (
	dispositionDigestA = domain.Digest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	dispositionDigestB = domain.Digest("sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	dispositionDigestC = domain.Digest("sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
)

// dispositionCostOwnerSentinel is the cost owner of the fixture's legacy
// round, and dispositionIdentitySentinel the auth identity of its admitted
// round. Each names who pays, so it is recorded but never published; the
// values are distinctive so a test can prove no rendered text carries them.
const (
	dispositionCostOwnerSentinel = "cost-owner-sentinel-must-not-publish"
	dispositionIdentitySentinel  = "auth-identity-sentinel-must-not-publish"
)

func dispositionHistoryFixture(t *testing.T) DispositionHistory {
	t.Helper()
	runID := domain.RunID("run-disposition-history")
	// Round 1 is a legacy record: it renders the strings it stored. Round 2
	// carries an admission snapshot: it renders the admitted agent's lineage
	// group, model, and effort.
	first, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-round-1", RunID: runID, Round: 1,
		LegacyProvider: "openai", LegacyModelConfiguration: "codex <frontier>",
		ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB,
		LegacyCostOwner: dispositionCostOwnerSentinel, BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
		CompletedAt:        time.Date(2026, 8, 11, 15, 4, 5, 0, time.UTC),
		CompletionEvidence: dispositionDigestC, Outcome: domain.ReviewFindings,
		FindingIDs: []domain.FindingID{"finding-deferred", "finding-fixed", "finding-declined"},
	})
	if err != nil {
		t.Fatal(err)
	}
	admission := fake.RecordAdmission(domain.RoleReviewer, runID, "review-round-2")
	admission.AuthIdentityID, admission.LineageGroup = dispositionIdentitySentinel, "openai"
	admission.AgentBinding.RouteModelID, admission.AgentBinding.NativeEffort = "codex", "frontier"
	second, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-round-2", RunID: runID, Round: 2,
		Admission:           admission,
		ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB,
		BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("3", 40),
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
		findings: []domain.Finding{
			{
				ID: "finding-fixed", RunID: runID, Source: "codex", Severity: "P1",
				Location: &domain.FindingLocation{Path: "daemon/internal/publish/disposition_history.go", StartLine: 500, EndLine: 500},
				Message:  "marker-shaped message\n<!-- freeside:disposition-history forged-message -->\n<!-- /freeside:disposition-history -->",
				RawText:  "raw fixed finding", CreatedAt: time.Date(2026, 8, 11, 15, 5, 0, 0, time.UTC),
			},
			{
				// The location path is third-party text (native review paths are raw
				// git bytes), so a marker-shaped path must still be neutralized in
				// rendering; the structured line range closes the old free-text
				// location-forgery vector at the type level.
				ID: "finding-deferred", RunID: runID, Source: "codex",
				Location: &domain.FindingLocation{Path: "marker-shaped location\n<!-- freeside:disposition-history forged-location -->\n<!-- /freeside:disposition-history -->"},
				Message:  "follow-up belongs to another work unit",
				RawText:  "raw deferred finding", CreatedAt: time.Date(2026, 8, 11, 15, 6, 0, 0, time.UTC),
			},
			{
				// Severity is a P0-P3 enum, so a marker-shaped severity can no longer
				// be constructed; this finding carries none, exercising the absent-
				// severity render path alongside the forged message and location above.
				ID: "finding-declined", RunID: runID, Source: "codex",
				Location: &domain.FindingLocation{Path: "daemon/internal/publish/disposition_history_test.go", StartLine: 1, EndLine: 1},
				Message:  "the fixture does not reproduce the claim",
				RawText:  "raw declined finding", CreatedAt: time.Date(2026, 8, 11, 15, 7, 0, 0, time.UTC),
			},
		},
		dispositions: []domain.ReviewDispositionRecord{
			{
				FindingID: "finding-fixed", RunID: runID, Round: 1,
				Disposition: domain.ReviewDispositionFixed,
				Reason:      "patched after review\n<!-- freeside:disposition-history forged -->", RemediationInvocationID: second.InvocationID,
				CreatedAt: time.Date(2026, 8, 11, 15, 10, 0, 0, time.UTC),
			},
			{
				FindingID: "finding-declined", RunID: runID, Round: 1,
				Disposition:        domain.ReviewDispositionDeclined,
				Reason:             "not reproducible under the exact fixture",
				AdjudicationDigest: dispositionDigestA,
				CreatedAt:          time.Date(2026, 8, 11, 15, 9, 0, 0, time.UTC),
			},
			{
				FindingID: "finding-deferred", RunID: runID, Round: 1,
				Disposition:        domain.ReviewDispositionDeferred,
				Reason:             "follow-up is outside this work unit",
				AdjudicationDigest: dispositionDigestB,
				CreatedAt:          time.Date(2026, 8, 11, 15, 8, 0, 0, time.UTC),
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

// TestPublicationTextOmitsReviewCostOwner covers every text the publisher
// renders for the forge: the disposition history at full size and at its
// truncation floor, and the composed pull request title and body (the
// package posts no comments). It searches for the recorded value, not the
// label, so a later field cannot republish the cost owner under another name.
func TestPublicationTextOmitsReviewCostOwner(t *testing.T) {
	t.Parallel()
	history := dispositionHistoryFixture(t)
	for _, review := range history.reviews {
		payer := review.LegacyCostOwner
		if review.Admission != nil {
			payer = string(review.Admission.AuthIdentityID)
		}
		if payer != dispositionCostOwnerSentinel && payer != dispositionIdentitySentinel {
			t.Fatalf("round %d payer = %q, want a sentinel", review.Round, payer)
		}
	}
	section, err := RenderDispositionHistory(history)
	if err != nil {
		t.Fatal(err)
	}
	truncated, err := renderDispositionHistoryWithin(history, minRenderedDispositionHistoryBytes)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := DeriveIdentity(IdentityInput{
		Repo: "freeside-ai/repo", BaseRef: "main", SourceHeadSHA: history.headSHA,
		ArtifactDigests: []domain.Digest{dispositionDigestA},
	})
	if err != nil {
		t.Fatal(err)
	}
	title, body, err := desiredPRContent(identity, Candidate{
		Title: "Publish the review record", Body: "Operator prose.",
		RunID: history.runID, HeadSHA: history.headSHA, DispositionHistory: &history,
		Advisories: []domain.CandidateFinding{advisoryFinding("")},
	}, closureResolution{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, dispositionHistoryOpenMarker) {
		t.Fatal("composed body omitted the disposition history")
	}
	for name, text := range map[string]string{
		"disposition history": section, "truncated disposition history": truncated,
		"pull request title": title, "pull request body": body,
	} {
		if strings.Contains(text, dispositionCostOwnerSentinel) || strings.Contains(text, dispositionIdentitySentinel) {
			t.Errorf("%s publishes who paid for a review:\n%s", name, text)
		}
	}
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
	for _, forged := range []string{"forged-location", "forged-message"} {
		if strings.Contains(section, "\n<!-- freeside:disposition-history "+forged+" -->") {
			t.Fatalf("%s escaped into a marker-shaped line", forged)
		}
	}
	if strings.Contains(section, "\n<!-- /freeside:disposition-history -->\n") {
		t.Fatal("recorded finding content closed the disposition section")
	}
	deferredStart := strings.Index(section, "<code>finding-deferred</code>")
	fixedStart := strings.Index(section, "<code>finding-fixed</code>")
	if deferredStart < 0 || fixedStart < 0 || strings.Contains(section[deferredStart:fixedStart], "    - Severity:") {
		t.Fatal("empty finding severity rendered a fabricated severity line")
	}
}

func TestDispositionHistoryFailsClosedOnIncompleteOrStaleRecords(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		mutate func(*dispositionHistoryInput)
	}{
		{"missing disposition", func(in *dispositionHistoryInput) { in.dispositions = in.dispositions[1:] }},
		{"missing finding", func(in *dispositionHistoryInput) { in.findings = nil }},
		{"finding from another run", func(in *dispositionHistoryInput) { in.findings[0].RunID = "another-run" }},
		{"extra finding", func(in *dispositionHistoryInput) {
			in.findings = append(in.findings, domain.Finding{
				ID: "extra-finding", RunID: in.runID, CreatedAt: time.Date(2026, 8, 11, 1, 0, 30, 0, time.UTC),
			})
		}},
		{"duplicate finding", func(in *dispositionHistoryInput) { in.findings = append(in.findings, in.findings[0]) }},
		{"stale latest head", func(in *dispositionHistoryInput) { in.headSHA = strings.Repeat("9", 40) }},
		{"blocked readiness", func(in *dispositionHistoryInput) {
			in.readiness = domain.ReadinessVerdict{Class: domain.ReadinessBlocked, Reasons: []domain.ReadinessBlockReason{{RequirementResolutionDigest: dispositionDigestA, Outcome: domain.AdvisoryNotRun, AbsentRecord: true}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			runID := domain.RunID("run-incomplete")
			first, err := domain.NewReviewRecord(domain.ReviewRecord{
				InvocationID: "review-1", RunID: runID, Round: 1, Admission: fake.RecordAdmission(domain.RoleReviewer, runID, "review-1"),
				ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB,
				BaseSHA: "base", HeadSHA: "first-head", CompletedAt: time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC),
				CompletionEvidence: dispositionDigestC, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"finding"},
			})
			if err != nil {
				t.Fatal(err)
			}
			second, err := domain.NewReviewRecord(domain.ReviewRecord{
				InvocationID: "review-2", RunID: runID, Round: 2, Admission: fake.RecordAdmission(domain.RoleReviewer, runID, "review-2"),
				ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB,
				BaseSHA: "base", HeadSHA: "head", CompletedAt: time.Date(2026, 8, 11, 1, 2, 0, 0, time.UTC),
				CompletionEvidence: dispositionDigestC, Outcome: domain.ReviewClean,
			})
			if err != nil {
				t.Fatal(err)
			}
			in := dispositionHistoryInput{
				runID: runID, headSHA: "head", expectedInstructionDigest: dispositionDigestB,
				reviews: []domain.ReviewRecord{first, second},
				findings: []domain.Finding{{
					ID: "finding", RunID: runID, Source: "codex", Severity: "P2",
					Location: &domain.FindingLocation{Path: "file.go", StartLine: 1, EndLine: 1}, Message: "message", RawText: "raw",
					CreatedAt: time.Date(2026, 8, 11, 1, 0, 30, 0, time.UTC),
				}},
				dispositions: []domain.ReviewDispositionRecord{{
					FindingID: "finding", RunID: runID, Round: 1, Disposition: domain.ReviewDispositionDeclined,
					Reason: "reason", AdjudicationDigest: dispositionDigestA,
					CreatedAt: time.Date(2026, 8, 11, 1, 1, 0, 0, time.UTC),
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

// finalFindingsRoundInput is a history whose latest review, at the published
// head, raised one finding that received final disposition `final`.
func finalFindingsRoundInput(t *testing.T, final domain.ReviewDispositionRecord) dispositionHistoryInput {
	t.Helper()
	runID := domain.RunID("run-final-findings")
	first, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-1", RunID: runID, Round: 1, Admission: fake.RecordAdmission(domain.RoleReviewer, runID, "review-1"),
		ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB,
		BaseSHA: "base", HeadSHA: "first-head", CompletedAt: time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC),
		CompletionEvidence: dispositionDigestC, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"finding-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-2", RunID: runID, Round: 2, Admission: fake.RecordAdmission(domain.RoleReviewer, runID, "review-2"),
		ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB,
		BaseSHA: "base", HeadSHA: "head", CompletedAt: time.Date(2026, 8, 11, 1, 2, 0, 0, time.UTC),
		CompletionEvidence: dispositionDigestC, Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"finding-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	finding := func(id domain.FindingID, at time.Time) domain.Finding {
		return domain.Finding{
			ID: id, RunID: runID, Source: "codex", Severity: "P3",
			Location: &domain.FindingLocation{Path: "file.go", StartLine: 1, EndLine: 1}, Message: "message", RawText: "raw",
			CreatedAt: at,
		}
	}
	final.FindingID, final.RunID, final.Round = "finding-2", runID, 2
	return dispositionHistoryInput{
		runID: runID, headSHA: "head", expectedInstructionDigest: dispositionDigestB,
		reviews: []domain.ReviewRecord{first, second},
		findings: []domain.Finding{
			finding("finding-1", time.Date(2026, 8, 11, 1, 0, 30, 0, time.UTC)),
			finding("finding-2", time.Date(2026, 8, 11, 1, 2, 30, 0, time.UTC)),
		},
		dispositions: []domain.ReviewDispositionRecord{
			{
				FindingID: "finding-1", RunID: runID, Round: 1,
				Disposition: domain.ReviewDispositionFixed, Reason: "patched", RemediationInvocationID: second.InvocationID,
				CreatedAt: time.Date(2026, 8, 11, 1, 1, 0, 0, time.UTC),
			},
			final,
		},
		readiness: domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: dispositionDigestC},
	}
}

// A round whose every finding is declined or deferred completes review, so it
// publishes as the latest round without a re-review of the unchanged head
// (#2055). The Wave 8 exit run stopped the daemon on exactly this history.
func TestDispositionHistoryAcceptsFullyDispositionedFinalRound(t *testing.T) {
	t.Parallel()
	for _, final := range []domain.ReviewDispositionRecord{
		{
			Disposition: domain.ReviewDispositionDeclined, Reason: "no decision to record",
			AdjudicationDigest: dispositionDigestA, CreatedAt: time.Date(2026, 8, 11, 1, 3, 0, 0, time.UTC),
		},
		{
			Disposition: domain.ReviewDispositionDeferred, Reason: "outside this work unit",
			AdjudicationDigest: dispositionDigestA, CreatedAt: time.Date(2026, 8, 11, 1, 3, 0, 0, time.UTC),
		},
	} {
		t.Run(string(final.Disposition), func(t *testing.T) {
			history, err := newDispositionHistory(finalFindingsRoundInput(t, final), nil)
			if err != nil {
				t.Fatalf("newDispositionHistory rejected a fully dispositioned final round: %v", err)
			}
			rendered, err := RenderDispositionHistory(history)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(rendered, string(final.Reason)) {
				t.Fatalf("rendered history omits the final round's disposition:\n%s", rendered)
			}
		})
	}
}

// Accepting a findings round as the latest keeps every other binding: the
// round must be at the published head and finished by declines or deferrals.
func TestDispositionHistoryRejectsUnfinishedFinalRound(t *testing.T) {
	t.Parallel()
	declined := domain.ReviewDispositionRecord{
		Disposition: domain.ReviewDispositionDeclined, Reason: "reason",
		AdjudicationDigest: dispositionDigestA, CreatedAt: time.Date(2026, 8, 11, 1, 3, 0, 0, time.UTC),
	}
	for _, test := range []struct {
		name   string
		mutate func(*dispositionHistoryInput)
	}{
		{"missing final disposition", func(in *dispositionHistoryInput) { in.dispositions = in.dispositions[:1] }},
		{"fixed without a later remediation review", func(in *dispositionHistoryInput) {
			in.dispositions[1] = domain.ReviewDispositionRecord{
				FindingID: "finding-2", RunID: in.runID, Round: 2,
				Disposition: domain.ReviewDispositionFixed, Reason: "patched", RemediationInvocationID: "review-1",
				CreatedAt: time.Date(2026, 8, 11, 1, 3, 0, 0, time.UTC),
			}
		}},
		{"stale latest head", func(in *dispositionHistoryInput) { in.headSHA = "other-head" }},
		{"unapproved instructions", func(in *dispositionHistoryInput) { in.expectedInstructionDigest = dispositionDigestC }},
	} {
		t.Run(test.name, func(t *testing.T) {
			in := finalFindingsRoundInput(t, declined)
			test.mutate(&in)
			if _, err := newDispositionHistory(in, nil); !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("newDispositionHistory = %v, want ErrParentKeyMismatch", err)
			}
		})
	}
}

// TestCurrentDispositionLineageExcludesLegacyPassOnTheAdmittedHead covers the
// upgrade window: a legacy pass whose adjudication never finished is
// superseded by the first admitted pass on the same head and may have no
// disposition, while a legacy pass on an earlier head was remediated and keeps
// its place and its dispositions.
func TestCurrentDispositionLineageExcludesLegacyPassOnTheAdmittedHead(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-legacy-lineage")
	at := time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC)
	record := func(round int, head string, admitted bool, outcome domain.ReviewOutcome, findings ...domain.FindingID) domain.ReviewRecord {
		t.Helper()
		id := domain.InvocationID(fmt.Sprintf("review-%d", round))
		in := domain.ReviewRecord{
			InvocationID: id, RunID: runID, Round: round, ConfigurationDigest: dispositionDigestA,
			InstructionDigest: dispositionDigestB, BaseSHA: "base", HeadSHA: head,
			CompletedAt: at.Add(time.Duration(round) * time.Minute), CompletionEvidence: dispositionDigestC,
			Outcome: outcome, FindingIDs: findings,
		}
		if admitted {
			in.Admission = fake.RecordAdmission(domain.RoleReviewer, runID, id)
		} else {
			in.LegacyProvider, in.LegacyModelConfiguration, in.LegacyCostOwner = "openai", "gpt-codex/high", "owner"
		}
		out, err := domain.NewReviewRecord(in)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	remediated := record(1, "head-1", false, domain.ReviewFindings, "fixed-finding")
	superseded := record(2, "head-2", false, domain.ReviewFindings, "unsettled-finding")
	admitted := record(3, "head-2", true, domain.ReviewClean)
	fixed := domain.ReviewDispositionRecord{
		FindingID: "fixed-finding", Round: 1, Disposition: domain.ReviewDispositionFixed,
	}
	partial := domain.ReviewDispositionRecord{
		FindingID: "unsettled-finding", Round: 2, Disposition: domain.ReviewDispositionDeclined,
	}

	reviews, dispositions := currentDispositionLineage(
		[]domain.ReviewRecord{remediated, superseded, admitted},
		[]domain.ReviewDispositionRecord{fixed, partial}, nil,
	)
	if len(reviews) != 2 || reviews[0].Round != 1 || reviews[1].Round != 3 ||
		len(dispositions) != 1 || dispositions[0].Round != 1 {
		t.Fatalf("lineage = %#v, %#v", reviews, dispositions)
	}

	// With no admitted pass, the legacy passes are the whole lineage, as they
	// were when written.
	reviews, _ = currentDispositionLineage([]domain.ReviewRecord{remediated, superseded}, nil, nil)
	if len(reviews) != 2 {
		t.Fatalf("legacy-only lineage = %#v", reviews)
	}

	// An admitted pass with unsettled findings is never filtered: only a
	// legacy pass is superseded by a later pass on its head.
	unsettled := record(2, "head-2", true, domain.ReviewFindings, "unsettled-finding")
	reviews, _ = currentDispositionLineage([]domain.ReviewRecord{remediated, unsettled, admitted}, nil, nil)
	if len(reviews) != 3 {
		t.Fatalf("admitted lineage = %#v", reviews)
	}
}

// An unpublished re-entry reviews the same candidate at a newer base (issue
// #2058). Its chain links the rounds at the admitted base to the rounds at the
// re-entry base, so their dispositions stay in the lineage; a round at any
// base the chain does not name for its round stays out, and so does every
// earlier round once a later published re-entry moves the latest round to a
// base the chain does not name.
func TestCurrentDispositionLineageFollowsUnpublishedReentrySegments(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-segments")
	at := func(round int, base, head string, findings ...domain.FindingID) domain.ReviewRecord {
		invocation := domain.InvocationID(fmt.Sprintf("review-%d", round))
		outcome := domain.ReviewClean
		if len(findings) > 0 {
			outcome = domain.ReviewFindings
		}
		record, err := domain.NewReviewRecord(domain.ReviewRecord{
			InvocationID: invocation, RunID: runID, Round: round,
			Admission: fake.RecordAdmission(domain.RoleReviewer, runID, invocation), ConfigurationDigest: dispositionDigestA,
			InstructionDigest: dispositionDigestA, BaseSHA: base, HeadSHA: head,
			CompletedAt: time.Date(2026, 10, 18, 2, round, 0, 0, time.UTC), CompletionEvidence: dispositionDigestC,
			Outcome: outcome, FindingIDs: findings,
		})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	first := at(1, "base-a", "head-1", "declined-finding")
	second := at(2, "base-a", "head-1", "deferred-finding")
	reentered := at(3, "base-b", "head-1")
	declined := domain.ReviewDispositionRecord{FindingID: "declined-finding", Round: 1, Disposition: domain.ReviewDispositionDeclined}
	deferred := domain.ReviewDispositionRecord{FindingID: "deferred-finding", Round: 2, Disposition: domain.ReviewDispositionDeferred}
	segments := []lineageSegment{{FromRound: 0, BaseSHA: "base-a"}, {FromRound: 3, BaseSHA: "base-b"}}

	reviews, dispositions := currentDispositionLineage(
		[]domain.ReviewRecord{first, second, reentered},
		[]domain.ReviewDispositionRecord{declined, deferred}, segments,
	)
	if len(reviews) != 3 || len(dispositions) != 2 {
		t.Fatalf("re-entry lineage dropped the admitted base's rounds: %#v, %#v", reviews, dispositions)
	}
	// Without the chain, the single-base rule keeps only the latest base.
	if reviews, _ := currentDispositionLineage([]domain.ReviewRecord{first, second, reentered}, nil, nil); len(reviews) != 1 {
		t.Fatalf("lineage linked bases with no re-entry chain: %#v", reviews)
	}
	// A round at a base the chain does not name for that round stays out.
	stray := at(2, "base-c", "head-1", "stray-finding")
	if reviews, _ := currentDispositionLineage([]domain.ReviewRecord{first, stray, reentered}, nil, segments); len(reviews) != 2 ||
		slices.ContainsFunc(reviews, func(r domain.ReviewRecord) bool { return r.BaseSHA == "base-c" }) {
		t.Fatalf("lineage admitted a base the chain does not name: %#v", reviews)
	}
	// A later round at a base the chain does not name keeps the single-base rule.
	moved := at(4, "base-d", "head-1")
	if reviews, _ := currentDispositionLineage([]domain.ReviewRecord{first, second, reentered, moved}, nil, segments); len(reviews) != 1 ||
		reviews[0].BaseSHA != "base-d" {
		t.Fatalf("lineage linked a base past the re-entry chain: %#v", reviews)
	}
	// A pass superseded by other instructions within its own segment stays out.
	stale := first
	stale.InstructionDigest = dispositionDigestB
	if reviews, _ := currentDispositionLineage([]domain.ReviewRecord{stale, second, reentered}, nil, segments); len(reviews) != 2 {
		t.Fatalf("lineage kept a round under other instructions: %#v", reviews)
	}
	// An advance whose base composes other instructions keeps the earlier
	// segment's rounds: each segment answers to its own latest round.
	oldFirst, oldSecond := first, second
	oldFirst.InstructionDigest = dispositionDigestB
	oldSecond.InstructionDigest = dispositionDigestB
	reviews, dispositions = currentDispositionLineage(
		[]domain.ReviewRecord{oldFirst, oldSecond, reentered},
		[]domain.ReviewDispositionRecord{declined, deferred}, segments,
	)
	if len(reviews) != 3 || len(dispositions) != 2 {
		t.Fatalf("instructions changed at the new base dropped the carried rounds: %#v, %#v", reviews, dispositions)
	}
	// The configuration equality still applies across bases.
	otherConfiguration := second
	otherConfiguration.ConfigurationDigest = dispositionDigestB
	if reviews, _ := currentDispositionLineage(
		[]domain.ReviewRecord{first, otherConfiguration, reentered}, nil, segments,
	); len(reviews) != 2 || reviews[0].Round != 1 {
		t.Fatalf("lineage kept a round under another configuration: %#v", reviews)
	}
}

func TestCurrentDispositionLineageExcludesSupersededAuthorityOnly(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-lineage")
	stale, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-stale", RunID: runID, Round: 1,
		Admission: fake.RecordAdmission(domain.RoleReviewer, runID, "review-stale"), ConfigurationDigest: dispositionDigestA,
		InstructionDigest: dispositionDigestA, BaseSHA: "base", HeadSHA: "head",
		CompletedAt: time.Date(2026, 8, 11, 2, 0, 0, 0, time.UTC), CompletionEvidence: dispositionDigestC,
		Outcome: domain.ReviewFindings, FindingIDs: []domain.FindingID{"stale-finding"},
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-current", RunID: runID, Round: 2,
		Admission: fake.RecordAdmission(domain.RoleReviewer, runID, "review-current"), ConfigurationDigest: dispositionDigestA,
		InstructionDigest: dispositionDigestB, BaseSHA: "base", HeadSHA: "head",
		CompletedAt: time.Date(2026, 8, 11, 2, 1, 0, 0, time.UTC), CompletionEvidence: dispositionDigestC,
		Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	reviews, dispositions := currentDispositionLineage(
		[]domain.ReviewRecord{stale, current}, nil, nil,
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
		[]domain.ReviewRecord{stale, current}, nil, nil,
	)
	if _, err := newDispositionHistory(dispositionHistoryInput{
		runID: runID, headSHA: "head", expectedInstructionDigest: dispositionDigestB,
		reviews: reviews,
		findings: []domain.Finding{{
			ID: "stale-finding", RunID: runID, CreatedAt: time.Date(2026, 8, 11, 2, 0, 30, 0, time.UTC),
		}},
		dispositions: dispositions,
		readiness:    domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: dispositionDigestC},
	}, nil); err == nil {
		t.Fatal("current-authority finding without a disposition was silently filtered")
	}
}

// TestReentryLineageSegmentsKeepTheReplacedCycle: an unpublished re-entry at
// base-b publishes, a feedback cycle then works at the admitted base-a, and a
// successor re-entry replaces it at base-c (issue #2111). The feedback's own
// rounds keep their base-a segment, so a disposition recorded there stays in
// the history the successor re-entry publishes.
func TestReentryLineageSegmentsKeepTheReplacedCycle(t *testing.T) {
	t.Parallel()
	runID := domain.RunID("run-successor-segments")
	reentry := func(version string, origin domain.PublicationSuccessorOrigin, predecessor domain.ItemID,
		prior domain.InvocationID, round int, base string,
	) domain.PublicationSuccessor {
		return domain.PublicationSuccessor{
			Version: version, Origin: origin, RunID: runID, PredecessorItemID: predecessor,
			PriorReviewInvocationID: prior, ReviewRound: round,
			Reentry: &domain.PublicationSuccessorReentry{
				Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: base, HeadSHA: "head-1",
			},
		}
	}
	unpublished := reentry(domain.PublicationUnpublishedReentryVersion, domain.PublicationSuccessorUnpublishedBaseAdvance,
		domain.ProductionReadyItemID(runID), "review-2", 3, "base-b")
	feedback := domain.PublicationSuccessor{
		Version: domain.PublicationContinuationVersion, Origin: domain.PublicationSuccessorFeedback,
		RunID: runID, CommandID: "return-1", FeedbackInvocationID: "inv-feedback-1",
		PredecessorItemID: unpublished.ReadyItemID(), PriorReviewInvocationID: "review-3", ReviewRound: 4,
	}
	successor := reentry(domain.PublicationSuccessorReentryVersion, domain.PublicationSuccessorBaseAdvance,
		feedback.ReadyItemID(), "review-5", 6, "base-c")
	further := reentry(domain.PublicationSuccessorReentryVersion, domain.PublicationSuccessorBaseAdvance,
		successor.ReadyItemID(), "review-6", 7, "base-d")
	priorBases := map[domain.InvocationID]string{
		"review-2": "base-a", "review-5": "base-a", "review-6": "base-c",
	}
	segments := reentryLineageSegments(
		[]domain.PublicationSuccessor{unpublished, feedback, successor, further}, priorBases)
	want := []lineageSegment{
		{FromRound: 0, BaseSHA: "base-a"},
		{FromRound: 3, BaseSHA: "base-b"},
		{FromRound: 4, BaseSHA: "base-a"},
		{FromRound: 6, BaseSHA: "base-c"},
		{FromRound: 7, BaseSHA: "base-d"},
	}
	if !slices.Equal(segments, want) {
		t.Fatalf("segments = %#v, want %#v", segments, want)
	}
	if segments := reentryLineageSegments([]domain.PublicationSuccessor{feedback}, priorBases); segments != nil {
		t.Fatalf("a chain without a re-entry has segments %#v", segments)
	}

	at := func(round int, base string, findings ...domain.FindingID) domain.ReviewRecord {
		invocation := domain.InvocationID(fmt.Sprintf("review-%d", round))
		outcome := domain.ReviewClean
		if len(findings) > 0 {
			outcome = domain.ReviewFindings
		}
		record, err := domain.NewReviewRecord(domain.ReviewRecord{
			InvocationID: invocation, RunID: runID, Round: round,
			Admission: fake.RecordAdmission(domain.RoleReviewer, runID, invocation), ConfigurationDigest: dispositionDigestA,
			InstructionDigest: dispositionDigestA, BaseSHA: base, HeadSHA: "head-1",
			CompletedAt: time.Date(2026, 10, 21, 2, round, 0, 0, time.UTC), CompletionEvidence: dispositionDigestC,
			Outcome: outcome, FindingIDs: findings,
		})
		if err != nil {
			t.Fatal(err)
		}
		return record
	}
	declined := domain.ReviewDispositionRecord{FindingID: "feedback-finding", Round: 5, Disposition: domain.ReviewDispositionDeclined}
	reviews, dispositions := currentDispositionLineage(
		[]domain.ReviewRecord{
			at(1, "base-a"), at(2, "base-a"), at(3, "base-b"), at(4, "base-a"),
			at(5, "base-a", "feedback-finding"), at(6, "base-c"),
		},
		[]domain.ReviewDispositionRecord{declined}, segments[:4],
	)
	if len(reviews) != 6 || len(dispositions) != 1 {
		t.Fatalf("successor re-entry lineage dropped the feedback cycle's rounds: %#v, %#v", reviews, dispositions)
	}
}
