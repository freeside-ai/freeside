package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

const reviewGuideProducer domain.InvocationID = "inv-review-guide"

type reviewGuideRepo struct {
	dir, baseSHA, headSHA string
}

// newReviewGuideRepo builds a base commit and a head commit whose files cover
// each way an excerpt resolves.
func newReviewGuideRepo(t *testing.T) reviewGuideRepo {
	t.Helper()
	repo := t.TempDir()
	runRemediationGit(t, repo, nil, "init", "-q", "-b", "main", "--object-format=sha1")
	write := func(name string, body []byte) {
		t.Helper()
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(message string) string {
		t.Helper()
		runRemediationGit(t, repo, nil, "add", "-A")
		runRemediationGit(t, repo, nil, "commit", "-q", "-m", message)
		return strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))
	}
	write("a.txt", []byte("base one\nbase two\n"))
	baseSHA := commit("base")

	write("a.txt", []byte("head one\nhead two\nhead three\n"))
	write("unterminated.txt", []byte("first\nlast"))
	write("empty.txt", nil)
	write("binary.dat", []byte("text\x00more\n"))
	write("huge.txt", []byte(strings.Repeat("x", maxReviewExcerptFileBytes+1)))
	write("wide.txt", []byte(strings.Repeat(strings.Repeat("w", 200)+"\n", 60)))
	write("dir/inner.txt", []byte("inner\n"))
	write("star*.txt", []byte("literal\n"))
	if err := os.Symlink("a.txt", filepath.Join(repo, "link.txt")); err != nil {
		t.Fatal(err)
	}
	return reviewGuideRepo{dir: repo, baseSHA: baseSHA, headSHA: commit("head")}
}

func reviewGuideExcerpt(path string, side domain.ReviewExcerptSide, start, end int) map[string]any {
	return map[string]any{
		"path": path, "side": string(side), "start_line": start, "end_line": end, "caption": "look at " + path,
	}
}

func reviewGuideStopOf(title string, excerpts ...map[string]any) map[string]any {
	if excerpts == nil {
		excerpts = []map[string]any{}
	}
	return map[string]any{"title": title, "summary": "why " + title, "attention": "risk", "excerpts": excerpts}
}

func reviewGuideBody(t *testing.T, stops ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"stops": stops, "questions": []string{"Is it right?"}, "notes": nil})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// reviewGuideClaimFor stores body and returns the claim the importer leaves
// for it: head-bound to headSHA from the producer.
func reviewGuideClaimFor(
	t *testing.T, artifacts *findingAdjudicationArtifactStore, body []byte, headSHA string,
) domain.AgentClaim {
	t.Helper()
	digest := domain.Digest(contentaddr.Sum(body))
	if _, err := artifacts.Put(digest, strings.NewReader(string(body))); err != nil {
		t.Fatal(err)
	}
	return domain.AgentClaim{
		Label: export.ReviewGuideEvidenceLabel, Artifact: "artifact-review-guide", Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: reviewGuideProducer,
			HeadBinding: domain.HeadBound, SourceHeadSHA: headSHA,
			SensitivityClass: domain.SensitivitySensitive,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(body)),
			CreatedAt: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC),
			Source:    domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
		},
	}
}

func resolveReviewGuideFixture(
	t *testing.T, repo reviewGuideRepo, artifacts *findingAdjudicationArtifactStore, claims []domain.AgentClaim,
) (*domain.ReviewGuide, error) {
	t.Helper()
	return resolveReviewGuide(t.Context(), t.TempDir(), repo.dir, artifacts, claims,
		reviewGuideProducer, repo.baseSHA, repo.headSHA)
}

