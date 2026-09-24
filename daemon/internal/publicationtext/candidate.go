package publicationtext

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

// Publisher section markers and byte budgets are shared by candidate screening
// and the publisher's renderers. Their values preserve the existing formats.
const (
	MarkerPrefix            = "<!-- freeside:publication-identity="
	MarkerSuffix            = " -->"
	MaxPullRequestBodyBytes = 64 << 10
	IdentityDigestBytes     = len("sha256:") + sha256.Size*2
	IdentityMarkerBytes     = len(MarkerPrefix) + IdentityDigestBytes + len(MarkerSuffix)
	MaxCandidateBodyBytes   = MaxPullRequestBodyBytes - 6*len("\n\n") -
		IdentityMarkerBytes - MaxRenderedVerificationBytes - MaxRenderedAdvisoriesBytes - MaxRenderedScopeDecisionBytes - MaxRenderedSourceReferenceBytes - MinRenderedDispositionHistoryBytes
	VerificationMarkerName             = "freeside:verification"
	MaxRenderedVerificationBytes       = 8 << 10
	SourceReferenceMarkerName          = "freeside:source-reference"
	SourceReferenceHeading             = "## Source issue"
	MaxRenderedSourceReferenceBytes    = 1 << 10
	DispositionHistoryMarkerName       = "freeside:disposition-history"
	MinRenderedDispositionHistoryBytes = 8 << 10
	AdvisoriesMarkerName               = "freeside:control-plane-advisories"
	AdvisoriesHeading                  = "## Freeside Control-Plane Advisories"
	MaxRenderedAdvisories              = 8
	MaxRenderedAdvisoryClaimBytes      = 512
	MaxRenderedAdvisoriesBytes         = MaxRenderedAdvisories*3*MaxRenderedAdvisoryClaimBytes + 1<<10
	ScopeDecisionMarkerName            = "freeside:scope-decision"
	ScopeDecisionHeading               = "## Freeside Scope Decision"
	MaxRenderedScopeDecisionBytes      = 12 << 10
)

// ValidateCandidateBody rejects publisher-owned sections and preserves room
// for the publisher to append its own evidence and control sections.
func ValidateCandidateBody(body string) error {
	if len(body) > MaxPullRequestBodyBytes {
		return fmt.Errorf("candidate body exceeds %d bytes", MaxPullRequestBodyBytes)
	}
	if len(body) > MaxCandidateBodyBytes {
		return fmt.Errorf(
			"candidate body exceeds %d bytes after reserving the publisher-owned sections",
			MaxCandidateBodyBytes,
		)
	}
	for line := range strings.Lines(body) {
		if strings.HasPrefix(strings.TrimSpace(line), MarkerPrefix) {
			return errors.New("candidate body contains a publication identity marker")
		}
	}
	if containsDispositionHistoryMarker(body) {
		return errors.New("candidate body contains a disposition history marker")
	}
	if containsAdvisoriesMarker(body) {
		return errors.New("candidate body contains a control-plane advisories marker or heading")
	}
	if containsScopeDecisionMarker(body) {
		return errors.New("candidate body contains a scope decision marker or heading")
	}
	if containsVerificationMarker(body) {
		return errors.New("candidate body contains a verification section marker or heading")
	}
	if ContainsSourceReferenceMarker(body) {
		return errors.New("candidate body contains a source reference marker or heading")
	}
	return nil
}

func containsVerificationMarker(body string) bool {
	return strings.Contains(strings.ToLower(body), VerificationMarkerName) || publicationrecord.ContainsVerificationHeading(body)
}

// ContainsSourceReferenceMarker recognizes the reserved marker or exact heading.
func ContainsSourceReferenceMarker(body string) bool {
	if strings.Contains(strings.ToLower(body), SourceReferenceMarkerName) {
		return true
	}
	// Match the section heading as a whole line, not the v1 prose line
	// "Source issue: <url>", which a v1 record keeps in its body.
	heading := strings.ToLower(SourceReferenceHeading)
	for line := range strings.Lines(body) {
		if strings.ToLower(strings.TrimSpace(line)) == heading {
			return true
		}
	}
	return false
}

func containsDispositionHistoryMarker(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, DispositionHistoryMarkerName)
}

func containsAdvisoriesMarker(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, AdvisoriesMarkerName) ||
		strings.Contains(lower, strings.ToLower(AdvisoriesHeading))
}

func containsScopeDecisionMarker(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, ScopeDecisionMarkerName) || strings.Contains(lower, strings.ToLower(ScopeDecisionHeading))
}
