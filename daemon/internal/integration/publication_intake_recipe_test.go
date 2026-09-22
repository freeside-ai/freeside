package integration_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	inferencefake "github.com/freeside-ai/freeside/daemon/internal/inference/fake"
)

const (
	intakeLiteralTitle = "Resolve freeside-ai/evidence-repo#82"
	intakeLiteralBody  = "Automated resolution of issue #82 in freeside-ai/evidence-repo, initiated by the \"freeside\" label. The implementation is derived from the specified specification, not the issue text."
)

func intakeRecipeMetadata() engine.ProductionPublication {
	metadata := productionPublicationMetadata()
	metadata.Title, metadata.Body = intakeLiteralTitle, intakeLiteralBody
	metadata.Recipe = engine.IntakePublicationRecipe
	return metadata
}

// TestIntakeRecipeAuthorsWithLiteralFallback proves a record that froze the
// intake recipe runs the publication-author role, and that an unavailable
// author renders the record's literal title and body rather than the agent's
// publication claim.
func TestIntakeRecipeAuthorsWithLiteralFallback(t *testing.T) {
	t.Run("authored", func(t *testing.T) {
		p := newAuthoredMetadataHarness(t, intakeRecipeMetadata(), nil)
		driver := scriptPublicationAuthor(t, p, inferencefake.Script{Response: inference.Response{
			Output: []byte(authoredExplainOutput), ComputeUnits: 5,
		}})
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		p.startAndRecordExport(t)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		body := assertAuthoredMetadata(t, p)
		if strings.Contains(body, intakeLiteralBody) {
			t.Fatal("authored intake body still carries the literal fallback")
		}
		if got := authorCallCount(driver); got != 1 {
			t.Fatalf("author calls = %d, want 1", got)
		}
	})
	t.Run("fallback", func(t *testing.T) {
		p := newAuthoredMetadataHarness(t, intakeRecipeMetadata(), nil)
		driver := scriptPublicationAuthor(t, p, inferencefake.Script{Err: errors.New("provider unavailable")})
		p.workflow = p.newEngine(t, productionCrashSeams{}, true)
		p.startAndRecordExport(t)
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
		prs := p.forge.pullRequests()
		if len(prs) != 1 || prs[0].Title != intakeLiteralTitle ||
			!strings.HasPrefix(prs[0].Body, intakeLiteralBody+"\n") {
			t.Fatalf("fallback PR = %+v, want the literal intake title and body", prs)
		}
		if strings.Contains(prs[0].Body, "## Agent-reported implementation (claim)") {
			t.Fatal("intake fallback rendered the agent's publication claim")
		}
		if got := authorCallCount(driver); got != 1 {
			t.Fatalf("author calls = %d, want 1", got)
		}
	})
}
