package importer

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const agentProposedTrailer = "Freeside-Agent-Proposed: true"

type commitMessageFindingKind string

const (
	messageFindingOverCap        commitMessageFindingKind = "message_over_cap"
	messageFindingEmpty          commitMessageFindingKind = "message_empty"
	messageFindingControl        commitMessageFindingKind = "message_control"
	messageFindingCloseDirective commitMessageFindingKind = "github_close_directive"
	messageFindingSkipDirective  commitMessageFindingKind = "github_skip_directive"
	messageFindingSpoofedTrailer commitMessageFindingKind = "github_spoofed_trailer"
)

var allCommitMessageFindingKinds = []commitMessageFindingKind{
	messageFindingOverCap,
	messageFindingEmpty,
	messageFindingControl,
	messageFindingCloseDirective,
	messageFindingSkipDirective,
	messageFindingSpoofedTrailer,
}

type commitMessageFinding struct {
	GroupOrdinal int
	Kind         commitMessageFindingKind
}

var (
	githubCloseDirective = regexp.MustCompile(`(?i)\b(?:close|closes|closed|fix|fixes|fixed|resolve|resolves|resolved)\s*:?\s+(?:#[0-9]+|[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[0-9]+|https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/issues/[0-9]+)\b`)
	githubSkipDirective  = regexp.MustCompile(`(?i)\[(?:skip ci|ci skip|no ci|skip actions|actions skip)\]`)
	githubTrailer        = regexp.MustCompile(`(?im)^\s*(?:skip-checks|signed-off-by|co-authored-by|reviewed-by|acked-by|tested-by|reported-by|suggested-by|helped-by|mentored-by|freeside-agent-proposed)\s*:`)
)

func screenCommitMessages(groups []resolvedCommitGroup, pol Policy) []commitMessageFinding {
	var findings []commitMessageFinding
	for i, group := range groups {
		if len(group.changes) == 0 {
			continue
		}
		if kind, err := screenCommitMessage(group.message, pol); err != nil {
			findings = append(findings, commitMessageFinding{GroupOrdinal: i + 1, Kind: kind})
		}
	}
	return findings
}

func screenCommitMessage(message string, pol Policy) (commitMessageFindingKind, error) {
	if len([]byte(message)) > pol.MaxCommitMessageBytes {
		return messageFindingOverCap, fmt.Errorf("exceeds the %d-byte cap", pol.MaxCommitMessageBytes)
	}
	if strings.TrimSpace(message) == "" {
		return messageFindingEmpty, fmt.Errorf("is empty or whitespace-only")
	}
	for _, r := range message {
		if r != '\n' && (unicode.IsControl(r) || unicode.In(r, unicode.Cf)) {
			return messageFindingControl, fmt.Errorf("contains a control or format code point")
		}
	}
	switch pol.MessageRuleset {
	case domain.MessageRulesetGitHub1:
		if githubCloseDirective.MatchString(message) {
			return messageFindingCloseDirective, fmt.Errorf("matches a github/1 close directive")
		}
		if githubSkipDirective.MatchString(message) {
			return messageFindingSkipDirective, fmt.Errorf("matches a github/1 CI-skip directive")
		}
		if githubTrailer.MatchString(message) {
			return messageFindingSpoofedTrailer, fmt.Errorf("matches a github/1 spoofable trailer")
		}
	}
	return "", nil
}

func labelAgentMessage(message string) string {
	return strings.TrimRight(message, "\n") + "\n\n" + agentProposedTrailer
}
