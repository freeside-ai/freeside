package publicationtext

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ScreenIssueTitle screens a one-line issue title under a named issue-text
// ruleset. Like every screen here, it is a pure function of the text and the
// ruleset version, and a refusal names the failing check, never the text.
func ScreenIssueTitle(ruleset domain.IssueTextRuleset, title string, maxBytes int) error {
	if strings.ContainsAny(title, "\r\n") {
		return errors.New("issue title: not_one_line")
	}
	return screenIssueText("title", ruleset, title, maxBytes)
}

// ScreenIssueBody screens an issue body under a named issue-text ruleset.
func ScreenIssueBody(ruleset domain.IssueTextRuleset, body string, maxBytes int) error {
	return screenIssueText("body", ruleset, body, maxBytes)
}

// screenIssueText dispatches on the ruleset. The switch dispatches behaviour
// and so omits default: a later ruleset version must choose its rules here,
// and an identifier outside the registry falls through to the refusal.
func screenIssueText(field string, ruleset domain.IssueTextRuleset, text string, maxBytes int) error {
	switch ruleset {
	case domain.IssueTextRulesetGitHubIssue1:
		if err := screenStages(text, maxBytes, true, screenGitHubIssue1); err != nil {
			return errors.New("issue " + field + ": " + err.Error())
		}
		return nil
	}
	return errors.New("issue " + field + ": unknown_ruleset")
}

// screenGitHubIssue1 holds the rules github-issue/1 adds to the publication
// prose screen (plan §5.11, §5.17). A webhook bot reads the raw issue body,
// where inert rendering protects nothing, so the screen refuses every @name
// token and every line that opens with a / command. Both rules
// over-approximate: an email address or an absolute path at the start of a
// line is refused too, because a raw reader may not tell them apart.
//
// U+2028 and U+2029 are refused outright. They are line terminators to some
// raw readers (a JavaScript multiline pattern among them) and to none of the
// line splitting here, so a command after one would otherwise sit mid-line
// for this screen and at a line start for the bot.
func screenGitHubIssue1(text string) error {
	if strings.ContainsAny(text, "\u2028\u2029") {
		return errors.New("line_separator")
	}
	if containsMentionToken(text) {
		return errors.New("mention")
	}
	for line := range strings.Lines(text) {
		if opensWithSlashCommand(line) {
			return errors.New("slash_command")
		}
	}
	return nil
}

// containsMentionToken reports whether an @ is directly followed by a
// character that can start a user, team, or bot name.
func containsMentionToken(text string) bool {
	for rest := text; ; {
		at := strings.IndexByte(rest, '@')
		if at < 0 {
			return false
		}
		rest = rest[at+1:]
		// An empty rest decodes to RuneError, which is none of these.
		r, _ := utf8.DecodeRuneInString(rest)
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '-' {
			return true
		}
	}
}

// opensWithSlashCommand reports whether a line's first visible characters
// are a / followed by a letter.
func opensWithSlashCommand(line string) bool {
	command, found := strings.CutPrefix(strings.TrimLeftFunc(line, unicode.IsSpace), "/")
	if !found {
		return false
	}
	r, _ := utf8.DecodeRuneInString(command)
	return unicode.IsLetter(r)
}
