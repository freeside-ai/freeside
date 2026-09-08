package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestCandidateBodyValidatorsAgreeAtBudgetBoundary(t *testing.T) {
	t.Parallel()
	low, high := 0, 64<<10
	for low < high {
		mid := (low + high + 1) / 2
		if err := publish.ValidateCandidateBody(strings.Repeat("x", mid)); err == nil {
			low = mid
		} else {
			high = mid - 1
		}
	}

	for _, size := range []int{low - 1, low, low + 1} {
		body := strings.Repeat("x", size)
		publishErr := publish.ValidateCandidateBody(body)
		fakeErr := fakepublication.ValidateCandidateBody(body)
		if fmt.Sprint(publishErr) != fmt.Sprint(fakeErr) {
			t.Errorf("%d-byte body: publish error %q, fake error %q", size, publishErr, fakeErr)
		}
	}
}

func TestCandidateBodyValidatorsReserveVerificationHeadings(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"## Verification", "### verification", "# VERIFICATION #\r\n", "Verification\n---\n", "Verification\r\n===\r\n",
		"<!-- freeside:verification -->", "<!-- /freeside:verification -->",
		"## Verification <!-- CI -->", "## Verification<!-- one --><!-- two --> ##\r\n",
		"## Verification <!-- multiple\nlines -->", "Ver<!-- split\ncomment -->ification\n---",
		"## **Verification**", "## _Ver_ifi**cation**", "## `Verification`", "## ~~Verification~~",
		"## [Verification](https://example.test/a_(b))", "## [Verification][checks]", "## [Verification][]",
		"## **[Ver](https://example.test)ification**", "## <strong>Ver</strong><em>ification</em>",
		"## &#86;erification", "## Verific&#97;tion&nbsp;", "## &#x56;erification",
		"**Verification**\r\n===\r\n", "[Verification](https://example.test)\n---",
		"<h2>Verification</h2>", "<h3 class=\"heading\">**Verification**</h3>",
		"<h2>\nVerification\n</h2>", "<h2\nclass=\"heading\">Verification</h2>",
		"<h2 title=\">\">Verification</h2>", "## <span title=\">\">Verification</span>",
		"## <span title='>'>Verification</span>", "<h2><span>\nVerification\n</span></h2>",
		"## [Verification](https://example.test \"tooltip ) suffix\")",
		"## [Verification](https://example.test 'tooltip ) suffix')",
		"## [Verification](https://example.test \"tool&quot;tip ) suffix\")",
		"## <span title=\"&quot; >\">Verification</span>",
		"## [Verification](https://example.test (tooltip suffix))", "## [Verification](https://example.test/it's)",
		"> ## Verification", "- ## Verification", "1. ## Verification", "> - ### **Verification**",
		"> **Verification**\n> ---", "## Why\nText\n## **Verification**\nPassed: not executed",
		"## Verification\n\n## *Verification*", "## Verification\t<!-- CI --> ##\n",
	} {
		if publish.ValidateCandidateBody(body) == nil || fakepublication.ValidateCandidateBody(body) == nil {
			t.Errorf("validator accepted publisher-owned heading or marker: %q", body)
		}
	}
	for _, body := range []string{
		"## Verification details", "## Verifying the fix", "## **Verification** details",
		"## [Verification details](https://example.test)", "Verification in prose.",
		"## Pre-Verification", "## Verification pending", "####### Verification",
		"## Why\nKeep every byte.\r\n\n", "## Verification <!-- hidden --> details",
		"**Verification details**\n---", "<h2>Verification details</h2>",
	} {
		if publish.ValidateCandidateBody(body) != nil || fakepublication.ValidateCandidateBody(body) != nil {
			t.Errorf("validator rejected distinct prose or heading: %q", body)
		}
	}
}

func TestCandidateBodyValidatorsReserveInvisibleVerificationHeadings(t *testing.T) {
	t.Parallel()
	for _, invisible := range []rune{
		'\u200b', '\u200c', '\u200d', '\u2060', '\ufeff', '\u00ad',
		'\u034f', '\ufe0f', '\u180b', '\u061c', '\u2066', '\u2069', '\U000e0100',
	} {
		for _, spelling := range []string{string(invisible), fmt.Sprintf("&#x%X;", invisible)} {
			for _, title := range []string{"Verification" + spelling, "Verifi" + spelling + "cation"} {
				for _, body := range []string{"## " + title, "> ### **" + title + "**", title + "\r\n---\r\n"} {
					if publish.ValidateCandidateBody(body) == nil || fakepublication.ValidateCandidateBody(body) == nil {
						t.Errorf("validator accepted invisible reserved title: %q", body)
					}
				}
			}
		}
	}
	for _, body := range []string{
		"## Verification details\u200b", "## Verifying the fix\ufe0f",
		"## Verification\u0301", "## Verifi cation", "Verification\u200b in prose.",
	} {
		if publish.ValidateCandidateBody(body) != nil || fakepublication.ValidateCandidateBody(body) != nil {
			t.Errorf("validator rejected a distinct visible title or prose: %q", body)
		}
	}
}
