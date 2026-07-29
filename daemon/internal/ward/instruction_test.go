package ward

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestComposeClaudeInstructionsBindsTrustedBaseScopes(t *testing.T) {
	t.Parallel()
	snapshot := t.TempDir()
	writeInstructionFixture(t, snapshot, "CLAUDE.md", "root instruction\n")
	writeInstructionFixture(t, snapshot, "service/CLAUDE.md", "service instruction\n")
	writeInstructionFixture(t, snapshot, ".git/CLAUDE.md", "git metadata poison\n")
	// A nested .git is ordinary repository content (a vendored checkout, a
	// committed fixture). Walking into it would hit expansion's fatal .git
	// refusal and make every run on such a repository unstartable.
	writeInstructionFixture(t, snapshot, "vendor/dep/.git/CLAUDE.md", "nested git poison\n")
	host := []byte("operator instruction\n")
	hostSum := sha256.Sum256(host)
	hs := HandoffSpec{Agent: AgentSpec{VendorInstructions: VendorInstructions{
		Vendor:  domain.AgentVendorClaude,
		Present: true,
		Digest:  domain.Digest("sha256:" + hex.EncodeToString(hostSum[:])),
		Body:    host,
	}}}

	body, binding, err := composeClaudeInstructions(
		hs,
		&runState{seedSnapshotDir: snapshot},
	)
	if err != nil {
		t.Fatalf("composeClaudeInstructions: %v", err)
	}
	want := "# Freeside Explicit Claude Instruction Bundle\n\n" +
		"Composition: claude_explicit_bundle_v2\n\n" +
		"Apply each digest-delimited repository block only within its named " +
		"path scope. The deepest matching repository scope takes precedence " +
		"among repository blocks. Apply the final operator-host block " +
		"globally; it takes precedence over every repository block.\n\n" +
		"## Trusted-Base Repository Instructions\n" +
		"\n### Scope \".\"\n\n" +
		"--- BEGIN REPOSITORY INSTRUCTION sha256:" +
		"34ffc874712e2c4b487cf9baec51b53af13d5f1c09dbb858e57ca4d00b71d6f7 ---\n" +
		"root instruction\n" +
		"--- END REPOSITORY INSTRUCTION sha256:" +
		"34ffc874712e2c4b487cf9baec51b53af13d5f1c09dbb858e57ca4d00b71d6f7 ---\n" +
		"\n### Scope \"service\"\n\n" +
		"--- BEGIN REPOSITORY INSTRUCTION sha256:" +
		"ce218fdcd8444f875ae97e90c4f0bd97030db3c42b36c2c93d4705e3adeac1eb ---\n" +
		"service instruction\n" +
		"--- END REPOSITORY INSTRUCTION sha256:" +
		"ce218fdcd8444f875ae97e90c4f0bd97030db3c42b36c2c93d4705e3adeac1eb ---\n" +
		"\n## Operator-Host Instructions\n\n" +
		"--- BEGIN OPERATOR-HOST INSTRUCTION sha256:" +
		hex.EncodeToString(hostSum[:]) + " ---\n" +
		"operator instruction\n" +
		"--- END OPERATOR-HOST INSTRUCTION sha256:" +
		hex.EncodeToString(hostSum[:]) + " ---\n"
	if string(body) != want {
		t.Fatalf("bundle = %q, want %q", body, want)
	}
	if binding.CompositionVersion != instructionCompositionVersion ||
		binding.HostDigest != hex.EncodeToString(hostSum[:]) {
		t.Fatalf("binding = %+v", binding)
	}
	manifest := sha256.New()
	for _, source := range []struct {
		path, body string
	}{
		{"CLAUDE.md", "root instruction\n"},
		{"service/CLAUDE.md", "service instruction\n"},
	} {
		sum := sha256.Sum256([]byte(source.body))
		manifest.Write([]byte(source.path))
		manifest.Write([]byte{0})
		manifest.Write([]byte(hex.EncodeToString(sum[:])))
		manifest.Write([]byte{0})
	}
	if binding.RepositoryManifestDigest != hex.EncodeToString(manifest.Sum(nil)) {
		t.Fatalf("repository manifest digest = %s", binding.RepositoryManifestDigest)
	}
	bundleSum := sha256.Sum256(body)
	if binding.BundleDigest != hex.EncodeToString(bundleSum[:]) {
		t.Fatalf("bundle digest = %s", binding.BundleDigest)
	}
	if strings.Contains(string(body), "git metadata poison") {
		t.Fatal("bundle included repository metadata as instructions")
	}
}

