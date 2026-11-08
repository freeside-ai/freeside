package importer

import (
	"encoding/json"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// MaxReviewGuideBytes caps the agent-written review guide file. The domain
// caps bound the decoded guide; this bounds the bytes read before decoding.
const MaxReviewGuideBytes = 64 << 10

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

// DecodeReviewGuide decodes and validates an agent-written review guide. It
// returns the guide with HeadSHA, ClaimDigest, and every excerpt's Text and
// UnresolvedReason empty: those are daemon facts the caller fills. The body
// is untrusted; any violation (over MaxReviewGuideBytes, invalid UTF-8, an
// unknown or missing key, or a domain cap or range breach) wraps
// ErrEvidenceInvalid. The domain caps are checked by domain.ReviewGuide's own
// Validate, so they live in one place.
func DecodeReviewGuide(body []byte) (domain.ReviewGuide, error) {
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
	if err := validateUnresolvedReviewGuide(guide); err != nil {
		return domain.ReviewGuide{}, fmt.Errorf("%w: %w", ErrEvidenceInvalid, err)
	}
	return guide, nil
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
// what a reviewer would actually see.
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

// reviewGuideClaim validates a review guide evidence entry against the
// launcher's declaration and decodes its body, returning the secret
// findings. The launcher declares the source with a fixed media type,
// head-independent binding (it writes the descriptor before any commit
// exists), and a sensitivity the item may carry inline; an entry that
// declares anything else was not written by the launcher, so it fails closed.
func reviewGuideClaim(e export.EvidenceEntry, info blobInfo) ([]Finding, error) {
	if e.MediaType != "application/json" ||
		e.Provenance.HeadBinding != export.EvidenceHeadIndependent ||
		e.Provenance.SourceHeadSHA != "" ||
		e.Provenance.SensitivityClass == export.EvidenceSensitivityHigh {
		return nil, fmt.Errorf("evidence entry %q declares media type %q, head binding %q, sensitivity %q; "+
			"want the launcher's application/json, head_independent, not high_sensitivity: %w",
			e.Label, e.MediaType, e.Provenance.HeadBinding, e.Provenance.SensitivityClass, ErrEvidenceInvalid)
	}
	if e.Size > MaxReviewGuideBytes {
		return nil, fmt.Errorf("evidence entry %q is %d bytes, limit %d: %w",
			e.Label, e.Size, MaxReviewGuideBytes, ErrEvidenceInvalid)
	}
	body, err := readScanBlob(info, e.Digest)
	if err != nil {
		return nil, fmt.Errorf("read evidence entry %q: %w", e.Label, err)
	}
	guide, err := DecodeReviewGuide(body)
	if err != nil {
		return nil, fmt.Errorf("evidence entry %q: %w", e.Label, err)
	}
	findings := scanText(e.Label, body)
	if len(findings) > 0 {
		return findings, nil
	}
	// One located finding is enough to reject the import; the raw scan's
	// line numbers are more useful, so this runs only when it found nothing.
	for i, s := range reviewGuideStrings(guide) {
		if matches := scanText(e.Label, []byte(s)); len(matches) > 0 {
			match := matches[0]
			match.Detail = fmt.Sprintf("decoded review-guide string %d", i+1)
			return []Finding{match}, nil
		}
	}
	return nil, nil
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
