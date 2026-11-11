package publicationtext

import (
	"regexp"
	"strings"
)

// The fenced-code rules here decide which lines GitHub renders as code. The
// authored-prose renderer uses them to keep a fence the author left open from
// swallowing the publisher's sections, and the publisher uses them to keep its
// source-issue reference out of a fence, where a close keyword closes nothing.
// Both callers must agree with GitHub, so they share one definition.

// codeFenceRe matches an opening or closing fenced-code delimiter: up to three
// leading spaces, then a run of at least three backticks or tildes.
var codeFenceRe = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})(.*)$")

// FenceOpener returns the delimiter run when line opens a fenced code block
// under GFM, and "" otherwise. A backtick fence's info string may not contain
// a backtick: GFM forbids it so inline code is not mistaken for a fence opener,
// and trusting such a line as a fence would treat the lines after it as code
// while GitHub renders them live. A tilde fence has no such restriction.
func FenceOpener(line string) string {
	m := codeFenceRe.FindStringSubmatch(line)
	if m == nil || (m[1][0] == '`' && strings.ContainsRune(m[2], '`')) {
		return ""
	}
	return m[1]
}

// ClosesFence reports whether line closes a fence opened by opener: the same
// fence character, at least as long as the opener, and nothing but spaces and
// tabs after it. Unicode whitespace such as a no-break space does not close a
// fence, so strings.TrimSpace (which trims it) must not decide closure: a fence
// wrongly treated as closed lets GitHub draw what follows into the code block.
func ClosesFence(line, opener string) bool {
	m := codeFenceRe.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	run, rest := m[1], m[2]
	return run[0] == opener[0] && len(run) >= len(opener) && strings.Trim(rest, " \t") == ""
}

// ClosingFence returns a bare closing delimiter matching an opener's fence
// character and length.
func ClosingFence(opener string) string {
	return strings.Repeat(string(opener[0]), len(opener))
}
