package importer

import (
	"fmt"
	"path"
	"strings"
)

// DefaultAutomationControlPatterns is the §5.5 automation-control path
// class: paths CI or an agent runtime executes with implicit
// GITHUB_TOKEN and OIDC authority even when no secret is named in YAML,
// plus the fixed configuration paths of common non-GitHub CI systems
// ("CI entrypoints") and executable agent-hook locations. These are a
// mandatory minimum, never disableable; matching is case-insensitive
// because a downstream checkout may be.
var DefaultAutomationControlPatterns = []string{
	".github/workflows/**",
	".github/actions/**",
	".github/dependabot.yml",
	".github/hooks/**",
	"**/action.yml",
	"**/action.yaml",
	".gitlab-ci.yml",
	"Jenkinsfile",
	".circleci/**",
	"azure-pipelines.yml",
	".travis.yml",
}

// DefaultReviewerInstructionPatterns is the §5.8 reviewer-instruction
// path class: AGENTS.md at any depth, AGENTS.override.md, .codex/**,
// and peers — the vendor instruction, agent-definition, and skill
// surfaces automated reviewers and coding agents auto-load, which must
// never be modified by the candidate they govern. A mandatory minimum,
// never disableable.
var DefaultReviewerInstructionPatterns = []string{
	"**/AGENTS.md",
	"**/AGENTS.override.md",
	".codex/**",
	"**/CLAUDE.md",
	"**/CLAUDE.local.md",
	".claude/**",
	"**/GEMINI.md",
	".gemini/**",
	".cursor/**",
	".cursorrules",
	".windsurfrules",
	".windsurf/rules/**",
	".github/copilot-instructions.md",
	".github/instructions/**",
	".github/agents/**",
	".github/skills/**",
	".agents/skills/**",
}

// DefaultGitMetadataPatterns flags git metadata that steers downstream
// checkout, diff, filter, or submodule behaviour when the imported
// commit is later materialized. .gitignore and .mailmap stay plain
// content: they influence neither execution nor trust.
var DefaultGitMetadataPatterns = []string{
	"**/.gitmodules",
	"**/.gitattributes",
}

// Pattern accessors: the plan's classes are mandatory minimums, so the
// defaults ALWAYS apply and any caller-supplied patterns are ADDED, not
// substituted. A caller can widen a gate but can never narrow or
// disable it (an empty or partial override cannot drop a default, the
// safety failure §12 guards against).
func (p Policy) automationControl() []string {
	return append(append([]string{}, DefaultAutomationControlPatterns...), p.ExtraAutomationControlPatterns...)
}

func (p Policy) reviewerInstruction() []string {
	return append(append([]string{}, DefaultReviewerInstructionPatterns...), p.ExtraReviewerInstructionPatterns...)
}

func (p Policy) gitMetadata() []string {
	return append(append([]string{}, DefaultGitMetadataPatterns...), p.ExtraGitMetadataPatterns...)
}

// applyPolicy evaluates the path-class, allowlist, and size policy over
// the derived change set, deletions included: removing a workflow or an
// AGENTS.md changes what CI runs and what reviewers obey exactly as
// adding one does. Class findings are publish-blocking routes, not
// import failures, so the commit still exists for the §5.5
// control-plane path.
func applyPolicy(changes []plannedChange, pol Policy) []Finding {
	var findings []Finding
	var contentBytes int64
	for _, c := range changes {
		// The mandatory classes match against the alias-normalized path
		// (trailing dot/space and NTFS ADS suffix trimmed,
		// HFS-ignorable stripped): a
		// canonical candidate path can still materialize as a protected
		// name on a downstream NTFS/HFS checkout, and the finding must
		// fire on the name that will exist there. The finding still
		// reports the actual candidate path.
		classPath := normalizeAliases(c.path)
		if matchAny(pol.automationControl(), classPath, true) {
			findings = append(findings, c.finding(FindingAutomationControlPath, string(c.kind)))
		}
		if matchAny(pol.reviewerInstruction(), classPath, true) {
			findings = append(findings, c.finding(FindingReviewerInstructionPath, string(c.kind)))
		}
		if matchAny(pol.gitMetadata(), classPath, true) {
			findings = append(findings, c.finding(FindingGitMetadataPath, string(c.kind)))
		}
		if pol.Allowlist != nil && !matchAny(pol.Allowlist, c.path, false) {
			findings = append(findings, c.finding(FindingAllowlistViolation, string(c.kind)))
		}
		// Size policy bounds content the candidate introduced. A deletion
		// introduces none, and a fromBase change is mode-only over bytes
		// already in the trusted base, so neither is size-accounted: a
		// chmod on a large tracked file must not trip a size_violation.
		if c.kind == ChangeDeleted || c.fromBase {
			continue
		}
		if pol.MaxBlobBytes > 0 && c.size > pol.MaxBlobBytes {
			findings = append(findings, Finding{Path: c.path, Kind: FindingSizeViolation, Detail: fmt.Sprintf("%d bytes exceed the per-file cap of %d", c.size, pol.MaxBlobBytes)})
		}
		contentBytes += c.size
	}
	if pol.MaxTotalBytes > 0 && contentBytes > pol.MaxTotalBytes {
		findings = append(findings, Finding{Kind: FindingSizeViolation, Detail: fmt.Sprintf("change set carries %d content bytes, cap %d", contentBytes, pol.MaxTotalBytes)})
	}
	return findings
}

// matchAny reports whether p matches any of the slash-separated glob
// patterns, where "**" spans any number of path segments and other
// segments use path.Match semantics. foldCase lowers both sides; the
// allowlist matches exactly, since it names this repository's declared
// paths.
func matchAny(patterns []string, p string, foldCase bool) bool {
	if foldCase {
		p = strings.ToLower(p)
	}
	for _, pat := range patterns {
		if foldCase {
			pat = strings.ToLower(pat)
		}
		if matchSegments(strings.Split(pat, "/"), strings.Split(p, "/")) {
			return true
		}
	}
	return false
}

func matchSegments(pat, segs []string) bool {
	if len(pat) == 0 {
		return len(segs) == 0
	}
	if pat[0] == "**" {
		if matchSegments(pat[1:], segs) {
			return true // ** spans zero segments
		}
		if len(segs) > 0 {
			return matchSegments(pat, segs[1:]) // ** consumes one and stays greedy
		}
		return false
	}
	if len(segs) == 0 {
		return false
	}
	ok, err := path.Match(pat[0], segs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pat[1:], segs[1:])
}

// validGlob reports whether every segment of a slash-separated pattern
// compiles under path.Match ("**" is handled specially by
// matchSegments and always valid). Options.validate rejects a policy
// pattern that fails this, so an unparseable custom glob fails closed at
// the boundary rather than silently matching nothing.
func validGlob(pattern string) error {
	for _, seg := range strings.Split(pattern, "/") {
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, ""); err != nil {
			return err
		}
	}
	return nil
}
