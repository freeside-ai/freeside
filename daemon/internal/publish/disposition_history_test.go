package publish

import (
	"errors"
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

// dispositionIdentitySentinel is the auth identity of the fixture's rounds.
// It names who pays, so it is recorded but never published; the value is
// distinctive so a test can prove no rendered text carries it.
const dispositionIdentitySentinel = "auth-identity-sentinel-must-not-publish"

func dispositionHistoryFixture(t *testing.T) DispositionHistory {
	t.Helper()
	runID := domain.RunID("run-disposition-history")
	// Each round renders its admitted agent's lineage group, model, and
	// effort. Round 1's model carries markup the renderer must escape.
	firstAdmission := fake.RecordAdmission(domain.RoleReviewer, runID, "review-round-1")
	firstAdmission.AuthIdentityID, firstAdmission.LineageGroup = dispositionIdentitySentinel, "openai"
	firstAdmission.AgentBinding.RouteModelID, firstAdmission.AgentBinding.NativeEffort = "codex <frontier>", "high"
	first, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-round-1", RunID: runID, Round: 1,
		Admission:           firstAdmission,
		ConfigurationDigest: dispositionDigestA, InstructionDigest: dispositionDigestB,
		BaseSHA: strings.Repeat("1", 40), HeadSHA: strings.Repeat("2", 40),
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
// renders from review records: the canonical disposition history the intent
// binds, and the composed pull request title and body (the package posts no
// comments). It searches for the recorded value, not the
// label, so a later field cannot republish the cost owner under another name.
func TestPublicationTextOmitsReviewCostOwner(t *testing.T) {
	t.Parallel()
	history := dispositionHistoryFixture(t)
	for _, review := range history.reviews {
		if payer := review.Admission.AuthIdentityID; payer != dispositionIdentitySentinel {
			t.Fatalf("round %d payer = %q, want the sentinel", review.Round, payer)
		}
	}
	section, err := RenderDispositionHistory(history)
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
	if !strings.Contains(body, "independent review") {
		t.Fatal("composed body omitted the review outcome")
	}
	for name, text := range map[string]string{
		"disposition history": section, "pull request title": title, "pull request body": body,
	} {
		if strings.Contains(text, dispositionIdentitySentinel) {
			t.Errorf("%s publishes who paid for a review:\n%s", name, text)
		}
	}
}

// preChangeDispositionHistoryDigest is the digest an intent binds for
// dispositionHistoryFixture under the canonical rendering an intent written
// before #3022 took the history out of the body bound. It is a literal, not
// today's renderer output, so a change to RenderDispositionHistory's bytes
// fails here even if the golden is regenerated. #3324 gave the fixture's
// first round an admission in place of its legacy strings and recomputed this
// literal with the renderer as it stood before that change (4481f907a).
const preChangeDispositionHistoryDigest = domain.Digest("sha256:caeb0e1cbf83b1ab851cb863c659a6499fb1cd156ca5db5383f4881c4fbe9a15")

// TestPreChangeIntentStillValidatesOnRecovery pins #3022 criterion 5's binding
// half: the body no longer shows the history, but a durable intent written
// before that change bound the digest of its canonical rendering, and recovery
// must still accept it while refusing any other digest.
func TestPreChangeIntentStillValidatesOnRecovery(t *testing.T) {
	t.Parallel()
	history := dispositionHistoryFixture(t)
	candidate := Candidate{RunID: history.runID, HeadSHA: history.headSHA, DispositionHistory: &history}
	intent := Intent{FormatVersion: IntentFormatCurrent, DispositionHistoryDigest: preChangeDispositionHistoryDigest}
	if err := ValidateIntentDispositionHistory(intent, candidate); err != nil {
		t.Fatalf("pre-change intent refused on recovery: %v", err)
	}
	intent.DispositionHistoryDigest = dispositionDigestA
	if err := ValidateIntentDispositionHistory(intent, candidate); !errors.Is(err, ErrPublicationConflict) {
		t.Fatalf("changed history digest = %v, want a publication conflict", err)
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