func TestResolveReviewGuideReadsEachExcerptFromItsSide(t *testing.T) {
	t.Parallel()
	repo := newReviewGuideRepo(t)
	artifacts := &findingAdjudicationArtifactStore{bodies: map[domain.Digest][]byte{}}
	head, base := domain.ReviewExcerptSideHead, domain.ReviewExcerptSideBase
	body := reviewGuideBody(t,
		reviewGuideStopOf("sides",
			reviewGuideExcerpt("a.txt", head, 2, 3),
			reviewGuideExcerpt("a.txt", base, 1, 2),
			reviewGuideExcerpt("unterminated.txt", head, 2, 2),
			reviewGuideExcerpt("star*.txt", head, 1, 1),
		),
		reviewGuideStopOf("missing",
			reviewGuideExcerpt("absent.txt", head, 1, 1),
			reviewGuideExcerpt("unterminated.txt", base, 1, 1),
			reviewGuideExcerpt("link.txt", head, 1, 1),
			reviewGuideExcerpt("dir", head, 1, 1),
		),
		reviewGuideStopOf("unreadable",
			reviewGuideExcerpt("a.txt", head, 3, 4),
			reviewGuideExcerpt("empty.txt", head, 1, 1),
			reviewGuideExcerpt("binary.dat", head, 1, 1),
			reviewGuideExcerpt("huge.txt", head, 1, 1),
		),
		reviewGuideStopOf("too wide", reviewGuideExcerpt("wide.txt", head, 1, 60)),
		reviewGuideStopOf("no excerpts"),
	)
	claim := reviewGuideClaimFor(t, artifacts, body, repo.headSHA)
	guide, err := resolveReviewGuideFixture(t, repo, artifacts, []domain.AgentClaim{claim})
	if err != nil {
		t.Fatal(err)
	}
	if guide.HeadSHA != repo.headSHA || guide.ClaimDigest != claim.Digest || len(guide.Stops) != 5 {
		t.Fatalf("guide = %#v", guide)
	}
	text := func(s string) *string { return &s }
	reason := func(r domain.ReviewExcerptUnresolved) *domain.ReviewExcerptUnresolved { return &r }
	want := [][]struct {
		text   *string
		reason *domain.ReviewExcerptUnresolved
	}{
		{{text: text("head two\nhead three")}, {text: text("base one\nbase two")}, {text: text("last")}, {text: text("literal")}},
		{
			{reason: reason(domain.ReviewExcerptPathMissing)},
			{reason: reason(domain.ReviewExcerptPathMissing)},
			{reason: reason(domain.ReviewExcerptPathMissing)},
			{reason: reason(domain.ReviewExcerptPathMissing)},
		},
		{
			{reason: reason(domain.ReviewExcerptRangeOutOfFile)},
			{reason: reason(domain.ReviewExcerptRangeOutOfFile)},
			{reason: reason(domain.ReviewExcerptBinary)},
			{reason: reason(domain.ReviewExcerptTooLarge)},
		},
		{{reason: reason(domain.ReviewExcerptTooLarge)}},
		{},
	}
	for i, stop := range guide.Stops {
		if len(stop.Excerpts) != len(want[i]) {
			t.Fatalf("stop %d has %d excerpts, want %d", i, len(stop.Excerpts), len(want[i]))
		}
		for j, got := range stop.Excerpts {
			w := want[i][j]
			if (got.Text == nil) != (w.text == nil) || (got.Text != nil && *got.Text != *w.text) ||
				(got.UnresolvedReason == nil) != (w.reason == nil) ||
				(got.UnresolvedReason != nil && *got.UnresolvedReason != *w.reason) {
				t.Errorf("stop %d excerpt %d = text %v reason %v, want text %v reason %v",
					i, j, deref(got.Text), deref(got.UnresolvedReason), deref(w.text), deref(w.reason))
			}
		}
	}
}

