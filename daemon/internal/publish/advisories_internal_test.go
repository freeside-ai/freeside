package publish

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func advisoryFinding(path string) domain.CandidateFinding {
	category := domain.ControlPlaneReviewerInstructions
	return domain.CandidateFinding{
		Class: domain.FindingClassControlPlane, Category: &category,
		Origin: domain.FindingOriginImport, Kind: "reviewer_instruction_path",
		Path: path, Disposition: domain.DispositionAdvisory,
	}
}

// TestAdvisoryFindingsSelectsOnlyAdvisoryDispositions: the publisher
// surfaces exactly the advisory stance; blocking and waived findings have
// other owners.
func TestAdvisoryFindingsSelectsOnlyAdvisoryDispositions(t *testing.T) {
	t.Parallel()
	blocking := advisoryFinding("CLAUDE.md")
	blocking.Disposition = domain.DispositionBlocking
	got := AdvisoryFindings([]domain.CandidateFinding{blocking, advisoryFinding("AGENTS.md")})
	if len(got) != 1 || got[0].Path != "AGENTS.md" {
		t.Fatalf("AdvisoryFindings = %+v, want the one advisory finding", got)
	}
	if AdvisoryFindings(nil) != nil {
		t.Fatal("empty input produced a non-nil slice")
	}
}

