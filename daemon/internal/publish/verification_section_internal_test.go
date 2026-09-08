package publish

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
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

func TestVerificationSectionGoldenAndDeterminism(t *testing.T) {
	t.Parallel()
	c, auth := verificationFixture(t)
	rep, digest, err := validateVerificationCandidate(c, auth)
	if err != nil {
		t.Fatal(err)
	}
	first, err := renderVerification(rep, digest, "verify-1", c.ImportResult.Claims)
	if err != nil {
		t.Fatal(err)
	}
	second, err := renderVerification(rep, digest, "verify-1", c.ImportResult.Claims)
	if err != nil || first != second {
		t.Fatalf("render is not deterministic: %v", err)
	}
	if strings.Contains(first, "imaginary") || strings.Contains(first, "<script>") {
		t.Fatal("raw claim or command markup escaped its boundary")
	}
	if strings.Contains(first, "networkless") {
		t.Fatal("report renderer asserted isolation absent from the bound report")
	}
	if !strings.Contains(first, "Any other check claimed in operator prose or agent evidence") ||
		!strings.Contains(first, "Publisher-owned review evidence is recorded separately.") {
		t.Fatal("not-run disclaimer must preserve separately rendered publisher review evidence")
	}
	golden.Assert(t, "verification-section", []byte(first+"\n"))
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

func TestVerificationSectionBounds(t *testing.T) {
	t.Parallel()
	c, auth := verificationFixture(t)
	rep, digest, err := validateVerificationCandidate(c, auth)
	if err != nil {
		t.Fatal(err)
	}
	rep.Steps = nil
	for range maxRenderedVerificationSteps + 3 {
		rep.Steps = append(rep.Steps, verify.Step{Argv: []string{"true"}})
	}
	section, err := renderVerification(rep, digest, "verify-1", nil)
	if err != nil || !strings.Contains(section, "Executed steps: 27") || !strings.Contains(section, "Omitted 3 further steps") {
		t.Fatalf("missing honest step cap: %s, %v", section, err)
	}
	rep.Steps = []verify.Step{{Argv: []string{"echo", strings.Repeat("<&", 1000)}}}
	section, err = renderVerification(rep, digest, "verify-1", nil)
	if err != nil || !strings.Contains(section, "truncated; content digest") {
		t.Fatalf("unbounded argv: %v", err)
	}
	claims := make([]domain.AgentClaim, 100)
	for i := range claims {
		claims[i] = c.ImportResult.Claims[0]
	}
	if _, err := renderVerification(rep, digest, "verify-1", claims); err == nil {
		t.Fatal("accepted oversized section")
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
