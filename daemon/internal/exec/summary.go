package exec

import (
	"strings"
	"unicode/utf8"
)

// MaxSummaryBytes bounds the human-readable outcome a stage result carries
// into the engine's durable records: long enough for an error string or a
// leading question, short enough that agent-controlled text cannot balloon a
// terminal row or an attention reason.
const MaxSummaryBytes = 512

// TruncateSummary normalizes s to valid UTF-8 and bounds it to
// MaxSummaryBytes on a rune boundary, marking a cut with an ellipsis. Every
// producer of a StageResult summary and every consumer that re-derives one
// use it, so the two agree byte for byte.
func TruncateSummary(s string) string {
	// Error strings can carry data from filesystem or process boundaries.
	// Normalize first so the durable JSON body and extracted summary column
	// cannot disagree about replacement of malformed byte sequences.
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= MaxSummaryBytes {
		return s
	}
	const suffix = "…"
	cut := MaxSummaryBytes - len(suffix)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + suffix
}
