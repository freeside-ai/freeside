package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// reviewGuideClaimFixture is the reserved claim a guide written against head
// is parsed from: agent-produced, head-bound, application/json, no inline
// text.
func reviewGuideClaimFixture(head string) domain.AgentClaim {
	return domain.AgentClaim{
		Label: domain.ReviewGuideClaimLabel, Artifact: "art-review-guide", Digest: "sha256:review-guide",
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: "inv-2",
			HeadBinding:          domain.HeadBound,
			SourceHeadSHA:        head,
			SensitivityClass:     domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1024,
			CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
			Source:    domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
}

// reviewGuideFixture is a valid two-stop guide written against head: one
// excerpt resolved on head, one unresolved on base. The golden and the
// app's AttentionFixtures carry the same stops.
func reviewGuideFixture(head string) domain.ReviewGuide {
	text := "func (g ReviewGuide) Validate() error {\n\tif g.HeadSHA == \"\" {\n"
	missing := domain.ReviewExcerptPathMissing
	notes := "The schema change is the contract; the rest follows it."
	return domain.ReviewGuide{
		HeadSHA:     head,
		ClaimDigest: "sha256:review-guide",
		Stops: []domain.ReviewStop{
			{
				Title:     "Start with the guide's validation",
				Summary:   "Validate enforces every cap the schema cannot express.",
				Attention: domain.ReviewAttentionContract,
				Excerpts: []domain.ReviewExcerpt{{
					Path: "daemon/internal/domain/review_guide.go", Side: domain.ReviewExcerptSideHead,
					StartLine: 10, EndLine: 11, Caption: "The head check comes first.",
					Text: &text,
				}},
			},
			{
				Title:     "The removed helper",
				Summary:   "The old helper is gone; nothing else called it.",
				Attention: domain.ReviewAttentionRisk,
				Excerpts: []domain.ReviewExcerpt{{
					Path: "daemon/internal/domain/old_helper.go", Side: domain.ReviewExcerptSideBase,
					StartLine: 1, EndLine: 20, Caption: "The helper before its removal.",
					UnresolvedReason: &missing,
				}},
			},
		},
		Questions: []string{"Should the span cap be per stop instead of per excerpt?"},
		Notes:     &notes,
	}
}

func TestReviewGuideValidateAcceptsFixtureAndEveryEnum(t *testing.T) {
	t.Parallel()
	if err := reviewGuideFixture("head").Validate(); err != nil {
		t.Fatalf("fixture guide = %v", err)
	}
	for _, attention := range domain.AllReviewAttentions {
		guide := reviewGuideFixture("head")
		guide.Stops[0].Attention = attention
		if err := guide.Validate(); err != nil {
			t.Errorf("attention %q = %v", attention, err)
		}
	}
	for _, side := range domain.AllReviewExcerptSides {
		guide := reviewGuideFixture("head")
		guide.Stops[0].Excerpts[0].Side = side
		if err := guide.Validate(); err != nil {
			t.Errorf("side %q = %v", side, err)
		}
	}
	for _, reason := range domain.AllReviewExcerptUnresolved {
		guide := reviewGuideFixture("head")
		guide.Stops[1].Excerpts[0].UnresolvedReason = &reason
		if err := guide.Validate(); err != nil {
			t.Errorf("unresolved reason %q = %v", reason, err)
		}
	}
	atCaps := reviewGuideFixture("head")
	atCaps.Stops[0].Title = strings.Repeat("é", domain.MaxReviewStopTitleRunes)
	atCaps.Stops[0].Excerpts[0].Path = strings.Repeat("a/", domain.MaxReviewExcerptPathRunes/2-1) + "bb"
	atCaps.Stops[0].Excerpts[0].EndLine = atCaps.Stops[0].Excerpts[0].StartLine + domain.MaxReviewExcerptLines - 1
	full := strings.Repeat("x", domain.MaxReviewExcerptTextBytes)
	atCaps.Stops[0].Excerpts[0].Text = &full
	atCaps.Questions = nil
	atCaps.Notes = nil
	atCaps.Stops[1].Excerpts = nil
	if err := atCaps.Validate(); err != nil {
		t.Fatalf("guide at its caps, without questions, notes, or excerpts = %v", err)
	}
}

func TestReviewGuideValidateRejects(t *testing.T) {
	t.Parallel()
	text := "line\n"
	reason := domain.ReviewExcerptTooLarge
	over := func(limit int) string { return strings.Repeat("é", limit+1) }
	excerpt := func(mutate func(*domain.ReviewExcerpt)) func(*domain.ReviewGuide) {
		return func(g *domain.ReviewGuide) { mutate(&g.Stops[0].Excerpts[0]) }
	}
	for name, mutate := range map[string]func(*domain.ReviewGuide){
		"no head":      func(g *domain.ReviewGuide) { g.HeadSHA = "" },
		"no claim":     func(g *domain.ReviewGuide) { g.ClaimDigest = "" },
		"no stops":     func(g *domain.ReviewGuide) { g.Stops = nil },
		"13 stops":     func(g *domain.ReviewGuide) { g.Stops = repeatStops(g.Stops[0], domain.MaxReviewGuideStops+1) },
		"5 excerpts":   func(g *domain.ReviewGuide) { g.Stops[0].Excerpts = repeatExcerpts(g.Stops[0].Excerpts[0], 5) },
		"9 questions":  func(g *domain.ReviewGuide) { g.Questions = make([]string, 9); fillStrings(g.Questions, "why?") },
		"empty title":  func(g *domain.ReviewGuide) { g.Stops[0].Title = "" },
		"long title":   func(g *domain.ReviewGuide) { g.Stops[0].Title = over(domain.MaxReviewStopTitleRunes) },
		"empty sum":    func(g *domain.ReviewGuide) { g.Stops[0].Summary = "" },
		"long summary": func(g *domain.ReviewGuide) { g.Stops[0].Summary = over(domain.MaxReviewStopSummaryRunes) },
		"attention":    func(g *domain.ReviewGuide) { g.Stops[0].Attention = "style" },
		"no attention": func(g *domain.ReviewGuide) { g.Stops[0].Attention = "" },
		"empty q":      func(g *domain.ReviewGuide) { g.Questions = []string{""} },
		"long q":       func(g *domain.ReviewGuide) { g.Questions = []string{over(domain.MaxReviewGuideQuestionRunes)} },
		"empty notes":  func(g *domain.ReviewGuide) { empty := ""; g.Notes = &empty },
		"long notes":   func(g *domain.ReviewGuide) { long := over(domain.MaxReviewGuideNotesRunes); g.Notes = &long },
		"side":         excerpt(func(e *domain.ReviewExcerpt) { e.Side = "merge" }),
		"no side":      excerpt(func(e *domain.ReviewExcerpt) { e.Side = "" }),
		"line 0":       excerpt(func(e *domain.ReviewExcerpt) { e.StartLine = 0 }),
		"reversed":     excerpt(func(e *domain.ReviewExcerpt) { e.StartLine, e.EndLine = 12, 11 }),
		"81 lines":     excerpt(func(e *domain.ReviewExcerpt) { e.StartLine, e.EndLine = 1, domain.MaxReviewExcerptLines+1 }),
		"empty path":   excerpt(func(e *domain.ReviewExcerpt) { e.Path = "" }),
		"absolute":     excerpt(func(e *domain.ReviewExcerpt) { e.Path = "/etc/passwd" }),
		"dot dot":      excerpt(func(e *domain.ReviewExcerpt) { e.Path = "daemon/../../secret" }),
		"control char": excerpt(func(e *domain.ReviewExcerpt) { e.Path = "daemon/a\tb.go" }),
		"long path": excerpt(func(e *domain.ReviewExcerpt) {
			e.Path = strings.Repeat("a/", domain.MaxReviewExcerptPathRunes/2) + "b"
		}),
		"empty cap":    excerpt(func(e *domain.ReviewExcerpt) { e.Caption = "" }),
		"long caption": excerpt(func(e *domain.ReviewExcerpt) { e.Caption = over(domain.MaxReviewExcerptCaptionRunes) }),
		"neither":      excerpt(func(e *domain.ReviewExcerpt) { e.Text = nil }),
		"both":         excerpt(func(e *domain.ReviewExcerpt) { e.Text, e.UnresolvedReason = &text, &reason }),
		"unknown reason": func(g *domain.ReviewGuide) {
			unknown := domain.ReviewExcerptUnresolved("permission_denied")
			g.Stops[1].Excerpts[0].UnresolvedReason = &unknown
		},
		"text over 8 KiB": excerpt(func(e *domain.ReviewExcerpt) {
			long := strings.Repeat("x", domain.MaxReviewExcerptTextBytes+1)
			e.Text = &long
		}),
		"text not UTF-8": excerpt(func(e *domain.ReviewExcerpt) { bad := "\xff"; e.Text = &bad }),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			guide := reviewGuideFixture("head")
			mutate(&guide)
			if err := guide.Validate(); !errors.Is(err, domain.ErrInvalidReviewGuide) {
				t.Fatalf("Validate() = %v, want ErrInvalidReviewGuide", err)
			}
		})
	}
}

