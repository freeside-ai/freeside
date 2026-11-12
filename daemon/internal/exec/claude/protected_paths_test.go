package claude

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
)

// The protected-path list renders last, after the resolved policy, one pattern
// per line; an empty list renders nothing, so an intent recorded before the
// list existed re-renders byte-identically.
func TestRenderPromptListsProtectedPaths(t *testing.T) {
	t.Parallel()
	inputs := stage.ProviderPromptInputs{
		Delivery:      launchDelivery,
		PromptPackage: []byte("prompt"), Specification: []byte("specification"), Policy: []byte("policy"),
	}
	const legacy = "prompt\n\n--- Approved work item specification ---\n\nspecification" +
		"\n\n--- Resolved per-run policy ---\n\npolicy\n"
	without, err := renderPromptParts(inputs)
	if err != nil {
		t.Fatal(err)
	}
	if without != legacy {
		t.Fatalf("prompt without protected paths changed:\n%q\nwant\n%q", without, legacy)
	}
	inputs.ProtectedPaths = []string{"**/.gitattributes", ".github/workflows/**"}
	with, err := renderPromptParts(inputs)
	if err != nil {
		t.Fatal(err)
	}
	section, ok := strings.CutPrefix(with, legacy+"\n--- Protected paths ---\n\n")
	if !ok {
		t.Fatalf("protected paths do not follow the policy:\n%s", with)
	}
	if !strings.HasSuffix(section, "\n\n**/.gitattributes\n.github/workflows/**\n") {
		t.Fatalf("patterns not listed one per line at the end:\n%s", section)
	}
	for _, phrase := range []string{"fails this run at import", "never verified, reviewed, or published"} {
		if !strings.Contains(section, phrase) {
			t.Fatalf("protected-path section omits %q:\n%s", phrase, section)
		}
	}
}

// Both writer prompts tell the writer what the rendered section means and how
// to stop instead of editing a protected path.
func TestPhase1AWriterPromptsHonorProtectedPaths(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"implementer.md", "remediator.md"} {
		body, err := os.ReadFile(filepath.Join(phase1APromptDir(t), name)) //nolint:gosec // fixed repository fixture
		if err != nil {
			t.Fatal(err)
		}
		for _, required := range []string{
			`The "Protected paths" section at the end of this prompt`,
			"fails the run at import",
			"Leave every matching path untouched.",
			"kind `" + string(domain.BlockedKindScopeExpansion) + "`",
		} {
			if !bytes.Contains(body, []byte(required)) {
				t.Errorf("%s omits %q", name, required)
			}
		}
	}
}
