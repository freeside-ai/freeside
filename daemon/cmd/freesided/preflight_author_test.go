package main

import (
	"context"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
)

func authorPreflightRepository(t *testing.T, files map[string]string) (string, string) {
	t.Helper()
	root := t.TempDir()
	authorPreflightGit(t, root, "init", "-q")
	authorPreflightGit(t, root, "config", "user.name", "Test")
	authorPreflightGit(t, root, "config", "user.email", "test@example.invalid")
	for path, body := range files {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	authorPreflightGit(t, root, "add", ".")
	authorPreflightGit(t, root, "commit", "-qm", "fixture")
	return root, authorPreflightGit(t, root, "rev-parse", "HEAD")
}

func authorPreflightGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	return authorPreflightGitInput(t, root, nil, args...)
}

// authorPreflightGitInput runs a fixture Git command with stdin, which may be
// nil. Every fixture Git command in this file goes through it, so one scrub
// keeps them all on the repository under root.
func authorPreflightGitInput(t *testing.T, root string, stdin io.Reader, args ...string) string {
	t.Helper()
	command := osexec.Command("git", append([]string{"-C", root}, args...)...) //nolint:gosec // Test-owned fixture directory and fixed Git arguments.
	command.Stdin = stdin
	command.Env = gitenv.Scrubbed()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestFixtureGitIgnoresAmbientGitDir(t *testing.T) {
	for _, tc := range []struct {
		name string
		git  func(t *testing.T, root string, args ...string)
	}{
		{"gitForAdoptTest", func(t *testing.T, root string, args ...string) {
			t.Helper()
			gitForAdoptTest(t, root, nil, args...)
		}},
		{"authorPreflightGit", func(t *testing.T, root string, args ...string) {
			t.Helper()
			authorPreflightGit(t, root, args...)
		}},
		{"authorPreflightGitInput", func(t *testing.T, root string, args ...string) {
			t.Helper()
			authorPreflightGitInput(t, root, strings.NewReader(""), args...)
		}},
	} {
		// Serial: t.Setenv panics in a parallel test.
		t.Run(tc.name, func(t *testing.T) {
			// GIT_DIR names the decoy before the first command runs, so a
			// helper that loses its scrub writes there and never to a
			// repository the caller's own environment names.
			decoy := t.TempDir()
			t.Setenv("GIT_DIR", filepath.Join(decoy, ".git"))
			tc.git(t, decoy, "init", "-q")
			fixture := t.TempDir()
			tc.git(t, fixture, "init", "-q")
			tc.git(t, fixture, "config", "freeside.fixture", "target")

			fixtureConfig, err := os.ReadFile(filepath.Join(fixture, ".git", "config")) //nolint:gosec // G304: fixed config path under a test-owned temp repository
			if err != nil {
				t.Errorf("fixture command did not create its target repository: %v", err)
			} else if !strings.Contains(string(fixtureConfig), "fixture = target") {
				t.Error("fixture command did not update its target repository")
			}
			decoyConfig, err := os.ReadFile(filepath.Join(decoy, ".git", "config")) //nolint:gosec // G304: fixed config path under a test-owned temp repository
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(decoyConfig), "fixture = target") {
				t.Error("fixture command inherited the ambient GIT_DIR and updated the decoy repository")
			}
		})
	}
}

func TestPreflightAuthorInputsUseExactBase(t *testing.T) {
	root, base := authorPreflightRepository(t, map[string]string{
		"AGENTS.md": "repository rules\n", "nested/AGENTS.md": "nested rules\n",
		"nested/AGENTS.override.md":        "nested override\n",
		".github/PULL_REQUEST_TEMPLATE.md": "template\n",
	})
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(strings.Repeat("x", 300<<10)), 0o600); err != nil {
		t.Fatal(err)
	}
	host := engine.ReviewHostInstructions{Present: true, Body: []byte("distinct host rules\n")}
	if err := checkPreflightAuthorInputs(context.Background(), preflightConfig{RepositoryCheckout: root, BaseSHA: base}, host); err != nil {
		t.Fatalf("dirty worktree changed exact-base author input: %v", err)
	}
	tree, err := preflightBaseTree(context.Background(), root, base)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, 0, len(tree))
	for path := range tree {
		paths = append(paths, path)
	}
	selected := engine.SelectCodexReviewInstructionPaths(paths)
	if len(selected) != 2 || selected[0] != "AGENTS.md" || selected[1] != "nested/AGENTS.override.md" {
		t.Fatalf("selected exact-base instructions = %v", selected)
	}
}

func TestPreflightAuthorInputBounds(t *testing.T) {
	for _, tc := range []struct {
		name       string
		files      map[string]string
		wantReason string
	}{
		{"template exceeds its limit", map[string]string{".github/PULL_REQUEST_TEMPLATE.md": strings.Repeat("t", (64<<10)+1)}, "pr_template: 65537 bytes (limit 65536)"},
		{"composed instructions exceed their limit", map[string]string{"AGENTS.md": strings.Repeat("a", 256<<10)}, "instruction_snapshot:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, base := authorPreflightRepository(t, tc.files)
			err := checkPreflightAuthorInputs(t.Context(), preflightConfig{RepositoryCheckout: root, BaseSHA: base}, engine.ReviewHostInstructions{})
			if err == nil || !strings.Contains(err.Error(), tc.wantReason) {
				t.Fatalf("preflight refusal = %v, want %q", err, tc.wantReason)
			}
		})
	}
}

