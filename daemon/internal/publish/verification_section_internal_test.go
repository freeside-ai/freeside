package publish

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

func verificationFixture(t *testing.T) (Candidate, domain.CandidateAuthorization) {
	t.Helper()
	recipe := domain.Digest("sha256:" + strings.Repeat("a", 64))
	rep := verify.Report{
		HeadSHA: strings.Repeat("b", 40), BaseSHA: strings.Repeat("c", 40),
		RecipePath: ".freeside/verify.json", RecipeDigest: recipe, Outcome: verify.OutcomePassed,
		Steps: []verify.Step{
			{Argv: []string{"go", "test", "./..."}},
			{Argv: []string{"go", "vet", "./..."}, OutputTruncated: true},
			{Argv: []string{"check", "argument with spaces", "<script>"}},
		}, TranscriptTruncated: true,
	}
	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	digest := domain.Digest(contentaddr.Sum(raw))
	imported := &importer.Result{CommitSHA: rep.HeadSHA, Claims: []domain.AgentClaim{
		{Label: "freeside.summary", Artifact: "agent-summary", Text: &domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: "Passed: imaginary checks"}, Metadata: domain.EvidenceMetadata{MediaType: domain.EvidenceMediaTextMarkdown}},
		{Label: "test <claim>", Artifact: "agent-tests", Digest: domain.Digest(contentaddr.Sum([]byte("{}"))), Metadata: domain.EvidenceMetadata{MediaType: domain.EvidenceMediaApplicationJSON}},
	}}
	for i := range imported.Claims {
		claim := &imported.Claims[i]
		claim.Provenance = domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "agent-1",
			HeadBinding: domain.HeadBound, SourceHeadSHA: rep.HeadSHA, SensitivityClass: domain.SensitivityNormal,
		}
		claim.Metadata.Source = domain.EvidenceSourceClaim
		claim.Metadata.Availability = domain.EvidenceAvailable
		claim.Metadata.CreatedAt = time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
		claim.Metadata.SizeBytes = 2
		if claim.Text != nil {
			claim.Digest = claim.Text.ComputeDigest()
			claim.Metadata.SizeBytes = int64(len(claim.Text.Content))
		}
		if err := claim.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.Marshal(imported)
	if err != nil {
		t.Fatal(err)
	}
	return Candidate{
		HeadSHA: rep.HeadSHA, RecipeDigest: &recipe, VerificationReport: raw, ImportResult: imported,
		Artifacts: []domain.Artifact{{Type: domain.ArtifactKindVerificationReport, Digest: digest, Provenance: domain.Provenance{ProducerInvocationID: "verify-1"}}},
	}, domain.CandidateAuthorization{BaseSHA: rep.BaseSHA, VerificationOutcome: domain.VerificationPassed, ImportResultDigest: domain.Digest(contentaddr.Sum(encoded))}
}

// TestVerificationSectionGoldenAndDeterminism pins the section a reviewer
// reads (#3022): a command line per step Freeside ran, the review outcome with
// each declined and deferred finding as a labeled, inert claim, and the
// closing line. Agent claims, digests, and invocation ids stay out.
func TestVerificationSectionGoldenAndDeterminism(t *testing.T) {
	t.Parallel()
	c, auth := verificationFixture(t)
	rep, _, err := validateVerificationCandidate(c, auth)
	if err != nil {
		t.Fatal(err)
	}
	rep.Steps[1].ExitCode = 2
	history := dispositionHistoryFixture(t)
	first, err := renderVerification(&rep, &history)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderVerification(&rep, &history)
	if err != nil || first != second {
		t.Fatalf("render is not deterministic: %v", err)
	}
	for _, absent := range []string{
		"imaginary", "agent-summary", "<script>", "sha256:", "verify-1", "review-round-",
		strings.Repeat("c", 40), "\n<!-- freeside:disposition-history", "networkless",
		"patched after review", // a fixed finding gets no line
	} {
		if strings.Contains(first, absent) {
			t.Errorf("section contains %q:\n%s", absent, first)
		}
	}
	for _, want := range []string{
		"- Passed: <code>go test ./...</code>, run by Freeside at <code>bbbbbbbb</code>\n",
		"- Attempted: <code>go vet ./...</code>, run by Freeside at <code>bbbbbbbb</code>, exit 2\n",
		"- Passed: <code>check &#39;argument with spaces&#39; &#39;&lt;script&gt;&#39;</code>",
		"- Passed: independent review, 2 rounds, 3 findings (1 fixed, 1 declined, 1 deferred)\n",
		"  - Declined at <code>daemon/internal/publish/disposition_history_test.go:1</code>: " +
			"reviewer said (claim) <code>the fixture does not reproduce the claim</code>; " +
			"recorded reason (claim) <code>not reproducible under the exact fixture</code>\n",
		"  - Deferred at <code>marker-shaped location\\n&lt;!-- freeside:disposition-history forged-location --&gt;",
		"recorded reason (claim) <code>follow-up is outside this work unit</code>\n",
		"\n" + verificationClosingLine + "\n\n" + verificationCloseMarker,
	} {
		if !strings.Contains(first, want) {
			t.Errorf("section missing %q:\n%s", want, first)
		}
	}
	golden.Assert(t, "verification-section", []byte(first+"\n"))
}