func repeatStops(stop domain.ReviewStop, n int) []domain.ReviewStop {
	out := make([]domain.ReviewStop, n)
	for i := range out {
		out[i] = stop
	}
	return out
}

func repeatExcerpts(excerpt domain.ReviewExcerpt, n int) []domain.ReviewExcerpt {
	out := make([]domain.ReviewExcerpt, n)
	for i := range out {
		out[i] = excerpt
	}
	return out
}

func fillStrings(out []string, value string) {
	for i := range out {
		out[i] = value
	}
}

// readyReviewGuideInput is a valid ready item carrying the fixture guide and
// its claim.
func readyReviewGuideInput() domain.AttentionItemInput {
	in := readyDetailInput()
	in.AgentClaims = append(in.AgentClaims, reviewGuideClaimFixture(in.PRHeadSHA))
	guide := reviewGuideFixture(in.PRHeadSHA)
	in.ReviewGuide = &guide
	return in
}

func TestAttentionItemAcceptsReviewGuideOnReadyItem(t *testing.T) {
	t.Parallel()
	in := readyReviewGuideInput()
	item, err := domain.NewAttentionItem(in, nil)
	if err != nil {
		t.Fatalf("ready item with a guide = %v", err)
	}
	// The item owns its copy of the guide.
	*in.ReviewGuide.Stops[0].Excerpts[0].Text = "changed"
	if *item.ReviewGuide.Stops[0].Excerpts[0].Text == "changed" {
		t.Fatal("item aliases its input's excerpt text")
	}
}

