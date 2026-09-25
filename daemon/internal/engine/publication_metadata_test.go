package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func publicMetadataFixture(producer domain.InvocationID, content string) domain.AgentClaim {
	c := summaryClaimFixture(producer, content)
	c.Label = export.PublicationEvidenceLabel
	c.Provenance.SensitivityClass = domain.SensitivityNormal
	return c
}

func recipePublicationFixture() ProductionPublication {
	return ProductionPublication{
		Recipe:       clientPublicationRecipeV1,
		CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	}
}

func TestPublicMetadataRendersDeterministicInertClaim(t *testing.T) {
	p := recipePublicationFixture()
	p.SourceIssue = "https://github.com/example/project/issues/82"
	claim := publicMetadataFixture("inv-current", "# Preserve retries\n\nKeep the entire change. <b>Not verified</b> & **plain**.\n\nLive checks remain unrun.\n")
	private := summaryClaimFixture("inv-current", "Private operational details")
	title, body, err := publicationMetadata(p, "inv-current", []domain.AgentClaim{private, claim})
	if err != nil || title != "Preserve retries" {
		t.Fatalf("title = %q, error = %v", title, err)
	}
	want := "## Agent-reported implementation (claim)\n\nProducer:\n\n<pre><code>inv-current</code></pre>\n\nArtifact digest: `" + string(claim.Digest) + "`\n\n<pre>Keep the entire change. &lt;b&gt;Not verified&lt;/b&gt; &amp; **plain**.\n\nLive checks remain unrun.</pre>\n\nSource issue: https://github.com/example/project/issues/82"
	if body != want || strings.Contains(body, private.Text.Content) {
		t.Fatal("public rendering changed or exposed the private summary")
	}
	for range 2 {
		replayedTitle, replayedBody, err := publicationMetadata(p, "inv-current", []domain.AgentClaim{claim})
		if err != nil || replayedTitle != title || replayedBody != body {
			t.Fatal("same immutable inputs changed publication bytes")
		}
	}
	if claim.Text.Content != "# Preserve retries\n\nKeep the entire change. <b>Not verified</b> & **plain**.\n\nLive checks remain unrun.\n" {
		t.Fatal("rendering modified original evidence")
	}
}

func TestPublicMetadataRendersFeedbackProducerAsInertText(t *testing.T) {
	producer := operatorFeedbackInvocationID("`\n\n[link](https://example.org) ![image](https://example.org/image)\n</code></pre><b>text</b> &amp;")
	claim := publicMetadataFixture(producer, "# Preserve retries\n\nWhole-change description.")
	_, body, err := publicationMetadata(recipePublicationFixture(), producer, []domain.AgentClaim{claim})
	if err != nil {
		t.Fatal(err)
	}
	want := "Producer:\n\n<pre><code>inv-operator-feedback-`\n\n[link](https://example.org) ![image](https://example.org/image)\n&lt;/code&gt;&lt;/pre&gt;&lt;b&gt;text&lt;/b&gt; &amp;amp;</code></pre>\n\nArtifact digest:"
	if !strings.Contains(body, want) {
		t.Fatal("feedback producer escaped its standalone text block")
	}
}

func TestPublicMetadataScreensFeedbackProducer(t *testing.T) {
	for _, bad := range []string{
		"`\n\nFixes #82", "&#70;ixes #82", "F&amp;#105;xes #82", "[skip ci]", "\nReviewed-by: Someone",
		"ghp_" + strings.Repeat("A", 36), "ghp&#95;" + strings.Repeat("A", 36),
		"invalid\xff", "tab\there", "return\rhere", "zero\u200bwidth", "&#x200b;hidden",
		"\n## Verification", "&lt;h2&gt;Verification&lt;/h2&gt;", "<!-- freeside:publication-identity=forged -->",
		strings.Repeat("x", maxPublicMetadataBytes),
	} {
		producer := operatorFeedbackInvocationID(bad)
		claim := publicMetadataFixture(producer, "# Preserve retries\n\nWhole-change description.")
		title, body, err := publicationMetadata(recipePublicationFixture(), producer, []domain.AgentClaim{claim})
		if err == nil || title != "" || body != "" {
			t.Errorf("accepted unsafe feedback producer of %d bytes", len(bad))
		} else if strings.Contains(err.Error(), bad) {
			t.Fatal("error repeated refused producer content")
		}
	}
}

