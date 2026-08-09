package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

const fallbackCommitSubjectMaxBytes = 72

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
func FallbackCommitMessage(in FallbackCommitMessageInput) string {
	title, failure := fallbackSpecificationTitle(in.Spec)
	return fallbackCommitMessageFromTitle(
		title, failure, in.BoundIssue, in.RunID, in.SpecDigest, in.Policy,
	)
}

func fallbackCommitMessageFromApprovedTitle(
	title string,
	boundIssue *int,
	runID domain.RunID,
	specDigest domain.Digest,
	policy importer.Policy,
) string {
	title = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(title), "."))
	failure := ""
	if title == "" {
		failure = "approved title is absent"
	}
	return fallbackCommitMessageFromTitle(
		title, failure, boundIssue, runID, specDigest, policy,
	)
}

func fallbackCommitMessageFromTitle(
	title, failure string,
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
		trace = "sha256:" + hex.EncodeToString(sum[:8])
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
