// Package credentialpattern is the one list of credential rules the daemon
// screens text with. The export scan (internal/exportscan), which refuses a
// handoff, and the importer (internal/importer), which raises import findings
// and backs ContainsSecret for agent replies, specifications, and publication
// prose, both read this list, so a rule changes here and nowhere else. Two
// hand-kept copies drifted once: the importer's GitHub token rule was
// narrower than the export scan's, and nothing tied them together.
//
// Each rule is high-signal and low false-positive: it anchors on a vendor's
// own token structure or an armored header rather than a generic word, so a
// match is the credential itself, not a mention of one. The set is
// deliberately narrow; §5.4 promises a best-effort scan of textual output,
// not universal detection, and a low false-positive set keeps a refusal
// meaningful. A rule that drops a word boundary does so on purpose: a token
// glued to a preceding word character or followed by an underscore is still
// a token (devlog/2026-07-30-0952-anthropic-secret-rule.md).
//
// Tier A: these rules decide what the export scan refuses and what the
// importer reports, so a change here is a change to both gates.
//
// The package imports only the standard library.
package credentialpattern
