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

// failureSummaryHeadBytes is how much of an over-cap failure summary's start
// TruncateFailureSummary keeps. The start names the operation that failed;
// the rest of the budget goes to the end, where a wrapped error chain puts
// its specific cause.
const failureSummaryHeadBytes = 128

// failureSummaryMarker joins the head and tail of a cut failure summary.
const failureSummaryMarker = " … "

// TruncateSummary normalizes s to valid UTF-8 and bounds it to
// MaxSummaryBytes on a rune boundary, keeping the head and marking a cut
// with an ellipsis. It bounds agent questions, which lead with what they
// ask; failure text uses TruncateFailureSummary instead. Every producer of
// a question summary and every consumer that re-derives one use it, so the
// two agree byte for byte.
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

// TruncateFailureSummary normalizes s to valid UTF-8 and bounds it to
// MaxSummaryBytes, keeping both ends of an over-cap input joined by an
// elision marker. It bounds failure text: an error wrapped outward through
// several layers carries its specific cause at the end, which a head-only
// cut would drop. Both cuts fall on rune boundaries, so the result is valid
// UTF-8. Every producer of a failure summary and every consumer that
// re-derives one use it, so the two agree byte for byte.
func TruncateFailureSummary(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	if len(s) <= MaxSummaryBytes {
		return s
	}
	head := failureSummaryHeadBytes
	for head > 0 && !utf8.RuneStart(s[head]) {
		head--
	}
	// The tail gets the rest of the budget, measured from the fixed head
	// size so a short head never lets the result grow past the cap.
	tail := len(s) - (MaxSummaryBytes - failureSummaryHeadBytes - len(failureSummaryMarker))
	for tail < len(s) && !utf8.RuneStart(s[tail]) {
		tail++
	}
	return s[:head] + failureSummaryMarker + s[tail:]
}
