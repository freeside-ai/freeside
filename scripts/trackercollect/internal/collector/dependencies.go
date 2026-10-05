package collector

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The typed relationships a Dependencies field may declare
// (docs/coordination.md, Relationship Types).
const (
	RelationshipStartsAfter   = "starts-after"
	RelationshipMergesAfter   = "merges-after"
	RelationshipStackedOn     = "stacked-on"
	RelationshipExclusiveWith = "exclusive-with"
)

// Dependency line kinds.
const (
	DependencyTyped = "typed"
	DependencyNone  = "none"
	// dependencyUnknown lines are reported as UNKNOWN relationships, never
	// among the typed dependencies.
	dependencyUnknown = "unknown"
)

const (
	TargetIssue       = "issue"
	TargetPullRequest = "pull_request"
)

const (
	reasonUntyped        = "does not lead with a typed relationship or none"
	reasonNoTarget       = "relationship keyword with no #N or PR #N target"
	reasonTargetOutRange = "target number is zero or exceeds the forge's integer range"
)

var (
	// dependencyLeadPattern matches a line that leads with a relationship
	// keyword: an optional list marker, an optional backtick, the keyword, and
	// an optional backtick and colon. The rest of the line is captured. A
	// keyword anywhere else on a line is prose and never types the line.
	dependencyLeadPattern = regexp.MustCompile("(?i)^(?:(?:[-*+]|[0-9]+[.)])[ \t]+)?`?(starts-after|merges-after|stacked-on|exclusive-with)\\b`?[ \t]*:?[ \t]*(.*)$")
	// dependencyNonePattern matches a line whose first word is "none".
	dependencyNonePattern      = regexp.MustCompile("(?i)^(?:(?:[-*+]|[0-9]+[.)])[ \t]+)?[*_`]*none\\b")
	dependencyKeywordNone      = regexp.MustCompile("(?i)^[*_`]*none\\b")
	dependencyTargetPattern    = regexp.MustCompile("(?i)^(?:(PR)[ \t]+)?#([0-9]+)\\b`?")
	dependencySeparatorPattern = regexp.MustCompile(`(?i)^[ \t]*(?:,[ \t]*and\b|,|/|&|and\b)[ \t]*`)
	issueReferencePattern      = regexp.MustCompile(`#([0-9]+)\b`)
	codeIndentPattern          = regexp.MustCompile(`(?m)^(?:    |\t)[ \t]*`)
)

type DependencyTarget struct {
	Kind   string `json:"kind"`
	Number int    `json:"number"`
}

func dependencyTargetList(targets []DependencyTarget) string {
	parts := make([]string, 0, len(targets))
	for _, target := range targets {
		if target.Kind == TargetPullRequest {
			parts = append(parts, fmt.Sprintf("PR #%d", target.Number))
		} else {
			parts = append(parts, fmt.Sprintf("#%d", target.Number))
		}
	}
	return strings.Join(parts, ", ")
}

// dependencyEntry is one classified line of a Dependencies section with the
// indented lines under it.
type dependencyEntry struct {
	kind string
	// relationship is the leading keyword. It is set on an unknown entry too
	// when the line leads with a keyword but names no usable target.
	relationship string
	targets      []DependencyTarget
	// line is the text as written; evidence is the same text with code and
	// HTML comments masked, which is what mentions are searched in.
	line     string
	evidence string
	reason   string
}

// parseDependencies classifies every line of one Dependencies section, as
// extractSections returns it with the heading first. A line is typed only
// when it leads with a relationship keyword and at least one target, so a
// spelling this parser does not know becomes an unknown entry: a missed
// relationship is reported, never read as a different one. An indented line
// that leads with no keyword continues the entry above it. Lines inside a
// fenced code block or an HTML comment are not evidence, as elsewhere in this
// package.
//
// The shared masking also treats every line indented four spaces or a tab as
// a code block, but under a list item the forge renders such a line as a
// nested item or a continuation. Dropping it could drop a declaration, so a
// line masked only for its indentation is read from the section with that
// indentation removed, where fences and comments still mask.
func parseDependencies(section string) []dependencyEntry {
	rawLines := strings.Split(section, "\n")
	evidenceLines := strings.Split(markdownEvidence(section), "\n")
	dedentedLines := strings.Split(markdownEvidence(codeIndentPattern.ReplaceAllString(section, "")), "\n")
	var entries []dependencyEntry
	for index := 1; index < len(evidenceLines); index++ {
		evidence := strings.TrimRight(evidenceLines[index], " \t\r")
		trimmed := strings.TrimLeft(evidence, " \t")
		indented := len(trimmed) != len(evidence)
		if trimmed == "" && hasCodeIndent(rawLines[index]) {
			trimmed = strings.TrimSpace(dedentedLines[index])
			indented = true
		}
		if trimmed == "" {
			continue
		}
		raw := strings.TrimSpace(rawLines[index])
		lead := dependencyLeadPattern.FindStringSubmatch(trimmed)
		if indented && lead == nil && len(entries) > 0 {
			last := &entries[len(entries)-1]
			last.line += "\n" + raw
			last.evidence += "\n" + trimmed
			continue
		}
		entry := dependencyEntry{line: raw, evidence: trimmed}
		switch {
		case lead != nil:
			entry.relationship = strings.ToLower(lead[1])
			entry.kind, entry.targets, entry.reason = parseDependencyTargets(lead[2])
		case dependencyNonePattern.MatchString(trimmed):
			entry.kind = DependencyNone
		default:
			entry.kind, entry.reason = dependencyUnknown, reasonUntyped
		}
		entries = append(entries, entry)
	}
	return entries
}

