package publish

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func testSourceReference(t *testing.T, kind domain.ClosureReference) string {
	t.Helper()
	section, err := renderSourceReference(closureResolution{
		outcome:   domain.ClosureOutcome{Reference: kind},
		target:    7,
		sourceURL: "https://github.com/other/repo/issues/7",
	})
	if err != nil || section == "" {
		t.Fatalf("renderSourceReference(%q) = %q, %v", kind, section, err)
	}
	return section
}

// TestPlaceSourceReferenceEndsTheWhySection covers criterion 9 of #3022: each
// reference kind closes an opening Why section, before the next section.
func TestPlaceSourceReferenceEndsTheWhySection(t *testing.T) {
	t.Parallel()
	prose := "## Why\n\nThe parser crashed on empty input.\n\n## What\n\nHandle the nil case."
	for _, kind := range []domain.ClosureReference{
		domain.ClosureReferenceCloses, domain.ClosureReferenceRefs, domain.ClosureReferenceDescriptiveLink,
	} {
		reference := testSourceReference(t, kind)
		got := placeSourceReference(reference, prose)
		want := "## Why\n\nThe parser crashed on empty input.\n\n" + reference + "\n\n## What\n\nHandle the nil case."
		if got != want {
			t.Errorf("%s placement =\n%s\nwant\n%s", kind, got, want)
		}
	}
}

func TestPlaceSourceReferenceEndsAWhyOnlyBody(t *testing.T) {
	t.Parallel()
	reference := testSourceReference(t, domain.ClosureReferenceCloses)
	for _, prose := range []string{"## Why\n\nOnly a reason.", "## Why\n\nOnly a reason.\n\n"} {
		if got, want := placeSourceReference(reference, prose), "## Why\n\nOnly a reason.\n\n"+reference; got != want {
			t.Errorf("placement in %q =\n%s\nwant\n%s", prose, got, want)
		}
	}
}

// TestPlaceSourceReferenceSkipsFencedSectionHeadings places the reference
// after a fence that closes inside Why, and never at a "## " line inside one:
// that line is code, not the next section.
func TestPlaceSourceReferenceSkipsFencedSectionHeadings(t *testing.T) {
	t.Parallel()
	reference := testSourceReference(t, domain.ClosureReferenceCloses)
	tests := map[string]struct{ prose, want string }{
		"closed fence inside Why": {
			prose: "## Why\n\n```sh\nmake crash\n```\n\n## What\n\nFix.",
			want:  "## Why\n\n```sh\nmake crash\n```\n\n" + reference + "\n\n## What\n\nFix.",
		},
		"section heading inside a fence": {
			prose: "## Why\n\n~~~~markdown\n## Not a section\n~~~\nstill code\n~~~~\n\n## What\n\nFix.",
			want:  "## Why\n\n~~~~markdown\n## Not a section\n~~~\nstill code\n~~~~\n\n" + reference + "\n\n## What\n\nFix.",
		},
	}
	for name, tt := range tests {
		if got := placeSourceReference(reference, tt.prose); got != tt.want {
			t.Errorf("%s: placement =\n%s\nwant\n%s", name, got, tt.want)
		}
	}
}

// TestPlaceSourceReferenceFallsBackToTheHead keeps the reference at the head
// whenever the rule can't show a placement inside Why safe.
func TestPlaceSourceReferenceFallsBackToTheHead(t *testing.T) {
	t.Parallel()
	reference := testSourceReference(t, domain.ClosureReferenceCloses)
	for name, prose := range map[string]string{
		"no Why heading":         "## What\n\nHandle the nil case.",
		"Why not first":          "Intro.\n\n## Why\n\nReason.",
		"other heading level":    "# Why\n\nReason.",
		"Why with suffix":        "## Why this matters\n\nReason.",
		"fence open at the end":  "## Why\n\n```\nunclosed\n## What",
		"raw HTML line":          "## Why\n\n<details>\n\nReason.\n\n</details>\n\n## What\n\nFix.",
		"indented raw HTML":      "## Why\n\n   <!-- note\n\n## What\n\nFix.",
		"HTML comment in Why":    "## Why\n\nReason.\n<!-- unclosed\n\n## What",
		"carriage-return header": "## Why\r\n\r\nReason.",
		// From review of #3022: GitHub ends the list item's
		// fence at "## How", so the bare fence after it opens a new block
		// that would hold the reference.
		"fence inside a list item": "## Why\n\n- Run:\n  ```sh\n  go test\n## How\n```\nx\n## What\n\nFix.",
		"indented fence opener":    "## Why\n\n   ```\ncode\n   ```\n\n## What",
		"carriage-return closer":   "## Why\n\n```\ncode\n```\r\n## How\n```\n## What",
	} {
		if got, want := placeSourceReference(reference, prose), reference+"\n\n"+prose; got != want {
			t.Errorf("%s: placement =\n%s\nwant the reference at the head", name, got)
		}
	}
}

