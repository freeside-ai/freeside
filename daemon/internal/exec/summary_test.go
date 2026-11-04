package exec_test

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func TestTruncateFailureSummaryKeepsUnderCapInput(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "stage failed", strings.Repeat("x", exec.MaxSummaryBytes)} {
		if got := exec.TruncateFailureSummary(s); got != s {
			t.Errorf("TruncateFailureSummary(%d bytes) = %d bytes %q, want the input unchanged", len(s), len(got), got)
		}
	}
}

// The detail an operator needs sits at the end of a wrapped error chain:
// each layer that wraps with %w first pushes the cause further right.
func TestTruncateFailureSummaryKeepsTheCauseAtTheEnd(t *testing.T) {
	t.Parallel()
	cause := "container ward-abc123: exit status 125: volume create: no space left on device"
	chain := fmt.Errorf("start stage: %w",
		fmt.Errorf("prepare workspace %s: %w", strings.Repeat("p", 600), fmt.Errorf("%s", cause)))
	text := chain.Error()
	if len(text) <= exec.MaxSummaryBytes || !strings.HasSuffix(text, cause) || len(cause) >= 100 {
		t.Fatalf("fixture must be over the cap with its cause in the last 100 bytes")
	}

	got := exec.TruncateFailureSummary(text)
	if !strings.HasSuffix(got, cause) {
		t.Errorf("failure summary %q lost the cause %q", got, cause)
	}
	if !strings.HasPrefix(got, "start stage: ") {
		t.Errorf("failure summary %q lost the operation at its head", got)
	}
	if !strings.Contains(got, " … ") {
		t.Errorf("failure summary %q does not mark the elision", got)
	}
	if len(got) > exec.MaxSummaryBytes {
		t.Errorf("failure summary is %d bytes, cap %d", len(got), exec.MaxSummaryBytes)
	}
}

// A multi-byte rune can straddle the head cut, the tail cut, or both. Each
// shift of an ASCII prefix or suffix moves where the runes fall, so the
// sweep covers every alignment of a 3-byte and a 4-byte rune at both cuts.
func TestTruncateFailureSummaryCutsOnRuneBoundaries(t *testing.T) {
	t.Parallel()
	for _, r := range []string{"é", "€", "😀"} {
		for lead := 0; lead < 4; lead++ {
			for trail := 0; trail < 4; trail++ {
				s := strings.Repeat("a", lead) + strings.Repeat(r, 400) + strings.Repeat("z", trail)
				got := exec.TruncateFailureSummary(s)
				if len(got) > exec.MaxSummaryBytes {
					t.Errorf("rune %q lead %d trail %d: %d bytes, cap %d", r, lead, trail, len(got), exec.MaxSummaryBytes)
				}
				if !utf8.ValidString(got) {
					t.Errorf("rune %q lead %d trail %d: invalid UTF-8 %q", r, lead, trail, got)
				}
				if !strings.HasSuffix(got, r+strings.Repeat("z", trail)) {
					t.Errorf("rune %q lead %d trail %d: lost the tail, got %q", r, lead, trail, got)
				}
			}
		}
	}
}

// Invalid bytes are replaced before the length check, so the producer and a
// re-deriving consumer see the same normalized text and the same cut.
func TestTruncateFailureSummaryNormalizesBeforeMeasuring(t *testing.T) {
	t.Parallel()
	under := "bad byte \xff here"
	if got, want := exec.TruncateFailureSummary(under), "bad byte � here"; got != want {
		t.Errorf("TruncateFailureSummary(%q) = %q, want %q", under, got, want)
	}
	// Each invalid byte (separated, since a run becomes one replacement)
	// grows from one byte to three: 264 raw bytes fit the cap, their
	// 524-byte normalization does not, and the cut must apply to the latter.
	over := strings.Repeat("\xffa", 130) + "tail"
	got := exec.TruncateFailureSummary(over)
	if !utf8.ValidString(got) || len(got) > exec.MaxSummaryBytes ||
		!strings.Contains(got, " … ") || !strings.HasSuffix(got, "tail") {
		t.Errorf("TruncateFailureSummary over normalized cap = %d bytes %q", len(got), got)
	}
}

// Questions lead with what they ask, so their bound keeps the head.
func TestTruncateSummaryKeepsTheHead(t *testing.T) {
	t.Parallel()
	question := "Which migration should run first? " + strings.Repeat("context ", 100)
	got := exec.TruncateSummary(question)
	if !strings.HasPrefix(got, "Which migration should run first? ") || !strings.HasSuffix(got, "…") {
		t.Errorf("TruncateSummary(question) = %q, want its head and a trailing ellipsis", got)
	}
	if len(got) > exec.MaxSummaryBytes || !utf8.ValidString(got) {
		t.Errorf("TruncateSummary(question) = %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}
