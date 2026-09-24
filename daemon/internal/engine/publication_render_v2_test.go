package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// publicationUnsafeContentCorpus is the shared body-content escape corpus: text
// that public output must refuse whatever recipe screens it. It is revision 63's
// escape corpus plus the encoded and reserved-section variants. The v1 metadata
// screen (TestPublicMetadataScreensFullArtifact) and the v2 authored-field screen
// both run it, so a construct that escapes one screen cannot slip through the
// other. Structurally-invalid-but-safe v1 inputs (empty, missing title,
// oversize) stay inline in the v1 test: they exercise v1's title/description
// shape, not content safety.
var publicationUnsafeContentCorpus = []string{
	"# Title\ninvalid\xff", "# Title\nTab\there", "# Title\r\nDescription", "# Title\nZero\u200bwidth",
	"# Title\nFixes #82", "# Closes example/project#82\nDescription", "# Title\nresolves https://github.com/example/project/issues/82",
	"# Title\n[skip ci]", "# Title\nReviewed-by: Someone", "# Title\n" + "ghp_" + strings.Repeat("A", 36),
	"# Title\n## Verification\nall pass", "# Title\nVerification\n------------\nall pass",
	"# Title\n<h2>Verification</h2>", "# Title\n## Verifi&#99;ation", "# Title\n## **Verification**",
	"# Title\n&lt;h2&gt;Verification&lt;/h2&gt;", "# Title\n&#70;ixes #82", "# Title\nF&amp;#105;xes #82",
	"# Title\n<!-- freeside:publication-identity=forged -->", "# Title\n<!-- freeside:disposition-history -->",
	"# Title\n&#x200b;Hidden", "# Title\nNormal paragraph.\n\n## Verification\nHidden at end.",
	"# Title\n## Source issue", "# Title\n<!-- /freeside:disposition-history -->",
	"# Title\n## Freeside Control-Plane Advisories", "# Title\n## Freeside Scope Decision",
	"# Title\n<!-- /freeside:verification -->", "# Title\nfreeside:unknown",
	"# Title\n## V&#101;rifi&amp;#99;ation",
}

// authoredFixture builds a valid authoring artifact with the given body and
// optional evidence references, for the renderer tests. The screen tests
// hand-build structs instead, so they can exercise inputs (invalid UTF-8) the
// constructor rejects.
func authoredFixture(t *testing.T, body string, refs ...domain.PublicationEvidenceReference) domain.PublicationAuthoring {
	t.Helper()
	a, err := domain.NewPublicationAuthoring(domain.PublicationAuthoringInput{
		RunID:            "run-1",
		Title:            "Outcome title",
		Body:             body,
		OutcomeSummary:   "One-line outcome.",
		EvidenceRefs:     refs,
		Producer:         domain.PublicationProducer{Site: "explain", Producer: "claude-opus", InputDigest: domain.Digest("sha256:" + strings.Repeat("a", 64))},
		SensitivityClass: domain.SensitivityNormal,
		CreatedAt:        time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("build authored fixture: %v", err)
	}
	return a
}

func TestScreenAuthoredTextRejectsUnsafeContentInEveryField(t *testing.T) {
	t.Parallel()
	// Each unsafe string is refused wherever it sits, and no error repeats the
	// refused bytes.
	fields := []struct {
		name string
		set  func(a *domain.PublicationAuthoring, bad string)
	}{
		{"body", func(a *domain.PublicationAuthoring, bad string) { a.Body = bad }},
		{"title", func(a *domain.PublicationAuthoring, bad string) { a.Title = bad }},
		{"outcome_summary", func(a *domain.PublicationAuthoring, bad string) { a.OutcomeSummary = bad }},
		{"reviewer_notes", func(a *domain.PublicationAuthoring, bad string) { a.ReviewerNotes = &bad }},
	}
	for _, f := range fields {
		for _, bad := range publicationUnsafeContentCorpus {
			a := domain.PublicationAuthoring{Title: "Safe title", Body: "Safe body.", OutcomeSummary: "Safe summary."}
			f.set(&a, bad)
			err := screenAuthoredText(a)
			if err == nil {
				t.Errorf("%s field accepted hostile fixture of %d bytes", f.name, len(bad))
			} else if strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "forged") {
				t.Fatal("error repeated refused content")
			}
			if err != nil && !strings.HasPrefix(err.Error(), "publication author "+f.name+": ") {
				t.Errorf("wrong field diagnostic: %v", err)
			}
		}
	}
}

