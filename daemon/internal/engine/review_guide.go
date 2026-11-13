package engine

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// maxReviewExcerptFileBytes caps the file an excerpt is read from. A larger
// file resolves as too_large without its bytes being read.
const maxReviewExcerptFileBytes = 1 << 20

// resolveReviewGuide builds the ready item's review guide from the producing
// invocation's imported claims. The stops, captions, questions, and notes are
// the agent's claim; each excerpt's text is read here from headSHA or baseSHA
// in the checkout that holds both, never taken from the handoff. No guide
// claim means no guide. An excerpt the daemon cannot read resolves with an
// unresolved reason rather than failing, so a stale pointer never blocks the
// item. The importer admits at most one guide claim per import and binds it
// to the commit it built, so any other claim shape is a contradiction.
//
// The guide grants nothing: it is display text from untrusted pointers.
func resolveReviewGuide(
	ctx context.Context, workDir, checkoutDir string, artifacts ArtifactStore,
	claims []domain.AgentClaim, producer domain.InvocationID, baseSHA, headSHA string,
) (*domain.ReviewGuide, error) {
	claim, found, err := reviewGuideClaim(claims, producer, headSHA)
	if err != nil || !found {
		return nil, err
	}
	body, err := loadArtifactBlob(artifacts, claim.Digest)
	if err != nil {
		return nil, fmt.Errorf("load review guide claim %s: %w", claim.Digest, err)
	}
	// The trims are the importer's to record: its Result carries them, and
	// this decode of the same bytes derives the same trimmed guide.
	guide, _, err := importer.DecodeReviewGuide(body)
	if err != nil {
		// The import accepted these bytes under the same decoder.
		return nil, fmt.Errorf("decode stored review guide %s: %w: %w", claim.Digest, err, domain.ErrParentKeyMismatch)
	}
	scratch, err := os.MkdirTemp(workDir, ".review-guide-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	runner, err := gitrun.New(gitrun.Options{Scratch: scratch})
	if err != nil {
		return nil, err
	}
	if _, err := runner.PinCheckout(ctx, checkoutDir); err != nil {
		return nil, fmt.Errorf("bind review guide checkout: %w", err)
	}
	objects := runner.Objects(ctx)
	defer objects.Close() //nolint:errcheck // stops the batch processes; reads already returned
	for i := range guide.Stops {
		for j := range guide.Stops[i].Excerpts {
			excerpt := &guide.Stops[i].Excerpts[j]
			commit := headSHA
			if excerpt.Side == domain.ReviewExcerptSideBase {
				commit = baseSHA
			}
			text, reason, err := readReviewExcerpt(ctx, runner, objects, commit, *excerpt)
			if err != nil {
				return nil, fmt.Errorf("resolve review guide stop %d excerpt %d: %w", i, j, err)
			}
			excerpt.Text, excerpt.UnresolvedReason = text, reason
		}
	}
	guide.HeadSHA, guide.ClaimDigest = headSHA, claim.Digest
	if err := guide.Validate(); err != nil {
		return nil, fmt.Errorf("resolved review guide: %w", err)
	}
	return &guide, nil
}

// reviewGuideClaim finds the one review guide claim among an import's claims.
// It must come from the producing invocation and be head-bound to the
// imported commit, as the importer leaves it.
func reviewGuideClaim(
	claims []domain.AgentClaim, producer domain.InvocationID, headSHA string,
) (domain.AgentClaim, bool, error) {
	var found []domain.AgentClaim
	for _, c := range claims {
		if c.Label == export.ReviewGuideEvidenceLabel {
			found = append(found, c)
		}
	}
	switch {
	case len(found) == 0:
		return domain.AgentClaim{}, false, nil
	case len(found) > 1:
		return domain.AgentClaim{}, false, fmt.Errorf("import carries %d review guide claims: %w",
			len(found), domain.ErrParentKeyMismatch)
	}
	c := found[0]
	if c.Validate() != nil || c.Provenance.ProducerInvocationID != producer ||
		c.Provenance.HeadBinding != domain.HeadBound || c.Provenance.SourceHeadSHA != headSHA ||
		c.Metadata.MediaType != domain.EvidenceMediaApplicationJSON {
		return domain.AgentClaim{}, false, fmt.Errorf(
			"review guide claim from %q is %q at %q, want head_bound at %q from %q: %w",
			c.Provenance.ProducerInvocationID, c.Provenance.HeadBinding, c.Provenance.SourceHeadSHA,
			headSHA, producer, domain.ErrParentKeyMismatch)
	}
	return c, true, nil
}

// readReviewExcerpt reads an excerpt's lines from commit. It returns the text,
// or the reason it cannot, or an error when git itself fails. The path is
// listed literally, so a pathspec character never widens it, and only a
// regular file at exactly that path is read: a symlink, submodule, or
// directory there resolves as path_missing.
func readReviewExcerpt(
	ctx context.Context, runner *gitrun.Runner, objects *gitrun.ObjectReader,
	commit string, excerpt domain.ReviewExcerpt,
) (*string, *domain.ReviewExcerptUnresolved, error) {
	unresolved := func(r domain.ReviewExcerptUnresolved) (*string, *domain.ReviewExcerptUnresolved, error) {
		return nil, &r, nil
	}
	listing, err := runner.Run(
		ctx, nil, "--literal-pathspecs", "ls-tree", "--full-tree", "-z", commit, "--", excerpt.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("list %s at %s: %w", excerpt.Path, commit, err)
	}
	blob := ""
	for _, record := range bytes.Split(listing, []byte{0}) {
		meta, name, ok := bytes.Cut(record, []byte{'\t'})
		if !ok || string(name) != excerpt.Path {
			continue
		}
		fields := strings.Fields(string(meta))
		if len(fields) == 3 && (fields[0] == "100644" || fields[0] == "100755") && fields[1] == "blob" {
			blob = fields[2]
		}
	}
	if blob == "" {
		return unresolved(domain.ReviewExcerptPathMissing)
	}
	info, err := objects.Info(ctx, blob)
	if err != nil {
		return nil, nil, fmt.Errorf("stat %s at %s: %w", excerpt.Path, commit, err)
	}
	if info.Size > maxReviewExcerptFileBytes {
		return unresolved(domain.ReviewExcerptTooLarge)
	}
	_, reader, err := objects.Open(ctx, blob)
	if err != nil {
		return nil, nil, fmt.Errorf("open %s at %s: %w", excerpt.Path, commit, err)
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s at %s: %w", excerpt.Path, commit, err)
	}
	if bytes.IndexByte(content, 0) >= 0 {
		return unresolved(domain.ReviewExcerptBinary)
	}
	// A final newline ends the last line rather than starting another.
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if len(content) == 0 {
		lines = nil
	}
	if excerpt.EndLine > len(lines) {
		return unresolved(domain.ReviewExcerptRangeOutOfFile)
	}
	text := strings.Join(lines[excerpt.StartLine-1:excerpt.EndLine], "\n")
	if !utf8.ValidString(text) {
		return unresolved(domain.ReviewExcerptBinary)
	}
	if len(text) > domain.MaxReviewExcerptTextBytes {
		return unresolved(domain.ReviewExcerptTooLarge)
	}
	return &text, nil, nil
}

// reviewGuideMatchesImport re-checks a stored guide against the checkpoint
// it rides on: a valid shape, the authorized head, and a claim digest naming
// exactly one imported claim, the reserved guide claim.
func reviewGuideMatchesImport(guide domain.ReviewGuide, claims []domain.AgentClaim, headSHA string) bool {
	if guide.Validate() != nil || guide.HeadSHA != headSHA {
		return false
	}
	named := 0
	for _, c := range claims {
		if c.Digest == guide.ClaimDigest {
			if c.Label != export.ReviewGuideEvidenceLabel {
				return false
			}
			named++
		}
	}
	return named == 1
}
