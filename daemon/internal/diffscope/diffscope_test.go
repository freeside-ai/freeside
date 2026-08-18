package diffscope_test

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/diffscope"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// fixtureDiff is a git diff -U0 with one modified file (a pure-addition hunk
// giving new-side lines 11-13, then a pure-deletion hunk that adds no new-side
// line) and one deleted file.
const fixtureDiff = `diff --git a/daemon/main.go b/daemon/main.go
index 1111111..2222222 100644
--- a/daemon/main.go
+++ b/daemon/main.go
@@ -10,0 +11,3 @@ func main() {
+	a()
+	b()
+	c()
@@ -20,2 +24,0 @@ func teardown() {
-	old()
-	older()
diff --git a/daemon/old.go b/daemon/old.go
deleted file mode 100644
index 3333333..0000000 100644
--- a/daemon/old.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package old
-
-func gone() {}
`

func loc(path string, start, end int) *domain.FindingLocation {
	return &domain.FindingLocation{Path: path, StartLine: start, EndLine: end}
}

func TestOverlaps(t *testing.T) {
	d, err := diffscope.Parse(fixtureDiff)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	cases := []struct {
		name string
		loc  *domain.FindingLocation
		want bool
	}{
		{"overlapping range accepted", loc("daemon/main.go", 12, 12), true},
		{"range spanning the changed span accepted", loc("daemon/main.go", 11, 13), true},
		{"range touching the changed span at its edge accepted", loc("daemon/main.go", 13, 40), true},
		{"non-overlapping range rejected (after)", loc("daemon/main.go", 14, 20), false},
		{"non-overlapping range rejected (before)", loc("daemon/main.go", 1, 10), false},
		{"pure-deletion hunk adds no new-side line", loc("daemon/main.go", 24, 24), false},
		{"nil location rejected", nil, false},
		{"malformed partial range rejected", loc("daemon/main.go", 0, 12), false},
		{"path absent from the diff rejected", loc("daemon/other.go", 12, 12), false},
		{"whole-file accepted for a touched path", loc("daemon/main.go", 0, 0), true},
		{"whole-file rejected for an absent path", loc("daemon/other.go", 0, 0), false},
		{"whole-file accepted for a deleted path", loc("daemon/old.go", 0, 0), true},
		{"line range rejected for a deleted path", loc("daemon/old.go", 2, 2), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := d.Overlaps(tc.loc); got != tc.want {
				t.Errorf("Overlaps(%+v) = %v, want %v", tc.loc, got, tc.want)
			}
		})
	}
}

// TestParseSingleLineHunk covers the omitted-count hunk form (`-n +m` == one
// line each): a single-line replacement at new line 6.
func TestParseSingleLineHunk(t *testing.T) {
	d, err := diffscope.Parse("--- a/x.go\n+++ b/x.go\n@@ -5 +6 @@\n-old\n+one\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !d.Overlaps(loc("x.go", 6, 6)) {
		t.Error("single-line hunk should place a change at new line 6")
	}
	if d.Overlaps(loc("x.go", 7, 7)) {
		t.Error("single-line hunk should not extend past new line 6")
	}
}

// TestParseNoPrefixOperands accepts a diff whose operands carry no a/ b/ prefix.
func TestParseNoPrefixOperands(t *testing.T) {
	d, err := diffscope.Parse("--- x.go\n+++ x.go\n@@ -0,0 +1,2 @@\n+a\n+b\n")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !d.Overlaps(loc("x.go", 1, 2)) {
		t.Error("unprefixed operands should still resolve the path")
	}
}

func TestParseEmpty(t *testing.T) {
	d, err := diffscope.Parse("")
	if err != nil {
		t.Fatalf("Parse empty: %v", err)
	}
	if d.Overlaps(loc("anything.go", 1, 1)) {
		t.Error("an empty diff overlaps nothing")
	}
}

// TestParseBodyLinesLookingLikeHeaders is the regression for the refuted bug:
// a deleted line whose content starts with "-- " renders as "--- " and an added
// line starting with "++ " renders as "+++ ", which must be consumed as hunk
// body, never mistaken for a file header.
func TestParseBodyLinesLookingLikeHeaders(t *testing.T) {
	diff := "diff --git a/x.sql b/x.sql\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/x.sql\n" +
		"+++ b/x.sql\n" +
		"@@ -5,1 +5,0 @@\n" +
		"--- a legacy SQL comment\n" + // a deleted "-- a legacy SQL comment" line
		"@@ -10,0 +11,1 @@\n" +
		"+++ increment the counter\n" // an added "++ increment the counter" line
	d, err := diffscope.Parse(diff)
	if err != nil {
		t.Fatalf("Parse rejected a diff with header-shaped body lines: %v", err)
	}
	if !d.Overlaps(loc("x.sql", 11, 11)) {
		t.Error("the added line at new line 11 was not recorded")
	}
	if d.Overlaps(loc("x.sql", 5, 5)) {
		t.Error("a deleted line has no new-side range to overlap")
	}
	if !d.Overlaps(loc("x.sql", 0, 0)) {
		t.Error("whole-file location should accept the touched path")
	}
}

