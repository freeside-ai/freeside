package domain

import (
	"fmt"
	"slices"
	"unicode/utf8"
)

// ReviewGuideClaimLabel labels the agent claim a review guide is parsed from
// (plan §7). It lives here, not only in export, because
// AttentionItem.Validate has to find that claim and domain does not import
// export; export.ReviewGuideEvidenceLabel carries the same string, and an
// export test pins the two together.
const ReviewGuideClaimLabel = "freeside.review-guide"

// Review guide caps. They bound what the agent can put on a ready card, so
// the item blob stays small enough to resend on every list and bootstrap
// read (plan §9). String caps count Unicode code points, as the schema's
// maxLength does; MaxReviewExcerptTextBytes counts UTF-8 bytes, because it
// bounds the daemon-read file text rather than agent prose.
const (
	MaxReviewGuideStops          = 12
	MaxReviewStopExcerpts        = 4
	MaxReviewGuideQuestions      = 8
	MaxReviewExcerptLines        = 80
	MaxReviewExcerptTextBytes    = 8 << 10
	MaxReviewStopTitleRunes      = 120
	MaxReviewStopSummaryRunes    = 2000
	MaxReviewExcerptCaptionRunes = 300
	MaxReviewExcerptPathRunes    = 1024
	MaxReviewGuideQuestionRunes  = 500
	MaxReviewGuideNotesRunes     = 2000
)

// ReviewAttention names what a review stop asks the reviewer to weigh. The
// zero value is invalid.
type ReviewAttention string

const (
	ReviewAttentionTradeoff      ReviewAttention = "tradeoff"
	ReviewAttentionContract      ReviewAttention = "contract"
	ReviewAttentionProduct       ReviewAttention = "product"
	ReviewAttentionRisk          ReviewAttention = "risk"
	ReviewAttentionAccessibility ReviewAttention = "accessibility"
)

// AllReviewAttentions is the single registration point for review attentions.
var AllReviewAttentions = []ReviewAttention{
	ReviewAttentionTradeoff,
	ReviewAttentionContract,
	ReviewAttentionProduct,
	ReviewAttentionRisk,
	ReviewAttentionAccessibility,
}

func (a ReviewAttention) valid() bool {
	switch a {
	case ReviewAttentionTradeoff, ReviewAttentionContract, ReviewAttentionProduct,
		ReviewAttentionRisk, ReviewAttentionAccessibility:
		return true
	default:
		return false
	}
}

// ReviewExcerptSide names the commit an excerpt reads: head is the imported
// commit, base its bound base. The zero value is invalid.
type ReviewExcerptSide string

const (
	ReviewExcerptSideHead ReviewExcerptSide = "head"
	ReviewExcerptSideBase ReviewExcerptSide = "base"
)

// AllReviewExcerptSides is the single registration point for excerpt sides.
var AllReviewExcerptSides = []ReviewExcerptSide{
	ReviewExcerptSideHead,
	ReviewExcerptSideBase,
}

func (s ReviewExcerptSide) valid() bool {
	switch s {
	case ReviewExcerptSideHead, ReviewExcerptSideBase:
		return true
	default:
		return false
	}
}

// ReviewExcerptUnresolved says why the daemon could not read an excerpt's
// text. The zero value is invalid.
type ReviewExcerptUnresolved string

const (
	// ReviewExcerptPathMissing: no file at the path on the excerpt's side.
	ReviewExcerptPathMissing ReviewExcerptUnresolved = "path_missing"
	// ReviewExcerptRangeOutOfFile: the line range runs past the file's end.
	ReviewExcerptRangeOutOfFile ReviewExcerptUnresolved = "range_out_of_file"
	// ReviewExcerptBinary: the file is not text.
	ReviewExcerptBinary ReviewExcerptUnresolved = "binary"
	// ReviewExcerptTooLarge: the range's text exceeds
	// MaxReviewExcerptTextBytes.
	ReviewExcerptTooLarge ReviewExcerptUnresolved = "too_large"
)

