package export

import (
	"fmt"
	"strings"
	"testing"
)

// TestIgnoreHint checks the hint against git's verdicts for the rule forms
// build tools write. Each want was checked against `git check-ignore
// --no-index` on the same tree when the case was written.
func TestIgnoreHint(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		rules map[string]string // rules file path -> content
		path  string
		want  bool
	}{
		{"no rules", nil, "app/.build/x", false},
		{"nested directory rule", map[string]string{"app/.gitignore": ".build/\n"}, "app/.build/checkouts/a.swift", true},
		{"nested rule matches at any depth", map[string]string{"app/.gitignore": ".build/\n"}, "app/pkg/.build/a", true},
		{"nested rule stays in its directory", map[string]string{"app/.gitignore": ".build/\n"}, "daemon/.build/a", false},
		{"directory-only rule skips a file", map[string]string{".gitignore": "out/\n"}, "out", false},
		{"bare name matches a file", map[string]string{".gitignore": "*.log\n"}, "a/b/c.log", true},
		{"anchored rule", map[string]string{".gitignore": "/dist\n"}, "dist/a.js", true},
		{"anchored rule is not unanchored", map[string]string{".gitignore": "/dist\n"}, "web/dist/a.js", false},
		{"inner slash anchors", map[string]string{".gitignore": "gen/out\n"}, "x/gen/out", false},
		{"double star", map[string]string{".gitignore": "**/gen/*.go\n"}, "a/b/gen/z.go", true},
		{"double star at zero depth", map[string]string{".gitignore": "**/gen/*.go\n"}, "gen/z.go", true},
		{"trailing double star", map[string]string{".gitignore": "cache/**\n"}, "cache/a/b", true},
		{"trailing double star skips the directory's name", map[string]string{".gitignore": "cache/**\n"}, "cache", false},
		{"negation re-includes a file", map[string]string{".gitignore": "*.log\n!keep.log\n"}, "keep.log", false},
		{"last match wins", map[string]string{".gitignore": "!keep.log\n*.log\n"}, "keep.log", true},
		{"deeper file overrides", map[string]string{".gitignore": "*.log\n", "a/.gitignore": "!*.log\n"}, "a/x.log", false},
		{"negation cannot reach inside an ignored directory", map[string]string{".gitignore": "build/\n!build/keep\n"}, "build/keep", true},
		{"comment is not a rule", map[string]string{".gitignore": "# x\n"}, "# x", false},
		{"escaped hash is a rule", map[string]string{".gitignore": "\\#x\n"}, "#x", true},
		{"bang class negates", map[string]string{".gitignore": "*.[!o]\n"}, "a.c", true},
		{"bang class excludes its member", map[string]string{".gitignore": "*.[!o]\n"}, "a.o", false},
		{"crlf line ending", map[string]string{".gitignore": "out\r\n"}, "out", true},
		{"trailing spaces trimmed", map[string]string{".gitignore": "out  \n"}, "out", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newIgnoreHint()
			for file, content := range tc.rules {
				h.add(file, []byte(content))
			}
			if got := h.ignored(tc.path); got != tc.want {
				t.Fatalf("ignored(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestIgnoreHintRuleCap: rules past maxIgnoreRules are dropped, which only
// weakens the hint; it never errors or grows without bound.
func TestIgnoreHintRuleCap(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	for i := range maxIgnoreRules {
		fmt.Fprintf(&b, "f%d\n", i)
	}
	b.WriteString("late\n")
	h := newIgnoreHint()
	h.add(".gitignore", []byte(b.String()))
	if !h.ignored("f0") {
		t.Fatal("first rule dropped")
	}
	if h.ignored("late") {
		t.Fatal("rule past the cap was kept")
	}
}

// TestIgnoreHintDropsLongRules: a rule past maxRuleSegments is dropped, so
// one hostile line cannot multiply the cost of every file's match.
func TestIgnoreHintDropsLongRules(t *testing.T) {
	t.Parallel()
	h := newIgnoreHint()
	h.add(".gitignore", []byte(strings.Repeat("*/", maxRuleSegments)+"z\n"))
	if h.count != 0 {
		t.Fatalf("kept %d rules, want the long rule dropped", h.count)
	}
}

// TestMatchSegmentsManyDoubleStars: repeated "**" stays polynomial.
func TestMatchSegmentsManyDoubleStars(t *testing.T) {
	t.Parallel()
	pattern := strings.Split(strings.Repeat("**/", 64)+"z", "/")
	parts := strings.Split(strings.Repeat("a/", 256)+"b", "/")
	if matchSegments(pattern, parts) {
		t.Fatal("matched a path without z")
	}
}
