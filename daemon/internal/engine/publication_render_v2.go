package engine

import (
	"html"
	"regexp"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
)

// clientPublicationRecipeV2 is the recipe that renders the publication-author
// role's prose as live, screened GitHub-Flavored Markdown, followed by the
// evidence references and a producer and digest footer. Records freeze their
// recipe, so a stored v2 record renders through renderAuthoredPublication while
// a v1 record keeps the frozen v1 rendering (plan §5.15, §5.12).
const clientPublicationRecipeV2 = "freeside.client-publication/v2"

// clientPublicationRecipeV3 renders the same screened prose as v2 with nothing
// after it: no evidence list, producer line, or artifact digest (plan §5.15,
// revision 115). The digest stays on the stored authoring artifact, and the
// publisher's Verification section labels the prose as its author's account.
// New client submissions freeze it (TaskSubmitter.SubmitTask).
const clientPublicationRecipeV3 = "freeside.client-publication/v3"

// IntakePublicationRecipeV1 is label intake's first publication-author
// contract (plan §5.15): the run freezes it beside the literal title and body
// the intake path writes, runs the same author role as client v2, renders like
// client v2, and renders the literal text as its fallback. A label record with
// no recipe keeps its literal text and replay bytes.
const IntakePublicationRecipeV1 = "freeside.intake-publication/v1"

// IntakePublicationRecipeV2 is IntakePublicationRecipeV1 with client v3's
// rendering. A new label-initiated run freezes it (daemoncompose's
// intakePublication); a run frozen under v1 keeps v1.
const IntakePublicationRecipeV2 = "freeside.intake-publication/v2"

// authoredPublicationRecipe reports whether a frozen recipe runs the
// publication-author role, the source-issue closure, and the publisher-owned
// source-reference section. v1 and literal records do none of these.
func authoredPublicationRecipe(recipe string) bool {
	return recipe == clientPublicationRecipeV2 || recipe == clientPublicationRecipeV3 ||
		intakePublicationRecipe(recipe)
}

// intakePublicationRecipe reports whether a frozen recipe is label intake's,
// which carries literal title and body as its fallback and so takes the literal
// checks and the literal fallback rendering.
func intakePublicationRecipe(recipe string) bool {
	return recipe == IntakePublicationRecipeV1 || recipe == IntakePublicationRecipeV2
}

// bareAuthoredProse reports whether a recipe renders the authored prose with
// nothing after it (client v3, intake v2). The older authored recipes keep
// their evidence list and producer footer byte for byte.
func bareAuthoredProse(recipe string) bool {
	return recipe == clientPublicationRecipeV3 || recipe == IntakePublicationRecipeV2
}

// screenAuthoredText refuses an authored artifact whose free-text fields would
// be unsafe as public output. It runs the identical v1 content checks (the
// commit-message screen, the secret scan, the reserved-publisher-section check,
// the freeside: ban, and the repeated HTML-unescape pass) on every text field,
// each under its own domain byte bound, and additionally folds out the Markdown
// delimiters GitHub collapses in its visible output (publicationtext.ScreenField),
// so v2 is at least as strict as v1 even though the renderer keeps emphasis and
// code live. A refused artifact is never rendered; the caller falls back to v1.
// The producer label is not screened here: renderAuthoredPublication renders it
// inert regardless of its content.
func screenAuthoredText(a domain.PublicationAuthoring) error {
	fields := []struct {
		name string
		text string
		max  int
	}{
		{"title", a.Title, domain.MaxPublicationAuthoringTitleBytes},
		{"body", a.Body, domain.MaxPublicationAuthoringBodyBytes},
		{"outcome_summary", a.OutcomeSummary, domain.MaxPublicationAuthoringOutcomeSummaryBytes},
	}
	if a.ReviewerNotes != nil {
		fields = append(fields, struct {
			name string
			text string
			max  int
		}{"reviewer_notes", *a.ReviewerNotes, domain.MaxPublicationAuthoringReviewerNotesBytes})
	}
	for _, f := range fields {
		if err := publicationtext.ScreenField(f.name, f.text, f.max); err != nil {
			return err
		}
	}
	return nil
}