// AllReviewExcerptUnresolved is the single registration point for unresolved
// excerpt reasons.
var AllReviewExcerptUnresolved = []ReviewExcerptUnresolved{
	ReviewExcerptPathMissing,
	ReviewExcerptRangeOutOfFile,
	ReviewExcerptBinary,
	ReviewExcerptTooLarge,
}

func (r ReviewExcerptUnresolved) valid() bool {
	switch r {
	case ReviewExcerptPathMissing, ReviewExcerptRangeOutOfFile,
		ReviewExcerptBinary, ReviewExcerptTooLarge:
		return true
	default:
		return false
	}
}

// ReviewGuide is a ready_for_final_review item's reading order for its pull
// request (plan §7). The stops, captions, questions, and notes are the
// agent's claim, parsed from the agent_claims entry ClaimDigest names; each
// excerpt's Text is a daemon fact read from HeadSHA or its bound base. The
// agent-written file is this shape without HeadSHA, ClaimDigest, and each
// excerpt's Text and UnresolvedReason (api/openapi.yaml, ReviewGuide).
//
// Validate checks the guide's own shape. AttentionItem.Validate checks its
// binding to the item: the type, HeadSHA against PRHeadSHA, and the claim.
// Questions is never nil on the wire, so an empty list has one encoding
// ([]); the sync projection normalizes it with the stops' Excerpts.
type ReviewGuide struct {
	HeadSHA     string       `json:"head_sha"`
	ClaimDigest Digest       `json:"claim_digest"`
	Stops       []ReviewStop `json:"stops"`
	Questions   []string     `json:"questions"`
	Notes       *string      `json:"notes"`
}

// ReviewStop is one stop in a review guide, in reading order.
type ReviewStop struct {
	Title     string          `json:"title"`
	Summary   string          `json:"summary"`
	Attention ReviewAttention `json:"attention"`
	Excerpts  []ReviewExcerpt `json:"excerpts"`
}

// ReviewExcerpt is a 1-based, inclusive line range a stop points at. Exactly
// one of Text and UnresolvedReason is set: Text when the daemon read the
// range, UnresolvedReason when it could not.
type ReviewExcerpt struct {
	Path             string                   `json:"path"`
	Side             ReviewExcerptSide        `json:"side"`
	StartLine        int                      `json:"start_line"`
	EndLine          int                      `json:"end_line"`
	Caption          string                   `json:"caption"`
	Text             *string                  `json:"text"`
	UnresolvedReason *ReviewExcerptUnresolved `json:"unresolved_reason"`
}

// Validate reports whether the guide's shape is within the contract's caps.
func (g ReviewGuide) Validate() error {
	if g.HeadSHA == "" {
		return fmt.Errorf("review guide head_sha: %w", ErrInvalidReviewGuide)
	}
	if g.ClaimDigest == "" {
		return fmt.Errorf("review guide claim_digest: %w", ErrInvalidReviewGuide)
	}
	if n := len(g.Stops); n == 0 || n > MaxReviewGuideStops {
		return fmt.Errorf("review guide has %d stops, want 1 to %d: %w",
			n, MaxReviewGuideStops, ErrInvalidReviewGuide)
	}
	for i, stop := range g.Stops {
		if err := stop.Validate(); err != nil {
			return fmt.Errorf("review guide stop %d: %w", i, err)
		}
	}
	if n := len(g.Questions); n > MaxReviewGuideQuestions {
		return fmt.Errorf("review guide has %d questions, limit %d: %w",
			n, MaxReviewGuideQuestions, ErrInvalidReviewGuide)
	}
	for i, q := range g.Questions {
		if err := checkReviewGuideText("question", q, MaxReviewGuideQuestionRunes); err != nil {
			return fmt.Errorf("review guide question %d: %w", i, err)
		}
	}
	if g.Notes != nil {
		if err := checkReviewGuideText("notes", *g.Notes, MaxReviewGuideNotesRunes); err != nil {
			return fmt.Errorf("review guide: %w", err)
		}
	}
	return nil
}

