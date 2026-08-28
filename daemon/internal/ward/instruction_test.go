package ward

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
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
		"Composition: claude_explicit_bundle_v3\n\n" +
		"Apply each digest-delimited fenced-literal repository block only " +
		"within its named path scope. The deepest matching repository scope " +
		"takes precedence among repository blocks. Apply the final " +
		"fenced-literal operator-host block globally; it takes precedence " +
		"over every repository block. Markdown constructs inside one " +
		"instruction body do not extend past its enclosing fence.\n\n" +
		"## Trusted-Base Repository Instructions\n" +
		"\n### Scope \".\"\n\n" +
		"--- BEGIN REPOSITORY INSTRUCTION sha256:" +
		"34ffc874712e2c4b487cf9baec51b53af13d5f1c09dbb858e57ca4d00b71d6f7 ---\n" +
		"```\n" +
		"root instruction\n" +
		"```\n" +
		"--- END REPOSITORY INSTRUCTION sha256:" +
		"34ffc874712e2c4b487cf9baec51b53af13d5f1c09dbb858e57ca4d00b71d6f7 ---\n" +
		"\n### Scope \"service\"\n\n" +
		"--- BEGIN REPOSITORY INSTRUCTION sha256:" +
		"ce218fdcd8444f875ae97e90c4f0bd97030db3c42b36c2c93d4705e3adeac1eb ---\n" +
		"```\n" +
		"service instruction\n" +
		"```\n" +
		"--- END REPOSITORY INSTRUCTION sha256:" +
		"ce218fdcd8444f875ae97e90c4f0bd97030db3c42b36c2c93d4705e3adeac1eb ---\n" +
		"\n## Operator-Host Instructions\n\n" +
		"--- BEGIN OPERATOR-HOST INSTRUCTION sha256:" +
		hex.EncodeToString(hostSum[:]) + " ---\n" +
		"```\n" +
		"operator instruction\n" +
		"```\n" +
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