// autolinkTokenRe matches every token GitHub would turn into an active link or
// notification inside a repository-context body: an @-mention (user or
// org/team), a GH-123 reference, an owner/repo#123 cross-reference, a bare #123
// issue reference, an http(s) URL, a www. host, and a 7-to-40-character hex
// commit token. renderAuthoredPublication wraps each match in an inline code
// span, because GitHub performs none of these substitutions inside code. The
// owner/repo# alternative precedes the bare #123 and hex alternatives so the
// leftmost, Perl-preference match consumes the whole cross-reference rather than
// only its tail. Over-matching (an all-hex ordinary word, an @ inside an email)
// only renders more text inert, which is the safe direction.
var autolinkTokenRe = regexp.MustCompile(
	`@[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})(?:/[A-Za-z0-9._-]+)?` + // @user or @org/team
		`|\bGH-[0-9]+` + // GH-123
		`|[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9._-]+#[0-9]+` + // owner/repo#123
		`|#[0-9]+` + // #123
		`|https?://[^\s<]+` + // bare URL
		`|\bwww\.[^\s<]+` + // www. host
		`|\b[0-9a-fA-F]{7,40}\b`, // commit-length hex token
)

// atxHeadingRe matches an ATX heading: up to three leading spaces, one to six
// #, then either end of line or a space and the heading text. A trailing run of
// # is a closing sequence, not content.
var atxHeadingRe = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?[ \t]*$`)

// listItemRe matches a bullet or ordered list marker and its content, preserving
// the leading indentation so nested lists survive.
var listItemRe = regexp.MustCompile(`^( {0,3})([-+*]|[0-9]{1,9}[.)])([ \t]+)(.*)$`)

// isThematicBreak reports whether line is a thematic break (horizontal rule):
// three or more of the same -, _, or * with only spaces and tabs between and
// around them. RE2 has no backreference for "the same character repeated", so
// this is checked in code. A thematic break is not on the allowlist, and its -
// and * spellings double as setext underlines and list markers, so it cannot be
// escaped inline; renderAuthoredBodyProse neutralizes the whole line instead.
// Extra leading indentation is treated leniently: over-matching only renders
// more inert, which is the safe direction.
func isThematicBreak(line string) bool {
	var marker byte
	count := 0
	for i := 0; i < len(line); i++ {
		switch c := line[i]; c {
		case ' ', '\t':
		case '-', '_', '*':
			if marker == 0 {
				marker = c
			} else if c != marker {
				return false
			}
			count++
		default:
			return false
		}
	}
	return count >= 3
}

// renderAuthoredPublication renders a stored, already-screened authoring
// artifact into a PR title and a PR body of live, neutralized GitHub-Flavored
// Markdown. It is a pure function: for a given artifact it always returns the
// same bytes, which is what retry, restart, and drift repair rely on. The body
// keeps the allowed constructs live (headings, paragraphs, bullet and numbered
// lists, emphasis, inline code, fenced code) while rendering everything that
// could reach outside the prose inert: raw HTML is escaped, autolink-triggering
// tokens are wrapped in code, links and images are reduced to their text, and
// any code fence the author left open is closed so the prose cannot swallow the
// publisher's own sections appended after it. Where the recipe keeps them, the
// evidence references and the producer provenance are rendered as inert,
// code-wrapped identifiers.
//
// The caller must run screenAuthoredText first and fall back to v1 on failure;
// this function assumes screened input and never itself rejects. It does not
// enforce the candidate-body byte budget: escaping can expand the body, so the
// authoritative fit check belongs to the caller that composes the final PR body.
//
// recipe is the record's frozen recipe. Under client v3 and intake v2 the body
// is the rendered prose alone; under client v2 and intake v1 it keeps the
// evidence references and the producer footer, whose bytes are frozen.
func renderAuthoredPublication(recipe string, a domain.PublicationAuthoring) (title, body string) {
	if bareAuthoredProse(recipe) {
		return a.Title, renderAuthoredBodyProse(a.Body)
	}
	var b strings.Builder
	b.WriteString(renderAuthoredBodyProse(a.Body))
	if len(a.EvidenceRefs) > 0 {
		b.WriteString("\n\nEvidence artifacts:\n")
		for _, ref := range a.EvidenceRefs {
			b.WriteString("\n- " + codeSpan(sanitizeInlineIdentifier(string(ref.ArtifactID))) +
				" (" + codeSpan(sanitizeInlineIdentifier(string(ref.Digest))) + ")")
		}
	}
	b.WriteString("\n\n---\n\nProduced by the " + codeSpan(sanitizeInlineIdentifier(a.Producer.Producer)) +
		" producer via the " + codeSpan(sanitizeInlineIdentifier(a.Producer.Site)) +
		" site. Artifact digest: " + codeSpan(sanitizeInlineIdentifier(string(a.Digest))) + ".")
	return a.Title, b.String()
}

// renderAuthoredBodyProse renders the author's Markdown body as neutralized live
// Markdown, block by block. A fenced code block's lines are emitted without
// neutralization: GitHub performs no autolinking, HTML, or emphasis inside
// code, so its content is already inert. A fence left open at end of input is
// closed with the same delimiter so nothing appended after the prose is drawn
// into the code block.
//
// That holds only if GitHub ends each fence on the line where this renderer
// does, so every fence is emitted at column 0. The opener's indentation (0 to
// 3 spaces) is stripped, up to as many leading spaces are stripped from each
// line inside the fence, as GFM does with an indented fence's content, and
// closure is decided on the emitted line. A column-0 opener is always
// top-level on GitHub: no list item continues past a column-0 fence opener, and
// prose outside fences has no blockquote, HTML block, table, or other fence,
// because escapeInertText escapes >, <, |, backticks, and tildes and the screen
// refuses tabs and carriage returns. Deciding closure on an indented opener
// in the input instead would disagree with GitHub, which ends a list item's
// fence when the list ends (#3053). The cost: a list item's code block renders
// after the item rather than nested in it.
func renderAuthoredBodyProse(bodyText string) string {
	lines := strings.Split(bodyText, "\n")
	var out []string
	var fence string // nonempty while inside a fenced code block: the exact delimiter
	var indent int   // the fence opener's indentation, stripped from its lines
	for _, line := range lines {
		if fence != "" {
			line = stripLeadingSpaces(line, indent)
			out = append(out, line)
			if publicationtext.ClosesFence(line, fence) {
				fence = ""
			}
			continue
		}
		if opener := publicationtext.FenceOpener(line); opener != "" {
			fence = opener
			trimmed := strings.TrimLeft(line, " ")
			indent = len(line) - len(trimmed)
			out = append(out, trimmed)
			continue
		}
		if m := atxHeadingRe.FindStringSubmatch(line); m != nil {
			hashes, text := m[1], m[2]
			if text == "" {
				out = append(out, hashes)
			} else {
				out = append(out, hashes+" "+neutralizeInlineText(text))
			}
			continue
		}
		if isThematicBreak(line) {
			// Checked before the list marker: a space-separated rule such as
			// "* * *" also matches a list item, but GitHub renders it as a
			// horizontal rule. Escaping the leading run makes GitHub render the
			// line as literal text rather than a rule, which also stops it acting
			// as a setext underline that would promote the previous line to a
			// heading. A real list item ("- text") has trailing content, so the
			// anchored rule pattern does not match it.
			out = append(out, `\`+line)
			continue
		}
		if m := listItemRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1]+m[2]+m[3]+neutralizeInlineText(m[4]))
			continue
		}
		out = append(out, neutralizeInlineText(line))
	}
	rendered := strings.Join(out, "\n")
	if fence != "" {
		rendered += "\n" + publicationtext.ClosingFence(fence)
	}
	return rendered
}

// stripLeadingSpaces removes up to n leading spaces from line.
func stripLeadingSpaces(line string, n int) string {
	i := 0
	for i < n && i < len(line) && line[i] == ' ' {
		i++
	}
	return line[i:]
}

// neutralizeInlineText renders one line of inline prose safe while keeping
// emphasis and inline code live. It walks the line as an alternation of code
// spans and plain text: a code span (a backtick run closed by an equal run) is
// emitted verbatim, since its content is inert; the plain text between spans is
// stripped of links, HTML-escaped, and has its autolink tokens code-wrapped by
// neutralizePlainText. An unterminated backtick run is treated as plain text.
func neutralizeInlineText(line string) string {
	var b strings.Builder
	rest := line
	for len(rest) > 0 {
		open := strings.IndexByte(rest, '`')
		if open < 0 {
			b.WriteString(neutralizePlainText(rest))
			break
		}
		b.WriteString(neutralizePlainText(rest[:open]))
		runLen := 1
		for open+runLen < len(rest) && rest[open+runLen] == '`' {
			runLen++
		}
		delim := rest[open : open+runLen]
		after := rest[open+runLen:]
		close := publicationtext.FindBacktickRun(after, runLen)
		if close < 0 {
			// No matching closing run: the backticks are literal text, escaped so
			// they cannot pair with a code span this renderer introduces later.
			b.WriteString(neutralizePlainText(delim))
			rest = after
			continue
		}
		// The span (delimiters and content) is inert; emit it unchanged.
		b.WriteString(delim + after[:close] + delim)
		rest = after[close+runLen:]
	}
	return b.String()
}

