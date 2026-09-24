package engine

import (
	"context"
	"errors"
	"fmt"
	"html"
	"regexp"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/publicationtext"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

const (
	clientPublicationRecipeV1 = "freeside.client-publication/v1"
	maxPublicMetadataBytes    = 8 << 10
)

var sourceIssueURL = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?/[A-Za-z0-9_.-]+/issues/[1-9][0-9]*$`)

// An immutable imported account cannot be repaired by re-reading this task.
// The existing task-level cancellation command is the recovery boundary;
// publish_blocked's card Stop does not carry that cancellation authority.
func (w *productionPublicationWorkflow) holdPublicMetadataTask(ctx context.Context, task productionPublicationTask, imported importer.Result, err error) (productionTaskOutcome, error) {
	const recovery = " This candidate's imported metadata cannot be replaced. Send the paired client's stop_task command to POST /commands with this task's task/project IDs, current task snapshot version and sync epoch; wait for cancellation=confirmed. If needed, admit an updated producer prompt package containing the public-output instructions. Then submit the request as a new task with a new command ID. Any existing PR remains unchanged: inspect and disposition it before submitting replacement work."
	return w.holdBlockedTask(ctx, task, imported, err.Error()+recovery, domain.HoldTrustBlocked)
}

// Only a complete canonical URL is a source reference. It is descriptive,
// never a bound issue or a grant of issue-closing authority.
func canonicalSourceIssue(source string) string {
	if len(source) > 2048 || !sourceIssueURL.MatchString(source) ||
		strings.Contains(source, "/./") || strings.Contains(source, "/../") {
		return ""
	}
	return source
}

// publicationMetadata consumes claims only after the caller authenticates the
// current export/import, specification, candidate and authorization. The recipe
// and those immutable inputs fix the bytes for retry, restart and drift repair.
// Do not change v1 rendering when adding a future recipe.
func publicationMetadata(p ProductionPublication, producer domain.InvocationID, claims []domain.AgentClaim) (string, string, error) {
	if err := p.Validate(); err != nil {
		return "", "", errors.New("publication metadata has invalid immutable recipe inputs")
	}
	// A literal record and the intake recipe's fallback both render the stored
	// literal text; neither reads the agent's publication claim.
	if p.Recipe == "" || p.Recipe == IntakePublicationRecipe {
		return p.Title, p.Body, nil
	}
	var selected *domain.AgentClaim
	for i := range claims {
		if claims[i].Label != export.PublicationEvidenceLabel {
			continue
		}
		if selected != nil {
			return "", "", errors.New("publication metadata contains duplicate public claims; exactly one whole-change publication.md file is required")
		}
		selected = &claims[i]
	}
	if selected == nil {
		return "", "", errors.New("publication requires public metadata from .freeside-evidence/publication.md; private summaries cannot be used")
	}
	c := *selected
	if c.Validate() != nil || c.Text == nil || c.Text.MediaType != domain.MediaTypeTextMarkdown ||
		c.Provenance.ProducerClass != domain.ProducerAgent || c.Provenance.ProducerInvocationID != producer ||
		c.Provenance.SensitivityClass != domain.SensitivityNormal || c.Provenance.HeadBinding != domain.HeadIndependent ||
		c.Provenance.SourceHeadSHA != "" {
		return "", "", errors.New("publication metadata is not one inline public Markdown claim from the current candidate producer")
	}
	text := c.Text.Content
	if err := screenPublicationText(text); err != nil {
		return "", "", err
	}
	// Feedback invocation IDs contain the client-supplied command ID. Provenance
	// authentication does not make those bytes safe for public output.
	if err := screenPublicationText(string(producer)); err != nil {
		return "", "", err
	}
	opening, description, found := strings.Cut(text, "\n")
	title := strings.TrimPrefix(opening, "# ")
	if !found || title == opening || title == "" || title != strings.TrimSpace(title) ||
		len(title) > maxProductionPublicationTitleBytes || strings.TrimSpace(description) == "" {
		return "", "", errors.New("publication metadata needs an opening '# Outcome title' line (at most 256 bytes) and a nonempty whole-change description")
	}
	// Raw HTML with escaped text keeps Markdown, HTML and entities inert. The
	// original artifact stays private and unchanged; it is never uploaded.
	body := fmt.Sprintf("## Agent-reported implementation (claim)\n\nProducer:\n\n<pre><code>%s</code></pre>\n\nArtifact digest: `%s`\n\n<pre>%s</pre>",
		html.EscapeString(string(producer)), c.Digest, html.EscapeString(strings.TrimSpace(description)))
	// A v1 record names the source issue in its prose. An authored record (even
	// when it falls back to v1 rendering) leaves the reference to the
	// publisher-owned source-reference section, which writes Closes/Refs or the
	// descriptive link (issue #1419 Part D, plan step 15).
	if p.SourceIssue != "" && !authoredPublicationRecipe(p.Recipe) {
		body += "\n\nSource issue: " + p.SourceIssue
	}
	if err := publish.ValidateCandidateBody(body); err != nil {
		return "", "", errors.New("publication metadata exceeds the public prose budget or conflicts with a publisher-owned section")
	}
	return title, body, nil
}

// Screen all input, including text outside the title/description and encoded
// forms, before rendering. Errors never repeat refused content.
func screenPublicationText(text string) error {
	return screenPublicationTextWithin(text, maxPublicMetadataBytes)
}

// screenPublicationTextWithin is screenPublicationText with the size cap made
// explicit, so recipe v2 can screen its larger authored fields under their own
// domain bounds while running the identical content checks. maxBytes caps the
// raw UTF-8 length and the commit-message screen; publish.ValidateCandidateBody
// still applies its own candidate-body ceiling, so a field larger than that
// ceiling is refused here and falls back to v1 rather than composing a PR body
// that cannot fit. v1 keeps its 8 KiB cap through screenPublicationText.
func screenPublicationTextWithin(text string, maxBytes int) error {
	return publicationtext.Screen(text, maxBytes, false)
}