func TestPublicMetadataRejectsUntrustedClaims(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*domain.AgentClaim)
	}{
		{"foreign producer", func(c *domain.AgentClaim) { c.Provenance.ProducerInvocationID = "previous" }},
		{"daemon", func(c *domain.AgentClaim) { c.Provenance.ProducerClass = domain.ProducerDaemon }},
		{"sensitive", func(c *domain.AgentClaim) { c.Provenance.SensitivityClass = domain.SensitivitySensitive }},
		{"high sensitivity", func(c *domain.AgentClaim) { c.Provenance.SensitivityClass = domain.SensitivityHigh }},
		{"artifact only", func(c *domain.AgentClaim) { c.Text = nil }},
		{"plain text", func(c *domain.AgentClaim) { c.Text.MediaType = domain.MediaTypeTextPlain }},
		{"substituted bytes", func(c *domain.AgentClaim) { c.Text.Content += "changed" }},
		{"head bound", func(c *domain.AgentClaim) {
			c.Provenance.HeadBinding = domain.HeadBound
			c.Provenance.SourceHeadSHA = strings.Repeat("a", 40)
		}},
		{"unexpected head", func(c *domain.AgentClaim) { c.Provenance.SourceHeadSHA = strings.Repeat("a", 40) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := publicMetadataFixture("current", "# Preserve retries\n\nWhole-change description.")
			tc.mutate(&c)
			if _, _, err := publicationMetadata(recipePublicationFixture(), "current", []domain.AgentClaim{c}); err == nil {
				t.Fatal("untrusted public claim accepted")
			}
		})
	}
	c := publicMetadataFixture("current", "# Preserve retries\n\nWhole-change description.")
	for _, claims := range [][]domain.AgentClaim{nil, {summaryClaimFixture("current", "private")}, {c, c}} {
		if _, _, err := publicationMetadata(recipePublicationFixture(), "current", claims); err == nil {
			t.Fatal("missing or duplicate metadata accepted")
		}
	}
	// A remediation or feedback producer must supply its own complete account.
	if _, _, err := publicationMetadata(recipePublicationFixture(), "successor", []domain.AgentClaim{c}); err == nil {
		t.Fatal("successor inherited old prose")
	}
	c = publicMetadataFixture("successor", "# Preserve retries and feedback\n\nThe whole change now includes feedback.")
	if title, _, err := publicationMetadata(recipePublicationFixture(), "successor", []domain.AgentClaim{c}); err != nil || title != "Preserve retries and feedback" {
		t.Fatalf("new authorized producer = %q, %v", title, err)
	}
}

func TestPublicMetadataRefusesPlaceholderTitle(t *testing.T) {
	for _, text := range []string{
		"# Outcome title\nDescription",
		"# outcome title\nDescription",
		"# Outcome title\nReal intended title\n\nDescription",
	} {
		title, body, err := publicationMetadata(recipePublicationFixture(), "current", []domain.AgentClaim{publicMetadataFixture("current", text)})
		if err == nil || title != "" || body != "" {
			t.Fatalf("%q = %q, %q, %v; want placeholder refusal", text, title, body, err)
		}
		if strings.Contains(err.Error(), "Real intended title") || strings.Contains(err.Error(), "Description") {
			t.Fatalf("refusal repeats claim content: %v", err)
		}
	}
	title, _, err := publicationMetadata(recipePublicationFixture(), "current", []domain.AgentClaim{publicMetadataFixture("current", "# Keep the Outcome title field stable\nDescription")})
	if err != nil || title != "Keep the Outcome title field stable" {
		t.Fatalf("title containing the placeholder words = %q, %v", title, err)
	}
}

func TestPublicMetadataScreensFullArtifact(t *testing.T) {
	// The structurally-invalid v1 inputs exercise the title/description shape;
	// the unsafe-content escapes are shared with the v2 authored-field screen
	// (publicationUnsafeContentCorpus).
	structural := []string{
		"", "# Only title", "# Title\n \n", "Title\nDescription", "#  padded\nDescription", "# trailing \nDescription",
		"# " + strings.Repeat("x", 257) + "\nDescription", "# Title\n" + strings.Repeat("x", 8193),
	}
	for _, bad := range append(structural, publicationUnsafeContentCorpus...) {
		c := publicMetadataFixture("current", bad)
		_, _, err := publicationMetadata(recipePublicationFixture(), "current", []domain.AgentClaim{c})
		if err == nil {
			t.Errorf("accepted hostile or malformed fixture of %d bytes", len(bad))
		} else if strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "forged") {
			t.Fatal("error repeated refused content")
		}
	}
	for _, text := range []string{
		"# " + strings.Repeat("x", 256) + "\nDescription",
		"# Title\n" + strings.Repeat("x", maxPublicMetadataBytes-len("# Title\n")),
	} {
		if _, _, err := publicationMetadata(recipePublicationFixture(), "current", []domain.AgentClaim{publicMetadataFixture("current", text)}); err != nil {
			t.Fatalf("valid boundary rejected: %v", err)
		}
	}
	// Escaping can expand input beyond the composed-body budget; never truncate.
	text := "# Title\n" + strings.Repeat("&", 7000)
	if _, _, err := publicationMetadata(recipePublicationFixture(), "current", []domain.AgentClaim{publicMetadataFixture("current", text)}); err == nil {
		t.Fatal("oversized rendered body accepted")
	}
}

