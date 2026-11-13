package importer

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

func at(i int) *int { return &i }

// TestImportReviewGuideTrimsSizeCaps: a guide whose only faults are size
// caps imports, its stored guide cut down to every cap in the agent's order,
// and the result records one trim per breach in reading order. The engine's
// decode of the same bytes derives the same guide and trims.
func TestImportReviewGuideTrimsSizeCaps(t *testing.T) {
	t.Parallel()
	stops := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"title": "t", "summary": "s", "attention": "risk", "excerpts": []any{}}
		}
		return out
	}
	excerpts := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = map[string]any{"path": "a.txt", "side": "head", "start_line": 1, "end_line": 1, "caption": "c"}
		}
		return out
	}
	questions := func(n int) []any {
		out := make([]any, n)
		for i := range out {
			out[i] = "q"
		}
		return out
	}
	cases := map[string]struct {
		mutate func(map[string]any)
		want   []domain.ReviewGuideTrim
		check  func(*testing.T, domain.ReviewGuide)
	}{
		// The #2753 exit run's guide: stop 1's first excerpt covered 75-190.
		"excerpt over 80 lines": {
			mutate: func(g map[string]any) {
				g["stops"] = append(g["stops"].([]any), map[string]any{
					"title": "Second", "summary": "s", "attention": "risk",
					"excerpts": []any{map[string]any{
						"path": "a.txt", "side": "head", "start_line": 75, "end_line": 190, "caption": "c",
					}},
				})
			},
			want: []domain.ReviewGuideTrim{{
				Cap: domain.ReviewGuideCapExcerptLines, Stop: at(1), Excerpt: at(0), Limit: 80, Found: 116,
			}},
			check: func(t *testing.T, g domain.ReviewGuide) {
				if e := g.Stops[1].Excerpts[0]; e.StartLine != 75 || e.EndLine != 154 {
					t.Fatalf("range = %d-%d, want 75-154", e.StartLine, e.EndLine)
				}
			},
		},
		"stops": {
			mutate: func(g map[string]any) { g["stops"] = stops(13) },
			want:   []domain.ReviewGuideTrim{{Cap: domain.ReviewGuideCapStops, Limit: 12, Found: 13}},
			check: func(t *testing.T, g domain.ReviewGuide) {
				if len(g.Stops) != 12 {
					t.Fatalf("stops = %d, want 12", len(g.Stops))
				}
			},
		},
		"excerpts per stop": {
			mutate: func(g map[string]any) {
				all := excerpts(5)
				all[0].(map[string]any)["caption"] = "first"
				guideStop(g, 0)["excerpts"] = all
			},
			want: []domain.ReviewGuideTrim{{Cap: domain.ReviewGuideCapStopExcerpts, Stop: at(0), Limit: 4, Found: 5}},
			check: func(t *testing.T, g domain.ReviewGuide) {
				if ex := g.Stops[0].Excerpts; len(ex) != 4 || ex[0].Caption != "first" {
					t.Fatalf("excerpts = %+v, want the first 4", ex)
				}
			},
		},
		"questions": {
			mutate: func(g map[string]any) { g["questions"] = questions(9) },
			want:   []domain.ReviewGuideTrim{{Cap: domain.ReviewGuideCapQuestions, Limit: 8, Found: 9}},
			check: func(t *testing.T, g domain.ReviewGuide) {
				if len(g.Questions) != 8 {
					t.Fatalf("questions = %d, want 8", len(g.Questions))
				}
			},
		},
		"title": {
			mutate: func(g map[string]any) { guideStop(g, 0)["title"] = strings.Repeat("é", 121) },
			want:   []domain.ReviewGuideTrim{{Cap: domain.ReviewGuideCapTitle, Stop: at(0), Limit: 120, Found: 121}},
			check: func(t *testing.T, g domain.ReviewGuide) {
				wantCut(t, g.Stops[0].Title, strings.Repeat("é", 119), 120)
			},
		},
		"summary": {
			mutate: func(g map[string]any) { guideStop(g, 0)["summary"] = strings.Repeat("s", 2500) },
			want:   []domain.ReviewGuideTrim{{Cap: domain.ReviewGuideCapSummary, Stop: at(0), Limit: 2000, Found: 2500}},
			check: func(t *testing.T, g domain.ReviewGuide) {
				wantCut(t, g.Stops[0].Summary, strings.Repeat("s", 1999), 2000)
			},
		},
		"caption": {
			mutate: func(g map[string]any) { guideExcerpt(g)["caption"] = strings.Repeat("c", 301) },
			want: []domain.ReviewGuideTrim{{
				Cap: domain.ReviewGuideCapCaption, Stop: at(0), Excerpt: at(0), Limit: 300, Found: 301,
			}},
			check: func(t *testing.T, g domain.ReviewGuide) {
				wantCut(t, g.Stops[0].Excerpts[0].Caption, strings.Repeat("c", 299), 300)
			},
		},
		"question": {
			mutate: func(g map[string]any) { g["questions"] = []any{"keep", strings.Repeat("q", 501)} },
			want:   []domain.ReviewGuideTrim{{Cap: domain.ReviewGuideCapQuestion, Question: at(1), Limit: 500, Found: 501}},
			check: func(t *testing.T, g domain.ReviewGuide) {
				wantCut(t, g.Questions[1], strings.Repeat("q", 499), 500)
			},
		},
		"notes": {
			mutate: func(g map[string]any) { g["notes"] = strings.Repeat("n", 2001) },
			want:   []domain.ReviewGuideTrim{{Cap: domain.ReviewGuideCapNotes, Limit: 2000, Found: 2001}},
			check: func(t *testing.T, g domain.ReviewGuide) {
				wantCut(t, *g.Notes, strings.Repeat("n", 1999), 2000)
			},
		},
		// Several breaches are each recorded once, in reading order.
		"several in reading order": {
			mutate: func(g map[string]any) {
				g["notes"] = strings.Repeat("n", 2001)
				g["questions"] = questions(9)
				guideExcerpt(g)["end_line"] = 81
				guideExcerpt(g)["caption"] = strings.Repeat("c", 301)
				guideStop(g, 0)["title"] = strings.Repeat("t", 121)
			},
			want: []domain.ReviewGuideTrim{
				{Cap: domain.ReviewGuideCapTitle, Stop: at(0), Limit: 120, Found: 121},
				{Cap: domain.ReviewGuideCapExcerptLines, Stop: at(0), Excerpt: at(0), Limit: 80, Found: 81},
				{Cap: domain.ReviewGuideCapCaption, Stop: at(0), Excerpt: at(0), Limit: 300, Found: 301},
				{Cap: domain.ReviewGuideCapQuestions, Limit: 8, Found: 9},
				{Cap: domain.ReviewGuideCapNotes, Limit: 2000, Found: 2001},
			},
		},
		// A breach inside a stop the count cap drops is covered by that one
		// trim; a trim never names a place the stored guide lacks.
		"breach in a dropped stop": {
			mutate: func(g map[string]any) {
				all := stops(13)
				all[12].(map[string]any)["title"] = strings.Repeat("t", 121)
				g["stops"] = all
			},
			want: []domain.ReviewGuideTrim{{Cap: domain.ReviewGuideCapStops, Limit: 12, Found: 13}},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := reviewGuideFixture()
			tc.mutate(g)
			content := marshalGuide(t, g)
			res, err := importReviewGuide(t, reviewGuideEntry(content), content, false)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if res.CommitSHA == "" || len(res.Findings) != 0 || len(res.Claims) != 1 {
				t.Fatalf("result = %+v, want a clean commit with the guide claim", res)
			}
			notice := res.ReviewGuideNotice
			if notice == nil || notice.Omission != nil || !reflect.DeepEqual(notice.Trims, tc.want) {
				t.Fatalf("notice = %+v, want trims %+v", notice, tc.want)
			}
			if err := notice.Validate(); err != nil {
				t.Fatalf("notice.Validate: %v", err)
			}
			guide, trims, err := DecodeReviewGuide([]byte(content))
			if err != nil {
				t.Fatalf("DecodeReviewGuide: %v", err)
			}
			if !reflect.DeepEqual(trims, tc.want) {
				t.Fatalf("decode trims = %+v, want the import's %+v", trims, tc.want)
			}
			if err := validateUnresolvedReviewGuide(guide); err != nil {
				t.Fatalf("trimmed guide breaks a cap: %v", err)
			}
			if tc.check != nil {
				tc.check(t, guide)
			}
		})
	}
}