func TestScreenAuthoredTextFirstFailure(t *testing.T) {
	a := domain.PublicationAuthoring{Title: "[skip ci]", Body: "## Verification", OutcomeSummary: "Closes #1"}
	if err := screenAuthoredText(a); err == nil || err.Error() != "publication author title: message_rules" {
		t.Fatalf("first failure = %v", err)
	}
	a.Title = "Safe title"
	if err := screenAuthoredText(a); err == nil || err.Error() != "publication author body: candidate_body_size_or_reserved_section" {
		t.Fatalf("next failure = %v", err)
	}
}

func TestScreenAuthoredTextAcceptsSafeArtifact(t *testing.T) {
	t.Parallel()
	notes := "Reviewer context, all inert."
	a := domain.PublicationAuthoring{
		Title: "Outcome title",
		// Benign emphasis and inline code must survive the delimiter-collapse pass:
		// collapsing them yields plain prose, not an unsafe token.
		Body:           "# Heading\n\nA paragraph mentioning @octocat and #123, with **bold**, _italic_, and `code`.",
		OutcomeSummary: "The change is complete.",
		ReviewerNotes:  &notes,
	}
	if err := screenAuthoredText(a); err != nil {
		t.Fatalf("safe artifact rejected: %v", err)
	}
}

func TestScreenAuthoredTextRejectsDelimiterSplicedSecrets(t *testing.T) {
	t.Parallel()
	// Each body splits a valid-format GitHub PAT (ghp_ + 36 chars) with a
	// construct GitHub collapses out of its visible text: emphasis, a code span,
	// backslash escapes, entity-encoded emphasis, link/image syntax whose
	// bracketed text the renderer keeps while dropping the target, and a code span
	// whose padding GitHub strips (a leading/trailing space, or interior newlines
	// that become spaces and are then padding-stripped). The raw source scan sees
	// the interrupting punctuation and passes; the visible form GitHub renders is
	// the intact token. The v2 field screen must refuse all of them, and must not
	// echo the reconstructed secret.
	lead, tail := strings.Repeat("z", 18), strings.Repeat("z", 16)
	for name, spliced := range map[string]string{
		"emphasis":          "ghp_" + lead + "**zz**" + tail,
		"code-span":         "ghp_" + lead + "`zz`" + tail,
		"backslash":         "ghp_" + lead + `\*\*zz\*\*` + tail,
		"entity-emphasis":   "ghp_" + lead + "&#42;&#42;zz&#42;&#42;" + tail,
		"inline-link":       "ghp_" + lead + "[zz](https://example.test)" + tail,
		"image":             "ghp_" + lead + "![zz](https://example.test)" + tail,
		"ref-link":          "ghp_" + lead + "[zz][r]" + tail,
		"code-span-padding": "ghp_" + lead + "` zz `" + tail,
		"code-span-newline": "ghp_" + lead + "`\nzz\n`" + tail,
	} {
		t.Run(name, func(t *testing.T) {
			a := domain.PublicationAuthoring{Title: "Safe title", Body: "# Fix\n\n" + spliced, OutcomeSummary: "Safe summary."}
			err := screenAuthoredText(a)
			if err == nil {
				t.Errorf("accepted a body that reconstructs a secret via %s", name)
			} else if strings.Contains(err.Error(), "ghp_") {
				t.Fatal("error repeated the reconstructed secret")
			}
		})
	}
}

