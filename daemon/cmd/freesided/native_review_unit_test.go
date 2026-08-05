package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
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
		{"<sub>![P3 Badge](https://img.shields.io/badge/P3-blue?style=flat)</sub> optional nit", "P3"},
		{"![P2 Badge](https://img.shields.io/badge/P2-yellow)", "P2"},
		{"**<sub><sub>![P4 Badge](https://img.shields.io/badge/P4-grey)</sub></sub>  out of range", ""},
		{"prose then an unrelated ![image](x) with no badge", ""},
		// Plain-text badge fallback (a badge typed as text).
		{"P1: dropped error", "P1"},
		{"P2 unchecked return", "P2"},
		{"[P3] optional nit", "P3"},
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