// parseDependencyTargets reads the targets that follow a relationship
// keyword: one #N or PR #N, then more after a comma, slash, ampersand, or
// "and". It stops at the first text that is not a target, which is the
// line's rationale.
func parseDependencyTargets(rest string) (string, []DependencyTarget, string) {
	if dependencyKeywordNone.MatchString(rest) {
		return DependencyNone, nil, ""
	}
	var targets []DependencyTarget
	for {
		match := dependencyTargetPattern.FindStringSubmatch(rest)
		if match == nil {
			break
		}
		parsed, err := strconv.ParseInt(match[2], 10, 32)
		if err != nil || parsed <= 0 {
			return dependencyUnknown, nil, reasonTargetOutRange
		}
		kind := TargetIssue
		if match[1] != "" {
			kind = TargetPullRequest
		}
		targets = append(targets, DependencyTarget{Kind: kind, Number: int(parsed)})
		rest = rest[len(match[0]):]
		separator := dependencySeparatorPattern.FindString(rest)
		if separator == "" {
			break
		}
		rest = rest[len(separator):]
	}
	if len(targets) == 0 {
		return dependencyUnknown, nil, reasonNoTarget
	}
	return DependencyTyped, targets, ""
}

// declaresExclusiveWith reports whether the entry is a typed exclusive-with
// declaration naming the issue.
func (entry dependencyEntry) declaresExclusiveWith(issue int) bool {
	if entry.kind != DependencyTyped || entry.relationship != RelationshipExclusiveWith {
		return false
	}
	for _, target := range entry.targets {
		if target.Kind == TargetIssue && target.Number == issue {
			return true
		}
	}
	return false
}

// mentionsExclusiveWith reports whether the entry carries the exclusive-with
// keyword anywhere, its continuation lines included.
func (entry dependencyEntry) mentionsExclusiveWith() bool {
	return strings.Contains(strings.ToLower(entry.evidence), RelationshipExclusiveWith)
}

func (entry dependencyEntry) mentionsIssue(issue int) bool {
	for _, match := range issueReferencePattern.FindAllStringSubmatch(entry.evidence, -1) {
		if sameNumber(match[1], issue) {
			return true
		}
	}
	return false
}

// sameNumber compares written digits with a number, so a zero-padded
// reference still names its issue.
func sameNumber(digits string, number int) bool {
	trimmed := strings.TrimLeft(digits, "0")
	return trimmed != "" && trimmed == strconv.Itoa(number)
}

// undeclaredReferences returns the #N references of an entry that carries
// the exclusive-with keyword, other than its declared exclusive-with targets.
// Any of them may be a partner the grammar did not type: a second target
// behind a separator it does not know, one on a continuation line, or one
// behind another relationship's keyword. An entry without the keyword
// returns none.
func (entry dependencyEntry) undeclaredReferences() []string {
	if !entry.mentionsExclusiveWith() {
		return nil
	}
	var undeclared []string
	seen := make(map[string]bool)
	for _, match := range issueReferencePattern.FindAllStringSubmatch(entry.evidence, -1) {
		declared := seen[match[1]]
		if entry.kind == DependencyTyped && entry.relationship == RelationshipExclusiveWith {
			for _, target := range entry.targets {
				declared = declared || sameNumber(match[1], target.Number)
			}
		}
		if !declared {
			seen[match[1]] = true
			undeclared = append(undeclared, "#"+match[1])
		}
	}
	return undeclared
}
