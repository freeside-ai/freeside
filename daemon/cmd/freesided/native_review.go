package main

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// defaultNativeReviewerLogin is the automated reviewer whose native activity is
// observed by default (AGENTS.md, Automated reviewer). It is wired here as a
// default and threaded through configuration, never hardcoded in the domain.
const defaultNativeReviewerLogin = "chatgpt-codex-connector"

// nativeReviewCleanPassContent is the reaction content a native reviewer posts
// on the PR description when its pass is clean and it files no review
// (AGENTS.md, Automated reviewer).
const nativeReviewCleanPassContent = "+1"

// buildNativeReviewObservations normalizes one raw PR review-activity
// observation into durable, readiness-inert domain observations bound to the
// ready item's PR identity and its head at observation time (plan §5.16, §7;
// issue #497). It filters to the configured reviewer logins (third-party
// content crossing the trust boundary), reuses domain.Finding for inline
// comment findings, and records a stale-head review's divergence rather than
// dropping it. A reviewer's clean-pass reaction on the PR description becomes a
// finding-free clean_pass_signal. It never inspects readiness.
func buildNativeReviewObservations(
	obs publish.PullReviewObservation, binding domain.ReadyItemPRBinding,
	reviewers map[string]bool, observedAt time.Time,
) []domain.NativeReviewObservation {
	commentsByReview := map[int64][]publish.PullReviewComment{}
	for _, c := range obs.Comments {
		if !reviewers[canonicalReviewerLogin(c.AuthorLogin)] || c.ID <= 0 {
			continue
		}
		commentsByReview[c.ReviewID] = append(commentsByReview[c.ReviewID], c)
	}

	var observations []domain.NativeReviewObservation
	for _, rv := range obs.Reviews {
		login := canonicalReviewerLogin(rv.AuthorLogin)
		if !reviewers[login] || rv.ID <= 0 || rv.CommitID == "" {
			continue
		}
		comments := commentsByReview[rv.ID]
		// Normalize to a deterministic finding order (by native comment id) so a
		// re-poll that returns the same comments in a different order does not
		// register as a material change (MaterialChangeFrom compares findings
		// element-wise).
		sort.Slice(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })
		findings := make([]domain.Finding, 0, len(comments))
		for _, c := range comments {
			findings = append(findings, nativeFinding(c, binding.RunID))
		}
		observation := domain.NativeReviewObservation{
			Repo: binding.Repo, RepositoryID: binding.RepositoryID, PRNumber: binding.PRNumber,
			Provider: domain.NativeReviewCodexGitHub, Kind: domain.NativeReviewFindings,
			NativeID: rv.ID, AuthorLogin: login,
			ReviewCommitSHA: boundedNativeText(rv.CommitID), ReviewState: boundedNativeText(rv.State),
			BindingHeadSHA: binding.HeadSHA,
			SubmittedAt:    rv.SubmittedAt.UTC(), ObservedAt: observedAt,
			Findings: findings,
		}
		if observation.Validate() == nil {
			observations = append(observations, observation)
		}
	}

	for _, rn := range obs.Reactions {
		// A clean-pass reaction carries no commit_id, so it can be attributed to
		// the current head only by time. Reactions persist across pushes, so a
		// leftover +1 from an earlier head keeps its old id and CreatedAt; reject
		// any reaction predating this head's binding so it is not re-recorded as a
		// fresh clean pass for the current head.
		if !reviewers[canonicalReviewerLogin(rn.AuthorLogin)] || rn.ID <= 0 ||
			rn.Content != nativeReviewCleanPassContent || rn.CreatedAt.Before(binding.RecordedAt) {
			continue
		}
		observation := domain.NativeReviewObservation{
			Repo: binding.Repo, RepositoryID: binding.RepositoryID, PRNumber: binding.PRNumber,
			Provider: domain.NativeReviewCodexGitHub, Kind: domain.NativeReviewCleanPass,
			NativeID: rn.ID, AuthorLogin: canonicalReviewerLogin(rn.AuthorLogin), BindingHeadSHA: binding.HeadSHA,
			SubmittedAt: rn.CreatedAt.UTC(), ObservedAt: observedAt,
		}
		if observation.Validate() == nil {
			observations = append(observations, observation)
		}
	}
	return observations
}

// canonicalReviewerLogin strips a trailing GitHub App "[bot]" suffix so a
// reviewer configured by its canonical login (e.g. chatgpt-codex-connector)
// matches the "[bot]" form GitHub's REST reviews, review-comments, and
// reactions endpoints return for a GitHub App (AGENTS.md, Automated reviewer).
// Without this, the exact-login filter drops every Codex review, comment, and
// reaction and the feature observes nothing in production. A real user login
// can never carry the reserved "[bot]" suffix, so the strip is unambiguous; the
// canonical login is what the durable observation records, keeping the evidence
// identity stable regardless of which API form the forge returned.
func canonicalReviewerLogin(login string) string {
	return strings.TrimSuffix(login, "[bot]")
}

