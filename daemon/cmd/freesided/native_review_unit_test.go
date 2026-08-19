package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestNativeReviewBadge(t *testing.T) {
	for _, tc := range []struct {
		body string
		want string
	}{
		// Real Codex inline-comment format: a shields.io badge image behind
		// bold/`<sub>` markdown, which the plain leading-token scan never reaches.
		{"**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-yellow?style=flat)</sub></sub>  Avoid losing native observations after write failures", "P2"},
		{"**<sub><sub>![P1 Badge](https://img.shields.io/badge/P1-red?style=flat)</sub></sub>  Blocking", "P1"},
		{"**<sub><sub>![P0 Badge](https://img.shields.io/badge/P0-darkred?style=flat)</sub></sub>  Critical", "P0"},
		{"<sub>![P3 Badge](https://img.shields.io/badge/P3-blue?style=flat)</sub> optional nit", "P3"},
		{"![P2 Badge](https://img.shields.io/badge/P2-yellow)", "P2"},
		{"**<sub><sub>![P4 Badge](https://img.shields.io/badge/P4-grey)</sub></sub>  out of range", ""},
		{"prose then an unrelated ![image](x) with no badge", ""},
		// Plain-text badge fallback (a badge typed as text).
		{"P1: dropped error", "P1"},
		{"P2 unchecked return", "P2"},
		{"[P3] optional nit", "P3"},
		{"P0: critical, must fix", "P0"},
		{"[P0] data loss", "P0"},
		{"  **P1** blocking", "P1"},
		{"(P2) consider", "P2"},
		{"Priority: high", ""},
		{"P4: out of range", ""},
		{"Ptolemy was an astronomer", ""},
		{"no badge at all", ""},
		{"", ""},
		{"P", ""},
	} {
		if got := nativeReviewBadge(tc.body); got != tc.want {
			t.Errorf("nativeReviewBadge(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestNativeFindingLocationMapping(t *testing.T) {
	when := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	// A line-bearing inline comment maps to the single-line range [line, line].
	lined := nativeFinding(publish.PullReviewComment{
		ID: 1, Path: "daemon/main.go", Line: 42, Body: "P1: dropped error", CreatedAt: when,
	}, "run-1")
	if lined.Severity != "P1" || lined.Location == nil ||
		lined.Location.Path != "daemon/main.go" || lined.Location.StartLine != 42 || lined.Location.EndLine != 42 {
		t.Fatalf("line-bearing finding location = %#v (sev %q)", lined.Location, lined.Severity)
	}
	// A multi-line comment carries StartLine (first) and Line (last).
	multiline := nativeFinding(publish.PullReviewComment{
		ID: 4, Path: "daemon/main.go", StartLine: 40, Line: 42, Body: "P2: range", CreatedAt: when,
	}, "run-1")
	if multiline.Location == nil || multiline.Location.StartLine != 40 || multiline.Location.EndLine != 42 {
		t.Fatalf("multi-line finding location = %#v", multiline.Location)
	}
	// A file-level comment (no line) maps to the whole-file location (0,0).
	fileLevel := nativeFinding(publish.PullReviewComment{
		ID: 2, Path: "daemon/main.go", Line: 0, Body: "general note", CreatedAt: when,
	}, "run-1")
	if fileLevel.Location == nil || fileLevel.Location.Path != "daemon/main.go" ||
		fileLevel.Location.StartLine != 0 || fileLevel.Location.EndLine != 0 {
		t.Fatalf("file-level finding location = %#v", fileLevel.Location)
	}
	// A comment with no path is a review-level observation: a nil location.
	pathless := nativeFinding(publish.PullReviewComment{
		ID: 3, Path: "", Line: 0, Body: "no path", CreatedAt: when,
	}, "run-1")
	if pathless.Location != nil {
		t.Fatalf("pathless finding location = %#v, want nil", pathless.Location)
	}
	// Every constructed finding must be domain-valid.
	for _, f := range []domain.Finding{lined, multiline, fileLevel, pathless} {
		if err := f.Validate(); err != nil {
			t.Errorf("native finding %s invalid: %v", f.ID, err)
		}
	}
}

func TestBoundedNativeText(t *testing.T) {
	// Invalid UTF-8 is sanitized so the stored body round-trips stably.
	got := boundedNativeText("ok\xffbad")
	if !utf8.ValidString(got) {
		t.Errorf("boundedNativeText left invalid UTF-8: %q", got)
	}

	// Oversized text is truncated to the cap on a rune boundary and stays valid.
	oversized := strings.Repeat("a", domain.MaxNativeReviewTextBytes+100)
	bounded := boundedNativeText(oversized)
	if len(bounded) > domain.MaxNativeReviewTextBytes {
		t.Errorf("boundedNativeText did not cap: %d bytes", len(bounded))
	}
	if !utf8.ValidString(bounded) {
		t.Error("boundedNativeText produced invalid UTF-8 after truncation")
	}
}