func TestScreenAuthoredTextAcceptsBenignInteriorNewlineSpan(t *testing.T) {
	t.Parallel()
	// Boundary counterpart to the code-span-newline reject case: a code span with
	// an interior newline but no surrounding padding renders as "z z" (the newline
	// becomes a single space that survives), so it does not reconstruct a
	// contiguous token. The code-span normalization must strip padding only, not
	// collapse the surviving separator, so this benign body stays accepted.
	lead, tail := strings.Repeat("z", 18), strings.Repeat("z", 16)
	a := domain.PublicationAuthoring{
		Title:          "Safe title",
		Body:           "# Fix\n\nghp_" + lead + "`z\nz`" + tail,
		OutcomeSummary: "Safe summary.",
	}
	if err := screenAuthoredText(a); err != nil {
		t.Fatalf("benign interior-newline code span falsely rejected: %v", err)
	}
}

func TestRenderAuthoredBodyProseNeutralizesGFMExtensions(t *testing.T) {
	t.Parallel()
	// Strikethrough and table pipes are GFM extensions outside the allowlist.
	prose := renderAuthoredBodyProse("See ~~old~~ text in a | b table.")
	if strings.Contains(prose, "~~") {
		t.Errorf("strikethrough not neutralized:\n%s", prose)
	}
	if !strings.Contains(prose, `\|`) {
		t.Errorf("table pipe not escaped:\n%s", prose)
	}
	// A thematic break is not on the allowlist and must render as literal text,
	// whether or not it is spaced (a spaced rule also matches a list marker).
	for _, rule := range []string{"---", "***", "___", "- - -", "* * *"} {
		got := renderAuthoredBodyProse("before\n\n" + rule + "\n\nafter")
		if !strings.Contains(got, `\`+rule) {
			t.Errorf("thematic break %q not neutralized:\n%s", rule, got)
		}
	}
	// A real list item keeps its live marker; the rule pattern must not catch it.
	list := renderAuthoredBodyProse("- a real item")
	if strings.HasPrefix(list, `\`) {
		t.Errorf("list item wrongly neutralized as a thematic break:\n%s", list)
	}
}

func TestClientPublicationRecipeV2Identifier(t *testing.T) {
	t.Parallel()
	// The recipe identifiers are stored-record contract strings: a frozen record
	// names its recipe and the engine dispatches on it. Pin them so a rename is a
	// deliberate, reviewed change rather than a silent drift.
	if clientPublicationRecipeV2 != "freeside.client-publication/v2" {
		t.Errorf("recipe identifier drifted: %q", clientPublicationRecipeV2)
	}
	if IntakePublicationRecipe != "freeside.intake-publication/v1" {
		t.Errorf("intake recipe identifier drifted: %q", IntakePublicationRecipe)
	}
}

func TestRenderAuthoredPublicationNeutralizesActiveConstructs(t *testing.T) {
	t.Parallel()
	body := "Ping @octocat, see #123 plus owner/repo#5 and GH-7.\n" +
		"Commit deadbeefcafe. See https://example.test/x and www.example.test here.\n" +
		"Raw <b>bold</b> & <script>alert(1)</script>.\n" +
		"A [link](https://example.test/link) and an ![image](https://example.test/img.png) and a [ref][1]."
	a := authoredFixture(t, body)
	if err := screenAuthoredText(a); err != nil {
		t.Fatalf("fixture failed the screen: %v", err)
	}
	_, rendered := renderAuthoredPublication(a)

	for _, wrapped := range []string{"`@octocat`", "`#123`", "`owner/repo#5`", "`GH-7`", "`deadbeefcafe`", "`https://example.test/x`"} {
		if !strings.Contains(rendered, wrapped) {
			t.Errorf("token not wrapped in a code span: %q\n%s", wrapped, rendered)
		}
	}
	if !strings.Contains(rendered, "www.example.test") || strings.Contains(rendered, " www.example.test ") {
		t.Errorf("www host not code-wrapped:\n%s", rendered)
	}
	// Raw HTML shows as text, not markup.
	for _, raw := range []string{"<b>", "<script>", "</script>"} {
		if strings.Contains(rendered, raw) {
			t.Errorf("raw HTML survived: %q\n%s", raw, rendered)
		}
	}
	if !strings.Contains(rendered, "&lt;b&gt;bold&lt;/b&gt;") {
		t.Errorf("HTML not escaped inert:\n%s", rendered)
	}
	// Link and image targets are dropped; their text survives.
	for _, target := range []string{"example.test/link", "example.test/img"} {
		if strings.Contains(rendered, target) {
			t.Errorf("link/image target survived: %q\n%s", target, rendered)
		}
	}
	for _, text := range []string{"link", "image", "ref"} {
		if !strings.Contains(rendered, text) {
			t.Errorf("link/image/ref text dropped: %q\n%s", text, rendered)
		}
	}
}

func TestRenderAuthoredPublicationKeepsAllowedConstructsLive(t *testing.T) {
	t.Parallel()
	body := "# Heading one\n\n" +
		"A paragraph with *emphasis*, _more_, and `inline code`.\n\n" +
		"- bullet a\n- bullet b\n\n" +
		"1. first\n2. second\n\n" +
		"```\ncode @notmention #999 https://example.test/inert\n```"
	a := authoredFixture(t, body)
	if err := screenAuthoredText(a); err != nil {
		t.Fatalf("fixture failed the screen: %v", err)
	}
	_, rendered := renderAuthoredPublication(a)
	for _, live := range []string{"# Heading one", "*emphasis*", "_more_", "`inline code`", "- bullet a", "1. first"} {
		if !strings.Contains(rendered, live) {
			t.Errorf("allowed construct not kept live: %q\n%s", live, rendered)
		}
	}
	// Inside a fenced block GitHub autolinks nothing, so the content is emitted
	// verbatim rather than code-wrapped a second time.
	if !strings.Contains(rendered, "code @notmention #999 https://example.test/inert") {
		t.Errorf("fenced code content was rewritten:\n%s", rendered)
	}
}

func TestRenderAuthoredBodyProseClosesOpenFence(t *testing.T) {
	t.Parallel()
	// An author who opens a fence and never closes it must not swallow the
	// publisher's sections appended after the prose.
	prose := renderAuthoredBodyProse("Intro.\n\n```go\nfmt.Println(\"x\")")
	if !strings.HasSuffix(prose, "\n```") {
		t.Errorf("open fence not closed:\n%s", prose)
	}
	// A well-formed fence is left with its own single closing delimiter.
	closed := renderAuthoredBodyProse("```\nx\n```")
	if strings.Count(closed, "```") != 2 {
		t.Errorf("closed fence was altered:\n%s", closed)
	}
}

func TestRenderAuthoredBodyProseMatchesGFMFenceGrammar(t *testing.T) {
	t.Parallel()
	// A backtick run followed by a non-ASCII space (here NBSP) does not close a
	// fence under GFM, so the renderer must keep the fence open and append its own
	// closing delimiter; otherwise GitHub draws the appended publisher sections
	// into the code block. strings.TrimSpace would wrongly treat the NBSP line as
	// a close.
	nbsp := renderAuthoredBodyProse("```\ncode\n``` ")
	if !strings.HasSuffix(nbsp, "\n```") {
		t.Errorf("NBSP after a backtick run wrongly closed the fence:\n%q", nbsp)
	}
	// A backtick fence whose info string contains a backtick does not open a fence
	// under GFM, so the line after it must be neutralized as prose, not emitted
	// verbatim. Here @octocat must be code-wrapped, not left as a live mention.
	badInfo := renderAuthoredBodyProse("```foo`bar\n@octocat\n```")
	if !strings.Contains(badInfo, "`@octocat`") {
		t.Errorf("backtick in the info string wrongly opened a verbatim fence:\n%q", badInfo)
	}
}

func TestRenderAuthoredPublicationRendersEvidenceAndProvenanceInert(t *testing.T) {
	t.Parallel()
	ref := domain.PublicationEvidenceReference{ArtifactID: "artifact-1", Digest: domain.Digest("sha256:" + strings.Repeat("b", 64))}
	a := authoredFixture(t, "A short body.", ref)
	title, rendered := renderAuthoredPublication(a)
	if title != "Outcome title" {
		t.Errorf("title = %q", title)
	}
	for _, want := range []string{
		"Evidence artifacts:",
		"`artifact-1`",
		"`sha256:" + strings.Repeat("b", 64) + "`",
		"Produced by the `claude-opus` producer via the `explain` site.",
		"Artifact digest: `" + string(a.Digest) + "`.",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("missing inert evidence/provenance text: %q\n%s", want, rendered)
		}
	}
}

