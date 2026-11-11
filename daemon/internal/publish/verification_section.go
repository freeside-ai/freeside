package publish

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

const (
	verificationMarkerName           = publicationtext.VerificationMarkerName
	verificationOpenMarker           = "<!-- " + verificationMarkerName + " -->"
	verificationCloseMarker          = "<!-- /" + verificationMarkerName + " -->"
	verificationHeading              = "## Verification"
	maxRenderedVerificationSteps     = 24
	maxRenderedVerificationArgvBytes = 512
	// verificationHeadPrefixLen is how much of the head a command bullet
	// names: enough to find the commit, short enough to read.
	verificationHeadPrefixLen = 8
	// maxRenderedVerificationClaimBytes bounds each recorded finding string,
	// as advisories bound theirs.
	maxRenderedVerificationClaimBytes = publicationtext.MaxRenderedAdvisoryClaimBytes
	maxRenderedVerificationFindings   = 20
	maxOmittedFindingsLineBytes       = 96
	// The section may use the disposition history's reserved floor as well as
	// its own: the body no longer carries the history (#3022), and
	// MaxCandidateBodyBytes still reserves both, so prose accepted before this
	// change still fits beside the declined and deferred findings.
	maxRenderedVerificationSectionBytes = publicationtext.MaxRenderedVerificationBytes +
		publicationtext.MinRenderedDispositionHistoryBytes
	verificationClosingLine = "Freeside ran only the checks listed here. " +
		"The rest of this description is its author's account."
)

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
	// A report that names an evaluated commit ran its recipe against the
	// prospective merge of the head into its base, not the head's own tree.
	// The section this gate feeds says its commands ran at the head, and the
	// binding above compares head and recipe only, so merge evidence is
	// refused here instead of being published as head evidence.
	if rep.EvaluatedSHA != "" {
		return verify.Report{}, nil, fmt.Errorf(
			"verification report evaluated a prospective merge, not the candidate head: %w", ErrUnauthorizedPublication,
		)
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

// renderVerification writes the publisher-owned Verification section for the
// target repository's reviewer (#3022): one bullet per recipe command Freeside
// ran at the head, one bullet for the independent review's outcome with each
// declined or deferred finding under it, and a closing line that labels
// everything else in the body as its author's account. rep is nil when the
// candidate carries no bound report, and history is nil when it carries no
// review records; the caller renders the section only when one is present.
//
// The section shows no digest, invocation id, base commit, or artifact list:
// those stay in the store and the clients. Every recorded string is rendered
// inert through dispositionCode, and every cap is a fixed byte or count bound,
// so third-party volume shortens the section and never fails it.
func renderVerification(rep *verify.Report, history *DispositionHistory) (string, error) {
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n\n%s\n\n", verificationOpenMarker, verificationHeading)
	if rep != nil {
		at := rep.HeadSHA[:min(len(rep.HeadSHA), verificationHeadPrefixLen)]
		for i, step := range rep.Steps {
			if i == maxRenderedVerificationSteps {
				fmt.Fprintf(&out, "- Omitted %d further commands Freeside ran.\n", len(rep.Steps)-i)
				break
			}
			command := shortClaim(commandLine(step.Argv), maxRenderedVerificationArgvBytes)
			if step.ExitCode == 0 {
				fmt.Fprintf(&out, "- Passed: %s, run by Freeside at %s\n", command, dispositionCode(at))
			} else {
				fmt.Fprintf(&out, "- Attempted: %s, run by Freeside at %s, exit %d\n", command, dispositionCode(at), step.ExitCode)
			}
		}
	}
	tail := "\n" + verificationClosingLine + "\n\n" + verificationCloseMarker
	if history != nil {
		if err := history.validate(); err != nil {
			return "", fmt.Errorf("render verification review: %w", err)
		}
		out.WriteString(reviewBullet(*history))
		findings := unresolvedFindingLines(*history)
		for i, line := range findings {
			// Leave room for the omitted-findings line and the tail, so the
			// section always ends with its closing line and marker.
			if i == maxRenderedVerificationFindings ||
				out.Len()+len(line)+maxOmittedFindingsLineBytes+len(tail) > maxRenderedVerificationSectionBytes {
				fmt.Fprintf(&out, "  - %d more declined or deferred findings are recorded in Freeside.\n", len(findings)-i)
				break
			}
			out.WriteString(line)
		}
	}
	out.WriteString(tail)
	if out.Len() > maxRenderedVerificationSectionBytes {
		return "", fmt.Errorf("verification section exceeds %d bytes", maxRenderedVerificationSectionBytes)
	}
	return out.String(), nil
}

// reviewBullet summarizes the review lineage that authorized publication:
// clean when no round raised a finding, otherwise its rounds and how each
// finding was dispositioned. newDispositionHistory already refused a lineage
// whose latest round is unfinished or whose readiness is blocked, so the
// review passed by the time this renders.
func reviewBullet(h DispositionHistory) string {
	if len(h.dispositions) == 0 {
		return "- Passed: independent review, no findings\n"
	}
	counts := map[domain.ReviewDisposition]int{}
	for _, disposition := range h.dispositions {
		counts[disposition.Disposition]++
	}
	return fmt.Sprintf("- Passed: independent review, %s, %s (%d fixed, %d declined, %d deferred)\n",
		plural(len(h.reviews), "round"), plural(len(h.dispositions), "finding"),
		counts[domain.ReviewDispositionFixed], counts[domain.ReviewDispositionDeclined],
		counts[domain.ReviewDispositionDeferred])
}

// unresolvedFindingLines renders one sub-bullet per declined or deferred
// finding, in the history's canonical round and finding order: its location,
// the reviewer's message, and the recorded reason, each a labeled claim. A
// fixed finding needs no line: the change already answers it.
func unresolvedFindingLines(h DispositionHistory) []string {
	findings := make(map[domain.FindingID]domain.Finding, len(h.findings))
	for _, finding := range h.findings {
		findings[finding.ID] = finding
	}
	var lines []string
	for _, disposition := range h.dispositions {
		var label string
		switch disposition.Disposition {
		case domain.ReviewDispositionDeclined:
			label = "Declined"
		case domain.ReviewDispositionDeferred:
			label = "Deferred"
		case domain.ReviewDispositionFixed:
			continue
		}
		if label == "" {
			continue
		}
		finding := findings[disposition.FindingID]
		var line strings.Builder
		line.WriteString("  - " + label)
		if finding.Location != nil {
			line.WriteString(" at " + shortClaim(finding.Location.String(), maxRenderedVerificationClaimBytes))
		}
		fmt.Fprintf(&line, ": reviewer said (claim) %s; recorded reason (claim) %s\n",
			shortClaim(finding.Message, maxRenderedVerificationClaimBytes),
			shortClaim(disposition.Reason, maxRenderedVerificationClaimBytes))
		lines = append(lines, line.String())
	}
	return lines
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// commandLine joins a recipe command's argv into one shell-style line, quoting
// an argument that is empty or holds anything beyond a conservative safe set,
// so the reader sees the argument boundaries the verifier ran.
func commandLine(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = shellQuote(arg)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	if arg != "" && strings.Trim(arg, shellSafeChars) == "" {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", `'\''`) + "'"
}

const shellSafeChars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./:=@%+,"
