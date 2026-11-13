package importer

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// MaxReviewGuideBytes caps the agent-written review guide file. The domain
// caps bound the decoded guide; this bounds the bytes read before decoding.
const MaxReviewGuideBytes = 64 << 10

// ReviewGuideOmitted reports whether the import leaves e out as a review
// guide over MaxReviewGuideBytes. The stage pipeline still stores the
// blob, which a replay re-imports, but returns no artifact for it, so no
// reply attaches it and no client names it. The live and recovery paths
// both derive their artifact lists through this one rule, so they agree.
func ReviewGuideOmitted(e export.EvidenceEntry) bool {
	return e.Label == export.ReviewGuideEvidenceLabel && e.Size > MaxReviewGuideBytes
}

// The agent-written review guide (api/openapi.yaml ReviewGuide): the domain
// shape without the daemon-filled head_sha, claim_digest, and each excerpt's
// text and unresolved_reason, which strict decoding therefore rejects as
// unknown fields. Every key is required. A missing string or line number
// decodes to a zero value that domain validation rejects, so only the keys
// whose zero value is legal (excerpts, questions, notes) are decoded through
// a type that tells absence apart.
type reviewGuideFile struct {
	Stops     []reviewGuideFileStop `json:"stops"`
	Questions *[]string             `json:"questions"`
	// Notes is required but nullable; json.RawMessage holds "null" for an
	// explicit null and stays nil only when the key is absent.
	Notes json.RawMessage `json:"notes"`
}

type reviewGuideFileStop struct {
	Title     string                    `json:"title"`
	Summary   string                    `json:"summary"`
	Attention domain.ReviewAttention    `json:"attention"`
	Excerpts  *[]reviewGuideFileExcerpt `json:"excerpts"`
}

type reviewGuideFileExcerpt struct {
	Path      string                   `json:"path"`
	Side      domain.ReviewExcerptSide `json:"side"`
	StartLine int                      `json:"start_line"`
	EndLine   int                      `json:"end_line"`
	Caption   string                   `json:"caption"`
}

// DecodeReviewGuide decodes and validates an agent-written review guide and
// trims it to the domain caps. It returns the trimmed guide with HeadSHA,
// ClaimDigest, and every excerpt's Text and UnresolvedReason empty (daemon
// facts the caller fills), and one trim per cap the agent broke, in reading
// order. The body is untrusted. A shape fault (over MaxReviewGuideBytes,
// invalid UTF-8, an unknown or missing key, an empty string, an invalid
// attention or side, a non-canonical or over-long path, a malformed line
// range, or no stops) wraps ErrEvidenceInvalid, including one inside a stop,
// excerpt, or question a count cap drops, so trimming never admits a
// malformed guide. Only size caps trim. The importer and the engine both
// call it on the same bytes, so they derive the same guide.
func DecodeReviewGuide(body []byte) (domain.ReviewGuide, []domain.ReviewGuideTrim, error) {
	_, trimmed, trims, err := decodeReviewGuide(body)
	return trimmed, trims, err
}

// decodeReviewGuide is DecodeReviewGuide that also returns the guide as the
// agent wrote it, for the secret scan to read what a trim cuts.
func decodeReviewGuide(body []byte) (written, trimmed domain.ReviewGuide, trims []domain.ReviewGuideTrim, err error) {
	written, err = decodeWrittenReviewGuide(body)
	if err != nil {
		return domain.ReviewGuide{}, domain.ReviewGuide{}, nil, err
	}
	trimmed, trims, err = trimReviewGuide(written)
	if err != nil {
		return domain.ReviewGuide{}, domain.ReviewGuide{}, nil, fmt.Errorf("%w: %w", ErrEvidenceInvalid, err)
	}
	// The backstop: anything trimming left over a cap, and any fault the
	// per-entry checks do not cover (no stops, empty notes), fails here.
	if err := validateUnresolvedReviewGuide(trimmed); err != nil {
		return domain.ReviewGuide{}, domain.ReviewGuide{}, nil, fmt.Errorf("%w: %w", ErrEvidenceInvalid, err)
	}
	return written, trimmed, trims, nil
}