// neutralizePlainText renders a run of non-code prose inert against every
// construct that could reach outside it, while leaving emphasis intact. It
// strips links and images to their text, then walks the remainder wrapping each
// autolink-triggering token in a code span and escaping the text around it.
func neutralizePlainText(s string) string {
	s = publicationtext.StripMarkdownLinks(s)
	var b strings.Builder
	rest := s
	for len(rest) > 0 {
		loc := autolinkTokenRe.FindStringIndex(rest)
		if loc == nil {
			b.WriteString(escapeInertText(rest))
			break
		}
		b.WriteString(escapeInertText(rest[:loc[0]]))
		// The token contains no '<' (the URL/host alternatives stop at it) so it is
		// safe inside a code span, where '&' and other punctuation render literally
		// and GitHub applies no autolinking. codeSpan sizes the fence to any
		// backticks the token itself contains.
		b.WriteString(codeSpan(rest[loc[0]:loc[1]]))
		rest = rest[loc[1]:]
	}
	return b.String()
}

// escapeInertText renders plain text (no code spans, no links) inert. It escapes
// HTML so raw tags and entities show as text, then backslash-escapes the
// Markdown punctuation for constructs outside the allowlist: the backslash
// itself first, then backticks and brackets (links, images, code spans) and the
// GFM extension markers ~ (strikethrough) and | (tables). Emphasis punctuation
// (* and _) is intentionally left live; thematic breaks built from it are
// neutralized at the block level (renderAuthoredBodyProse).
func escapeInertText(s string) string {
	s = html.EscapeString(s)
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, "`", "\\`")
	s = strings.ReplaceAll(s, "[", `\[`)
	s = strings.ReplaceAll(s, "]", `\]`)
	s = strings.ReplaceAll(s, "~", `\~`)
	s = strings.ReplaceAll(s, "|", `\|`)
	return s
}

// codeSpan wraps s in an inline code span with a backtick fence longer than any
// run of backticks inside s, padding with a space when s begins or ends with a
// backtick, per the CommonMark code-span rules. The content is emitted verbatim;
// callers pass tokens that contain no '<'.
func codeSpan(s string) string {
	longest, cur := 0, 0
	for i := 0; i < len(s); i++ {
		if s[i] == '`' {
			cur++
			if cur > longest {
				longest = cur
			}
		} else {
			cur = 0
		}
	}
	fence := strings.Repeat("`", longest+1)
	pad := ""
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		pad = " "
	}
	return fence + pad + s + pad + fence
}

// sanitizeInlineIdentifier collapses all whitespace (including newlines and
// tabs) in an identifier string to single spaces and trims the ends, so the
// value stays on one line inside the code span that wraps it. It keeps the
// producer label and evidence identifiers inert even though they are not run
// through the prose screen.
func sanitizeInlineIdentifier(s string) string {
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}