func TestAttentionItemRejectsUnboundReviewGuide(t *testing.T) {
	t.Parallel()
	inlineText := domain.ClaimText{MediaType: domain.MediaTypeTextPlain, Content: "{}"}
	for name, tc := range map[string]struct {
		mutate func(*domain.AttentionItemInput)
		want   error
	}{
		"moved head": {
			func(in *domain.AttentionItemInput) { in.ReviewGuide.HeadSHA = "other" },
			domain.ErrCardFactInconsistent,
		},
		"invalid shape": {
			func(in *domain.AttentionItemInput) { in.ReviewGuide.Stops = nil },
			domain.ErrInvalidReviewGuide,
		},
		"claim missing": {
			func(in *domain.AttentionItemInput) { in.AgentClaims = in.AgentClaims[:len(in.AgentClaims)-1] },
			domain.ErrCardFactInconsistent,
		},
		"digest names no claim": {
			func(in *domain.AttentionItemInput) { in.ReviewGuide.ClaimDigest = "sha256:elsewhere" },
			domain.ErrCardFactInconsistent,
		},
		"duplicate claim": {
			func(in *domain.AttentionItemInput) {
				second := reviewGuideClaimFixture(in.PRHeadSHA)
				second.Label = "freeside.review-guide-copy"
				in.AgentClaims = append(in.AgentClaims, second)
			},
			domain.ErrCardFactInconsistent,
		},
		"second reserved claim": {
			func(in *domain.AttentionItemInput) {
				second := reviewGuideClaimFixture(in.PRHeadSHA)
				second.Artifact, second.Digest = "art-review-guide-2", "sha256:review-guide-2"
				in.AgentClaims = append(in.AgentClaims, second)
			},
			domain.ErrCardFactInconsistent,
		},
		"wrong label": {
			func(in *domain.AttentionItemInput) { in.AgentClaims[len(in.AgentClaims)-1].Label = "guide" },
			domain.ErrCardFactInconsistent,
		},
		"wrong media": {
			func(in *domain.AttentionItemInput) {
				in.AgentClaims[len(in.AgentClaims)-1].Metadata.MediaType = domain.EvidenceMediaTextMarkdown
			},
			domain.ErrCardFactInconsistent,
		},
		"inline text": {
			func(in *domain.AttentionItemInput) {
				claim := &in.AgentClaims[len(in.AgentClaims)-1]
				claim.Text = &inlineText
				claim.Digest = inlineText.ComputeDigest()
				in.ReviewGuide.ClaimDigest = claim.Digest
			},
			domain.ErrCardFactInconsistent,
		},
		"claim at another head": {
			func(in *domain.AttentionItemInput) {
				in.AgentClaims[len(in.AgentClaims)-1].Provenance.SourceHeadSHA = "other"
			},
			domain.ErrCardFactInconsistent,
		},
		"high-sensitivity claim": {
			func(in *domain.AttentionItemInput) {
				in.AgentClaims[len(in.AgentClaims)-1].Provenance.SensitivityClass = domain.SensitivityHigh
			},
			domain.ErrCardFactInconsistent,
		},
		"head independent": {
			func(in *domain.AttentionItemInput) {
				claim := &in.AgentClaims[len(in.AgentClaims)-1]
				claim.Provenance.HeadBinding = domain.HeadIndependent
				claim.Provenance.SourceHeadSHA = ""
			},
			domain.ErrCardFactInconsistent,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := readyReviewGuideInput()
			tc.mutate(&in)
			if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, tc.want) {
				t.Fatalf("NewAttentionItem() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAttentionItemRejectsReviewGuideOutsideReadyItem(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, typ := range domain.AllAttentionTypes {
		if typ == domain.AttentionReadyForFinalReview {
			continue
		}
		in := validItemInput(typ)
		if in.PRHeadSHA == "" {
			in.PRHeadSHA = "head"
		}
		in.AgentClaims = append(in.AgentClaims, reviewGuideClaimFixture(in.PRHeadSHA))
		if _, err := domain.NewAttentionItem(in, nil); err != nil {
			// The base input is not valid for this type with a head and a
			// claim; the guide's own check is covered by the types that are.
			continue
		}
		guide := reviewGuideFixture(in.PRHeadSHA)
		in.ReviewGuide = &guide
		if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, domain.ErrCardFactOutsideItem) {
			t.Errorf("type %q with a guide = %v, want ErrCardFactOutsideItem", typ, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no non-ready type accepted the base input; the test checked nothing")
	}
}

// trimmedReviewGuideNotice is a notice for the fixture guide: its stop count
// trimmed from 13 and its first stop's first excerpt from 116 lines.
func trimmedReviewGuideNotice() domain.ReviewGuideNotice {
	return domain.ReviewGuideNotice{Trims: []domain.ReviewGuideTrim{
		{Cap: domain.ReviewGuideCapStops, Limit: domain.MaxReviewGuideStops, Found: 13},
		{
			Cap: domain.ReviewGuideCapExcerptLines, Stop: ptr(0), Excerpt: ptr(0),
			Limit: domain.MaxReviewExcerptLines, Found: 116,
		},
	}}
}

func TestReviewGuideNoticeValidate(t *testing.T) {
	t.Parallel()
	omitted := domain.ReviewGuideOmissionFileTooLarge
	unknownOmission := domain.ReviewGuideOmission("lost")
	for _, c := range domain.AllReviewGuideCaps {
		trim := domain.ReviewGuideTrim{Cap: c, Limit: 1, Found: 2}
		switch c {
		case domain.ReviewGuideCapStops, domain.ReviewGuideCapQuestions, domain.ReviewGuideCapNotes:
		case domain.ReviewGuideCapStopExcerpts, domain.ReviewGuideCapTitle, domain.ReviewGuideCapSummary:
			trim.Stop = ptr(0)
		case domain.ReviewGuideCapExcerptLines, domain.ReviewGuideCapCaption:
			trim.Stop, trim.Excerpt = ptr(0), ptr(0)
		case domain.ReviewGuideCapQuestion:
			trim.Question = ptr(0)
		}
		if err := (domain.ReviewGuideNotice{Trims: []domain.ReviewGuideTrim{trim}}).Validate(); err != nil {
			t.Errorf("cap %q at its place = %v", c, err)
		}
	}
	if err := (domain.ReviewGuideNotice{Omission: &omitted}).Validate(); err != nil {
		t.Errorf("omission = %v", err)
	}
	for name, notice := range map[string]domain.ReviewGuideNotice{
		"empty":              {},
		"empty trims":        {Trims: []domain.ReviewGuideTrim{}},
		"omission and trims": {Omission: &omitted, Trims: trimmedReviewGuideNotice().Trims},
		"unknown omission":   {Omission: &unknownOmission},
		"unknown cap":        {Trims: []domain.ReviewGuideTrim{{Cap: "length", Limit: 1, Found: 2}}},
		"count cap with a stop": {Trims: []domain.ReviewGuideTrim{
			{Cap: domain.ReviewGuideCapStops, Stop: ptr(0), Limit: 12, Found: 13},
		}},
		"excerpt cap without an excerpt": {Trims: []domain.ReviewGuideTrim{
			{Cap: domain.ReviewGuideCapExcerptLines, Stop: ptr(0), Limit: 80, Found: 90},
		}},
		"question cap without a question": {Trims: []domain.ReviewGuideTrim{
			{Cap: domain.ReviewGuideCapQuestion, Limit: 500, Found: 501},
		}},
		"negative index": {Trims: []domain.ReviewGuideTrim{
			{Cap: domain.ReviewGuideCapTitle, Stop: ptr(-1), Limit: 120, Found: 121},
		}},
		"found at the limit": {Trims: []domain.ReviewGuideTrim{
			{Cap: domain.ReviewGuideCapStops, Limit: 12, Found: 12},
		}},
		"zero limit": {Trims: []domain.ReviewGuideTrim{
			{Cap: domain.ReviewGuideCapStops, Limit: 0, Found: 1},
		}},
	} {
		if err := notice.Validate(); !errors.Is(err, domain.ErrInvalidReviewGuide) {
			t.Errorf("%s = %v, want ErrInvalidReviewGuide", name, err)
		}
	}
}

func TestAttentionItemAcceptsReviewGuideNoticeOnReadyItem(t *testing.T) {
	t.Parallel()
	in := readyReviewGuideInput()
	notice := trimmedReviewGuideNotice()
	in.ReviewGuideNotice = &notice
	item, err := domain.NewAttentionItem(in, nil)
	if err != nil {
		t.Fatalf("ready item with trims = %v", err)
	}
	// The item owns its copy of the notice.
	*in.ReviewGuideNotice.Trims[1].Stop = 1
	if *item.ReviewGuideNotice.Trims[1].Stop != 0 {
		t.Fatal("item aliases its input's trim index")
	}

	omitted := readyReviewGuideInput()
	omitted.ReviewGuide = nil
	omitted.AgentClaims = omitted.AgentClaims[:len(omitted.AgentClaims)-1]
	reason := domain.ReviewGuideOmissionFileTooLarge
	omitted.ReviewGuideNotice = &domain.ReviewGuideNotice{Omission: &reason, Trims: []domain.ReviewGuideTrim{}}
	if _, err := domain.NewAttentionItem(omitted, nil); err != nil {
		t.Fatalf("ready item with an omission = %v", err)
	}
}

func TestAttentionItemRejectsUnboundReviewGuideNotice(t *testing.T) {
	t.Parallel()
	reason := domain.ReviewGuideOmissionFileTooLarge
	for name, tc := range map[string]struct {
		mutate func(*domain.AttentionItemInput)
		want   error
	}{
		"omission beside a guide": {
			func(in *domain.AttentionItemInput) {
				in.ReviewGuideNotice = &domain.ReviewGuideNotice{Omission: &reason}
			},
			domain.ErrCardFactInconsistent,
		},
		"trims beside no guide": {
			func(in *domain.AttentionItemInput) {
				in.ReviewGuide = nil
				in.AgentClaims = in.AgentClaims[:len(in.AgentClaims)-1]
			},
			domain.ErrCardFactInconsistent,
		},
		"stop the guide lacks": {
			func(in *domain.AttentionItemInput) { *in.ReviewGuideNotice.Trims[1].Stop = 2 },
			domain.ErrCardFactInconsistent,
		},
		"excerpt the stop lacks": {
			func(in *domain.AttentionItemInput) { *in.ReviewGuideNotice.Trims[1].Excerpt = 1 },
			domain.ErrCardFactInconsistent,
		},
		"question the guide lacks": {
			func(in *domain.AttentionItemInput) {
				in.ReviewGuideNotice.Trims = append(in.ReviewGuideNotice.Trims, domain.ReviewGuideTrim{
					Cap: domain.ReviewGuideCapQuestion, Question: ptr(1), Limit: 500, Found: 501,
				})
			},
			domain.ErrCardFactInconsistent,
		},
		"invalid shape": {
			func(in *domain.AttentionItemInput) { in.ReviewGuideNotice.Trims[0].Found = 12 },
			domain.ErrInvalidReviewGuide,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			in := readyReviewGuideInput()
			notice := trimmedReviewGuideNotice()
			in.ReviewGuideNotice = &notice
			tc.mutate(&in)
			if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, tc.want) {
				t.Fatalf("NewAttentionItem() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestAttentionItemRejectsReviewGuideNoticeOutsideReadyItem(t *testing.T) {
	t.Parallel()
	reason := domain.ReviewGuideOmissionFileTooLarge
	checked := 0
	for _, typ := range domain.AllAttentionTypes {
		if typ == domain.AttentionReadyForFinalReview {
			continue
		}
		in := validItemInput(typ)
		if _, err := domain.NewAttentionItem(in, nil); err != nil {
			continue
		}
		in.ReviewGuideNotice = &domain.ReviewGuideNotice{Omission: &reason}
		if _, err := domain.NewAttentionItem(in, nil); !errors.Is(err, domain.ErrCardFactOutsideItem) {
			t.Errorf("type %q with a notice = %v, want ErrCardFactOutsideItem", typ, err)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no non-ready type accepted the base input; the test checked nothing")
	}
}