// TestPlaceSourceReferenceTreatsBacktickInfoAsProse pins that a backtick
// line whose info string holds a backtick opens no fence under GFM, so the
// "## " line after it ends the section as GitHub renders it.
func TestPlaceSourceReferenceTreatsBacktickInfoAsProse(t *testing.T) {
	t.Parallel()
	reference := testSourceReference(t, domain.ClosureReferenceRefs)
	prose := "## Why\n\n``` a`b\n\n## What\n\nFix."
	want := "## Why\n\n``` a`b\n\n" + reference + "\n\n## What\n\nFix."
	if got := placeSourceReference(reference, prose); got != want {
		t.Errorf("placement =\n%s\nwant\n%s", got, want)
	}
}

func TestPlaceSourceReferenceWithoutReferenceOrProse(t *testing.T) {
	t.Parallel()
	reference := testSourceReference(t, domain.ClosureReferenceCloses)
	if got := placeSourceReference("", "## Why\n\nReason."); got != "## Why\n\nReason." {
		t.Errorf("no reference changed the prose: %q", got)
	}
	if got := placeSourceReference(reference, ""); got != reference {
		t.Errorf("no prose = %q, want the reference alone", got)
	}
	if strings.Contains(placeSourceReference("", ""), "\n") {
		t.Error("empty inputs produced separators")
	}
}

// TestDesiredPRContentFitsTheWhyPlacement composes the placement's worst
// case: a full-size reference closing a full-size Why section that has no
// blank line before the next heading, so placement adds a byte over heading
// the body, beside every bounded publisher section. Joining the reference and
// the prose also removes a separator, so the reserve still covers it.
func TestDesiredPRContentFitsTheWhyPlacement(t *testing.T) {
	t.Parallel()
	head := strings.Repeat("b", 40)
	history := oversizedDispositionHistory(t, head)
	identity, err := DeriveIdentity(IdentityInput{
		Repo: "freeside-ai/repo", BaseRef: "main", SourceHeadSHA: head,
		ArtifactDigests: []domain.Digest{"sha256:" + domain.Digest(strings.Repeat("c", 64))},
	})
	if err != nil {
		t.Fatal(err)
	}
	advisories := make([]domain.CandidateFinding, maxRenderedAdvisories+5)
	for i := range advisories {
		advisory := advisoryFinding("")
		advisory.Detail = strings.Repeat("&", 4096)
		advisories[i] = advisory
	}
	frame := len(sourceReferenceOpenMarker + "\n\nSource issue: \n\n" + sourceReferenceCloseMarker)
	closure := closureResolution{
		outcome:   domain.ClosureOutcome{Reference: domain.ClosureReferenceDescriptiveLink},
		sourceURL: "https://github.com/o/r/issues/" + strings.Repeat("9", maxRenderedSourceReferenceBytes-frame-len("https://github.com/o/r/issues/")),
	}
	tail := "\n## What"
	prose := "## Why\n" + strings.Repeat("x", maxCandidateBodyBytes-len("## Why\n")-len(tail)) + tail
	if err := ValidateCandidateBody(prose); err != nil {
		t.Fatalf("exact candidate body budget: %v", err)
	}
	_, body, err := desiredPRContent(identity, Candidate{
		Title: "Sized Why", Body: prose, RunID: "run-aggregate-findings", HeadSHA: head,
		DispositionHistory: &history, Advisories: advisories,
	}, closure)
	if err != nil {
		t.Fatalf("compose the Why placement at full size: %v", err)
	}
	if len(body) > maxPullRequestBodyBytes {
		t.Fatalf("composed body bytes = %d, want at most %d", len(body), maxPullRequestBodyBytes)
	}
	reference := strings.Index(body, sourceReferenceOpenMarker)
	if reference < 0 || !strings.HasPrefix(body, "## Why\n") || reference > strings.Index(body, "\n## What") {
		t.Fatal("full-size reference did not close the Why section")
	}
}
