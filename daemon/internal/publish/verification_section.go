package publish

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	verificationMarkerName           = "freeside:verification"
	verificationOpenMarker           = "<!-- " + verificationMarkerName + " -->"
	verificationCloseMarker          = "<!-- /" + verificationMarkerName + " -->"
	verificationHeading              = "## Verification"
	maxRenderedVerificationSteps     = 24
	maxRenderedVerificationArgvBytes = 512
	maxRenderedVerificationBytes     = 8 << 10
)

func containsVerificationMarker(body string) bool {
	return strings.Contains(strings.ToLower(body), verificationMarkerName) || publicationrecord.ContainsVerificationHeading(body)
}

// candidateVerificationReport binds the rendered bytes to an evidence artifact.
// The authorization gate separately binds that artifact set and the import result
// to the durable authorization before any external effect.
func candidateVerificationReport(c Candidate) (verify.Report, *domain.Artifact, error) {
	var artifact *domain.Artifact
	for i := range c.Artifacts {
		if c.Artifacts[i].Type == domain.ArtifactKindVerificationReport {
			if artifact != nil {
				return verify.Report{}, nil, fmt.Errorf("multiple verification reports: %w", ErrUnauthorizedPublication)
			}
			artifact = &c.Artifacts[i]
		}
	}
	// Legacy callers with no verifier evidence have no executed recipe to report.
	if artifact == nil && len(c.VerificationReport) == 0 {
		return verify.Report{}, nil, nil
	}
	if artifact == nil || len(c.VerificationReport) == 0 || c.ImportResult == nil ||
		artifact.Digest != domain.Digest(contentaddr.Sum(c.VerificationReport)) {
		return verify.Report{}, nil, fmt.Errorf("verification report or import result missing or unbound: %w", ErrUnauthorizedPublication)
	}
	rep, err := verify.ParseReport(c.VerificationReport)
	if err != nil {
		return verify.Report{}, nil, fmt.Errorf("verification report: %w: %w", err, ErrUnauthorizedPublication)
	}
	if c.RecipeDigest == nil || rep.HeadSHA != c.HeadSHA || rep.RecipeDigest != *c.RecipeDigest {
		return verify.Report{}, nil, fmt.Errorf("verification report does not describe candidate: %w", ErrUnauthorizedPublication)
	}
	return rep, artifact, nil
}

func validateVerificationCandidate(c Candidate, auth domain.CandidateAuthorization) (verify.Report, domain.Digest, error) {
	rep, artifact, err := candidateVerificationReport(c)
	if err != nil || artifact == nil {
		return rep, "", err
	}
	imported, err := json.Marshal(c.ImportResult)
	if err != nil {
		return verify.Report{}, "", fmt.Errorf("digest import result: %w: %w", err, ErrUnauthorizedPublication)
	}
	if rep.BaseSHA != auth.BaseSHA || domain.VerificationOutcome(rep.Outcome) != auth.VerificationOutcome ||
		domain.Digest(contentaddr.Sum(imported)) != auth.ImportResultDigest {
		return verify.Report{}, "", fmt.Errorf("verification or import result disagrees with authorization: %w", ErrUnauthorizedPublication)
	}
	return rep, artifact.Digest, nil
}

func renderVerification(rep verify.Report, digest domain.Digest, invocation domain.InvocationID, claims []domain.AgentClaim) (string, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n\n%s\n\n", verificationOpenMarker, verificationHeading)
	for _, fact := range []struct {
		label, value string
	}{
		{"Outcome", string(rep.Outcome)},
		{"Head", rep.HeadSHA},
		{"Base", rep.BaseSHA},
		{"Recipe digest", string(rep.RecipeDigest)},
		{"Verification invocation", string(invocation)},
		{"Report artifact digest", string(digest)},
	} {
		fmt.Fprintf(&out, "- %s: %s\n", fact.label, dispositionCode(fact.value))
	}
	out.WriteString("\nFreeside ran these recipe commands at the head above. Any other check claimed in operator prose or agent evidence was not run by Freeside. Publisher-owned review evidence is recorded separately.\n\n")
	fmt.Fprintf(&out, "Executed steps: %d.\n\n", len(rep.Steps))
	for i, step := range rep.Steps {
		if i == maxRenderedVerificationSteps {
			fmt.Fprintf(&out, "\nOmitted %d further steps; see the report artifact.\n", len(rep.Steps)-i)
			break
		}
		// JSON preserves argument boundaries, including spaces and shell syntax.
		argv, err := json.Marshal(step.Argv)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "%d. %s, exit %d", i+1, boundedClaim(string(argv), maxRenderedVerificationArgvBytes), step.ExitCode)
		if step.OutputTruncated {
			out.WriteString("; output truncated")
		}
		out.WriteByte('\n')
	}
	if rep.TranscriptTruncated {
		out.WriteString("\nTranscript truncated.\n")
	}
	out.WriteString("\n### Agent-Reported Evidence (claims, not run by Freeside)\n\n")
	if len(claims) == 0 {
		out.WriteString("None.\n")
	}
	for _, claim := range claims {
		fmt.Fprintf(&out, "- Label: %s; artifact: %s; digest: %s; media type: %s\n",
			boundedClaim(claim.Label, 512), boundedClaim(string(claim.Artifact), 512),
			boundedClaim(string(claim.Digest), 512), boundedClaim(string(claim.Metadata.MediaType), 512))
	}
	fmt.Fprintf(&out, "\n%s", verificationCloseMarker)
	if out.Len() > maxRenderedVerificationBytes {
		return "", fmt.Errorf("verification section exceeds %d bytes", maxRenderedVerificationBytes)
	}
	return out.String(), nil
}
