package publish

import (
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The advisories section is publisher-owned like the disposition history:
// its marker and its heading are refused in candidate prose so an agent
// cannot forge a section, above or below the real one, that claims fewer
// or different advisories than the authorization binds.
const (
	advisoriesMarkerName  = "freeside:control-plane-advisories"
	advisoriesOpenMarker  = "<!-- " + advisoriesMarkerName + " -->"
	advisoriesCloseMarker = "<!-- /" + advisoriesMarkerName + " -->"
	advisoriesHeading     = "## Freeside Control-Plane Advisories"
	// maxRenderedAdvisories caps the entry count; each entry's path, kind,
	// and detail render as bounded claims, so the whole section has a fixed
	// byte ceiling (maxRenderedAdvisoriesBytes, pinned by test) and a
	// candidate touching many or long instruction paths cannot push the
	// composed body past the forge limit and fail its own publication. The
	// omitted remainder is counted, and the complete set stays bound in the
	// candidate authorization.
	maxRenderedAdvisories         = 8
	maxRenderedAdvisoryClaimBytes = 512
	maxRenderedAdvisoriesBytes    = maxRenderedAdvisories*3*maxRenderedAdvisoryClaimBytes + 1<<10
)

// AdvisoryFindings returns the advisory-disposition findings of an
// authorization's finding set, in the order given, for the publisher to
// surface (plan §5.8, revision 42). No other disposition is the publisher's
// to render: a blocking finding never reaches publication, and a waived one
// belongs to the disposition history. An empty result is nil so the
// authorization gate compares sets, not slice headers.
func AdvisoryFindings(findings []domain.CandidateFinding) []domain.CandidateFinding {
	var advisories []domain.CandidateFinding
	for _, finding := range findings {
		if finding.Disposition == domain.DispositionAdvisory {
			advisories = append(advisories, finding)
		}
	}
	return advisories
}

// renderAdvisories renders the deterministic advisories section. Path,
// kind, and detail are candidate-derived data: each renders through the
// same escaping code span as a disposition-history claim, under a tighter
// per-claim bound so eight entries stay a small fraction of the body.
func renderAdvisories(findings []domain.CandidateFinding) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s\n\n%s\n\n", advisoriesOpenMarker, advisoriesHeading)
	out.WriteString("This change edits paths Freeside classes as control-plane content and " +
		"publishes them as reviewed repository content instead of blocking (plan §5.8). " +
		"Freeside composes agent and reviewer instructions from the trusted base, never " +
		"from a candidate, so these edits could not govern their own review; once merged " +
		"they govern later runs. Review them deliberately.\n\n")
	for i, finding := range findings {
		if i == maxRenderedAdvisories {
			fmt.Fprintf(&out, "- %d more advisory findings omitted; the complete set is bound in the candidate authorization.\n",
				len(findings)-maxRenderedAdvisories)
			break
		}
		label := "control plane"
		if finding.Category != nil {
			label = dispositionLabel(string(*finding.Category))
		}
		location := "path bytes " + boundedClaim(finding.PathHex, maxRenderedAdvisoryClaimBytes)
		if finding.Path != "" {
			location = boundedClaim(finding.Path, maxRenderedAdvisoryClaimBytes)
		}
		fmt.Fprintf(&out, "- %s: %s (%s", label, location, boundedClaim(finding.Kind, maxRenderedAdvisoryClaimBytes))
		if finding.Detail != "" {
			fmt.Fprintf(&out, "; %s", boundedClaim(finding.Detail, maxRenderedAdvisoryClaimBytes))
		}
		out.WriteString(")\n")
	}
	fmt.Fprintf(&out, "\n%s", advisoriesCloseMarker)
	return out.String()
}

// containsAdvisoriesMarker reports whether prose carries the section's
// marker or heading, in any letter case.
func containsAdvisoriesMarker(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, advisoriesMarkerName) ||
		strings.Contains(lower, strings.ToLower(advisoriesHeading))
}
