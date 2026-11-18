package publish

import (
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// hostileClaims holds one claim per inline construct GitHub still parses
// inside a raw-HTML <code> element (#3054).
var hostileClaims = []string{
	"see [docs](https://evil.example) **now**",
	"![pixel](https://evil.example/p.png)",
	"*em* _em_ __strong__ ~strike~ ~~strike~~",
	"`nested` and ``double``",
	"$`x^2`$ and $$y$$",
	"note[^1] and [ref]\n\n[ref]: https://evil.example",
	`\*not emphasis\* and C:\path\file`,
	"<https://evil.example> https://evil.example ftp://evil.example",
	"www.evil.example WWW.evil.example Www.evil.example",
	"mailto:a@evil.example xmpp:a@evil.example a.b@evil.example",
	"a\r\nb <!-- freeside:verification --> & \"quoted\" 'single'",
	"@mention #123 :emoji: | pipe",
	"invalid \xff utf-8 and 日本語",
}

// rawSyntax matches a character that must never reach the page raw inside
// a claim, or a www. GitHub would autolink.
var rawSyntax = regexp.MustCompile("[\\\\`*_~\\[\\]$:<>\"'\n\r]|(?i)www\\.")

func TestInertClaimRendersMarkdownLiterally(t *testing.T) {
	t.Parallel()
	for _, claim := range hostileClaims {
		rendered := inertClaim(claim, 1<<20)
		inner, opened := strings.CutPrefix(rendered, "<code>")
		inner, closed := strings.CutSuffix(inner, "</code>")
		if !opened || !closed {
			t.Fatalf("%q: not one code span: %s", claim, rendered)
		}
		if strings.Contains(inner, "<code>") || strings.Contains(inner, "</code>") {
			t.Fatalf("%q: claim opens or closes a span: %s", claim, rendered)
		}
		// Every @ follows the <wbr> that splits an email autolink.
		bare := strings.ReplaceAll(inner, "<wbr>@", "")
		if strings.Contains(bare, "@") {
			t.Errorf("%q: @ without <wbr>: %s", claim, rendered)
		}
		if match := rawSyntax.FindString(bare); match != "" {
			t.Errorf("%q: raw %q in %s", claim, match, rendered)
		}
		shown := html.UnescapeString(strings.ReplaceAll(inner, "<wbr>", ""))
		want := strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(claim)
		// html.UnescapeString leaves an invalid byte as it is, so the
		// comparison is exact for every claim.
		if shown != want {
			t.Errorf("%q: shows %q", claim, shown)
		}
	}
}

// TestInertClaimTruncatesOnWholeUnits checks every limit across a
// punctuated claim: the output ends on one whole character's rendering,
// then the close and note, and stays within the limit.
func TestInertClaimTruncatesOnWholeUnits(t *testing.T) {
	t.Parallel()
	claim := "a_b\\c\n@d<e>&f www.g 日 [h](i)"
	units := inertClaimUnits(claim)
	full := "<code>" + strings.Join(units, "") + "</code>"
	const note = "</code> (truncated)"
	for limit := len("<code>") + len(note); limit <= len(full)+1; limit++ {
		got := inertClaim(claim, limit)
		if len(got) > limit {
			t.Fatalf("limit %d: %d bytes", limit, len(got))
		}
		if got == full {
			continue
		}
		kept, ok := strings.CutSuffix(strings.TrimPrefix(got, "<code>"), note)
		if !ok {
			t.Fatalf("limit %d: no truncation note: %s", limit, got)
		}
		prefix := ""
		for _, unit := range units {
			if prefix == kept || len(prefix)+len(unit) > len(kept) {
				break
			}
			prefix += unit
		}
		if prefix != kept {
			t.Fatalf("limit %d: kept %q is not whole units", limit, kept)
		}
	}
	if got := boundedInertClaim(strings.Repeat("_", 100), 200); !strings.Contains(got, "</code> (truncated; content digest <code>sha256:") || len(got) > 200 {
		t.Fatalf("bounded claim lost its digest note or limit: %s", got)
	}
}

// TestVerificationRendersFindingMarkdownInert shows a reviewer's Markdown
// as text in the most prominent section: no link or emphasis syntax
// survives in the declined finding's line.
func TestVerificationRendersFindingMarkdownInert(t *testing.T) {
	t.Parallel()
	history := dispositionHistoryFixture(t)
	var declined domain.FindingID
	for _, disposition := range history.dispositions {
		if disposition.Disposition == domain.ReviewDispositionDeclined {
			declined = disposition.FindingID
		}
	}
	for i := range history.findings {
		if history.findings[i].ID == declined {
			history.findings[i].Message = "see [docs](https://evil.example) **now**"
		}
	}
	section, err := renderVerification(nil, &history)
	if err != nil {
		t.Fatal(err)
	}
	want := "reviewer said (claim) <code>see &#91;docs&#93;(https&#58;//evil.example) &#42;&#42;now&#42;&#42;</code>"
	if !strings.Contains(section, want) || strings.Contains(section, "[docs](") || strings.Contains(section, "**now") {
		t.Fatalf("finding message is not inert:\n%s", section)
	}
}