// Validate reports whether the stop's shape is within the contract's caps.
func (s ReviewStop) Validate() error {
	if err := checkReviewGuideText("title", s.Title, MaxReviewStopTitleRunes); err != nil {
		return err
	}
	if err := checkReviewGuideText("summary", s.Summary, MaxReviewStopSummaryRunes); err != nil {
		return err
	}
	if !s.Attention.valid() {
		return fmt.Errorf("attention %q: %w", s.Attention, ErrInvalidReviewGuide)
	}
	if n := len(s.Excerpts); n > MaxReviewStopExcerpts {
		return fmt.Errorf("%d excerpts, limit %d: %w", n, MaxReviewStopExcerpts, ErrInvalidReviewGuide)
	}
	for i, excerpt := range s.Excerpts {
		if err := excerpt.Validate(); err != nil {
			return fmt.Errorf("excerpt %d: %w", i, err)
		}
	}
	return nil
}

// Validate reports whether the excerpt's range, caption, and resolution are
// well-formed.
func (e ReviewExcerpt) Validate() error {
	if n := utf8.RuneCountInString(e.Path); n > MaxReviewExcerptPathRunes {
		return fmt.Errorf("path is %d characters, limit %d: %w", n, MaxReviewExcerptPathRunes, ErrInvalidReviewGuide)
	}
	if !canonicalRepoPath(e.Path) {
		return fmt.Errorf("path %q is not a canonical repository path: %w", e.Path, ErrInvalidReviewGuide)
	}
	if !e.Side.valid() {
		return fmt.Errorf("side %q: %w", e.Side, ErrInvalidReviewGuide)
	}
	if e.StartLine < 1 || e.StartLine > e.EndLine {
		return fmt.Errorf("line range %d-%d: %w", e.StartLine, e.EndLine, ErrInvalidReviewGuide)
	}
	if span := e.EndLine - e.StartLine + 1; span > MaxReviewExcerptLines {
		return fmt.Errorf("line range %d-%d spans %d lines, limit %d: %w",
			e.StartLine, e.EndLine, span, MaxReviewExcerptLines, ErrInvalidReviewGuide)
	}
	if err := checkReviewGuideText("caption", e.Caption, MaxReviewExcerptCaptionRunes); err != nil {
		return err
	}
	switch {
	case e.Text == nil && e.UnresolvedReason == nil:
		return fmt.Errorf("excerpt has neither text nor unresolved_reason: %w", ErrInvalidReviewGuide)
	case e.Text != nil && e.UnresolvedReason != nil:
		return fmt.Errorf("excerpt has both text and unresolved_reason: %w", ErrInvalidReviewGuide)
	case e.Text != nil:
		// An empty range text is legal: the range may cover empty lines.
		if !utf8.ValidString(*e.Text) {
			return fmt.Errorf("excerpt text is not UTF-8: %w", ErrInvalidReviewGuide)
		}
		if n := len(*e.Text); n > MaxReviewExcerptTextBytes {
			return fmt.Errorf("excerpt text is %d bytes, limit %d: %w",
				n, MaxReviewExcerptTextBytes, ErrInvalidReviewGuide)
		}
	default:
		if !e.UnresolvedReason.valid() {
			return fmt.Errorf("unresolved_reason %q: %w", *e.UnresolvedReason, ErrInvalidReviewGuide)
		}
	}
	return nil
}

