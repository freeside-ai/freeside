package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
)

const (
	fallbackCommitSubjectMaxBytes = 72
	// fallbackCommitReasonMaxBytes bounds the reason a v2 message carries:
	// a sentence or two, never the specification body.
	fallbackCommitReasonMaxBytes = 320
)

// FallbackCommitMessageInput is the immutable authority for one
// daemon-authored fallback commit message.
type FallbackCommitMessageInput struct {
	Spec       []byte
	BoundIssue *int
	RunID      domain.RunID
	SpecDigest domain.Digest
	Policy     importer.Policy
}

// FallbackCommitMessage derives the single-commit floor from the approved,
// digest-bound specification rather than workspace or forge content. It is
// total: an unusable title produces a conspicuous rewrite marker while the
// body retains the durable trace facts and failure reason.
//
// It returns the v2 recipe: v1's message with the specification's opening
// paragraph, screened and cut to two sentences, as the body's first
// paragraph. When that reason is unusable the result is byte-identical to
// v1, so the reason never pushes a message onto the rewrite floor.
//
// Both recipes also depend on importer.ScreenMessage, and v2 on
// publicationtext.Screen and autolinkTokenRe. Publication replay re-derives
// the stored message, so a change to any of them that alters a derivation
// refuses runs imported before it: such a change must freeze the current
// derivation as another accepted recipe.
func FallbackCommitMessage(in FallbackCommitMessageInput) string {
	return fallbackCommitMessage(in, true)
}

// fallbackCommitMessageV1 is the frozen pre-reason recipe. Runs imported
// before v2 stored its output, so publication replay still accepts it; it
// must never change.
func fallbackCommitMessageV1(in FallbackCommitMessageInput) string {
	return fallbackCommitMessage(in, false)
}

// FallbackCommitMessageMatches reports whether message is one of the
// daemon's own fallback derivations, v2 or the frozen v1, from in. It
// re-checks a stored message against the approved specification, issue
// binding, run ID, and specification digest, so publication replay accepts
// a run imported under either recipe and refuses any other message.
func FallbackCommitMessageMatches(in FallbackCommitMessageInput, message string) bool {
	return message == FallbackCommitMessage(in) || message == fallbackCommitMessageV1(in)
}

func fallbackCommitMessage(in FallbackCommitMessageInput, withReason bool) string {
	title, failure := fallbackSpecificationTitle(in.Spec)
	reason := ""
	if withReason && failure == "" {
		reason = fallbackSpecificationReason(in.Spec)
	}
	return fallbackCommitMessageFromTitle(
		title, reason, failure, in.BoundIssue, in.RunID, in.SpecDigest, in.Policy,
	)
}

func fallbackCommitMessageFromTitle(
	title, reason, failure string,
	boundIssue *int,
	runID domain.RunID,
	specDigest domain.Digest,
	policy importer.Policy,
) string {
	subject := title
	if boundIssue != nil {
		subject += fmt.Sprintf(" (#%d)", *boundIssue)
	}
	body := fallbackCommitMessageBody(boundIssue, runID, specDigest, "")
	message := subject + "\n\n" + body
	switch {
	case failure != "":
	case len([]byte(subject)) > fallbackCommitSubjectMaxBytes:
		failure = fmt.Sprintf("subject exceeds the %d-byte limit", fallbackCommitSubjectMaxBytes)
	case !utf8.ValidString(message):
		failure = "message is not valid UTF-8"
	default:
		if err := importer.ScreenMessage(message, policy); err != nil {
			failure = "message screening failed: " + err.Error()
		}
	}
	if failure == "" {
		// The reason is added only to a message that already passes without
		// it, and dropped when the whole message then fails: wrapping can
		// put a trailer-shaped word at a line start, and the reason counts
		// against the policy's byte cap.
		if reason == "" {
			return message
		}
		withReason := subject + "\n\n" + wrapCommitMessageLine(reason) + "\n\n" + body
		if utf8.ValidString(withReason) && importer.ScreenMessage(withReason, policy) == nil {
			return withReason
		}
		return message
	}

	floor := fallbackCommitMessageFloorSubject(boundIssue, runID)
	floorMessage := floor + "\n\n" + fallbackCommitMessageBody(
		boundIssue, runID, specDigest, "Commit message derivation failed: "+failure+".",
	)
	if len([]byte(floor)) <= fallbackCommitSubjectMaxBytes {
		if err := importer.ScreenMessage(floorMessage, policy); err == nil {
			return floorMessage
		}
	}
	return "REWRITE ME"
}