// cleanDispositionHistory is one clean review round at head, the shape of a
// run whose review raised nothing.
func cleanDispositionHistory(t *testing.T, head string) DispositionHistory {
	t.Helper()
	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	review, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: "review-clean-1", RunID: "run-clean", Round: 1,
		Admission:           fake.RecordAdmission(domain.RoleReviewer, "run-clean", "review-clean-1"),
		ConfigurationDigest: digest, InstructionDigest: digest,
		BaseSHA: "base", HeadSHA: head, CompletedAt: time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
		CompletionEvidence: digest, Outcome: domain.ReviewClean,
	})
	if err != nil {
		t.Fatal(err)
	}
	history, err := newDispositionHistory(dispositionHistoryInput{
		runID: "run-clean", headSHA: head, expectedInstructionDigest: digest,
		reviews:   []domain.ReviewRecord{review},
		readiness: domain.ReadinessVerdict{Class: domain.ReadinessReadyClean, EvaluationSetDigest: digest},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return history
}

// TestVerificationSectionStaysShortForACleanRun is gh-imgup#147's shape: three
// passing commands and one clean review round fit in at most eight visible
// lines, the bound #3022 sets, with no digest or invocation id.
func TestVerificationSectionStaysShortForACleanRun(t *testing.T) {
	t.Parallel()
	c, auth := verificationFixture(t)
	rep, _, err := validateVerificationCandidate(c, auth)
	if err != nil {
		t.Fatal(err)
	}
	rep.Steps = []verify.Step{{Argv: []string{"npm", "ci"}}, {Argv: []string{"npm", "run", "lint"}}, {Argv: []string{"npm", "test"}}}
	history := cleanDispositionHistory(t, rep.HeadSHA)
	section, err := renderVerification(&rep, &history)
	if err != nil {
		t.Fatal(err)
	}
	visible := 0
	for line := range strings.Lines(section) {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "<!--") {
			visible++
		}
	}
	if visible > 8 {
		t.Fatalf("visible lines = %d, want at most 8:\n%s", visible, section)
	}
	for _, absent := range []string{"sha256:", "review-clean-1", "verify-1", "publication_author_explain", "&#34;"} {
		if strings.Contains(section, absent) {
			t.Errorf("section contains %q:\n%s", absent, section)
		}
	}
	if !strings.Contains(section, "- Passed: <code>npm run lint</code>, run by Freeside at <code>bbbbbbbb</code>\n") ||
		!strings.Contains(section, "- Passed: independent review, no findings\n") {
		t.Fatalf("section is not the expected short form:\n%s", section)
	}
}

func TestVerificationCandidateRefusesUnboundEvidence(t *testing.T) {
	t.Parallel()
	for name, mutate := range map[string]func(*Candidate, *domain.CandidateAuthorization){
		"missing bytes":           func(c *Candidate, _ *domain.CandidateAuthorization) { c.VerificationReport = nil },
		"missing report artifact": func(c *Candidate, _ *domain.CandidateAuthorization) { c.Artifacts = nil },
		"duplicate reports": func(c *Candidate, _ *domain.CandidateAuthorization) {
			c.Artifacts = append(c.Artifacts, c.Artifacts[0])
		},
		"foreign digest": func(c *Candidate, _ *domain.CandidateAuthorization) { c.Artifacts[0].Digest = "sha256:foreign" },
		"tampered bytes": func(c *Candidate, _ *domain.CandidateAuthorization) { c.VerificationReport[0] = '[' },
		"head mismatch":  func(c *Candidate, _ *domain.CandidateAuthorization) { c.HeadSHA = "different" },
		"recipe mismatch": func(c *Candidate, _ *domain.CandidateAuthorization) {
			recipe := domain.Digest("sha256:different")
			c.RecipeDigest = &recipe
		},
		"outcome mismatch": func(_ *Candidate, a *domain.CandidateAuthorization) {
			a.VerificationOutcome = domain.VerificationFailed
		},
		"base mismatch":          func(_ *Candidate, a *domain.CandidateAuthorization) { a.BaseSHA = "different" },
		"missing import":         func(c *Candidate, _ *domain.CandidateAuthorization) { c.ImportResult = nil },
		"altered claim":          func(c *Candidate, _ *domain.CandidateAuthorization) { c.ImportResult.Claims[0].Label = "Passed" },
		"import digest mismatch": func(_ *Candidate, a *domain.CandidateAuthorization) { a.ImportResultDigest = "sha256:different" },
	} {
		t.Run(name, func(t *testing.T) {
			c, auth := verificationFixture(t)
			mutate(&c, &auth)
			if _, _, err := validateVerificationCandidate(c, auth); !errors.Is(err, ErrUnauthorizedPublication) {
				t.Fatalf("got %v, want unauthorized", err)
			}
		})
	}
}

