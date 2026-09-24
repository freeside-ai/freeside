// Package publicationtext screens publication prose before inference audit
// storage and again when the engine reconstructs an authored artifact.
package publicationtext

import (
	"errors"
	"html"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// ScreenField attaches a fixed author field name to a content-free refusal.
// Unknown names fail closed without being interpolated into the diagnostic.
func ScreenField(field, text string, maxBytes int) error {
	switch field {
	case "title", "body", "outcome_summary", "reviewer_notes":
	default:
		return errors.New("publication author output: unknown field")
	}
	if err := Screen(text, maxBytes, true); err != nil {
		return errors.New("publication author " + field + ": " + err.Error())
	}
	return nil
}

// Screen preserves the source screen for v1 and additionally checks the
// Markdown visible-text approximation for authored v2 fields. Each HTML decode
// and optional Markdown collapse is screened until stable. The candidate-body
// check has its own smaller budget, independent of the field's storage bound.
// Errors identify the failing screen, never its input or a nested error.
func Screen(text string, maxBytes int, collapseMarkdown bool) error {
	if !utf8.ValidString(text) || len(text) > maxBytes {
		return errors.New("encoding_or_size")
	}
	for {
		if importer.ScreenMessage(text, importer.Policy{
			MaxCommitMessageBytes: maxBytes, MessageRuleset: domain.MessageRulesetGitHub1,
		}) != nil {
			return errors.New("message_rules")
		}
		if importer.ContainsSecret([]byte(text)) {
			return errors.New("secret_detection")
		}
		if ValidateCandidateBody(text) != nil {
			return errors.New("candidate_body_size_or_reserved_section")
		}
		if strings.Contains(strings.ToLower(text), "freeside:") {
			return errors.New("publisher_marker")
		}
		decoded := html.UnescapeString(text)
		if collapseMarkdown {
			decoded = collapseMarkdownDelimiters(decoded)
		}
		if decoded == text {
			return nil
		}
		text = decoded
	}
}
