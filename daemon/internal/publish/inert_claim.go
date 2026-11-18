package publish

import (
	"html"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// The body's sections (Verification, Advisories, Scope Decision) render a
// recorded claim through inertClaimUnits, which is stricter than
// dispositionCode (#3054). GitHub parses Markdown inside a raw-HTML <code>
// element, so escaping only HTML leaves links, images, emphasis, math,
// footnotes, and autolinks live in a claim. The disposition history keeps
// dispositionCode, because the publication intent binds its bytes.
//
// The encoded set comes from GitHub's renderer (POST /markdown, mode gfm,
// 2026-11-18), not from a specification; see
// devlog/2026-11-18-1040-inert-markdown-claims.md for why each member is
// there.

// inertClaimUnits renders value one character at a time, so a truncation can
// cut between two characters' renderings and never inside an escape.
// Joined, the units show the original characters, with a newline and a
// carriage return shown as \n and \r. Bytes that are not valid UTF-8 pass
// through one at a time, as dispositionCode passes them.
func inertClaimUnits(value string) []string {
	units := make([]string, 0, len(value))
	for i := 0; i < len(value); {
		r, size := utf8.DecodeRuneInString(value[i:])
		afterWWW := i >= 3 && strings.EqualFold(value[i-3:i], "www")
		units = append(units, inertUnit(value[i:i+size], r, afterWWW))
		i += size
	}
	return units
}

func inertUnit(char string, r rune, afterWWW bool) string {
	switch r {
	case '\n':
		return "&#92;n"
	case '\r':
		return "&#92;r"
	// Markdown punctuation GitHub still parses inside <code>: escapes,
	// nested code, emphasis, strikethrough, links, footnotes, math, and the
	// scheme separator of a bare URL autolink. A numeric reference shows the
	// character without letting it take part in any of them.
	case '\\', '`', '*', '_', '~', '[', ']', '$', ':':
		return "&#" + strconv.Itoa(int(r)) + ";"
	// GitHub links an email address after it decodes references, so no
	// entity stops it. A raw-HTML split does, and GitHub's sanitizer drops
	// the <wbr> from the rendered page.
	case '@':
		return "<wbr>@"
	case '.':
		// GitHub matches a www. autolink on the raw text; encoding only that
		// dot keeps paths and sentences readable in the raw body.
		if afterWWW {
			return "&#46;"
		}
	}
	return html.EscapeString(char)
}

// inertClaim renders value as an inert code span of at most limit bytes,
// closing a truncated span before " (truncated)". The Verification section
// uses it: its reader has no use for a digest, and the full value stays in
// the store.
func inertClaim(value string, limit int) string {
	return truncatedInertClaim(value, limit, func() string { return " (truncated)" })
}

// boundedInertClaim is inertClaim that identifies omitted bytes by the
// value's content address, for the Advisories and Scope Decision sections.
// The digest is a fixed hex form, so dispositionCode renders it.
func boundedInertClaim(value string, limit int) string {
	return truncatedInertClaim(value, limit, func() string {
		return " (truncated; content digest " + dispositionCode(contentaddr.Sum([]byte(value))) + ")"
	})
}

// truncatedInertClaim keeps whole units only, so a cut never splits an
// escape: it appends units while the span, its close, and note fit limit.
func truncatedInertClaim(value string, limit int, note func() string) string {
	units := inertClaimUnits(value)
	rendered := "<code>" + strings.Join(units, "") + "</code>"
	if len(rendered) <= limit {
		return rendered
	}

	suffix := "</code>" + note()
	budget := limit - len("<code>") - len(suffix)
	var out strings.Builder
	out.WriteString("<code>")
	for _, unit := range units {
		if len(unit) > budget {
			break
		}
		budget -= len(unit)
		out.WriteString(unit)
	}
	return out.String() + suffix
}
