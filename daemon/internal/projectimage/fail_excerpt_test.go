package projectimage

import (
	"fmt"
	"strings"
	"testing"
)

func TestFailingTestsKeepsGoFailureLinesFromTheWholeStream(t *testing.T) {
	t.Parallel()
	var many strings.Builder
	for i := range maxFailingTestLines + 3 {
		fmt.Fprintf(&many, "--- FAIL: TestCase%d (0.00s)\n", i)
	}
	for _, tc := range []struct {
		name   string
		writes []string
		want   string
	}{
		{
			name:   "line split across writes",
			writes: []string{"ok  \tpkg/a\t0.1s\n--- FA", "IL: TestSplit (0.01s)\n"},
			want:   "--- FAIL: TestSplit (0.01s)",
		},
		{
			name:   "indented subtest and failed package",
			writes: []string{"--- FAIL: TestParent (0.00s)\n    --- FAIL: TestParent/child (0.00s)\n        x_test.go:9: boom\nFAIL\n"},
			want:   "--- FAIL: TestParent (0.00s)\n    --- FAIL: TestParent/child (0.00s)",
		},
		{
			name:   "package build failure without a trailing newline",
			writes: []string{"FAIL\n", "FAIL\tgithub.com/x/y [build failed]"},
			want:   "FAIL\tgithub.com/x/y [build failed]",
		},
		{
			name:   "bare FAIL names nothing",
			writes: []string{"FAIL\nexit status 1\n"},
			want:   "",
		},
		{
			name:   "more than the limit",
			writes: []string{many.String()},
			want: strings.Join(strings.Split(many.String(), "\n")[:maxFailingTestLines], "\n") +
				"\n… and 3 more",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var tests failingTests
			for _, write := range tc.writes {
				if n, err := tests.Write([]byte(write)); n != len(write) || err != nil {
					t.Fatalf("Write = %d, %v; want full consumption and no error", n, err)
				}
			}
			if got := tests.summary(); got != tc.want {
				t.Fatalf("summary = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFailingTestsBoundsLongLines(t *testing.T) {
	t.Parallel()
	var tests failingTests
	long := "--- FAIL: Test" + strings.Repeat("x", 2*maxPendingLineBytes)
	for _, part := range []string{long[:100], long[100:], "\n"} {
		_, _ = tests.Write([]byte(part))
	}
	got := tests.summary()
	if len(tests.pending) != 0 || !strings.HasPrefix(got, "--- FAIL: Testxxx") ||
		len(got) != maxFailingTestLineBytes+len("…") {
		t.Fatalf("summary = %d bytes %q..., want one line cut at %d bytes", len(got), got[:20], maxFailingTestLineBytes)
	}
}