func TestComposeClaudeInstructionsExpandsTrustedSnapshotImports(t *testing.T) {
	t.Parallel()
	snapshot := t.TempDir()
	writeInstructionFixture(
		t, snapshot, "CLAUDE.md",
		"# Pointer\n\n@AGENTS.md\n",
	)
	writeInstructionFixture(
		t, snapshot, "AGENTS.md",
		"# Repository Rules\n\n@rules/testing.md\n",
	)
	writeInstructionFixture(
		t, snapshot, "rules/testing.md",
		"Run the complete test suite.\n",
	)

	body, _, err := composeClaudeInstructions(
		HandoffSpec{},
		&runState{seedSnapshotDir: snapshot},
	)
	if err != nil {
		t.Fatalf("composeClaudeInstructions: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"# Pointer", "# Repository Rules", "Run the complete test suite.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("bundle omitted imported content %q", want)
		}
	}
	if strings.Contains(got, "@AGENTS.md") ||
		strings.Contains(got, "@rules/testing.md") {
		t.Fatal("bundle retained an import whose target is unavailable in the mounted volume")
	}
}

func TestComposeClaudeInstructionsRefusesUnboundedOrIrregularSources(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		stage func(*testing.T, string)
	}{
		{
			name: "oversized repository source",
			stage: func(t *testing.T, root string) {
				writeInstructionFixture(
					t,
					root,
					"CLAUDE.md",
					strings.Repeat("x", int(domain.MaxVendorInstructionBytes)+1),
				)
			},
		},
		{
			name: "symbolic link source",
			stage: func(t *testing.T, root string) {
				target := filepath.Join(root, "target")
				if err := os.WriteFile(target, []byte("poison"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(root, instructionFileName)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "import outside snapshot",
			stage: func(t *testing.T, root string) {
				writeInstructionFixture(t, root, "CLAUDE.md", "@../outside.md\n")
			},
		},
		{
			name: "symbolic link import",
			stage: func(t *testing.T, root string) {
				target := filepath.Join(root, "rules.md")
				if err := os.WriteFile(target, []byte("rules"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(root, "AGENTS.md")); err != nil {
					t.Fatal(err)
				}
				writeInstructionFixture(t, root, "CLAUDE.md", "@AGENTS.md\n")
			},
		},
		{
			name: "symbolic link parent import",
			stage: func(t *testing.T, root string) {
				outside := t.TempDir()
				if err := os.WriteFile(
					filepath.Join(outside, "rules.md"), []byte("foreign rules"), 0o600,
				); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
					t.Fatal(err)
				}
				writeInstructionFixture(t, root, "CLAUDE.md", "@linked/rules.md\n")
			},
		},
		{
			name: "import cycle",
			stage: func(t *testing.T, root string) {
				writeInstructionFixture(t, root, "CLAUDE.md", "@AGENTS.md\n")
				writeInstructionFixture(t, root, "AGENTS.md", "@CLAUDE.md\n")
			},
		},
		{
			name: "aggregate repository sources",
			stage: func(t *testing.T, root string) {
				part := strings.Repeat("x", int(domain.MaxVendorInstructionBytes/2)+1)
				writeInstructionFixture(t, root, "CLAUDE.md", part)
				writeInstructionFixture(t, root, "service/CLAUDE.md", part)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			tc.stage(t, root)
			_, _, err := composeClaudeInstructions(
				HandoffSpec{},
				&runState{seedSnapshotDir: root},
			)
			if !errors.Is(err, ErrConformance) {
				t.Fatalf("composeClaudeInstructions = %v, want ErrConformance", err)
			}
		})
	}
}

// A managed repository's CLAUDE.md is ordinary markdown, and ordinary
// markdown starts lines with "@": a decorator in a fenced example, a CSS
// at-rule, a handle, an asset suffix. None of those name an instruction file,
// and refusing to compose over them would make every run against such a
// repository unstartable. They keep their literal text; the hostile shapes
// above still fail closed.
func TestComposeClaudeInstructionsKeepsUnresolvableAtLinesLiteral(t *testing.T) {
	t.Parallel()
	snapshot := t.TempDir()
	lines := []string{
		"@media (prefers-color-scheme: dark)",
		"@Override",
		"@freeside-ai/freeside",
		"@logo@2x.png",
		"@docs",
		"@AGENTS.md/nested.md",
	}
	writeInstructionFixture(t, snapshot, "docs/keep.md", "unreferenced\n")
	writeInstructionFixture(t, snapshot, "AGENTS.md", "real rules\n")
	writeInstructionFixture(
		t, snapshot, "CLAUDE.md",
		"# Rules\n\n"+strings.Join(lines, "\n")+"\n\n@AGENTS.md\n",
	)

	body, _, err := composeClaudeInstructions(
		HandoffSpec{},
		&runState{seedSnapshotDir: snapshot},
	)
	if err != nil {
		t.Fatalf("composeClaudeInstructions: %v", err)
	}
	got := string(body)
	for _, line := range lines {
		if !strings.Contains(got, line) {
			t.Errorf("bundle dropped unresolvable literal line %q", line)
		}
	}
	if !strings.Contains(got, "real rules") {
		t.Error("bundle omitted the resolvable import beside the literal lines")
	}
	if strings.Contains(got, "unreferenced") {
		t.Error("bundle inlined a directory import target")
	}
}

func writeInstructionFixture(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