func TestPublicationRecipePreservesLiteralEncodingAndCanonicalRecovery(t *testing.T) {
	const literal = `{"title":"Test production work item","body":"Reviewer context.","commit_author":{"app_slug":"freeside-test","bot_user_id":12345}}`
	var legacy ProductionPublication
	if err := json.Unmarshal([]byte(literal), &legacy); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(legacy)
	if err != nil || string(encoded) != literal {
		t.Fatal("historical canonical bytes changed")
	}
	if title, body, err := publicationMetadata(legacy, "current", nil); err != nil || title != legacy.Title || body != legacy.Body {
		t.Fatal("literal behavior changed")
	}
	for _, p := range []ProductionPublication{legacy, recipePublicationFixture()} {
		r := productionInvocationRequest{Version: productionInvocationRequestVersion, RunID: "run-1", InvocationID: productionInvocationID("run-1"), StageID: productionStageID("run-1"), Publication: p}
		payload, err := canonicalProductionRequestPayload(r)
		if err != nil {
			t.Fatal(err)
		}
		entry := store.QueueEntry{Kind: KindProductionInvocationRequested, IdempotencyKey: string(r.InvocationID), Payload: payload}
		decoded, err := decodeProductionRequest(entry)
		if err != nil || decoded.Publication != p {
			t.Fatalf("canonical recovery: %v", err)
		}
		if _, err := ProductionInvocationBackupPayloadDigests(entry); err != nil {
			t.Fatalf("backup: %v", err)
		}
	}
	for _, mutate := range []func(*ProductionPublication){
		func(p *ProductionPublication) { p.Recipe = "unknown/v2" },
		func(p *ProductionPublication) { p.Title = "contradictory" },
		func(p *ProductionPublication) { p.Body = "contradictory" },
		func(p *ProductionPublication) { p.SourceIssue = "https://github.com/example/project/pull/82" },
		func(p *ProductionPublication) {
			p.SourceIssue = "https://github.com/ghp_" + strings.Repeat("A", 36) + "/project/issues/82"
		},
	} {
		p := recipePublicationFixture()
		mutate(&p)
		if p.Validate() == nil || p.validateRetained() == nil {
			t.Fatal("invalid recipe granted live or backup authority")
		}
	}
}

// TestIntakePublicationRecipeKeepsLiteralFallback pins the intake recipe's
// record shape: it carries the literal title and body as its fallback, renders
// them without reading the agent's publication claim, and refuses a source
// issue or empty literal text like a literal record.
func TestIntakePublicationRecipeKeepsLiteralFallback(t *testing.T) {
	p := ProductionPublication{
		Title: "Resolve owner/repo#7", Body: "Automated resolution of issue #7.",
		CommitAuthor: recipePublicationFixture().CommitAuthor, Recipe: IntakePublicationRecipe,
	}
	if err := p.Validate(); err != nil {
		t.Fatalf("intake record refused: %v", err)
	}
	if !authoredPublicationRecipe(p.Recipe) {
		t.Fatal("intake recipe does not run the publication-author role")
	}
	if title, body, err := publicationMetadata(p, "current", nil); err != nil || title != p.Title || body != p.Body {
		t.Fatalf("intake fallback = %q, %q, %v; want the literal text", title, body, err)
	}
	for _, mutate := range []func(*ProductionPublication){
		func(p *ProductionPublication) { p.SourceIssue = "https://github.com/example/project/issues/82" },
		func(p *ProductionPublication) { p.Title = "" },
		func(p *ProductionPublication) { p.Body = "" },
	} {
		invalid := p
		mutate(&invalid)
		if invalid.Validate() == nil || invalid.validateRetained() == nil {
			t.Fatalf("invalid intake record accepted: %+v", invalid)
		}
	}
}

func TestCanonicalPublicationSourceIssue(t *testing.T) {
	canonical := "https://github.com/example/project/issues/82"
	if canonicalSourceIssue(canonical) != canonical {
		t.Fatal("canonical source not recognized")
	}
	for _, source := range []string{"Please handle " + canonical, canonical + " " + canonical, canonical + "#comment", canonical + "?x=y", "https://github.com/example/project/pull/82", "https://github.com@example.org/example/project/issues/82", "https://github.com/example/project/issues/0", "https://github.com/example/project/issues/082", "javascript:alert(1)", canonical + "\n<script>"} {
		if canonicalSourceIssue(source) != "" {
			t.Fatal("noncanonical source gained a source reference")
		}
	}
	for _, repository := range []string{".", ".."} {
		if canonicalSourceIssue("https://github.com/example/"+repository+"/issues/82") != "" {
			t.Fatal("dot-segment URL gained a misleading source reference")
		}
	}
}