func TestRenderAuthoredPublicationIsDeterministic(t *testing.T) {
	t.Parallel()
	ref := domain.PublicationEvidenceReference{ArtifactID: "artifact-1", Digest: domain.Digest("sha256:" + strings.Repeat("c", 64))}
	body := "# Outcome\n\n" +
		"Shipped the fix for @octocat in #123 (owner/repo#5, GH-7).\n" +
		"Commit deadbeefcafe, docs at https://example.test/x and www.example.test.\n\n" +
		"- Escaped <b>tag</b> & entity.\n- A [link](https://example.test/l) dropped its target.\n\n" +
		"```\nverbatim @notmention #999\n```"
	a := authoredFixture(t, body, ref)
	if err := screenAuthoredText(a); err != nil {
		t.Fatalf("fixture failed the screen: %v", err)
	}
	_, first := renderAuthoredPublication(a)
	_, second := renderAuthoredPublication(a)
	if first != second {
		t.Fatal("renderer is not deterministic")
	}
	golden.Assert(t, "publication_render_v2_body", []byte(first))
}

// liveMarkdownCorpus is the rendered-body fixture the opt-in live check sends
// through GitHub's Markdown API. It exercises every active construct the screen
// permits as prose (mentions, issue and cross references, a commit token, bare
// URLs, raw HTML, links and images) so the API's own renderer proves them inert.
const liveMarkdownCorpus = "Ping @octocat, see #123, owner/repo#5, GH-7 and commit deadbeefcafe.\n" +
	"Docs at https://example.test/x and www.example.test.\n" +
	"Raw <b>bold</b> and a [link](https://example.test/l) and ![image](https://example.test/i.png)."

