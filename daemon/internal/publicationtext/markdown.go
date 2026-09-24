package publicationtext

import (
	"regexp"
	"strings"
)

// markdownBackslashEscapeRe matches a CommonMark backslash escape: a backslash
// before an ASCII punctuation character. GitHub renders it as the punctuation
// alone, so collapseMarkdownDelimiters resolves it to model the visible text.
var markdownBackslashEscapeRe = regexp.MustCompile(`\\([[:punct:]])`)

// markdownEmphasisStripper removes the * emphasis delimiter GitHub drops from
// its visible output. It deliberately does NOT strip _: an underscore is a valid
// secret character (ghp_...), and GitHub only treats _ as emphasis at word
// boundaries, never intraword, so it can never splice a contiguous alphanumeric
// token. Stripping _ would instead delete the literal underscore from a real
// token prefix and make the screen weaker. Removing every *, matched or not, is
// a safe over-approximation: it is not a secret character, so stripping it can
// only join fragments (stricter), never destroy a token. Code-span backticks are
// handled by collapseCodeSpans, not here, because modeling GitHub's code-span
// whitespace normalization needs the span boundaries a blind strip would lose.
var markdownEmphasisStripper = strings.NewReplacer("*", "")

// collapseMarkdownDelimiters approximates GitHub's visible text by applying the
// same character-dropping transforms renderAuthoredPublication applies: it
// reduces links and images to their visible text (the renderer's
// StripMarkdownLinks), resolves backslash escapes, normalizes code spans to
// their rendered content (collapseCodeSpans), and strips the * emphasis
// delimiter. Each is a visible-text reduction GitHub performs, so a token split
// only by one of them passes the raw source scan and then reappears intact once
// GitHub renders the prose: `[zz](url)`/`![zz](url)`/`[zz][ref]` collapse to
// their text, a padded code span “ ` zz ` “ renders as `zz`, and so on.
// Modeling every reduction the renderer performs, not a hand-picked subset,
// keeps the screen's collapsed form a superset of the renderer's visible output.
// It is used only by the recipe v2 field screen, one step of a fixpoint that
// also HTML-unescapes, so a delimiter or bracket revealed by decoding an entity
// (for example `&#42;` -> `*`) is folded out on the next pass.
func collapseMarkdownDelimiters(s string) string {
	s = StripMarkdownLinks(s)
	s = markdownBackslashEscapeRe.ReplaceAllString(s, "$1")
	s = collapseCodeSpans(s)
	return markdownEmphasisStripper.Replace(s)
}

// collapseCodeSpans replaces each inline code span with the visible text GitHub
// renders for it, dropping the backtick delimiters. It walks the string as an
// alternation of code spans and other text, reusing FindBacktickRun for the
// CommonMark closing rule, so it knows each span's boundaries and can apply the
// two whitespace normalizations GitHub performs on code-span content: interior
// line endings become single spaces, and one leading and one trailing space are
// removed when the content both begins and ends with a space and is not all
// spaces. Modeling this closes the reconstruction where a secret split by a
// padded span such as “ ` zz ` “ (or a newline-padded span) passes the raw
// scan yet renders as an intact token. An unterminated backtick run is literal
// text GitHub shows verbatim, so it is emitted unchanged rather than stripped.
func collapseCodeSpans(s string) string {
	var b strings.Builder
	rest := s
	for len(rest) > 0 {
		open := strings.IndexByte(rest, '`')
		if open < 0 {
			b.WriteString(rest)
			break
		}
		b.WriteString(rest[:open])
		runLen := 1
		for open+runLen < len(rest) && rest[open+runLen] == '`' {
			runLen++
		}
		delim := rest[open : open+runLen]
		after := rest[open+runLen:]
		closeAt := FindBacktickRun(after, runLen)
		if closeAt < 0 {
			// No matching closing run: the backticks are literal text GitHub shows,
			// so they neither open a span nor splice a token. Emit them unchanged.
			b.WriteString(delim)
			rest = after
			continue
		}
		b.WriteString(normalizeCodeSpanContent(after[:closeAt]))
		rest = after[closeAt+runLen:]
	}
	return b.String()
}

// normalizeCodeSpanContent returns the visible text GitHub renders for a code
// span's raw content: interior line endings collapse to single spaces, then one
// leading and one trailing space are stripped when the content begins and ends
// with a space but is not entirely spaces (the CommonMark code-span padding
// rule). The space characters are U+0020 only, matching CommonMark.
func normalizeCodeSpanContent(c string) string {
	c = strings.ReplaceAll(c, "\r\n", " ")
	c = strings.NewReplacer("\r", " ", "\n", " ").Replace(c)
	if len(c) >= 2 && c[0] == ' ' && c[len(c)-1] == ' ' && strings.Trim(c, " ") != "" {
		c = c[1 : len(c)-1]
	}
	return c
}

// inlineLinkRe and refLinkRe strip a Markdown link or image to its visible text
// or alt text, dropping the target entirely (contract, settled item 8: no active
// link or image). The URL alternative allows one level of balanced parens, which
// CommonMark permits. Any link these miss is not left active: its URL is caught
// by autolinkTokenRe and code-wrapped, and its brackets are backslash-escaped by
// escapeInertText, so the worst case is inert text, never a live link.
var (
	inlineLinkRe = regexp.MustCompile(`!?\[([^\[\]]*)\]\((?:[^()]|\([^()]*\))*\)`)
	refLinkRe    = regexp.MustCompile(`!?\[([^\[\]]*)\]\[[^\[\]]*\]`)
)

// FindBacktickRun returns the start index in s of the first run of exactly n
// backticks (bounded by a non-backtick on each side), or -1. This is the
// CommonMark rule for closing a code span opened with n backticks.
func FindBacktickRun(s string, n int) int {
	i := 0
	for i < len(s) {
		if s[i] != '`' {
			i++
			continue
		}
		start := i
		for i < len(s) && s[i] == '`' {
			i++
		}
		if i-start == n {
			return start
		}
	}
	return -1
}

// StripMarkdownLinks reduces inline and reference links and images to their
// visible text, dropping every target. It repeats until stable so nested or
// adjacent constructs are fully reduced, bounded to avoid pathological input.
func StripMarkdownLinks(s string) string {
	for range 8 {
		next := inlineLinkRe.ReplaceAllString(s, "$1")
		next = refLinkRe.ReplaceAllString(next, "$1")
		if next == s {
			break
		}
		s = next
	}
	return s
}