func TestPreflightAuthorInputsAcceptLargeSupportedTree(t *testing.T) {
	root, base := authorPreflightRepository(t, map[string]string{"AGENTS.md": "repository rules\n"})
	blob := authorPreflightGit(t, root, "rev-parse", base+":AGENTS.md")
	var entries strings.Builder
	fmt.Fprintf(&entries, "100644 blob %s\tAGENTS.md\n", blob)
	// Git plumbing keeps this large-repository fixture small on disk. These
	// regular files remain well below publication's 100,000-entry limit.
	for i := range 20_000 {
		fmt.Fprintf(&entries, "100644 blob %s\tfile-%05d-%s.txt\n", blob, i, strings.Repeat("x", 200))
	}
	tree := authorPreflightGitInput(t, root, strings.NewReader(entries.String()), "mktree")
	base = authorPreflightGit(t, root, "commit-tree", tree, "-p", base, "-m", "large tree")
	listing := authorPreflightGit(t, root, "ls-tree", "-rz", "--full-tree", base)
	if len(listing) <= 4<<20 || len(listing) >= 64<<20 {
		t.Fatalf("fixture listing is %d bytes, want between 4 and 64 MiB", len(listing))
	}
	if err := checkPreflightAuthorInputs(t.Context(), preflightConfig{RepositoryCheckout: root, BaseSHA: base}, engine.ReviewHostInstructions{}); err != nil {
		t.Fatalf("supported large tree with small author inputs was refused: %v", err)
	}
}

func TestPreflightAuthorInputsIgnoreReplacementRefs(t *testing.T) {
	root, base := authorPreflightRepository(t, map[string]string{
		"AGENTS.md": strings.Repeat("a", 256<<10),
	})
	originalBlob := authorPreflightGit(t, root, "rev-parse", base+":AGENTS.md")
	shortPath := filepath.Join(t.TempDir(), "short")
	if err := os.WriteFile(shortPath, []byte("short instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	shortBlob := authorPreflightGit(t, root, "hash-object", "-w", shortPath)
	authorPreflightGit(t, root, "replace", originalBlob, shortBlob)
	err := checkPreflightAuthorInputs(t.Context(), preflightConfig{RepositoryCheckout: root, BaseSHA: base}, engine.ReviewHostInstructions{})
	if err == nil || !strings.Contains(err.Error(), "instruction_snapshot:") {
		t.Fatalf("replacement ref changed exact-base author input: %v", err)
	}
}

func TestPreflightAuthorAcceptsTrackedTemplateSymlink(t *testing.T) {
	root, _ := authorPreflightRepository(t, map[string]string{"AGENTS.md": "repository rules\n"})
	path := filepath.Join(root, ".github", "PULL_REQUEST_TEMPLATE.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("template-target.md", path); err != nil {
		t.Fatal(err)
	}
	authorPreflightGit(t, root, "add", ".")
	authorPreflightGit(t, root, "commit", "-qm", "template symlink")
	base := authorPreflightGit(t, root, "rev-parse", "HEAD")
	if err := checkPreflightAuthorInputs(t.Context(), preflightConfig{RepositoryCheckout: root, BaseSHA: base}, engine.ReviewHostInstructions{}); err != nil {
		t.Fatalf("tracked template symlink was refused: %v", err)
	}
}

func TestPreflightAuthorAcceptsDistinct84401ByteBundle(t *testing.T) {
	repository := strings.Repeat("r", 41695)
	hostBody := []byte(strings.Repeat("h", 41695))
	bundle, _, err := exec.ComposeCodexReviewInstructions(
		exec.ReviewHostInstructionInput{Present: true, Body: hostBody},
		[]exec.ReviewInstructionSourceInput{{Path: "AGENTS.md", Body: []byte(repository)}},
	)
	if err != nil {
		t.Fatal(err)
	}
	hostBody = append(hostBody, []byte(strings.Repeat("h", 84401-len(bundle)))...)
	root, base := authorPreflightRepository(t, map[string]string{"AGENTS.md": repository})
	host := engine.ReviewHostInstructions{Present: true, Body: hostBody}
	if err := checkPreflightAuthorInputs(t.Context(), preflightConfig{RepositoryCheckout: root, BaseSHA: base}, host); err != nil {
		t.Fatal(err)
	}
	bundle, _, err = exec.ComposeCodexReviewInstructions(
		exec.ReviewHostInstructionInput{Present: true, Body: hostBody},
		[]exec.ReviewInstructionSourceInput{{Path: "AGENTS.md", Body: []byte(repository)}},
	)
	if err != nil || len(bundle) != 84401 || contentaddr.Sum(hostBody) == contentaddr.Sum([]byte(repository)) {
		t.Fatalf("distinct bundle size=%d err=%v", len(bundle), err)
	}
}
