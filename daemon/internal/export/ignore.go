package export

import (
	"bytes"
	"path"
	"slices"
	"strings"
)

// The ignore hint decides only the order in which the blob budgets are spent:
// regular files the workspace's own .gitignore rules leave unignored are
// offered blobs first, ignored ones (build output, package caches) last. It
// never removes a manifest entry, never sets blob_omitted by itself, and grants
// nothing: the importer judges ignored-ness against the exact base, so a hint
// that is wrong, or written by a hostile agent, can at worst cause an omitted
// blob, which import blocks on unless the base itself ignores the path. That is
// why the agent-controlled rules may be read here at all.
//
// The rules are parsed from the bytes the blob pass already streamed from each
// .gitignore (the workspace is still read exactly once) and are bounded by
// maxIgnoreFileBytes, maxIgnoreRules, and maxRuleSegments, so a hostile rule
// set cannot balloon memory, and one file's match costs at most the rules
// above it times their segments times the file's depth. That cost is not
// otherwise capped: a hostile agent can still slow its own export toward the
// ward's exporter timeout, which fails only its own handoff, as many other
// workspace shapes already can. The matcher follows gitignore(5) for the
// forms build tools write (comments, negation, directory-only and anchored
// patterns, `**`, `[!...]` classes); rules files git would also read but the
// walk never visits (.git/info/exclude, core.excludesFile) are out of scope.
const (
	// maxIgnoreFileBytes is the largest .gitignore whose rules are read; a
	// larger file still gets its blob and simply contributes no hint.
	maxIgnoreFileBytes = 64 << 10
	// maxIgnoreRules bounds the rules kept across the whole workspace; rules
	// past it are dropped, which only weakens the hint.
	maxIgnoreRules = 1000
	// maxRuleSegments drops a rule with more path segments than this; real
	// rules have a handful, and a dropped rule only weakens the hint.
	maxRuleSegments = 64
)

type ignoreRule struct {
	negate  bool
	dirOnly bool
	// segments is the slash-separated pattern relative to the rules file's
	// directory; an unanchored pattern is prefixed with "**".
	segments []string
}

// ignoreHint holds the parsed rules keyed by the directory whose .gitignore
// declared them ("" for the workspace root), and caches each directory's
// verdict so a large ignored tree is judged once per directory.
type ignoreHint struct {
	rules map[string][]ignoreRule
	count int
	dirs  map[string]bool
}

func newIgnoreHint() *ignoreHint {
	return &ignoreHint{rules: map[string][]ignoreRule{}, dirs: map[string]bool{}}
}

// isIgnoreFile reports whether a manifest path is a .gitignore whose rules
// feed the hint.
func isIgnoreFile(p string) bool { return path.Base(p) == ".gitignore" }

// add parses one .gitignore's content as the rules of its directory.
func (h *ignoreHint) add(file string, content []byte) {
	dir := path.Dir(file)
	if dir == "." {
		dir = ""
	}
	for line := range bytes.SplitSeq(content, []byte("\n")) {
		if h.count >= maxIgnoreRules {
			return
		}
		if rule, ok := parseIgnoreLine(string(line)); ok {
			h.rules[dir] = append(h.rules[dir], rule)
			h.count++
		}
	}
}

func parseIgnoreLine(line string) (ignoreRule, bool) {
	line = strings.TrimSuffix(line, "\r")
	// Unescaped trailing spaces are not part of the pattern.
	for strings.HasSuffix(line, " ") && !strings.HasSuffix(line, "\\ ") {
		line = line[:len(line)-1]
	}
	if line == "" || strings.HasPrefix(line, "#") {
		return ignoreRule{}, false
	}
	var rule ignoreRule
	if strings.HasPrefix(line, "!") {
		rule.negate = true
		line = line[1:]
	} else if strings.HasPrefix(line, `\!`) || strings.HasPrefix(line, `\#`) {
		line = line[1:]
	}
	if strings.HasSuffix(line, "/") {
		rule.dirOnly = true
		line = strings.TrimRight(line, "/")
	}
	// A slash anywhere but the end anchors the pattern to the rules file's
	// directory; otherwise it matches a name at any depth below it.
	anchored := strings.Contains(line, "/")
	line = strings.TrimPrefix(line, "/")
	if line == "" {
		return ignoreRule{}, false
	}
	rule.segments = strings.Split(line, "/")
	if !anchored {
		rule.segments = append([]string{"**"}, rule.segments...)
	}
	if len(rule.segments) > maxRuleSegments {
		return ignoreRule{}, false
	}
	for i, seg := range rule.segments {
		rule.segments[i] = caretNegation(seg)
	}
	return rule, true
}