// liveMarkdownEnabled skips a live test unless the opt-in env var is set.
func liveMarkdownEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("FREESIDE_MARKDOWN_LIVE_TEST") != "1" {
		t.Skip("live GitHub Markdown rendering is opt-in: set FREESIDE_MARKDOWN_LIVE_TEST=1, " +
			"optionally FREESIDE_MARKDOWN_LIVE_TOKEN (a token to lift the anonymous rate limit) and " +
			"FREESIDE_MARKDOWN_LIVE_REPO (owner/name for the repository context, default octocat/Hello-World)")
	}
}

// renderMarkdownViaGitHub renders text through GitHub's own GFM Markdown API in
// the configured repository context and returns the HTML. It is the external
// oracle the opt-in live tests use: GitHub's renderer, not our model, decides
// what the visible output is.
func renderMarkdownViaGitHub(t *testing.T, text string) string {
	t.Helper()
	repo := os.Getenv("FREESIDE_MARKDOWN_LIVE_REPO")
	if repo == "" {
		repo = "octocat/Hello-World"
	}
	payload, err := json.Marshal(map[string]string{"text": text, "mode": "gfm", "context": repo})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "https://api.github.com/markdown", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	if tok := os.Getenv("FREESIDE_MARKDOWN_LIVE_TOKEN"); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("markdown API request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read markdown API response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("markdown API status %d: %s", resp.StatusCode, out)
	}
	return string(out)
}

