package domain

import (
	"slices"
	"strings"
	"unicode"
)

// blockingBadgeScanBytes bounds how far past its opening bracket the badge
// scan reads, so a long comment cannot drive an unbounded scan.
const blockingBadgeScanBytes = 64

// ExternalCommentLabeledBlocking reports whether an external reviewer's
// comment opens with a bracketed badge, such as "[P3, blocking]" or
// "[P3 | blocking]", one of whose tokens is "blocking" in any case. A token
// is a run of letters, digits, hyphens, and underscores, so "non-blocking"
// is not one. Leading whitespace and emphasis are skipped, as the severity
// badge parse skips them. The text
// is untrusted, but a match only holds a finding for a person, so it grants
// nothing; a "blocking" anywhere else in the comment is not read.
func ExternalCommentLabeledBlocking(message string) bool {
	body, ok := strings.CutPrefix(strings.TrimLeft(message, " \t\n\r*"), "[")
	if !ok {
		return false
	}
	if len(body) > blockingBadgeScanBytes {
		body = body[:blockingBadgeScanBytes]
	}
	badge, _, closed := strings.Cut(body, "]")
	if !closed {
		return false
	}
	return slices.ContainsFunc(strings.FieldsFunc(badge, badgeSeparator), func(token string) bool {
		return strings.EqualFold(token, "blocking")
	})
}

func badgeSeparator(r rune) bool {
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '_'
}