func TestComposeClaudeInstructionsUsesRootedPaths(t *testing.T) {
	snapshot := t.TempDir()
	root, err := os.OpenRoot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck // test-owned snapshot handle
	components := make([]string, 256)
	for i := range 255 {
		components[i] = strings.Repeat("x", 15)
	}
	components[0] = strings.Repeat("x", 21)
	components[255] = instructionFileName
	rel := strings.Join(components, "/")
	if len(rel) != 4095 {
		t.Fatalf("fixture path bytes = %d, want 4095", len(rel))
	}
	if err := root.MkdirAll(filepath.FromSlash(filepath.Dir(rel)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := root.WriteFile(filepath.FromSlash(rel), []byte("deep instruction\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, _, err := composeClaudeInstructions(HandoffSpec{}, &runState{seedSnapshotDir: snapshot})
	if err != nil {
		t.Fatalf("compose near-limit instruction: %v", err)
	}
	if !strings.Contains(string(body), "deep instruction\n") {
		t.Fatalf("deep instruction missing from bundle")
	}
	if runtime.GOOS == "linux" {
		rawDir := string([]byte{'c', 'a', 'f', 0xe9})
		rawPath := rawDir + "/" + instructionFileName
		if err := root.Mkdir(rawDir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := root.WriteFile(rawPath, []byte("raw instruction\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		body, _, err := composeClaudeInstructions(HandoffSpec{}, &runState{seedSnapshotDir: snapshot})
		if err != nil {
			t.Fatalf("compose raw non-UTF-8 instruction: %v", err)
		}
		if !strings.Contains(string(body), "raw instruction\n") {
			t.Fatal("raw non-UTF-8 instruction missing from bundle")
		}
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

// TestComposeClaudeInstructionsIsolatesMarkdownConstructs is the #946 core
// case: an unclosed or maximal Markdown fence inside an earlier trusted-base
// CLAUDE.md must not capture the later path scopes or the operator-host block.
// The bundle wraps every body in a backtick fence sized past the body's own
// runs, so no generated boundary is ever left inside a fence.
func TestComposeClaudeInstructionsIsolatesMarkdownConstructs(t *testing.T) {
	t.Parallel()
	snapshot := t.TempDir()
	rootBody := "root instruction\n```go\nunclosed backtick fence\n"
	// A nested source whose maximal internal run is six backticks and which
	// ends without a trailing newline: the wrapper must still out-length it.
	nestedBody := "nested instruction\n```\n````\n`````\n``````\nno trailing newline"
	writeInstructionFixture(t, snapshot, "CLAUDE.md", rootBody)
	writeInstructionFixture(t, snapshot, "vendor/lib/CLAUDE.md", nestedBody)
	host := []byte("operator instruction\n~~~yaml\nunclosed tilde fence\n")
	hostSum := sha256.Sum256(host)
	hs := HandoffSpec{Agent: AgentSpec{VendorInstructions: VendorInstructions{
		Vendor:  domain.AgentVendorClaude,
		Present: true,
		Digest:  domain.Digest("sha256:" + hex.EncodeToString(hostSum[:])),
		Body:    host,
	}}}

	body, binding, err := composeClaudeInstructions(
		hs, &runState{seedSnapshotDir: snapshot},
	)
	if err != nil {
		t.Fatalf("composeClaudeInstructions: %v", err)
	}

	// The security property: no scope header, BEGIN, or END boundary, nor the
	// operator-host heading, survives inside any wrapper fence. Three blocks:
	// root, vendor/lib, operator host.
	assertClaudeInstructionBlocksIsolated(t, body, 3)

	// Digests still bind the raw bodies, upstream of the fence framing: fencing
	// changed the bundle bytes but not the source-manifest or host authority.
	if binding.HostDigest != hex.EncodeToString(hostSum[:]) {
		t.Fatalf("host digest %q no longer binds the raw host body", binding.HostDigest)
	}
	manifest := sha256.New()
	for _, source := range []struct{ path, body string }{
		{"CLAUDE.md", rootBody},
		{"vendor/lib/CLAUDE.md", nestedBody},
	} {
		sum := sha256.Sum256([]byte(source.body))
		manifest.Write([]byte(source.path))
		manifest.Write([]byte{0})
		manifest.Write([]byte(hex.EncodeToString(sum[:])))
		manifest.Write([]byte{0})
	}
	if binding.RepositoryManifestDigest != hex.EncodeToString(manifest.Sum(nil)) {
		t.Fatalf("repository manifest digest %q no longer binds raw bodies",
			binding.RepositoryManifestDigest)
	}

	// Deterministic: identical trusted input yields identical bytes and digest.
	again, againBinding, err := composeClaudeInstructions(
		hs, &runState{seedSnapshotDir: snapshot},
	)
	if err != nil {
		t.Fatalf("composeClaudeInstructions (again): %v", err)
	}
	if string(again) != string(body) || againBinding.BundleDigest != binding.BundleDigest {
		t.Fatal("identical inputs produced a different bundle or bundle digest")
	}
}

// TestWriteClaudeInstructionLiteralSizesFence pins the wrapper-fence rule
// directly: the fence is one backtick longer than the body's longest backtick
// run, with a floor of three, so an N-backtick construct can never close it.
func TestWriteClaudeInstructionLiteralSizesFence(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		body      string
		wantFence int
	}{
		{"no backticks", "plain body\n", 3},
		{"three-run floor stays above", "a\n```\nb\n", 4},
		{"five run", "a\n`````\nb\n", 6},
		{"run at end without newline", "a\n``````", 7},
		{"empty body", "", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			writeClaudeInstructionLiteral(&buf, []byte(tc.body))
			lines := strings.Split(buf.String(), "\n")
			if len(lines) < 2 {
				t.Fatalf("literal too short: %q", buf.String())
			}
			open := testBacktickFenceRun(lines[0])
			if open != tc.wantFence {
				t.Fatalf("opening fence = %d backticks, want %d", open, tc.wantFence)
			}
			// The wrapper opens and closes with the same fence, and every
			// body line strictly between them has a shorter backtick run.
			closeIdx := -1
			for i := len(lines) - 1; i >= 1; i-- {
				if testBacktickFenceRun(lines[i]) > 0 {
					closeIdx = i
					break
				}
			}
			if closeIdx <= 0 {
				t.Fatalf("no closing fence found in %q", buf.String())
			}
			if run := testBacktickFenceRun(lines[closeIdx]); run != tc.wantFence {
				t.Fatalf("closing fence = %d backticks, want %d", run, tc.wantFence)
			}
			for _, line := range lines[1:closeIdx] {
				if run := testBacktickFenceRun(line); run >= tc.wantFence {
					t.Fatalf("internal run %d not shorter than fence %d", run, tc.wantFence)
				}
			}
		})
	}
}

// assertClaudeInstructionBlocksIsolated walks the composed bundle and fails if
// any generated boundary (the bundle/section headings, a scope header, or a
// BEGIN/END marker) appears inside a wrapper fence, proving each instruction
// body's Markdown cannot reach past its own fence. Mirror of exec's
// assertReviewInstructionBlocksIsolated (#949), adapted to the Claude headings.
func assertClaudeInstructionBlocksIsolated(t *testing.T, bundle []byte, wantBlocks int) {
	t.Helper()
	insideFence := false
	wantFence := false
	wantEnd := false
	fenceLength := 0
	blocks := 0
	for lineNumber, line := range strings.Split(string(bundle), "\n") {
		generatedHeading := line == "# Freeside Explicit Claude Instruction Bundle" ||
			line == "## Trusted-Base Repository Instructions" ||
			line == "## Operator-Host Instructions" || strings.HasPrefix(line, "### Scope ")
		begin := strings.HasPrefix(line, "--- BEGIN ")
		end := strings.HasPrefix(line, "--- END ")
		if (generatedHeading || begin || end) && insideFence {
			t.Fatalf("generated boundary at line %d was captured inside a fence: %q", lineNumber+1, line)
		}
		if begin {
			if wantFence || wantEnd {
				t.Fatalf("nested BEGIN boundary at line %d", lineNumber+1)
			}
			wantFence = true
			blocks++
			continue
		}
		run := testBacktickFenceRun(line)
		switch {
		case wantFence:
			if run < 3 {
				t.Fatalf("instruction body at line %d did not start with a wrapper fence", lineNumber+1)
			}
			insideFence = true
			wantFence = false
			fenceLength = run
			continue
		case insideFence && run >= fenceLength:
			insideFence = false
			wantEnd = true
			fenceLength = 0
			continue
		}
		if end {
			if !wantEnd {
				t.Fatalf("END boundary at line %d did not follow a closed wrapper", lineNumber+1)
			}
			wantEnd = false
			continue
		}
		if wantEnd {
			t.Fatalf("line %d intervened between wrapper and END boundary: %q", lineNumber+1, line)
		}
	}
	if insideFence || wantFence || wantEnd {
		t.Fatalf("unterminated generated block: inside=%v wantFence=%v wantEnd=%v", insideFence, wantFence, wantEnd)
	}
	if blocks != wantBlocks {
		t.Fatalf("generated blocks = %d, want %d", blocks, wantBlocks)
	}
}

// testBacktickFenceRun returns the length of a line that is a run of three or
// more backticks and nothing else, and zero otherwise. Mirror of exec's
// helper of the same shape (#949).
func testBacktickFenceRun(line string) int {
	if len(line) < 3 || strings.Trim(line, "`") != "" {
		return 0
	}
	return len(line)
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