func fallbackCommitMessageFloorSubject(boundIssue *int, runID domain.RunID) string {
	if boundIssue != nil {
		return fmt.Sprintf("REWRITE ME: commit message missing for work item #%d", *boundIssue)
	}
	prefix := "REWRITE ME: commit message missing (run "
	suffix := ")"
	trace := string(runID)
	if !safeCommitTraceValue(trace) || len([]byte(prefix+trace+suffix)) > fallbackCommitSubjectMaxBytes {
		sum := sha256.Sum256([]byte(trace))
		trace = contentaddr.Format(sum[:])[:len("sha256:")+16]
	}
	return prefix + trace + suffix
}

func fallbackSpecificationTitle(spec []byte) (string, string) {
	for _, rawLine := range strings.Split(string(spec), "\n") {
		line := strings.TrimSuffix(rawLine, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		line = strings.TrimSpace(line)
		hashes := 0
		for hashes < len(line) && line[hashes] == '#' {
			hashes++
		}
		if hashes == 0 || hashes > 6 || hashes == len(line) ||
			(line[hashes] != ' ' && line[hashes] != '\t') {
			return "", "approved specification has no leading ATX title"
		}
		title := strings.TrimSpace(line[hashes:])
		trimmed := strings.TrimRight(title, "#")
		if len(trimmed) < len(title) && strings.HasSuffix(trimmed, " ") {
			title = strings.TrimSpace(trimmed)
		}
		title = strings.TrimSpace(strings.TrimSuffix(title, "."))
		if title == "" {
			return "", "approved specification title is empty"
		}
		return title, ""
	}
	return "", "approved specification title is absent"
}

// fallbackSpecificationReason returns the approved specification's first
// prose paragraph after its title, cut to at most two sentences and
// fallbackCommitReasonMaxBytes, or "" when there is no usable one. The text
// is agent-written and reaches GitHub in a commit body, so it passes the
// publication prose screen and must not contain anything GitHub turns into a
// link or notification in a commit (autolinkTokenRe); a reason that fails
// either is dropped, never rendered inert.
func fallbackSpecificationReason(spec []byte) string {
	lines := strings.Split(string(spec), "\n")
	i := 0
	// Skip the title line fallbackSpecificationTitle accepted.
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	i++
	for i < len(lines) && strings.TrimSpace(lines[i]) == "" {
		i++
	}
	var paragraph []string
	for ; i < len(lines); i++ {
		line := strings.TrimSuffix(lines[i], "\r")
		if strings.TrimSpace(line) == "" {
			break
		}
		if len(paragraph) > 0 && setextUnderline(line) {
			// The paragraph is a setext heading, not prose.
			return ""
		}
		if len(paragraph) == 0 && markdownBlockStart(line, true) {
			return ""
		}
		if len(paragraph) > 0 && markdownBlockStart(line, false) {
			break
		}
		paragraph = append(paragraph, strings.TrimSpace(line))
	}
	reason := firstSentences(strings.Join(paragraph, " "), fallbackCommitReasonMaxBytes)
	if reason == "" ||
		publicationtext.Screen(reason, fallbackCommitReasonMaxBytes, false) != nil {
		return ""
	}
	// wrapCommitMessageLine hard-splits a word longer than a line, which
	// could start a fresh link token mid-word after this screen ran. With
	// no such word, wrapping only turns spaces into newlines, which keeps
	// every token boundary the screen saw.
	for _, word := range strings.Fields(reason) {
		if len(word) > fallbackCommitSubjectMaxBytes {
			return ""
		}
	}
	// Check each HTML-decoding stage, as the prose screen does. A "www."
	// anywhere is refused too: GFM links one after '_', where the
	// autolink pattern's word boundary does not hold.
	for text := reason; ; {
		if autolinkTokenRe.MatchString(text) || strings.Contains(strings.ToLower(text), "www.") {
			return ""
		}
		decoded := html.UnescapeString(text)
		if decoded == text {
			break
		}
		text = decoded
	}
	return reason
}

// markdownBlockStart reports whether line opens something other than a
// paragraph: a heading, list item, quote, table row, HTML block, fence, or
// thematic break, or, when it would open the paragraph (opening), indented
// code. An indented line inside a paragraph only continues it.
func markdownBlockStart(line string, opening bool) bool {
	indent := 0
	for indent < len(line) && line[indent] == ' ' {
		indent++
	}
	if indent >= 4 || strings.HasPrefix(line[indent:], "\t") {
		return opening
	}
	rest := line[indent:]
	switch {
	case rest == "":
		return false
	case strings.ContainsRune("#>|<", rune(rest[0])),
		strings.HasPrefix(rest, "```"), strings.HasPrefix(rest, "~~~"):
		return true
	case strings.ContainsRune("-*+", rune(rest[0])):
		return len(rest) == 1 || rest[1] == ' ' || rest[1] == '\t' ||
			thematicBreak(rest)
	case rest[0] == '_':
		return thematicBreak(rest)
	}
	digits := 0
	for digits < len(rest) && digits < 9 && rest[digits] >= '0' && rest[digits] <= '9' {
		digits++
	}
	if digits == 0 || digits == len(rest) || (rest[digits] != '.' && rest[digits] != ')') {
		return false
	}
	after := rest[digits+1:]
	return after == "" || after[0] == ' ' || after[0] == '\t'
}

func thematicBreak(rest string) bool {
	trimmed := strings.ReplaceAll(strings.ReplaceAll(rest, " ", ""), "\t", "")
	return len(trimmed) >= 3 && strings.Count(trimmed, trimmed[:1]) == len(trimmed)
}

func setextUnderline(line string) bool {
	trimmed := strings.TrimSpace(line)
	return trimmed != "" && (strings.Count(trimmed, "=") == len(trimmed) ||
		strings.Count(trimmed, "-") == len(trimmed))
}

// firstSentences returns the first two sentences of text when they fit in
// maxBytes, else the first when it fits, else "". A sentence ends at '.',
// '!', or '?' followed by a space and an uppercase letter, or at the end of
// the text.
func firstSentences(text string, maxBytes int) string {
	first := sentenceEnd(text, 0)
	second := sentenceEnd(text, first)
	switch {
	case second <= maxBytes:
		return text[:second]
	case first <= maxBytes:
		return text[:first]
	default:
		return ""
	}
}

func sentenceEnd(text string, from int) int {
	for i := from; i < len(text)-2; i++ {
		if !strings.ContainsRune(".!?", rune(text[i])) || text[i+1] != ' ' {
			continue
		}
		next, _ := utf8.DecodeRuneInString(text[i+2:])
		if unicode.IsUpper(next) {
			return i + 1
		}
	}
	return len(text)
}

func fallbackCommitMessageBody(
	boundIssue *int,
	runID domain.RunID,
	specDigest domain.Digest,
	failure string,
) string {
	lines := make([]string, 0, 4)
	if boundIssue != nil {
		lines = append(lines, fmt.Sprintf("Work item: #%d.", *boundIssue))
	}
	lines = append(lines,
		wrapCommitMessageTrace("Run ID", string(runID)),
		wrapCommitMessageTrace("Specification digest", string(specDigest)),
	)
	if failure != "" {
		lines = append(lines, wrapCommitMessageLine(failure))
	}
	return strings.Join(lines, "\n")
}

func wrapCommitMessageTrace(label, value string) string {
	if safeCommitTraceValue(value) {
		return wrapCommitMessageLine(label + ": " + value + ".")
	}
	return wrapCommitMessageLine(label + " (hex): " + hex.EncodeToString([]byte(value)) + ".")
}

func safeCommitTraceValue(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("._:/-", r):
		default:
			return false
		}
	}
	return true
}

func wrapCommitMessageLine(line string) string {
	words := strings.Fields(line)
	if len(words) == 0 {
		return ""
	}
	var wrapped strings.Builder
	column := 0
	for _, word := range words {
		wordBytes := len([]byte(word))
		if wordBytes > fallbackCommitSubjectMaxBytes {
			if column > 0 {
				wrapped.WriteByte('\n')
				column = 0
			}
			for len(word) > fallbackCommitSubjectMaxBytes {
				wrapped.WriteString(word[:fallbackCommitSubjectMaxBytes])
				wrapped.WriteByte('\n')
				word = word[fallbackCommitSubjectMaxBytes:]
			}
			wordBytes = len(word)
		}
		if column > 0 && column+1+wordBytes > fallbackCommitSubjectMaxBytes {
			wrapped.WriteByte('\n')
			column = 0
		}
		if column > 0 {
			wrapped.WriteByte(' ')
			column++
		}
		wrapped.WriteString(word)
		column += wordBytes
	}
	return wrapped.String()
}
