package publicationtext

import (
	"os"
	"strings"
	"testing"
)

func TestScreenCategories(t *testing.T) {
	for _, tc := range []struct {
		name, text, category string
		max                  int
	}{
		{"encoding", "invalid\xff", "encoding_or_size", 100},
		{"field bound", "four", "encoding_or_size", 3},
		{"empty", "", "message_rules", 100},
		{"control", "a\tb", "message_rules", 100},
		{"closing", "Closes #1", "message_rules", 100},
		{"ci", "[skip ci]", "message_rules", 100},
		{"trailer", "Reviewed-by: someone", "message_rules", 100},
		{"secret", "ghp_" + strings.Repeat("x", 36), "secret_detection", 100},
		{"spliced", "ghp_" + strings.Repeat("x", 18) + "**xx**" + strings.Repeat("x", 16), "secret_detection", 100},
		{"reserved", "## Verification", "candidate_body_size_or_reserved_section", 100},
		{"source reference heading", "## Source Issue", "candidate_body_size_or_reserved_section", 100},
		{"legacy source reference heading", "## Source issue", "candidate_body_size_or_reserved_section", 100},
		{"candidate budget", strings.Repeat("x", 32<<10), "candidate_body_size_or_reserved_section", 32 << 10},
		{"marker", "freeside:unknown", "publisher_marker", 100},
		{"recursive entity", "F&amp;#105;xes #1", "message_rules", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ScreenField("body", tc.text, tc.max)
			if err == nil || err.Error() != "publication author body: "+tc.category {
				t.Fatalf("category = %v, want %s", err, tc.category)
			}
		})
	}
}

// This is the public template at freeasinbird/gh-imgup commit
// 5ad83e9b8e7fe096af323128d7af6d1d4020cfcc, .github/pull_request_template.md.
// It demonstrates conflicting requirements, not the discarded live response.
func TestPinnedTemplateConflict(t *testing.T) {
	template, err := os.ReadFile("testdata/gh-imgup-template.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, requirement := range []string{"## Verification", "Closes #11", "Passed:", "Checked:", "Attempted:", "Not run:"} {
		if !strings.Contains(string(template), requirement) {
			t.Fatalf("fixture missing %q", requirement)
		}
	}
	for _, copied := range []string{"## Verification\n\n- Passed: unit tests.", "Closes #11"} {
		if ScreenField("body", copied, 1000) == nil {
			t.Fatal("accepted a forbidden template requirement")
		}
	}
	prose := "## Why\n\nPrevent empty-input crashes.\n\n## What\n\n- Handle the empty case.\n\n## Review Notes\n\nCheck evidence and the source reference are supplied separately by the publisher. Its fixed check format does not use the template's requested status prefixes."
	if err := ScreenField("body", prose, 1000); err != nil {
		t.Fatal(err)
	}
}