// decodeWrittenReviewGuide strictly decodes the file into the domain shape,
// untrimmed and unvalidated beyond its keys.
func decodeWrittenReviewGuide(body []byte) (domain.ReviewGuide, error) {
	var file reviewGuideFile
	if err := strictjson.Decode(
		body, &file, strictjson.RejectInvalidUTF8, strictjson.Limit(MaxReviewGuideBytes),
	); err != nil {
		return domain.ReviewGuide{}, fmt.Errorf("decode review guide: %w: %w", ErrEvidenceInvalid, err)
	}
	if file.Questions == nil {
		return domain.ReviewGuide{}, fmt.Errorf("review guide questions is missing or null: %w", ErrEvidenceInvalid)
	}
	if file.Notes == nil {
		return domain.ReviewGuide{}, fmt.Errorf("review guide notes is missing: %w", ErrEvidenceInvalid)
	}
	var notes *string
	if err := json.Unmarshal(file.Notes, &notes); err != nil {
		return domain.ReviewGuide{}, fmt.Errorf("decode review guide notes: %w: %w", ErrEvidenceInvalid, err)
	}
	guide := domain.ReviewGuide{Questions: *file.Questions, Notes: notes}
	if file.Stops != nil {
		guide.Stops = make([]domain.ReviewStop, 0, len(file.Stops))
	}
	for i, stop := range file.Stops {
		if stop.Excerpts == nil {
			return domain.ReviewGuide{}, fmt.Errorf("review guide stop %d excerpts is missing or null: %w",
				i, ErrEvidenceInvalid)
		}
		excerpts := make([]domain.ReviewExcerpt, 0, len(*stop.Excerpts))
		for _, e := range *stop.Excerpts {
			excerpts = append(excerpts, domain.ReviewExcerpt{
				Path: e.Path, Side: e.Side, StartLine: e.StartLine, EndLine: e.EndLine, Caption: e.Caption,
			})
		}
		guide.Stops = append(guide.Stops, domain.ReviewStop{
			Title: stop.Title, Summary: stop.Summary, Attention: stop.Attention, Excerpts: excerpts,
		})
	}
	return guide, nil
}