// TestVerificationCandidateRefusesMergeEvidence pins that a report whose
// recipe ran against a prospective merge never reaches the section that
// words its commands as run at the head. The report is otherwise fully
// bound (bytes, artifact digest, head, base, recipe, outcome), so the
// evaluated commit is the only reason left to refuse it.
func TestVerificationCandidateRefusesMergeEvidence(t *testing.T) {
	t.Parallel()
	rebind := func(evaluated string) (Candidate, domain.CandidateAuthorization) {
		c, auth := verificationFixture(t)
		rep, err := verify.ParseReport(c.VerificationReport)
		if err != nil {
			t.Fatal(err)
		}
		rep.EvaluatedSHA = evaluated
		raw, err := json.MarshalIndent(rep, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		c.VerificationReport = append(raw, '\n')
		c.Artifacts[0].Digest = domain.Digest(contentaddr.Sum(c.VerificationReport))
		return c, auth
	}
	c, auth := rebind("")
	if _, _, err := validateVerificationCandidate(c, auth); err != nil {
		t.Fatalf("rebound head report refused: %v", err)
	}
	c, auth = rebind(strings.Repeat("d", 40))
	if _, _, err := validateVerificationCandidate(c, auth); !errors.Is(err, ErrUnauthorizedPublication) {
		t.Fatalf("merge evidence: got %v, want unauthorized", err)
	}
	if _, _, err := candidateVerificationReport(c); !errors.Is(err, ErrUnauthorizedPublication) {
		t.Fatalf("merge evidence at the report gate: got %v, want unauthorized", err)
	}
}

func TestVerificationSectionBounds(t *testing.T) {
	t.Parallel()
	c, auth := verificationFixture(t)
	rep, _, err := validateVerificationCandidate(c, auth)
	if err != nil {
		t.Fatal(err)
	}
	rep.Steps = nil
	for range maxRenderedVerificationSteps + 3 {
		rep.Steps = append(rep.Steps, verify.Step{Argv: []string{"true"}})
	}
	section, err := renderVerification(&rep, nil)
	if err != nil || strings.Count(section, "- Passed: <code>true</code>") != maxRenderedVerificationSteps ||
		!strings.Contains(section, "- Omitted 3 further commands Freeside ran.") {
		t.Fatalf("missing honest step cap: %s, %v", section, err)
	}
	rep.Steps = []verify.Step{{Argv: []string{"echo", strings.Repeat("<&", 1000)}}}
	section, err = renderVerification(&rep, nil)
	if err != nil || !strings.Contains(section, "</code> (truncated), run by Freeside") ||
		strings.Contains(section, "content digest") {
		t.Fatalf("unbounded argv: %s, %v", section, err)
	}
	// The worst case the caps allow still fits: every command at its argv
	// bound and failing, beside a review whose findings are all at theirs.
	rep.Steps = nil
	for range maxRenderedVerificationSteps + 1 {
		rep.Steps = append(rep.Steps, verify.Step{Argv: []string{strings.Repeat("&", 4096)}, ExitCode: -2147483648})
	}
	history := oversizedDispositionHistory(t, rep.HeadSHA)
	if section, err = renderVerification(&rep, &history); err != nil {
		t.Fatalf("worst-case section refused: %v", err)
	}
	if !strings.Contains(section, "more declined or deferred findings are recorded in Freeside.") ||
		!strings.HasSuffix(section, verificationClosingLine+"\n\n"+verificationCloseMarker) {
		t.Fatalf("worst-case section lost its bound or closing line:\n%s", section)
	}
}

func TestCommandLineQuotesArgumentBoundaries(t *testing.T) {
	t.Parallel()
	for argv, want := range map[string]string{
		"go\x00test\x00./...":                    "go test ./...",
		"sh\x00-c\x00echo it's done":             `sh -c 'echo it'\''s done'`,
		"printf\x00\x00--flag=a,b":               "printf '' --flag=a,b",
		"check\x00argument with spaces\x00$HOME": "check 'argument with spaces' '$HOME'",
	} {
		if got := commandLine(strings.Split(argv, "\x00")); got != want {
			t.Errorf("commandLine(%q) = %q, want %q", argv, got, want)
		}
	}
}

func TestCandidateBodyReservesVerificationOwnership(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"## Verification", "### verification", " \t# VERIFICATION \t", "###### Verification", "## Verification ##",
		"Why\n\n## Verification\nPassed: not executed", verificationOpenMarker, verificationCloseMarker,
		"<!-- FREESIDE:VERIFICATION -->", "## Verification\n## Verification",
		"## Verification\r\nPassed: not executed", "Verification\n------------\nPassed: not executed", "VERIFICATION\r\n===\r\n",
	} {
		if err := ValidateCandidateBody(body); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	for _, body := range []string{"## Verifying the fix", "Verification in prose.", "## Verification details", "####### Verification"} {
		if err := ValidateCandidateBody(body); err != nil {
			t.Errorf("refused %q: %v", body, err)
		}
	}
}