// nativeFinding normalizes one inline review comment into a domain.Finding.
// The comment body is third-party content, so it is sanitized to valid UTF-8
// and bounded to the domain size cap before it is stored (issue #180
// precedent); the severity is lifted from the leading priority badge.
func nativeFinding(c publish.PullReviewComment, runID domain.RunID) domain.Finding {
	body := boundedNativeText(c.Body)
	// The comment path is third-party content (git stores paths as raw bytes,
	// so a legacy filename can carry invalid UTF-8 with no adversary); sanitize
	// it on the same trust boundary as the body before it reaches the store.
	path := boundedNativeText(c.Path)
	// A pathed comment maps to a structured location: a line-bearing comment to
	// the range [start, line] (a multi-line comment supplies StartLine; a
	// single-line one collapses to [line, line]), a file-level comment (no line)
	// to the whole-file location (0,0). A comment with no path is a review-level
	// observation and carries a nil location (§7 fails closed on it downstream).
	var location *domain.FindingLocation
	if path != "" {
		loc := domain.FindingLocation{Path: path}
		if c.Line > 0 {
			start := c.Line
			if c.StartLine > 0 {
				start = c.StartLine
			}
			loc.StartLine, loc.EndLine = start, c.Line
		}
		location = &loc
	}
	return domain.Finding{
		ID:        domain.FindingID(fmt.Sprintf("native-comment-%d", c.ID)),
		RunID:     runID,
		Source:    string(domain.NativeReviewCodexGitHub),
		Severity:  domain.FindingSeverity(nativeReviewBadge(body)),
		Location:  location,
		Message:   body,
		RawText:   body,
		CreatedAt: c.CreatedAt.UTC(),
	}
}

// nativeReviewBadgePrefixBytes bounds how far into a third-party review body the
// shield-badge scan reads, so a runaway body cannot drive an unbounded scan.
const nativeReviewBadgePrefixBytes = 512

// nativeReviewBadge lifts a P0/P1/P2/P3 priority badge from a native review
// comment body, or "" when none is present. Real Codex comments open with a shields.io
// badge image (`**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-...)`),
// so the badge sits behind markdown the plain-text scan never reaches: the
// shield-image form is parsed first, then a plain leading-`Pn`-token form (a
// badge typed as text) as a fallback.
func nativeReviewBadge(body string) string {
	if sev := shieldBadgeSeverity(body); sev != "" {
		return sev
	}
	return leadingBadgeSeverity(body)
}

// shieldBadgeSeverity extracts P0/P1/P2/P3 from a shields.io badge image's alt text
// (`![Pn Badge]`) within a bounded prefix of the body, or "" when no badge image
// is present. It tolerates framing whitespace between the image marker and the
// badge letter and skips a non-badge image to a later one.
func shieldBadgeSeverity(body string) string {
	prefix := body
	if len(prefix) > nativeReviewBadgePrefixBytes {
		prefix = prefix[:nativeReviewBadgePrefixBytes]
	}
	for {
		i := strings.Index(prefix, "![")
		if i < 0 {
			return ""
		}
		if sev := badgeToken(strings.TrimLeft(prefix[i+2:], " \t")); sev != "" {
			return sev
		}
		prefix = prefix[i+2:]
	}
}

// leadingBadgeSeverity extracts P0/P1/P2/P3 from a plain-text badge at the front of
// the body (`P2: ...`, `[P3] ...`), tolerating framing punctuation before it.
func leadingBadgeSeverity(body string) string {
	return badgeToken(strings.TrimLeft(body, " \t\n\r*[("))
}

// badgeToken returns "P0"/"P1"/"P2"/"P3" when s opens with that exact badge
// token followed by a boundary (punctuation, space, or end), else "". It
// rejects a longer token like "Priority" or an out-of-range "P4".
func badgeToken(s string) string {
	if len(s) < 2 || s[0] != 'P' {
		return ""
	}
	switch s[1] {
	case '0', '1', '2', '3':
	default:
		return ""
	}
	if len(s) > 2 {
		switch s[2] {
		case ' ', '\t', ':', ']', ')', '.', '-', ',', '*':
		default:
			return ""
		}
	}
	return s[:2]
}

// boundedNativeText sanitizes third-party review text to valid UTF-8 and caps
// it at the domain size limit, truncating on a rune boundary so the stored
// body stays valid UTF-8 and round-trips stably (the json.Marshal invalid-UTF-8
// trap, issue #180).
func boundedNativeText(s string) string {
	s = strings.ToValidUTF8(s, "�")
	if len(s) <= domain.MaxNativeReviewTextBytes {
		return s
	}
	truncated := s[:domain.MaxNativeReviewTextBytes]
	return strings.ToValidUTF8(truncated, "")
}