// trimReviewGuide cuts a decoded guide down to the domain size caps, keeping
// the agent's order: the first stops, excerpts, and questions up to their
// count caps, the first lines of an over-long range, and the start of an
// over-long string, ending in an ellipsis. Every stop, excerpt, and question
// is shape-checked first, including those a count cap drops. A trim is
// recorded only at a place the trimmed guide keeps; a dropped entry's own
// breaches are covered by the count trim that dropped it. The input is not
// modified.
func trimReviewGuide(written domain.ReviewGuide) (domain.ReviewGuide, []domain.ReviewGuideTrim, error) {
	var trims []domain.ReviewGuideTrim
	record := func(trim domain.ReviewGuideTrim, kept bool) {
		if kept {
			trims = append(trims, trim)
		}
	}
	guide := written
	if n := len(written.Stops); n > domain.MaxReviewGuideStops {
		record(domain.ReviewGuideTrim{
			Cap: domain.ReviewGuideCapStops, Limit: domain.MaxReviewGuideStops, Found: n,
		}, true)
	}
	guide.Stops = make([]domain.ReviewStop, 0, min(len(written.Stops), domain.MaxReviewGuideStops))
	for i, stop := range written.Stops {
		keptStop := i < domain.MaxReviewGuideStops
		at := func(trim domain.ReviewGuideTrim) domain.ReviewGuideTrim {
			index := i
			trim.Stop = &index
			return trim
		}
		var trim *domain.ReviewGuideTrim
		if stop.Title, trim = trimReviewGuideText(
			domain.ReviewGuideCapTitle, stop.Title, domain.MaxReviewStopTitleRunes); trim != nil {
			record(at(*trim), keptStop)
		}
		if stop.Summary, trim = trimReviewGuideText(
			domain.ReviewGuideCapSummary, stop.Summary, domain.MaxReviewStopSummaryRunes); trim != nil {
			record(at(*trim), keptStop)
		}
		if n := len(stop.Excerpts); n > domain.MaxReviewStopExcerpts {
			record(at(domain.ReviewGuideTrim{
				Cap: domain.ReviewGuideCapStopExcerpts, Limit: domain.MaxReviewStopExcerpts, Found: n,
			}), keptStop)
		}
		excerpts := make([]domain.ReviewExcerpt, 0, min(len(stop.Excerpts), domain.MaxReviewStopExcerpts))
		for j, excerpt := range stop.Excerpts {
			kept := keptStop && j < domain.MaxReviewStopExcerpts
			atExcerpt := func(trim domain.ReviewGuideTrim) domain.ReviewGuideTrim {
				trim = at(trim)
				index := j
				trim.Excerpt = &index
				return trim
			}
			// Only a well-formed range is shortened: a malformed one is a shape
			// fault that the check below must still see.
			if span := excerpt.EndLine - excerpt.StartLine + 1; excerpt.StartLine >= 1 &&
				excerpt.StartLine <= excerpt.EndLine && span > domain.MaxReviewExcerptLines {
				excerpt.EndLine = excerpt.StartLine + domain.MaxReviewExcerptLines - 1
				record(atExcerpt(domain.ReviewGuideTrim{
					Cap: domain.ReviewGuideCapExcerptLines, Limit: domain.MaxReviewExcerptLines, Found: span,
				}), kept)
			}
			if excerpt.Caption, trim = trimReviewGuideText(
				domain.ReviewGuideCapCaption, excerpt.Caption, domain.MaxReviewExcerptCaptionRunes); trim != nil {
				record(atExcerpt(*trim), kept)
			}
			if err := validateUnresolvedExcerpt(excerpt); err != nil {
				return domain.ReviewGuide{}, nil, fmt.Errorf("review guide stop %d: excerpt %d: %w", i, j, err)
			}
			if j < domain.MaxReviewStopExcerpts {
				excerpts = append(excerpts, excerpt)
			}
		}
		stop.Excerpts = excerpts
		// The excerpts were checked one by one above; this checks the stop's
		// own title, summary, and attention.
		check := stop
		check.Excerpts = nil
		if err := check.Validate(); err != nil {
			return domain.ReviewGuide{}, nil, fmt.Errorf("review guide stop %d: %w", i, err)
		}
		if keptStop {
			guide.Stops = append(guide.Stops, stop)
		}
	}
	if n := len(written.Questions); n > domain.MaxReviewGuideQuestions {
		record(domain.ReviewGuideTrim{
			Cap: domain.ReviewGuideCapQuestions, Limit: domain.MaxReviewGuideQuestions, Found: n,
		}, true)
	}
	guide.Questions = make([]string, 0, min(len(written.Questions), domain.MaxReviewGuideQuestions))
	for i, question := range written.Questions {
		if question == "" {
			return domain.ReviewGuide{}, nil, fmt.Errorf("review guide question %d is empty: %w",
				i, domain.ErrInvalidReviewGuide)
		}
		kept := i < domain.MaxReviewGuideQuestions
		cut, trim := trimReviewGuideText(domain.ReviewGuideCapQuestion, question, domain.MaxReviewGuideQuestionRunes)
		if trim != nil {
			index := i
			trim.Question = &index
			record(*trim, kept)
		}
		if kept {
			guide.Questions = append(guide.Questions, cut)
		}
	}
	if written.Notes != nil {
		notes, trim := trimReviewGuideText(domain.ReviewGuideCapNotes, *written.Notes, domain.MaxReviewGuideNotesRunes)
		guide.Notes = &notes
		if trim != nil {
			record(*trim, true)
		}
	}
	return guide, trims, nil
}