// caretNegation rewrites git's "[!" class negation to the "[^" form
// path.Match understands; path.Match would read "!" as a class member.
func caretNegation(seg string) string {
	if !strings.Contains(seg, "[!") {
		return seg
	}
	b := []byte(seg)
	for i := 0; i < len(b); i++ {
		switch {
		case b[i] == '\\':
			i++ // skip the escaped byte
		case b[i] == '[' && i+1 < len(b) && b[i+1] == '!':
			b[i+1] = '^'
		}
	}
	return string(b)
}

// ignored reports whether the hint puts a regular file last. Like git, a file
// under an ignored directory stays ignored whatever a deeper rule says.
func (h *ignoreHint) ignored(file string) bool {
	if h.count == 0 {
		return false
	}
	return h.underIgnoredDir(file) || h.verdict(file, false)
}

// underIgnoredDir reports whether any directory above p is ignored. Git never
// reads a rules file inside an ignored directory, so neither does the hint.
func (h *ignoreHint) underIgnoredDir(p string) bool {
	if h.count == 0 {
		return false
	}
	for i := range len(p) {
		if p[i] == '/' && h.dirIgnored(p[:i]) {
			return true
		}
	}
	return false
}

// forgetVerdicts drops the cached directory verdicts, which were computed
// before every rules file was read.
func (h *ignoreHint) forgetVerdicts() { h.dirs = map[string]bool{} }

func (h *ignoreHint) dirIgnored(dir string) bool {
	if v, ok := h.dirs[dir]; ok {
		return v
	}
	v := h.verdict(dir, true)
	h.dirs[dir] = v
	return v
}

// verdict applies every rules file from the root down to p's parent in order,
// so the last matching rule wins and a deeper file overrides a shallower one.
func (h *ignoreHint) verdict(p string, isDir bool) bool {
	parts := strings.Split(p, "/")
	ignored := false
	apply := func(base string, rel []string) {
		for _, r := range h.rules[base] {
			if (!r.dirOnly || isDir) && matchSegments(r.segments, rel) {
				ignored = !r.negate
			}
		}
	}
	apply("", parts)
	end := 0
	for depth := 1; depth < len(parts); depth++ {
		end += len(parts[depth-1])
		if base := p[:end]; len(h.rules[base]) > 0 {
			apply(base, parts[depth:])
		}
		end++ // the slash
	}
	return ignored
}

// matchSegments matches a pattern's segments against a path's, where "**"
// spans zero or more segments (one or more when it ends the pattern, so
// "foo/**" matches inside foo but not foo itself) and any other segment is a
// path.Match glob. It is a dynamic program over (pattern, path) positions,
// so repeated "**" cannot make it exponential.
func matchSegments(pattern, parts []string) bool {
	// reach[j] reports whether the pattern prefix consumed so far can end
	// just before parts[j].
	reach := make([]bool, len(parts)+1)
	reach[0] = true
	for i, seg := range pattern {
		next := make([]bool, len(parts)+1)
		if seg == "**" {
			minSpan := 0
			if i == len(pattern)-1 {
				minSpan = 1
			}
			for j, ok := range reach {
				if ok {
					for k := j + minSpan; k <= len(parts); k++ {
						next[k] = true
					}
					break
				}
			}
		} else {
			for j := range parts {
				if reach[j] {
					if ok, err := path.Match(seg, parts[j]); ok && err == nil {
						next[j+1] = true
					}
				}
			}
		}
		if !slices.Contains(next, true) {
			return false
		}
		reach = next
	}
	return reach[len(parts)]
}