// TestParseNoNewlineMarker tolerates the "\ No newline at end of file" marker,
// which appears in a hunk body but does not count toward the hunk's length.
func TestParseNoNewlineMarker(t *testing.T) {
	diff := "--- a/y.go\n+++ b/y.go\n@@ -1,1 +1,1 @@\n-old line\n\\ No newline at end of file\n+new line\n\\ No newline at end of file\n"
	d, err := diffscope.Parse(diff)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !d.Overlaps(loc("y.go", 1, 1)) {
		t.Error("the replaced line at new line 1 was not recorded")
	}
}

// TestParsePureRename records no range for a content-free rename, so a finding
// on the renamed path does not overlap (there is no reviewed change).
func TestParsePureRename(t *testing.T) {
	diff := "diff --git a/old.go b/new.go\nsimilarity index 100%\nrename from old.go\nrename to new.go\n"
	d, err := diffscope.Parse(diff)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Overlaps(loc("new.go", 0, 0)) || d.Overlaps(loc("old.go", 0, 0)) {
		t.Error("a pure rename has no reviewed content and should not overlap")
	}
}

// TestParseRejectsTruncatedHunkBody fails closed on a diff cut short mid-hunk:
// the header declared more body lines than are present, so later content may be
// missing and a header-declared range must not be returned.
func TestParseRejectsTruncatedHunkBody(t *testing.T) {
	// Header promises 100 new lines; only one is present.
	diff := "--- a/x.go\n+++ b/x.go\n@@ -0,0 +1,100 @@\n+only one line\n"
	if _, err := diffscope.Parse(diff); err == nil {
		t.Error("Parse accepted a truncated hunk body; want a fail-closed error")
	}
}

// TestParseGitQuotedPath decodes git's C-style path quoting so a finding on a
// non-ASCII path resolves. `café.go` is emitted by git as "b/caf\303\251.go".
func TestParseGitQuotedPath(t *testing.T) {
	diff := "--- \"a/caf\\303\\251.go\"\n+++ \"b/caf\\303\\251.go\"\n@@ -0,0 +1,1 @@\n+added\n"
	d, err := diffscope.Parse(diff)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !d.Overlaps(loc("café.go", 1, 1)) {
		t.Error("a finding on the decoded non-ASCII path did not overlap")
	}
	if d.Overlaps(loc("caf\\303\\251.go", 1, 1)) {
		t.Error("the still-quoted operand should not index the diff")
	}
}

// TestParseGarbageAfterQuotedPath fails closed on a quoted operand followed by
// non-timestamp bytes: the malformed header must not index the hunk under the
// decoded path, so a finding on it does not overlap.
func TestParseGarbageAfterQuotedPath(t *testing.T) {
	diff := "--- \"a/x.go\"garbage\n+++ \"b/x.go\"garbage\n@@ -0,0 +1,1 @@\n+added\n"
	d, err := diffscope.Parse(diff)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if d.Overlaps(loc("x.go", 1, 1)) {
		t.Error("a finding from a malformed quoted header must not overlap")
	}
}

func TestParseErrors(t *testing.T) {
	cases := []struct {
		name string
		diff string
	}{
		{"malformed hunk header", "--- a/x.go\n+++ b/x.go\n@@ nonsense @@\n"},
		{"hunk before any file header", "@@ -1,0 +1,1 @@\n+a\n"},
		{"file header names no path", "--- /dev/null\n+++ /dev/null\n"},
		// A context line has no place in a -U0 diff. The old parser consumed it
		// (a side on each counter) and recorded the whole new-side span 1-2 as
		// changed, so a finding on the unchanged context line 1 would overlap;
		// the parser now fails closed on the context marker instead.
		{"context line rejected in a -U0 diff", "--- a/x.go\n+++ b/x.go\n@@ -1,2 +1,2 @@\n context1\n-old2\n+new2\n"},
		{"context line past an exhausted side", "--- a/x.go\n+++ b/x.go\n@@ -0,0 +1,2 @@\n+added\n unchanged\n"},
		{"removed line past an exhausted side", "--- a/x.go\n+++ b/x.go\n@@ -0,0 +1,2 @@\n+added\n-gone\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := diffscope.Parse(tc.diff); err == nil {
				t.Errorf("Parse(%q) = nil error, want error", tc.diff)
			}
		})
	}
}