// trimReviewGuideText cuts value to limit code points, the last of them an
// ellipsis, and returns the trim it made, or nil when value fits.
func trimReviewGuideText(kind domain.ReviewGuideCap, value string, limit int) (string, *domain.ReviewGuideTrim) {
	n := utf8.RuneCountInString(value)
	if n <= limit {
		return value, nil
	}
	runes := []rune(value)
	return string(runes[:limit-1]) + "…", &domain.ReviewGuideTrim{Cap: kind, Limit: limit, Found: n}
}

// validateUnresolvedExcerpt runs domain validation on an excerpt whose
// daemon-filled text holds a placeholder, as validateUnresolvedReviewGuide
// does for the whole guide.
func validateUnresolvedExcerpt(excerpt domain.ReviewExcerpt) error {
	placeholder := ""
	excerpt.Text, excerpt.UnresolvedReason = &placeholder, nil
	return excerpt.Validate()
}

// validateUnresolvedReviewGuide runs domain validation on a copy whose
// daemon-filled fields hold placeholders, so the agent's part of the guide is
// checked against the same caps a resolved guide must meet. An empty text is
// legal there, so the placeholder adds no constraint of its own.
func validateUnresolvedReviewGuide(guide domain.ReviewGuide) error {
	placeholder := ""
	check := guide
	check.HeadSHA, check.ClaimDigest = "unresolved", "unresolved"
	check.Stops = make([]domain.ReviewStop, len(guide.Stops))
	for i, stop := range guide.Stops {
		stop.Excerpts = append([]domain.ReviewExcerpt(nil), stop.Excerpts...)
		for j := range stop.Excerpts {
			stop.Excerpts[j].Text = &placeholder
		}
		check.Stops[i] = stop
	}
	return check.Validate()
}

// reviewGuideStrings lists the guide's decoded strings, for the secret scan.
// The raw-body scan misses a credential spelled with JSON escapes; this is
// what a reviewer would actually see. It is given the guide as written, so
// it also covers what trimming cuts.
func reviewGuideStrings(guide domain.ReviewGuide) []string {
	out := append([]string(nil), guide.Questions...)
	if guide.Notes != nil {
		out = append(out, *guide.Notes)
	}
	for _, stop := range guide.Stops {
		out = append(out, stop.Title, stop.Summary)
		for _, e := range stop.Excerpts {
			out = append(out, e.Path, e.Caption)
		}
	}
	return out
}

// reviewGuideImport is what the reserved review guide entry contributes to an
// import: the secret findings in its body, the notice a ready item carries
// when the guide was trimmed or left out, and whether the entry becomes a
// claim at all. A guide left out yields no claim, so no ready item names it.
type reviewGuideImport struct {
	findings []Finding
	notice   *domain.ReviewGuideNotice
	omitted  bool
}