// htmlTagRe strips HTML tags so the rendered HTML can be reduced to the visible
// text a reader sees. renderedPATRe matches a valid-format GitHub PAT payload.
var (
	htmlTagRe     = regexp.MustCompile(`<[^>]*>`)
	renderedPATRe = regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`)
)

func TestRenderAuthoredPublicationLiveMarkdownStaysInert(t *testing.T) {
	liveMarkdownEnabled(t)
	ref := domain.PublicationEvidenceReference{ArtifactID: "artifact-1", Digest: domain.Digest("sha256:" + strings.Repeat("b", 64))}
	a := authoredFixture(t, liveMarkdownCorpus, ref)
	if err := screenAuthoredText(a); err != nil {
		t.Fatalf("fixture failed the screen: %v", err)
	}
	_, rendered := renderAuthoredPublication(a)
	htmlOut := renderMarkdownViaGitHub(t, rendered)
	// The author prose must never produce an active link, an image, a mention
	// hovercard, or an issue/commit reference in GitHub's own renderer.
	for _, forbidden := range []string{"<a ", "<a>", "<img", "user-mention", "issue-link", "commit-link", "team-mention"} {
		if strings.Contains(htmlOut, forbidden) {
			t.Errorf("author prose produced %q in GitHub-rendered HTML:\n%s", forbidden, htmlOut)
		}
	}
}

// adversarialSecretCorpus builds authored bodies that split a valid-format PAT
// with a construct GitHub collapses out of its visible text. Every entry is
// designed to reconstruct once GitHub renders it; the screen must refuse them
// all (pinned offline by TestScreenAuthoredTextRejectsDelimiterSplicedSecrets),
// and the live oracle confirms the reconstruction is real against GitHub's own
// renderer. The ref-link entry carries its definition so GitHub resolves it.
func adversarialSecretCorpus() []string {
	lead, tail := strings.Repeat("z", 18), strings.Repeat("z", 16)
	return []string{
		"ghp_" + lead + "**zz**" + tail,
		"ghp_" + lead + "`zz`" + tail,
		"ghp_" + lead + `\*\*zz\*\*` + tail,
		"ghp_" + lead + "&#42;&#42;zz&#42;&#42;" + tail,
		"ghp_" + lead + "[zz](https://example.test)" + tail,
		"ghp_" + lead + "![zz](https://example.test)" + tail,
		"ghp_" + lead + "[zz][r]" + tail + "\n\n[r]: https://example.test",
		"ghp_" + lead + "` zz `" + tail,
		"ghp_" + lead + "`\nzz\n`" + tail,
	}
}

// TestScreenRefusesWhatGitHubReconstructs is the completeness oracle for the
// visible-text-reconstruction class. For each adversarial body it asks GitHub's
// own renderer for the visible text; whenever that text reconstructs a valid PAT
// payload, the screen must have refused the body. A GFM reduction the collapse
// fails to model would surface here as a body GitHub reconstructs but the screen
// accepted, turning "did I miss a GFM clause?" into a tested assertion rather
// than hand-enumeration. Opt-in: it needs network access to the Markdown API.
func TestScreenRefusesWhatGitHubReconstructs(t *testing.T) {
	liveMarkdownEnabled(t)
	for i, body := range adversarialSecretCorpus() {
		a := domain.PublicationAuthoring{Title: "Safe title", Body: "# Fix\n\n" + body, OutcomeSummary: "Safe summary."}
		screened := screenAuthoredText(a)

		visible := html.UnescapeString(htmlTagRe.ReplaceAllString(renderMarkdownViaGitHub(t, body), ""))
		reconstructs := renderedPATRe.MatchString(visible)

		switch {
		case reconstructs && screened == nil:
			t.Errorf("case %d: GitHub reconstructs a PAT but the screen accepted the body; an unmodeled GFM reduction leaks", i)
		case !reconstructs:
			// The fixture did not reconstruct in GitHub (for example a renderer
			// change); not a leak, but flag it so a strawman fixture is noticed.
			t.Logf("case %d: GitHub did not reconstruct a PAT for this fixture (screened=%v)", i, screened != nil)
		}
	}
}