// TestDesiredPRContentRendersAdvisoriesBetweenProseAndHistory: the section
// is publisher-owned, escapes candidate-derived text, renders a
// non-representable path by its bytes, and sits before the disposition
// history and identity marker.
func TestDesiredPRContentRendersAdvisoriesBetweenProseAndHistory(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("a", 40)
	identity, err := DeriveIdentity(IdentityInput{
		Repo: "freeside-ai/repo", BaseRef: "main", SourceHeadSHA: head,
		ArtifactDigests: []domain.Digest{"sha256:" + domain.Digest(strings.Repeat("c", 64))},
	})
	if err != nil {
		t.Fatal(err)
	}
	hostile := advisoryFinding("")
	hostile.PathHex = "6261"
	hostile.Detail = "deleted <script>"
	_, body, err := desiredPRContent(identity, Candidate{
		Title: "Advisories", Body: "prose\n", RunID: "run-advisory", HeadSHA: head,
		Advisories: []domain.CandidateFinding{advisoryFinding("docs/AGENTS.md"), hostile},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## Freeside Control-Plane Advisories",
		"- reviewer instructions: <code>docs/AGENTS.md</code> (<code>reviewer_instruction_path</code>)",
		"- reviewer instructions: path bytes <code>6261</code> (<code>reviewer_instruction_path</code>; <code>deleted &lt;script&gt;</code>)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	prose := strings.Index(body, "prose")
	open := strings.Index(body, advisoriesOpenMarker)
	closing := strings.Index(body, advisoriesCloseMarker)
	marker := strings.Index(body, identity.Marker())
	if prose >= open || open >= closing || closing >= marker {
		t.Fatalf("section order prose=%d open=%d close=%d marker=%d", prose, open, closing, marker)
	}
	if _, plain, err := desiredPRContent(identity, Candidate{
		Title: "No advisories", Body: "prose\n", RunID: "run-plain", HeadSHA: head,
	}); err != nil || strings.Contains(plain, advisoriesMarkerName) {
		t.Fatalf("advisory-free candidate rendered a section (err %v):\n%s", err, plain)
	}
}

// TestRenderAdvisoriesBoundsTheSection: beyond the entry cap the remainder
// is counted rather than rendered, every claim is byte-bounded, and the
// whole section stays under its pinned ceiling even when each field is at
// the importer's path limit and escapes five-to-one, so the composed body
// cannot cross the forge limit through this section.
func TestRenderAdvisoriesBoundsTheSection(t *testing.T) {
	t.Parallel()
	findings := make([]domain.CandidateFinding, maxRenderedAdvisories+5)
	for i := range findings {
		hostile := advisoryFinding("")
		hostile.PathHex = strings.Repeat("&", 4096)
		hostile.Kind = strings.Repeat("&", 4096)
		hostile.Detail = strings.Repeat("&", 4096)
		hostile.Path = fmt.Sprintf("%s/%d/AGENTS.md", strings.Repeat("&", 4096), i)
		findings[i] = hostile
	}
	section := renderAdvisories(findings)
	if got := strings.Count(section, "\n- "); got != maxRenderedAdvisories+1 {
		t.Fatalf("rendered %d entries, want %d plus the omission line", got-1, maxRenderedAdvisories)
	}
	if !strings.Contains(section, "- 5 more advisory findings omitted") {
		t.Fatalf("omission line missing:\n%s", section)
	}
	if len(section) > maxRenderedAdvisoriesBytes {
		t.Fatalf("section bytes = %d, want at most %d", len(section), maxRenderedAdvisoriesBytes)
	}
	if maxRenderedAdvisoriesBytes >= maxPullRequestBodyBytes/2 {
		t.Fatalf("section ceiling %d leaves too little of the %d-byte body", maxRenderedAdvisoriesBytes, maxPullRequestBodyBytes)
	}
}

// TestValidateCandidateBodyRejectsAdvisoriesMarker: candidate prose cannot
// forge the publisher-owned section by marker or by heading, in any letter
// case.
func TestValidateCandidateBodyRejectsAdvisoriesMarker(t *testing.T) {
	t.Parallel()
	if err := ValidateCandidateBody("Fine body\n"); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		advisoriesOpenMarker, "<!-- FREESIDE:CONTROL-PLANE-ADVISORIES -->",
		advisoriesHeading, "## freeside control-plane advisories\n\n- none",
	} {
		if err := ValidateCandidateBody("prose\n" + body + "\n"); err == nil {
			t.Errorf("body with %q accepted", body)
		}
	}
}

// TestAuthorizationGateBindsAdvisoriesToTheRecord: the rendered set is a
// claim about the authorization, so the gate rejects a candidate that hides
// or invents an advisory, and accepts exactly the record's set.
func TestAuthorizationGateBindsAdvisoriesToTheRecord(t *testing.T) {
	t.Parallel()
	recipe := domain.Digest("sha256:" + strings.Repeat("a", 64))
	profile := domain.Digest("sha256:" + strings.Repeat("b", 64))
	evidence, err := domain.ComputeEvidenceSnapshotDigest(nil)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := domain.NewCandidateAuthorization(domain.CandidateAuthorizationInput{
		Repo: "freeside-ai/repo", BaseSHA: strings.Repeat("0", 40), HeadSHA: strings.Repeat("1", 40),
		ImportResultDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)), VerificationRecipeDigest: recipe,
		EvidenceSnapshotDigest: evidence, VerificationOutcome: domain.VerificationPassed,
		Findings:           []domain.CandidateFinding{advisoryFinding("AGENTS.md")},
		TrustProfileDigest: profile, InvocationID: "inv-1",
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{
		Repo: "freeside-ai/repo", HeadSHA: strings.Repeat("1", 40),
		RecipeDigest: &recipe, TrustProfileDigest: &profile, AuthorizationID: &auth.ID,
		Advisories: AdvisoryFindings(auth.Findings),
	}
	if err := validateAuthorizationCandidate(candidate, auth); err != nil {
		t.Fatalf("matching advisories rejected: %v", err)
	}
	for name, advisories := range map[string][]domain.CandidateFinding{
		"hidden":   nil,
		"invented": {advisoryFinding("AGENTS.md"), advisoryFinding("CLAUDE.md")},
		"swapped":  {advisoryFinding("CLAUDE.md")},
	} {
		candidate.Advisories = advisories
		if err := validateAuthorizationCandidate(candidate, auth); !errors.Is(err, ErrUnauthorizedPublication) {
			t.Errorf("%s advisories: err = %v, want ErrUnauthorizedPublication", name, err)
		}
	}
}