// reviewGuideClaim validates a review guide evidence entry against the
// launcher's declaration and decodes its body. The launcher declares the
// source with a fixed media type, head-independent binding (it writes the
// descriptor before any commit exists), and a sensitivity the item may carry
// inline; an entry that declares anything else was not written by the
// launcher, so it fails closed. A body over MaxReviewGuideBytes cannot be
// decoded within the read bound, so the guide is left out rather than
// failing the run; its raw body is still scanned up to maxScan, as a
// markdown entry's is. The secret scan reads the body and every decoded
// string before trimming, so text a trim cuts is still scanned.
func reviewGuideClaim(e export.EvidenceEntry, info blobInfo, maxScan int64) (reviewGuideImport, error) {
	if e.MediaType != "application/json" ||
		e.Provenance.HeadBinding != export.EvidenceHeadIndependent ||
		e.Provenance.SourceHeadSHA != "" ||
		e.Provenance.SensitivityClass == export.EvidenceSensitivityHigh {
		return reviewGuideImport{}, fmt.Errorf("evidence entry %q declares media type %q, head binding %q, sensitivity %q; "+
			"want the launcher's application/json, head_independent, not high_sensitivity: %w",
			e.Label, e.MediaType, e.Provenance.HeadBinding, e.Provenance.SensitivityClass, ErrEvidenceInvalid)
	}
	if ReviewGuideOmitted(e) {
		omission := domain.ReviewGuideOmissionFileTooLarge
		entry := reviewGuideImport{
			notice:  &domain.ReviewGuideNotice{Omission: &omission, Trims: []domain.ReviewGuideTrim{}},
			omitted: true,
		}
		if e.Size <= maxScan {
			body, err := readScanBlob(info, e.Digest)
			if err != nil {
				return reviewGuideImport{}, fmt.Errorf("read evidence entry %q: %w", e.Label, err)
			}
			entry.findings = scanText(e.Label, body)
		}
		return entry, nil
	}
	body, err := readScanBlob(info, e.Digest)
	if err != nil {
		return reviewGuideImport{}, fmt.Errorf("read evidence entry %q: %w", e.Label, err)
	}
	written, _, trims, err := decodeReviewGuide(body)
	if err != nil {
		return reviewGuideImport{}, fmt.Errorf("evidence entry %q: %w", e.Label, err)
	}
	var entry reviewGuideImport
	if len(trims) > 0 {
		entry.notice = &domain.ReviewGuideNotice{Trims: trims}
	}
	if findings := scanText(e.Label, body); len(findings) > 0 {
		entry.findings = findings
		return entry, nil
	}
	// One located finding is enough to reject the import; the raw scan's
	// line numbers are more useful, so this runs only when it found nothing.
	for i, s := range reviewGuideStrings(written) {
		if matches := scanText(e.Label, []byte(s)); len(matches) > 0 {
			match := matches[0]
			match.Detail = fmt.Sprintf("decoded review-guide string %d", i+1)
			entry.findings = []Finding{match}
			return entry, nil
		}
	}
	return entry, nil
}

// checkReviewGuideDigestUnique refuses an import whose guide claim shares its
// digest with another claim. A ready item names its guide claim by digest
// alone and requires exactly one claim to carry it, so such an import would
// publish a pull request whose ready item can never be built. Labels may
// otherwise share a digest; only the guide's is named this way.
func checkReviewGuideDigestUnique(claims []domain.AgentClaim) error {
	for _, guide := range claims {
		if guide.Label != export.ReviewGuideEvidenceLabel {
			continue
		}
		for _, other := range claims {
			if other.Label != guide.Label && other.Digest == guide.Digest {
				return fmt.Errorf("evidence entry %q has the same content as %q: %w",
					guide.Label, other.Label, ErrEvidenceInvalid)
			}
		}
	}
	return nil
}

// bindReviewGuideClaim binds the review guide claim to the commit this import
// built. The launcher can declare no head, because the daemon authors the
// commit after the agent exits; the guide's excerpts are read from this
// commit, so the claim names it. A recomputed artifact id keeps one id per
// complete provenance. An import that builds no commit leaves the claim
// head-independent, which no ready item accepts. buildClaims admits only a
// head-independent guide entry, so the rebind never overwrites a head.
func bindReviewGuideClaim(result *Result) error {
	for i := range result.Claims {
		claim := &result.Claims[i]
		if claim.Label != export.ReviewGuideEvidenceLabel {
			continue
		}
		claim.Provenance.HeadBinding = domain.HeadBound
		claim.Provenance.SourceHeadSHA = result.CommitSHA
		claim.Artifact = agentArtifactID(claim.Provenance, export.Digest(claim.Digest))
		if err := claim.Validate(); err != nil {
			return fmt.Errorf("bind review guide claim: %w", err)
		}
	}
	return nil
}