// wantCut checks a string cut to its cap: the kept prefix, then an
// ellipsis, limit code points in all.
func wantCut(t *testing.T, got, prefix string, limit int) {
	t.Helper()
	if got != prefix+"…" || utf8.RuneCountInString(got) != limit {
		t.Fatalf("cut string = %q (%d code points), want %d ending in an ellipsis",
			got, utf8.RuneCountInString(got), limit)
	}
}

// TestImportReviewGuideWithinCapsHasNoNotice: a guide that fits every cap
// imports whole, with no notice, so its result encodes as it did before the
// notice existed.
func TestImportReviewGuideWithinCapsHasNoNotice(t *testing.T) {
	t.Parallel()
	content := marshalGuide(t, reviewGuideFixture())
	res, err := importReviewGuide(t, reviewGuideEntry(content), content, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.ReviewGuideNotice != nil {
		t.Fatalf("notice = %+v, want none", res.ReviewGuideNotice)
	}
}

// TestImportReviewGuideOverReadBoundIsOmitted: a guide over
// MaxReviewGuideBytes cannot be decoded within the read bound, so the import
// goes on without it: no claim names it, and the notice says why.
func TestImportReviewGuideOverReadBoundIsOmitted(t *testing.T) {
	t.Parallel()
	valid := marshalGuide(t, reviewGuideFixture())
	content := valid[:len(valid)-1] + strings.Repeat(" ", MaxReviewGuideBytes) + "}"
	res, err := importReviewGuide(t, reviewGuideEntry(content), content, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" || len(res.Findings) != 0 || len(res.Claims) != 0 {
		t.Fatalf("result = %+v, want a clean commit with no guide claim", res)
	}
	want := domain.ReviewGuideOmissionFileTooLarge
	notice := res.ReviewGuideNotice
	if notice == nil || notice.Omission == nil || *notice.Omission != want || notice.Trims == nil ||
		len(notice.Trims) != 0 {
		t.Fatalf("notice = %+v, want omission %q with empty trims", notice, want)
	}
	if err := notice.Validate(); err != nil {
		t.Fatalf("notice.Validate: %v", err)
	}

	// The left-out body is still stored, so it is scanned like any entry
	// within the scan bound: a credential in it is still a finding.
	secret := valid[:len(valid)-1] + strings.Repeat(" ", MaxReviewGuideBytes) +
		`,"x":"ghp_` + strings.Repeat("A", 36) + `"}`
	res, err = importReviewGuide(t, reviewGuideEntry(secret), secret, false)
	if err != nil {
		t.Fatalf("Import with a secret: %v", err)
	}
	if len(res.Findings) != 1 || len(res.Claims) != 0 {
		t.Fatalf("result = %+v, want one secret finding and no guide claim", res)
	}
}

// TestImportReviewGuideScansWhatATrimCuts: the secret scan reads the guide
// as written, so a credential in text a trim cuts, or in a stop a count cap
// drops, is still a finding. Each token is spelled with a JSON escape, so
// only the decoded-string scan can see it.
func TestImportReviewGuideScansWhatATrimCuts(t *testing.T) {
	t.Parallel()
	escaped := "ghp_" + strings.Repeat("A", 8) + `\` + "u0041" + strings.Repeat("A", 30)
	for name, place := range map[string]func(map[string]any){
		"cut summary tail": func(g map[string]any) {
			guideStop(g, 0)["summary"] = strings.Repeat("s", 2000) + " MARKER"
		},
		"dropped stop": func(g map[string]any) {
			stops := make([]any, 13)
			for i := range stops {
				stops[i] = map[string]any{"title": "t", "summary": "s", "attention": "risk", "excerpts": []any{}}
			}
			stops[12].(map[string]any)["summary"] = "MARKER"
			g["stops"] = stops
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			g := reviewGuideFixture()
			place(g)
			content := strings.Replace(marshalGuide(t, g), "MARKER", escaped, 1)
			res, err := importReviewGuide(t, reviewGuideEntry(content), content, false)
			if err != nil {
				t.Fatalf("Import: %v", err)
			}
			if len(res.Findings) != 1 || res.Findings[0].Kind != FindingSecret ||
				res.Findings[0].Path != export.ReviewGuideEvidenceLabel || res.Findings[0].Detail == "" {
				t.Fatalf("findings = %+v, want one decoded-string secret finding on the guide", res.Findings)
			}
		})
	}
}