func deref[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestResolveReviewGuideWithoutClaimIsNil(t *testing.T) {
	t.Parallel()
	repo := newReviewGuideRepo(t)
	artifacts := &findingAdjudicationArtifactStore{bodies: map[domain.Digest][]byte{}}
	other := reviewGuideClaimFor(t, artifacts, []byte(`{}`), repo.headSHA)
	other.Label = "summary"
	guide, err := resolveReviewGuideFixture(t, repo, artifacts, []domain.AgentClaim{other})
	if err != nil || guide != nil {
		t.Fatalf("guide = %#v, err = %v; want no guide", guide, err)
	}
}

func TestResolveReviewGuideRejectsAClaimTheImporterWouldNotLeave(t *testing.T) {
	t.Parallel()
	repo := newReviewGuideRepo(t)
	cases := map[string]func(*[]domain.AgentClaim){
		"two claims": func(c *[]domain.AgentClaim) { *c = append(*c, (*c)[0]) },
		"other producer": func(c *[]domain.AgentClaim) {
			(*c)[0].Provenance.ProducerInvocationID = "inv-other"
		},
		"other head": func(c *[]domain.AgentClaim) {
			(*c)[0].Provenance.SourceHeadSHA = repo.baseSHA
		},
		"head independent": func(c *[]domain.AgentClaim) {
			(*c)[0].Provenance.HeadBinding, (*c)[0].Provenance.SourceHeadSHA = domain.HeadIndependent, ""
		},
		"markdown": func(c *[]domain.AgentClaim) {
			(*c)[0].Metadata.MediaType = domain.EvidenceMediaTextMarkdown
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			artifacts := &findingAdjudicationArtifactStore{bodies: map[domain.Digest][]byte{}}
			claims := []domain.AgentClaim{reviewGuideClaimFor(t, artifacts,
				reviewGuideBody(t, reviewGuideStopOf("stop")), repo.headSHA)}
			mutate(&claims)
			guide, err := resolveReviewGuideFixture(t, repo, artifacts, claims)
			if !errors.Is(err, domain.ErrParentKeyMismatch) || guide != nil {
				t.Fatalf("guide = %#v, err = %v; want ErrParentKeyMismatch", guide, err)
			}
		})
	}
	t.Run("stored bytes no longer decode", func(t *testing.T) {
		t.Parallel()
		artifacts := &findingAdjudicationArtifactStore{bodies: map[domain.Digest][]byte{}}
		claim := reviewGuideClaimFor(t, artifacts, []byte(`{"stops":[]}`), repo.headSHA)
		guide, err := resolveReviewGuideFixture(t, repo, artifacts, []domain.AgentClaim{claim})
		if !errors.Is(err, domain.ErrParentKeyMismatch) || guide != nil {
			t.Fatalf("guide = %#v, err = %v; want ErrParentKeyMismatch", guide, err)
		}
	})
}

func TestReviewGuideMatchesImportRechecksTheStoredGuide(t *testing.T) {
	t.Parallel()
	repo := newReviewGuideRepo(t)
	artifacts := &findingAdjudicationArtifactStore{bodies: map[domain.Digest][]byte{}}
	claim := reviewGuideClaimFor(t, artifacts,
		reviewGuideBody(t, reviewGuideStopOf("stop", reviewGuideExcerpt("a.txt", domain.ReviewExcerptSideHead, 1, 1))),
		repo.headSHA)
	claims := []domain.AgentClaim{claim}
	guide, err := resolveReviewGuideFixture(t, repo, artifacts, claims)
	if err != nil {
		t.Fatal(err)
	}
	if !reviewGuideMatchesImport(*guide, claims, repo.headSHA) {
		t.Fatal("resolved guide does not match its own import")
	}
	relabeled := claim
	relabeled.Label = "summary"
	cases := map[string]struct {
		guide  func(domain.ReviewGuide) domain.ReviewGuide
		claims []domain.AgentClaim
	}{
		"other head": {guide: func(g domain.ReviewGuide) domain.ReviewGuide {
			g.HeadSHA = repo.baseSHA
			return g
		}, claims: claims},
		"digest names no claim": {guide: func(g domain.ReviewGuide) domain.ReviewGuide {
			g.ClaimDigest = adjudicationDigest("e")
			return g
		}, claims: claims},
		"digest names another label": {
			guide:  func(g domain.ReviewGuide) domain.ReviewGuide { return g },
			claims: []domain.AgentClaim{relabeled},
		},
		"invalid shape": {guide: func(g domain.ReviewGuide) domain.ReviewGuide {
			g.Stops = nil
			return g
		}, claims: claims},
	}
	for name, c := range cases {
		if reviewGuideMatchesImport(c.guide(*guide), c.claims, repo.headSHA) {
			t.Errorf("%s: tampered guide matches the import", name)
		}
	}
}

// TestReentryViewCarriesNoReviewGuide pins that an in-place re-entry, which
// exports nothing, presents no guide: the guide belongs to the head its
// producer imported.
func TestReentryViewCarriesNoReviewGuide(t *testing.T) {
	t.Parallel()
	view := productionReentryCheckpoint{HeadSHA: strings.Repeat("a", 40)}.view("owner/repo")
	if view.ReviewGuide != nil {
		t.Fatalf("re-entry view review guide = %#v", view.ReviewGuide)
	}
}
