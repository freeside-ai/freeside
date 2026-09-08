package publicationrecord

import (
	"html"
	"regexp"
	"strings"
	"unicode"
)

const headingTagContents = `(?:[^>"']|"[^"]*"|'[^']*')*`

var (
	headingComments  = regexp.MustCompile(`(?s)<!--.*?-->`)
	headingTags      = regexp.MustCompile(`(?s)<` + headingTagContents + `>`)
	headingHTML      = regexp.MustCompile(`(?is)<h[1-6](?:[\t \r\n]+` + headingTagContents + `)?>(.*?)</h[1-6][\t \r\n]*>`)
	headingContainer = regexp.MustCompile(`^(?:>[\t ]*|[-+*][\t ]+|[0-9]{1,9}[.)][\t ]+)`)
	headingATX       = regexp.MustCompile(`^#{1,6}[\t ]+(.+)$`)
	headingClosing   = regexp.MustCompile(`[\t ]+#+[\t ]*$`)
	headingUnderline = regexp.MustCompile(`^(?:=+|-+)[\t ]*$`)
)

// ContainsVerificationHeading reserves the visible Verification title for both
// publication validators. It conservatively normalizes heading candidates,
// including ambiguous inline markup, rather than implementing a Markdown
// renderer. Distinct visible titles remain allowed; source bytes never change.
// Callers bound the body size before calling this function.
func ContainsVerificationHeading(body string) bool {
	body = headingComments.ReplaceAllString(body, "")
	for _, match := range headingHTML.FindAllStringSubmatch(body, -1) {
		if verificationTitle(match[1]) {
			return true
		}
	}
	previous := ""
	for line := range strings.SplitSeq(body, "\n") {
		line = strings.TrimSpace(line)
		for {
			prefix := headingContainer.FindStringIndex(line)
			if prefix == nil {
				break
			}
			line = strings.TrimSpace(line[prefix[1]:])
		}
		if match := headingATX.FindStringSubmatch(line); match != nil {
			title := headingClosing.ReplaceAllString(match[1], "")
			if verificationTitle(title) {
				return true
			}
		}
		if headingUnderline.MatchString(line) && verificationTitle(previous) {
			return true
		}
		previous = line
	}
	return false
}

func verificationTitle(title string) bool {
	title = headingTags.ReplaceAllString(title, "")
	var visible strings.Builder
	for i := 0; i < len(title); i++ {
		// Inline links and reference links keep their labels, not destinations.
		// Balanced destinations include URLs containing parentheses.
		if title[i] == ']' && i+1 < len(title) && (title[i+1] == '(' || title[i+1] == '[') {
			open := title[i+1]
			close := byte(')')
			if open == '[' {
				close = ']'
			}
			depth := 1
			var quote byte
			end := i + 2
			for ; end < len(title) && depth > 0; end++ {
				if title[end] == '\\' && end+1 < len(title) {
					end++
					continue
				}
				if quote != 0 {
					if title[end] == quote {
						quote = 0
					}
					continue
				}
				if open == '(' && (title[end] == '"' || title[end] == '\'') &&
					strings.ContainsRune(" \t\r\n", rune(title[end-1])) {
					quote = title[end]
					continue
				}
				switch title[end] {
				case open:
					depth++
				case close:
					depth--
				}
			}
			if depth == 0 {
				i = end - 1
				continue
			}
		}
		switch title[i] {
		case '*', '_', '~', '`', '[', ']', '\\':
			// Treat formatting and escapes conservatively even when unbalanced.
		default:
			visible.WriteByte(title[i])
		}
	}
	// Entities are visible text, not delimiters in HTML attributes or link titles.
	decoded := html.UnescapeString(visible.String())
	// Invisible format characters and variation selectors cannot distinguish an
	// operator heading from the reserved title. Visible characters remain exact.
	decoded = strings.Map(func(r rune) rune {
		if unicode.In(r, unicode.Cf, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector) {
			return -1
		}
		return r
	}, decoded)
	return strings.EqualFold(strings.TrimSpace(decoded), "Verification")
}