// validateReviewGuide binds a present guide to its item: only a
// ready_for_final_review item carries one, the guide was written against the
// item's head, and ClaimDigest names exactly one agent claim, the reserved
// application/json claim the guide was parsed from. That claim keeps no
// inline text (only the reserved summary claim does) and is head-bound to the
// same head, so the guide's provenance stays on the claim. The guide carries
// the claim's prose inline on the item, which clients cache on disk, so the
// claim may not be high-sensitivity, as AgentClaim.Validate requires of
// inline claim text.
func (i AttentionItem) validateReviewGuide() error {
	g := i.ReviewGuide
	if i.Type != AttentionReadyForFinalReview {
		return fmt.Errorf("item %s type %q carries a review guide: %w",
			i.ID, i.Type, ErrCardFactOutsideItem)
	}
	if err := g.Validate(); err != nil {
		return fmt.Errorf("item %s review_guide: %w", i.ID, err)
	}
	if g.HeadSHA != i.PRHeadSHA {
		return fmt.Errorf("item %s review guide head %q, want pr_head_sha %q: %w",
			i.ID, g.HeadSHA, i.PRHeadSHA, ErrCardFactInconsistent)
	}
	var claim AgentClaim
	named, labeled := 0, 0
	for _, c := range i.AgentClaims {
		if c.Digest == g.ClaimDigest {
			named++
			claim = c
		}
		if c.Label == ReviewGuideClaimLabel {
			labeled++
		}
	}
	if named != 1 || labeled != 1 || claim.Label != ReviewGuideClaimLabel {
		return fmt.Errorf("item %s review guide claim %s names %d claims and %d carry label %q, want one: %w",
			i.ID, g.ClaimDigest, named, labeled, ReviewGuideClaimLabel, ErrCardFactInconsistent)
	}
	if claim.Metadata.MediaType != EvidenceMediaApplicationJSON || claim.Text != nil {
		return fmt.Errorf("item %s review guide claim has media type %q and inline text %t, want %q without text: %w",
			i.ID, claim.Metadata.MediaType, claim.Text != nil, EvidenceMediaApplicationJSON, ErrCardFactInconsistent)
	}
	if claim.Provenance.HeadBinding != HeadBound || claim.Provenance.SourceHeadSHA != g.HeadSHA {
		return fmt.Errorf("item %s review guide claim provenance %q at %q, want head_bound at %q: %w",
			i.ID, claim.Provenance.HeadBinding, claim.Provenance.SourceHeadSHA, g.HeadSHA, ErrCardFactInconsistent)
	}
	if claim.Provenance.SensitivityClass == SensitivityHigh {
		return fmt.Errorf("item %s review guide claim is %q, but the guide is inline: %w",
			i.ID, claim.Provenance.SensitivityClass, ErrCardFactInconsistent)
	}
	return nil
}

// checkReviewGuideText rejects an empty string or one over limit code points.
func checkReviewGuideText(field, value string, limit int) error {
	if value == "" {
		return fmt.Errorf("%s: %w", field, ErrInvalidReviewGuide)
	}
	if n := utf8.RuneCountInString(value); n > limit {
		return fmt.Errorf("%s is %d characters, limit %d: %w", field, n, limit, ErrInvalidReviewGuide)
	}
	return nil
}

// cloneReviewGuide deep-copies a guide so an item never aliases its caller's
// slices or pointers.
func cloneReviewGuide(in *ReviewGuide) *ReviewGuide {
	if in == nil {
		return nil
	}
	out := *in
	out.Questions = slices.Clone(in.Questions)
	out.Notes = clonePtr(in.Notes)
	if in.Stops != nil {
		out.Stops = make([]ReviewStop, len(in.Stops))
		for i, stop := range in.Stops {
			stop.Excerpts = slices.Clone(stop.Excerpts)
			for j := range stop.Excerpts {
				stop.Excerpts[j].Text = clonePtr(stop.Excerpts[j].Text)
				stop.Excerpts[j].UnresolvedReason = clonePtr(stop.Excerpts[j].UnresolvedReason)
			}
			out.Stops[i] = stop
		}
	}
	return &out
}
